package mcpdiscover_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpdiscover"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpops"
)

const fixtureProfile = "fixture-profile"

type response struct {
	want   []string
	stdout string
	stderr string
	err    error
}

type fakeRunner struct {
	t         *testing.T
	responses []response
	calls     [][]string
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	f.t.Helper()
	copied := append([]string(nil), args...)
	f.calls = append(f.calls, copied)
	if len(f.responses) == 0 {
		f.t.Fatalf("unexpected command: %q", args)
	}
	next := f.responses[0]
	f.responses = f.responses[1:]
	if !reflect.DeepEqual(args, next.want) {
		f.t.Fatalf("command = %q\nwant    = %q", args, next.want)
	}
	return []byte(next.stdout), []byte(next.stderr), next.err
}

func (f *fakeRunner) done() {
	f.t.Helper()
	if len(f.responses) != 0 {
		f.t.Fatalf("%d fake responses were not consumed", len(f.responses))
	}
}

func command(parts ...string) []string {
	return append(append([]string(nil), parts...), "--output", "json", "--profile", fixtureProfile)
}

func TestDiscoverAllRoutesWithExplicitProfileScopesAndPagination(t *testing.T) {
	fake := &fakeRunner{t: t, responses: []response{
		{
			want:   command("genie", "list-spaces"),
			stdout: `{"spaces":[{"space_id":"space_beta"}],"next_page_token":"genie_page_2"}`,
			stderr: "an advisory that must not corrupt JSON",
		},
		{
			want:   append(command("genie", "list-spaces"), "--page-token", "genie_page_2"),
			stdout: `{"spaces":[{"id":"space_alpha"}]}`,
		},
		{
			want:   command("vector-search-endpoints", "list-endpoints"),
			stdout: `{"endpoints":[{"name":"endpoint_beta"},{"endpoint_name":"endpoint_alpha"}]}`,
		},
		{
			want: command("vector-search-indexes", "list-indexes", "endpoint_beta"),
			stdout: `{"vector_indexes":[
				{"name":"catalog_b.schema_b.index_b"},
				{"name":"catalog_a.schema_a.index_a"}
			]}`,
		},
		{
			want:   command("vector-search-indexes", "list-indexes", "endpoint_alpha"),
			stdout: `[]`,
		},
		// Input scopes are intentionally reversed. The adapter sorts them before
		// issuing commands, making behavior reproducible.
		{
			want:   command("functions", "list", "catalog_a", "schema_a"),
			stdout: `{"functions":[{"full_name":"catalog_a.schema_a.function_b"},{"name":"function_a"}]}`,
		},
		{
			want:   command("functions", "list", "catalog_b", "schema_b"),
			stdout: `[]`,
		},
		{
			want:   command("ai-gateway", "list-mcp-services", "--parent", "schemas/catalog_a.schema_a"),
			stdout: `{"mcp_services":[{"full_name":"catalog_a.schema_a.service_a"}]}`,
		},
		{
			want:   command("ai-gateway", "list-mcp-services", "--parent", "schemas/catalog_b.schema_b"),
			stdout: `{"services":[{"name":"service_b"}]}`,
		},
	}}
	adapter, err := mcpdiscover.New(fixtureProfile, fake, mcpdiscover.WithScopes(
		mcpdiscover.Scope{Catalog: "catalog_b", Schema: "schema_b"},
		mcpdiscover.Scope{Catalog: "catalog_a", Schema: "schema_a"},
		mcpdiscover.Scope{Catalog: "catalog_a", Schema: "schema_a"}, // duplicate is ignored
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	fake.done()

	want := []mcpops.Identifier{
		mcpops.AISearchIdentifier("catalog_a", "schema_a", "index_a"),
		mcpops.AISearchIdentifier("catalog_b", "schema_b", "index_b"),
		mcpops.FunctionsIdentifier("catalog_a", "schema_a"),
		mcpops.FunctionIdentifier("catalog_a", "schema_a", "function_a"),
		mcpops.FunctionIdentifier("catalog_a", "schema_a", "function_b"),
		mcpops.FunctionsIdentifier("catalog_b", "schema_b"),
		mcpops.GenieIdentifier("space_alpha"),
		mcpops.GenieIdentifier("space_beta"),
		mcpops.MCPServiceIdentifier("catalog_a", "schema_a", "service_a"),
		mcpops.MCPServiceIdentifier("catalog_b", "schema_b", "service_b"),
		mcpops.SkillsIdentifier(
			[2]string{"catalog_a", "schema_a"},
			[2]string{"catalog_b", "schema_b"},
		),
		mcpops.SQLIdentifier(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identifiers:\n got: %#v\nwant: %#v", got, want)
	}

	for _, id := range got {
		serialized := string(id.Kind) + " " + strings.Join(id.Resource, " ")
		for _, forbidden := range []string{fixtureProfile, "https://", "token", "host"} {
			if strings.Contains(strings.ToLower(serialized), strings.ToLower(forbidden)) {
				t.Fatalf("result contains connection material %q: %#v", forbidden, id)
			}
		}
	}
	for _, args := range fake.calls {
		if !containsPair(args, "--profile", fixtureProfile) {
			t.Fatalf("command does not explicitly select profile: %q", args)
		}
		for _, arg := range args {
			switch arg {
			case "create", "delete", "update", "put", "set", "edit":
				t.Fatalf("discovery issued a write-shaped command: %q", args)
			}
		}
	}
}

func TestEmptyObjectMeansAnEmptyList(t *testing.T) {
	fake := &fakeRunner{t: t, responses: []response{{
		want:   []string{"genie", "list-spaces", "--output", "json", "--profile", fixtureProfile},
		stdout: `{}`,
	}}}
	adapter, err := mcpdiscover.New(fixtureProfile, fake, mcpdiscover.WithKinds(mcpconfig.KindGenie))
	if err != nil {
		t.Fatal(err)
	}
	got, err := mcpops.Discover(context.Background(), adapter)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Discover() = %#v, want empty", got)
	}
}

func TestWithKindsRunsOnlyRequestedRoutes(t *testing.T) {
	fake := &fakeRunner{t: t, responses: []response{{
		want:   command("genie", "list-spaces"),
		stdout: `[{"space_id":"space_one"}]`,
	}}}
	adapter, err := mcpdiscover.New(fixtureProfile, fake,
		mcpdiscover.WithKinds(mcpconfig.KindGenie),
		mcpdiscover.WithScopes(mcpdiscover.Scope{Catalog: "unused_catalog", Schema: "unused_schema"}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	fake.done()
	want := []mcpops.Identifier{mcpops.GenieIdentifier("space_one")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestSQLIsBuiltInAndEmptyKindSelectionDoesNothing(t *testing.T) {
	t.Run("SQL", func(t *testing.T) {
		fake := &fakeRunner{t: t}
		adapter, err := mcpdiscover.New(fixtureProfile, fake, mcpdiscover.WithKinds(mcpconfig.KindSQL))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := adapter.Discover(context.Background())
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if !reflect.DeepEqual(got, []mcpops.Identifier{mcpops.SQLIdentifier()}) {
			t.Fatalf("got %#v", got)
		}
		if len(fake.calls) != 0 {
			t.Fatalf("SQL built-in unexpectedly invoked CLI: %q", fake.calls)
		}
	})
	t.Run("none", func(t *testing.T) {
		fake := &fakeRunner{t: t}
		adapter, err := mcpdiscover.New(fixtureProfile, fake, mcpdiscover.WithKinds())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := adapter.Discover(context.Background())
		if err != nil || len(got) != 0 || len(fake.calls) != 0 {
			t.Fatalf("got=%#v calls=%q err=%v", got, fake.calls, err)
		}
	})
}

func TestScopedRoutesNeverCrawlWithoutExplicitScopes(t *testing.T) {
	fake := &fakeRunner{t: t}
	adapter, err := mcpdiscover.New(fixtureProfile, fake, mcpdiscover.WithKinds(
		mcpconfig.KindFunctions, mcpconfig.KindSkills, mcpconfig.KindMCPService,
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 0 || len(fake.calls) != 0 {
		t.Fatalf("unscoped discovery got=%#v calls=%q", got, fake.calls)
	}
}

func TestScopedResultsRequireSuccessfulListPermission(t *testing.T) {
	const profileSecret = "private-fixture-profile"
	const tokenSecret = "placeholder-sensitive-token-value"
	const hostSecret = "https://private-fixture.cloud.databricks.com/path"
	fake := &fakeRunner{t: t, responses: []response{{
		want: command("functions", "list", "catalog_a", "schema_a"),
		stderr: "profile=" + profileSecret + " DATABRICKS_TOKEN=" + tokenSecret +
			" request " + hostSecret + " denied",
		err: errors.New("exit status 1"),
	}}}
	// command() uses fixtureProfile, so set the fake expectation to this test's profile.
	fake.responses[0].want[len(fake.responses[0].want)-1] = profileSecret
	adapter, err := mcpdiscover.New(profileSecret, fake,
		mcpdiscover.WithKinds(mcpconfig.KindFunctions),
		mcpdiscover.WithScopes(mcpdiscover.Scope{Catalog: "catalog_a", Schema: "schema_a"}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := adapter.Discover(context.Background())
	if err == nil {
		t.Fatal("Discover succeeded without list permission")
	}
	if got != nil {
		t.Fatalf("partial results escaped: %#v", got)
	}
	text := err.Error()
	for _, secret := range []string{profileSecret, tokenSecret, hostSecret} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked %q: %q", secret, text)
		}
	}
	if !strings.Contains(text, "[REDACTED]") || !strings.Contains(text, "list functions") {
		t.Fatalf("error lacks safe diagnostic: %q", text)
	}
}

func TestMCPServiceAIPResourceNameIsScoped(t *testing.T) {
	fake := &fakeRunner{t: t, responses: []response{{
		want:   command("ai-gateway", "list-mcp-services", "--parent", "schemas/catalog_a.schema_a"),
		stdout: `[{"name":"mcp-services/catalog_a.schema_a.service_a"}]`,
	}}}
	adapter, err := mcpdiscover.New(fixtureProfile, fake,
		mcpdiscover.WithKinds(mcpconfig.KindMCPService),
		mcpdiscover.WithScopes(mcpdiscover.Scope{Catalog: "catalog_a", Schema: "schema_a"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mcpops.Discover(context.Background(), adapter)
	if err != nil {
		t.Fatal(err)
	}
	want := []mcpops.Identifier{mcpops.MCPServiceIdentifier("catalog_a", "schema_a", "service_a")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover() = %#v, want %#v", got, want)
	}
}

func TestMalformedJSONUsesStdoutOnlyAndDoesNotEchoIt(t *testing.T) {
	const sensitiveBody = `{"spaces":[{"space_id":"DATABRICKS_TOKEN=fixture-sensitive-value"}]`
	fake := &fakeRunner{t: t, responses: []response{{
		want:   command("genie", "list-spaces"),
		stdout: sensitiveBody,
		stderr: "nonfatal advisory",
	}}}
	adapter, err := mcpdiscover.New(fixtureProfile, fake, mcpdiscover.WithKinds(mcpconfig.KindGenie))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter.Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unsupported JSON response shape") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "fixture-sensitive-value") || strings.Contains(err.Error(), "advisory") {
		t.Fatalf("parse error echoed command streams: %q", err)
	}
}

func TestAISearchUnavailableShapeIsClearAndDoesNotGuess(t *testing.T) {
	fake := &fakeRunner{t: t, responses: []response{{
		want:   command("vector-search-endpoints", "list-endpoints"),
		stdout: `{"unknown_endpoint_shape":[]}`,
	}}}
	adapter, err := mcpdiscover.New(fixtureProfile, fake, mcpdiscover.WithKinds(mcpconfig.KindAISearch))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter.Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "list AI Search endpoints: unsupported JSON response shape") {
		t.Fatalf("error = %v", err)
	}
	fake.done()
}

func TestRejectsOutOfScopeAndInvalidItems(t *testing.T) {
	tests := []struct {
		name       string
		kind       mcpconfig.Kind
		command    []string
		stdout     string
		wantErr    string
		withScopes bool
	}{
		{
			name:    "AI Search name is not three part",
			kind:    mcpconfig.KindAISearch,
			command: command("vector-search-endpoints", "list-endpoints"),
			stdout:  `{"endpoints":[{}]}`,
			wantErr: "missing endpoint name",
		},
		{
			name:       "function outside scope",
			kind:       mcpconfig.KindFunctions,
			command:    command("functions", "list", "catalog_a", "schema_a"),
			stdout:     `[{"full_name":"other_catalog.schema_a.function_a"}]`,
			wantErr:    "outside the requested",
			withScopes: true,
		},
		{
			name:       "service outside scope",
			kind:       mcpconfig.KindMCPService,
			command:    command("ai-gateway", "list-mcp-services", "--parent", "schemas/catalog_a.schema_a"),
			stdout:     `[{"full_name":"catalog_a.other_schema.service_a"}]`,
			wantErr:    "outside the requested",
			withScopes: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRunner{t: t, responses: []response{{want: tc.command, stdout: tc.stdout}}}
			options := []mcpdiscover.Option{mcpdiscover.WithKinds(tc.kind)}
			if tc.withScopes {
				options = append(options, mcpdiscover.WithScopes(mcpdiscover.Scope{Catalog: "catalog_a", Schema: "schema_a"}))
			}
			adapter, err := mcpdiscover.New(fixtureProfile, fake, options...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = adapter.Discover(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestPaginationRejectsRepeatedToken(t *testing.T) {
	fake := &fakeRunner{t: t, responses: []response{
		{
			want:   command("genie", "list-spaces"),
			stdout: `{"spaces":[],"next_page_token":"same_token"}`,
		},
		{
			want:   append(command("genie", "list-spaces"), "--page-token", "same_token"),
			stdout: `{"spaces":[],"next_page_token":"same_token"}`,
		},
	}}
	adapter, err := mcpdiscover.New(fixtureProfile, fake, mcpdiscover.WithKinds(mcpconfig.KindGenie))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter.Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "repeated pagination token") {
		t.Fatalf("error = %v", err)
	}
}

func TestConstructionValidationAndExecConstructors(t *testing.T) {
	fake := &fakeRunner{t: t}
	tests := []struct {
		name    string
		profile string
		runner  mcpdiscover.Runner
		opts    []mcpdiscover.Option
		want    string
	}{
		{name: "empty profile", runner: fake, want: "profile is required"},
		{name: "control profile", profile: "bad\nprofile", runner: fake, want: "control characters"},
		{name: "nil runner", profile: fixtureProfile, want: "runner is required"},
		{name: "unsupported kind", profile: fixtureProfile, runner: fake, opts: []mcpdiscover.Option{mcpdiscover.WithKinds(mcpconfig.KindLocal)}, want: "unsupported discovery kind"},
		{name: "unsafe scope", profile: fixtureProfile, runner: fake, opts: []mcpdiscover.Option{mcpdiscover.WithScopes(mcpdiscover.Scope{Catalog: "bad.catalog", Schema: "schema_a"})}, want: "invalid discovery scope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mcpdiscover.New(tc.profile, tc.runner, tc.opts...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	runner := mcpdiscover.NewExecRunner("")
	if runner.Path != "databricks" {
		t.Fatalf("default executable = %q", runner.Path)
	}
	if _, err := mcpdiscover.NewExec(fixtureProfile, mcpdiscover.WithKinds()); err != nil {
		t.Fatalf("NewExec constructor: %v", err)
	}
}

func TestContextCancellationAvoidsCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeRunner{t: t}
	adapter, err := mcpdiscover.New(fixtureProfile, fake)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter.Discover(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("cancelled discovery made calls: %q", fake.calls)
	}
}

func containsPair(values []string, first, second string) bool {
	for i := 0; i+1 < len(values); i++ {
		if values[i] == first && values[i+1] == second {
			return true
		}
	}
	return false
}
