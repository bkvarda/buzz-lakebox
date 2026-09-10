package mcpprobe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
)

const (
	testHost  = "https://workspace.example.invalid"
	testToken = "placeholder-sensitive-token-value"
)

func testServer(kind mcpconfig.Kind, resources ...string) mcpconfig.Server {
	return mcpconfig.Server{Name: "generic-server", Kind: kind, Resource: append([]string(nil), resources...), Auth: mcpconfig.AuthEnv}
}

func TestNewRequiresExplicitConnectionInputs(t *testing.T) {
	for _, tc := range []struct{ name, host, token string }{
		{name: "missing host", token: testToken},
		{name: "missing token", host: testHost},
		{name: "host control", host: testHost + "\n", token: testToken},
		{name: "token control", host: testHost, token: testToken + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.host, tc.token); err == nil {
				t.Fatal("New accepted invalid explicit inputs")
			}
		})
	}
	if _, err := New(testHost, testToken); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestEndpointOptionsMatchDeployTypedMapping(t *testing.T) {
	tests := []struct {
		server    mcpconfig.Server
		kind      string
		resources []string
		schemas   []string
	}{
		{testServer(mcpconfig.KindSQL), "sql", nil, nil},
		{testServer(mcpconfig.KindGenie, "space_example"), "genie", []string{"space_example"}, nil},
		{testServer(mcpconfig.KindAISearch, "catalog", "schema", "index"), "ai-search", []string{"catalog", "schema", "index"}, nil},
		{testServer(mcpconfig.KindVectorSearch, "catalog", "schema"), "vector-search", []string{"catalog", "schema"}, nil},
		{testServer(mcpconfig.KindFunctions, "catalog", "schema", "function"), "functions", []string{"catalog", "schema", "function"}, nil},
		{testServer(mcpconfig.KindMCPService, "catalog", "schema", "service"), "mcp-service", []string{"catalog", "schema", "service"}, nil},
		{testServer(mcpconfig.KindSkills, "catalog_a", "schema_a", "catalog_b", "schema_b"), "skills", nil, []string{"catalog_a.schema_a", "catalog_b.schema_b"}},
	}
	for _, tc := range tests {
		got, err := endpointOptions(tc.server)
		if err != nil {
			t.Fatalf("endpointOptions(%s): %v", tc.server.Kind, err)
		}
		if string(got.Kind) != tc.kind || !reflect.DeepEqual(got.Resources, tc.resources) || !reflect.DeepEqual(got.Schemas, tc.schemas) {
			t.Errorf("endpointOptions(%s) = %+v", tc.server.Kind, got)
		}
	}
}

