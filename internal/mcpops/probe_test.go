package mcpops_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpops"
)

type fakeProbeResponse struct {
	initializeErr error
	tools         []mcpops.Tool
	listErr       error
}

type probeCall struct {
	server       mcpconfig.Server
	deadline     time.Time
	listDeadline time.Time
	listed       bool
	closed       bool
}

type fakeProber struct {
	responses map[string]fakeProbeResponse
	calls     []probeCall
}

func (f *fakeProber) Initialize(ctx context.Context, server mcpconfig.Server) (mcpops.InitializedServer, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("fake expected deadline")
	}
	response := f.responses[server.Name]
	if response.initializeErr != nil {
		return nil, response.initializeErr
	}
	f.calls = append(f.calls, probeCall{server: server, deadline: deadline})
	return &fakeSession{owner: f, call: len(f.calls) - 1, response: response}, nil
}

type fakeSession struct {
	owner    *fakeProber
	call     int
	response fakeProbeResponse
}

func (s *fakeSession) ListTools(ctx context.Context) ([]mcpops.Tool, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("fake expected tools/list deadline")
	}
	s.owner.calls[s.call].listed = true
	s.owner.calls[s.call].listDeadline = deadline
	return s.response.tools, s.response.listErr
}

func (s *fakeSession) Close() error {
	s.owner.calls[s.call].closed = true
	return nil
}

func TestProbeValidatesAndReportsEveryServer(t *testing.T) {
	const privateValue = "placeholder-sensitive-value"
	document := []byte(`{
		"schema":"buzz-managed-mcp",
		"version":1,
		"servers":[
			{"name":"warehouse","kind":"sql","auth":"env"},
			{"name":"space","kind":"genie","resource":["example_space"],"auth":"env"},
			{"name":"search","kind":"ai-search","resource":["example_catalog","example_schema","example_index"],"auth":"env"}
		]
	}`)
	fake := &fakeProber{responses: map[string]fakeProbeResponse{
		"warehouse": {tools: []mcpops.Tool{{Name: "run_statement"}, {Name: "list_warehouses"}}},
		"space":     {tools: []mcpops.Tool{{Name: "ask_question"}}},
		"search":    {listErr: errors.New("DATABRICKS_TOKEN=" + privateValue)},
	}}

	started := time.Now()
	got, err := mcpops.Probe(context.Background(), document, fake, mcpops.ProbeOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(got.Servers) != 3 || len(fake.calls) != 3 {
		t.Fatalf("servers/calls = %d/%d, want 3/3", len(got.Servers), len(fake.calls))
	}
	if names := got.Servers[0].ToolNames; !reflect.DeepEqual(names, []string{"list_warehouses", "run_statement"}) {
		t.Fatalf("sorted warehouse names = %v", names)
	}
	if got.Servers[0].ToolCount != 2 || got.Servers[1].ToolCount != 1 {
		t.Fatalf("tool counts = %d, %d", got.Servers[0].ToolCount, got.Servers[1].ToolCount)
	}
	if got.Servers[2].ToolCount != 0 || len(got.Servers[2].ToolNames) != 0 {
		t.Fatalf("failed result unexpectedly has tools: %#v", got.Servers[2])
	}
	if strings.Contains(got.Servers[2].Error, privateValue) || !strings.Contains(got.Servers[2].Error, "[REDACTED]") {
		t.Fatalf("probe error was not redacted: %q", got.Servers[2].Error)
	}

	for i, call := range fake.calls {
		entry := got.Servers[i]
		if call.server.Name != entry.Name || call.server.Kind != entry.Kind {
			t.Errorf("call/result[%d] mismatch: %#v / %#v", i, call, entry)
		}
		if !call.deadline.Equal(entry.Deadline) || !call.listDeadline.Equal(entry.Deadline) {
			t.Errorf("deadline[%d] initialize=%v list=%v result=%v", i, call.deadline, call.listDeadline, entry.Deadline)
		}
		if !call.listed || !call.closed {
			t.Errorf("call[%d] listed/closed = %v/%v, want true/true", i, call.listed, call.closed)
		}
		if call.deadline.Before(started.Add(1500*time.Millisecond)) || call.deadline.After(time.Now().Add(2500*time.Millisecond)) {
			t.Errorf("deadline[%d] %v does not reflect two-second budget", i, call.deadline)
		}
	}

	// Probe must not let a prober mutate parsed config storage retained by the
	// caller or another invocation; servers are passed by defensive value/copy.
	fake.calls[1].server.Resource[0] = "changed"
	if got.Servers[1].Name != "space" {
		t.Fatalf("unexpected result mutation: %#v", got)
	}
}

func TestProbeRejectsConfigBeforeCallingProber(t *testing.T) {
	fake := &fakeProber{}
	_, err := mcpops.Probe(context.Background(), []byte(`{
		"schema":"buzz-managed-mcp","version":1,
		"servers":[{"name":"space","kind":"genie","auth":"env"}]
	}`), fake, mcpops.ProbeOptions{})
	if err == nil || !strings.Contains(err.Error(), "one space identifier") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("invalid config made %d probe calls", len(fake.calls))
	}
}

func TestProbeRejectsNonManagedAndUnknownFieldDocuments(t *testing.T) {
	for _, document := range []string{
		`{"schema":"other","version":1,"servers":[]}`,
		`{"schema":"buzz-managed-mcp","version":1,"servers":[],"host":"workspace.invalid"}`,
	} {
		fake := &fakeProber{}
		if _, err := mcpops.Probe(context.Background(), []byte(document), fake, mcpops.ProbeOptions{}); err == nil {
			t.Fatalf("Probe accepted %s", document)
		}
		if len(fake.calls) != 0 {
			t.Fatalf("invalid document made %d calls", len(fake.calls))
		}
	}
}

func TestProbeCatalogValidationIsPerServer(t *testing.T) {
	document := []byte(`{"schema":"buzz-managed-mcp","version":1,"servers":[
		{"name":"first","kind":"sql","auth":"env"},
		{"name":"second","kind":"sql","auth":"env"}
	]}`)
	fake := &fakeProber{responses: map[string]fakeProbeResponse{
		"first":  {tools: []mcpops.Tool{{Name: "same"}, {Name: "same"}}},
		"second": {tools: []mcpops.Tool{{Name: "healthy"}}},
	}}
	got, err := mcpops.Probe(context.Background(), document, fake, mcpops.ProbeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Servers[0].Error, "duplicate") || got.Servers[1].ToolCount != 1 {
		t.Fatalf("unexpected results: %#v", got.Servers)
	}
}

func TestProbeRequiresDependenciesAndValidTimeout(t *testing.T) {
	document := []byte(`{"schema":"buzz-managed-mcp","version":1,"servers":[]}`)
	if _, err := mcpops.Probe(context.Background(), document, nil, mcpops.ProbeOptions{}); err == nil {
		t.Fatal("nil prober accepted")
	}
	if _, err := mcpops.Probe(context.Background(), document, &fakeProber{}, mcpops.ProbeOptions{Timeout: -time.Second}); err == nil {
		t.Fatal("negative timeout accepted")
	}
	if _, err := mcpops.Discover(context.Background(), nil); err == nil {
		t.Fatal("nil discoverer accepted")
	}
}