func TestAdapterNativeHTTPHandshakeAndToolsList(t *testing.T) {
	const sessionID = "native-session"
	var mu sync.Mutex
	var methods []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/2.0/mcp/sql" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("authorization header mismatch")
		}
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != sessionID {
				t.Errorf("delete session = %q", r.Header.Get("Mcp-Session-Id"))
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		methods = append(methods, request.Method)
		mu.Unlock()
		if request.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", sessionID)
		}
		if request.Method == "notifications/initialized" {
			if len(request.ID) != 0 {
				t.Error("notification had id")
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != sessionID {
				t.Errorf("tools/list session = %q", r.Header.Get("Mcp-Session-Id"))
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"generic_read"},{"name":"generic_write"}]}}`)
		default:
			t.Errorf("unexpected method %q", request.Method)
		}
	}))
	defer server.Close()

	adapter, err := New(server.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	adapter.client = server.Client()
	adapter.closeTimeout = time.Second

	initialized, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSQL))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := initialized.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := []string{tools[0].Name, tools[1].Name}; !reflect.DeepEqual(got, []string{"generic_read", "generic_write"}) {
		t.Fatalf("tools = %v", got)
	}
	if err := initialized.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 3 || methods[0] != "initialize" || !contains(methods[1:], "notifications/initialized") || !contains(methods[1:], "tools/list") {
		t.Fatalf("methods = %v", methods)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestAdapterNegotiatesSupportedVersions(t *testing.T) {
	for version := range supportedProtocolVersions {
		t.Run(version, func(t *testing.T) {
			adapter := testAdapter(t, fakeHelperConfig{protocolVersion: version})
			initialized, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSQL))
			if err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if got := initialized.(*session).protocolVersion; got != version {
				t.Fatalf("negotiated version = %q, want %q", got, version)
			}
			if err := initialized.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAdapterRejectsUnsupportedNegotiatedVersion(t *testing.T) {
	adapter := testAdapter(t, fakeHelperConfig{protocolVersion: "2099-01-01"})
	_, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSQL))
	if err == nil || !strings.Contains(err.Error(), "unsupported MCP protocol version") {
		t.Fatalf("error = %v", err)
	}
}

func TestAdapterParsesJSONRPCAndRedactsErrors(t *testing.T) {
	adapter := testAdapter(t, fakeHelperConfig{protocolVersion: offeredProtocolVersion, listError: "request rejected for " + testToken + " at " + testHost})
	session, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSQL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), testHost) {
		t.Fatalf("sensitive connection input survived error redaction: %q", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") || !strings.Contains(err.Error(), "JSON-RPC error") {
		t.Fatalf("error lost safe context: %q", err)
	}
	_ = session.Close()
}

func TestAdapterMalformedJSONAndContextCancellation(t *testing.T) {
	t.Run("malformed JSON", func(t *testing.T) {
		adapter := testAdapter(t, fakeHelperConfig{malformedInitialize: true})
		_, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSQL))
		if err == nil || !strings.Contains(err.Error(), "malformed JSON-RPC") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		adapter := testAdapter(t, fakeHelperConfig{neverReply: true})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := adapter.Initialize(ctx, testServer(mcpconfig.KindSQL))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context deadline", err)
		}
	})
}

func TestAdapterRejectsLocalServerWithoutStartingBridge(t *testing.T) {
	adapter, err := New(testHost, testToken)
	if err != nil {
		t.Fatal(err)
	}
	started := false
	adapter.start = func(context.Context, *http.Client, *url.URL, string) childProcess {
		started = true
		return nil
	}
	server := mcpconfig.Server{Name: "generic-local", Kind: mcpconfig.KindLocal, Command: "generic-command"}
	if _, err := adapter.Initialize(context.Background(), server); err == nil {
		t.Fatal("local server accepted")
	}
	if started {
		t.Fatal("bridge started for local server")
	}
}

func testAdapter(t *testing.T, config fakeHelperConfig) *Adapter {
	t.Helper()
	adapter, err := New(testHost, testToken)
	if err != nil {
		t.Fatal(err)
	}
	adapter.closeTimeout = 20 * time.Millisecond
	adapter.start = func(context.Context, *http.Client, *url.URL, string) childProcess { return newFakeProcess(config) }
	return adapter
}

type fakeHelperConfig struct {
	protocolVersion     string
	tools               []string
	listError           string
	malformedInitialize bool
	neverReply          bool
}

type fakeProcess struct {
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	kill    func()
	done    chan error
	wait    sync.Once
	waitErr error
}

func newFakeProcess(config fakeHelperConfig) *fakeProcess {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	process := &fakeProcess{stdinW: stdinW, stdoutR: stdoutR, done: make(chan error, 1)}
	var killOnce sync.Once
	process.kill = func() {
		killOnce.Do(func() {
			_ = stdinR.CloseWithError(errors.New("killed"))
			_ = stdinW.CloseWithError(errors.New("killed"))
			_ = stdoutW.CloseWithError(errors.New("killed"))
			_ = stdoutR.CloseWithError(errors.New("killed"))
		})
	}
	go func() {
		err := runFakeHelper(stdinR, stdoutW, config)
		_ = stdinR.Close()
		_ = stdoutW.Close()
		process.done <- err
	}()
	return process
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdinW }
func (p *fakeProcess) Stdout() io.ReadCloser { return p.stdoutR }
func (p *fakeProcess) Wait() error           { p.wait.Do(func() { p.waitErr = <-p.done }); return p.waitErr }
func (p *fakeProcess) Kill() error           { p.kill(); return nil }

func runFakeHelper(stdin io.Reader, stdout io.Writer, config fakeHelperConfig) error {
	decoder := json.NewDecoder(stdin)
	encoder := json.NewEncoder(stdout)
	initialized := false
	for {
		var message struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if config.neverReply {
			continue
		}
		switch message.Method {
		case "initialize":
			if config.malformedInitialize {
				_, _ = io.WriteString(stdout, "not-json\n")
				continue
			}
			version := config.protocolVersion
			if version == "" {
				version = offeredProtocolVersion
			}
			if err := encoder.Encode(map[string]interface{}{"jsonrpc": "2.0", "id": message.ID, "result": map[string]interface{}{"protocolVersion": version, "capabilities": map[string]interface{}{}}}); err != nil {
				return err
			}
		case "notifications/initialized":
			initialized = true
		case "tools/list":
			if !initialized {
				return errors.New("tools/list before initialized notification")
			}
			if config.listError != "" {
				if err := encoder.Encode(map[string]interface{}{"jsonrpc": "2.0", "id": message.ID, "error": map[string]interface{}{"code": -32000, "message": config.listError}}); err != nil {
					return err
				}
				continue
			}
			tools := make([]map[string]string, len(config.tools))
			for i, name := range config.tools {
				tools[i] = map[string]string{"name": name}
			}
			if err := encoder.Encode(map[string]interface{}{"jsonrpc": "2.0", "id": message.ID, "result": map[string]interface{}{"tools": tools}}); err != nil {
				return err
			}
		}
	}
}
