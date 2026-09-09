package mcpconfig

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

func validConfig(servers ...Server) Config {
	return Config{Schema: CurrentSchema, Version: CurrentVersion, Servers: servers}
}

func TestParseValidVersionedJSON(t *testing.T) {
	raw := []byte(`{
		"schema":"buzz-managed-mcp",
		"version":1,
		"servers":[
			{"name":"shellbox","kind":"local","command":"shellbox-mcp","args":["--read-only"],"inherit_env":["HOME"]},
			{"name":"warehouse","kind":"sql","auth":"env"},
			{"name":"sales-search","kind":"ai-search","resource":["prod","sales","index"],"auth":"env"},
			{"name":"catalog-tools","kind":"functions","resource":["prod","tools"],"auth":"env"},
			{"name":"support","kind":"mcp-service","resource":["system","ai","support-bot"],"auth":"env"},
			{"name":"team-skills","kind":"skills","resource":["main","team_skills"],"auth":"env"}
		]
	}`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := len(cfg.Servers), 6; got != want {
		t.Fatalf("len(Servers) = %d, want %d", got, want)
	}
	if got := cfg.Servers[2].Resource; !reflect.DeepEqual(got, []string{"prod", "sales", "index"}) {
		t.Fatalf("Resource = %#v", got)
	}
}

func TestParseRejectsNonContractJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"unknown field", `{"schema":"buzz-managed-mcp","version":1,"servers":[],"url":"https://evil.example"}`, "unknown field"},
		{"server custom url", `{"schema":"buzz-managed-mcp","version":1,"servers":[{"name":"x","kind":"sql","auth":"env","url":"https://evil.example"}]}`, "unknown field"},
		{"missing schema", `{"version":1,"servers":[]}`, "schema"},
		{"wrong schema", `{"schema":"other","version":1,"servers":[]}`, "schema"},
		{"wrong version", `{"schema":"buzz-managed-mcp","version":2,"servers":[]}`, "version"},
		{"trailing document", `{"schema":"buzz-managed-mcp","version":1,"servers":[]} {}`, "multiple JSON values"},
		{"trailing junk", `{"schema":"buzz-managed-mcp","version":1,"servers":[]} nope`, "trailing data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateAllRemoteKinds(t *testing.T) {
	tests := []Server{
		{Name: "sql", Kind: KindSQL, Auth: AuthEnv},
		{Name: "genie", Kind: KindGenie, Resource: []string{"01ef-dead-beef"}, Auth: AuthEnv},
		{Name: "ai-search", Kind: KindAISearch, Resource: []string{"catalog", "schema", "index"}, Auth: AuthEnv},
		{Name: "vector-search", Kind: KindVectorSearch, Resource: []string{"catalog", "schema"}, Auth: AuthEnv},
		{Name: "functions", Kind: KindFunctions, Resource: []string{"catalog", "schema"}, Auth: AuthEnv},
		{Name: "service", Kind: KindMCPService, Resource: []string{"catalog", "schema", "service-name"}, Auth: AuthEnv},
		{Name: "skills-all", Kind: KindSkills, Auth: AuthEnv},
		{Name: "skills-scoped", Kind: KindSkills, Resource: []string{"catalog", "schema"}, Auth: AuthEnv},
	}
	for _, server := range tests {
		t.Run(string(server.Kind)+"/"+server.Name, func(t *testing.T) {
			if err := validConfig(server).Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidateRejectsInvalidTopLevelAndNames(t *testing.T) {
	tooMany := make([]Server, MaxServers+1)
	for i := range tooMany {
		tooMany[i] = Server{Name: string(rune('a' + i)), Kind: KindLocal, Command: "mcp"}
	}
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"too many", validConfig(tooMany...), "maximum of 16"},
		{"empty name", validConfig(Server{Kind: KindLocal, Command: "mcp"}), ".name"},
		{"leading dash", validConfig(Server{Name: "-bad", Kind: KindLocal, Command: "mcp"}), "not safe"},
		{"space", validConfig(Server{Name: "not safe", Kind: KindLocal, Command: "mcp"}), "not safe"},
		{"too long", validConfig(Server{Name: strings.Repeat("x", 65), Kind: KindLocal, Command: "mcp"}), "1-64"},
		{"separator", validConfig(Server{Name: "bad__name", Kind: KindLocal, Command: "mcp"}), "must not contain"},
		{"reserved exact", validConfig(Server{Name: "bzmux", Kind: KindLocal, Command: "mcp"}), "reserved bzmux"},
		{"reserved folded", validConfig(Server{Name: "BzMuX", Kind: KindLocal, Command: "mcp"}), "reserved bzmux"},
		{"duplicate", validConfig(Server{Name: "one", Kind: KindLocal, Command: "mcp"}, Server{Name: "one", Kind: KindLocal, Command: "mcp2"}), "duplicates"},
		{"duplicate folded", validConfig(Server{Name: "One", Kind: KindLocal, Command: "mcp"}, Server{Name: "one", Kind: KindLocal, Command: "mcp2"}), "case-insensitively unique"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateLocalContract(t *testing.T) {
	valid := validConfig(Server{
		Name:       "legacy-local",
		Kind:       KindLocal,
		Command:    "legacy.mcp-server",
		Args:       []string{"--mode", "read-only", "plain value"},
		InheritEnv: []string{"HOME", "DATABRICKS_TOKEN"},
	})
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid local config rejected: %v", err)
	}

	tests := []struct {
		name   string
		server Server
		want   string
	}{
		{"empty command", Server{Name: "x", Kind: KindLocal}, "bare command"},
		{"command path", Server{Name: "x", Kind: KindLocal, Command: "/bin/mcp"}, "bare command"},
		{"reserved command", Server{Name: "x", Kind: KindLocal, Command: "bzmux"}, "reserved"},
		{"resource", Server{Name: "x", Kind: KindLocal, Command: "mcp", Resource: []string{"x"}}, "only valid"},
		{"auth", Server{Name: "x", Kind: KindLocal, Command: "mcp", Auth: AuthEnv}, "only valid"},
		{"url arg", Server{Name: "x", Kind: KindLocal, Command: "mcp", Args: []string{"https://other.example/mcp"}}, "URL"},
		{"secret assignment", Server{Name: "x", Kind: KindLocal, Command: "mcp", Args: []string{"--token=dapi123"}}, "literal secret"},
		{"bearer", Server{Name: "x", Kind: KindLocal, Command: "mcp", Args: []string{"Bearer abc"}}, "literal secret"},
		{"databricks token prefix", Server{Name: "x", Kind: KindLocal, Command: "mcp", Args: []string{"dapi123456"}}, "literal secret"},
		{"shell expansion", Server{Name: "x", Kind: KindLocal, Command: "mcp", Args: []string{"${HOME}/thing"}}, "environment-specific"},
		{"command substitution", Server{Name: "x", Kind: KindLocal, Command: "mcp", Args: []string{"$(hostname)"}}, "environment-specific"},
		{"windows expansion", Server{Name: "x", Kind: KindLocal, Command: "mcp", Args: []string{"%USERPROFILE%\\thing"}}, "environment-specific"},
		{"control", Server{Name: "x", Kind: KindLocal, Command: "mcp", Args: []string{"line\nbreak"}}, "control"},
		{"bad env", Server{Name: "x", Kind: KindLocal, Command: "mcp", InheritEnv: []string{"BAD=VALUE"}}, "environment variable name"},
		{"duplicate env", Server{Name: "x", Kind: KindLocal, Command: "mcp", InheritEnv: []string{"HOME", "HOME"}}, "duplicates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validConfig(tt.server).Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateRemoteShapeAndResources(t *testing.T) {
	tests := []struct {
		name   string
		server Server
		want   string
	}{
		{"unknown kind", Server{Name: "x", Kind: "http", Auth: AuthEnv}, "unsupported"},
		{"missing auth", Server{Name: "x", Kind: KindSQL}, ".auth"},
		{"bad auth", Server{Name: "x", Kind: KindSQL, Auth: "pat"}, "must be"},
		{"remote command", Server{Name: "x", Kind: KindSQL, Command: "curl", Auth: AuthEnv}, "must be omitted"},
		{"remote args", Server{Name: "x", Kind: KindSQL, Args: []string{"--url"}, Auth: AuthEnv}, "must be omitted"},
		{"sql resource", Server{Name: "x", Kind: KindSQL, Resource: []string{"warehouse"}, Auth: AuthEnv}, "no components"},
		{"genie missing", Server{Name: "x", Kind: KindGenie, Auth: AuthEnv}, "one space"},
		{"search arity", Server{Name: "x", Kind: KindAISearch, Resource: []string{"cat", "schema"}, Auth: AuthEnv}, "catalog, schema, and index"},
		{"functions arity", Server{Name: "x", Kind: KindFunctions, Resource: []string{"cat"}, Auth: AuthEnv}, "catalog and schema"},
		{"service arity", Server{Name: "x", Kind: KindMCPService, Resource: []string{"cat", "schema"}, Auth: AuthEnv}, "catalog, schema, and service"},
		{"skills arity", Server{Name: "x", Kind: KindSkills, Resource: []string{"cat"}, Auth: AuthEnv}, "no components or"},
		{"slash", Server{Name: "x", Kind: KindGenie, Resource: []string{"../../escape"}, Auth: AuthEnv}, "identifier segment"},
		{"dot", Server{Name: "x", Kind: KindGenie, Resource: []string{"cat.schema"}, Auth: AuthEnv}, "identifier segment"},
		{"url component", Server{Name: "x", Kind: KindGenie, Resource: []string{"https:"}, Auth: AuthEnv}, "identifier segment"},
		{"separator component", Server{Name: "x", Kind: KindGenie, Resource: []string{"bad__id"}, Auth: AuthEnv}, "must not contain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validConfig(tt.server).Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestRemoteEndpointIsDerivedAndSameHostRelative(t *testing.T) {
	tests := []struct {
		server Server
		want   string
	}{
		{Server{Name: "sql", Kind: KindSQL, Auth: AuthEnv}, "/api/2.0/mcp/sql"},
		{Server{Name: "genie", Kind: KindGenie, Resource: []string{"space-id"}, Auth: AuthEnv}, "/api/2.0/mcp/genie/space-id"},
		{Server{Name: "search", Kind: KindAISearch, Resource: []string{"cat", "schema", "index"}, Auth: AuthEnv}, "/api/2.0/mcp/ai-search/cat/schema/index"},
		{Server{Name: "functions", Kind: KindFunctions, Resource: []string{"cat", "schema"}, Auth: AuthEnv}, "/api/2.0/mcp/functions/cat/schema"},
		{Server{Name: "service", Kind: KindMCPService, Resource: []string{"cat", "schema", "svc"}, Auth: AuthEnv}, "/ai-gateway/mcp-services/cat.schema.svc"},
		{Server{Name: "skills", Kind: KindSkills, Auth: AuthEnv}, "/ai-gateway/skills/"},
		{Server{Name: "skills-scoped", Kind: KindSkills, Resource: []string{"cat", "schema", "ml", "prod"}, Auth: AuthEnv}, "/ai-gateway/skills/?schema=cat.schema&schema=ml.prod"},
	}
	for _, tt := range tests {
		t.Run(tt.server.Name, func(t *testing.T) {
			var remote Remote
			_, err := ToMuxConfig(validConfig(tt.server), Options{
				RemoteCommand: "bridge",
				RemoteArgs: func(got Remote) ([]string, error) {
					remote = got
					return []string{got.Endpoint}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if remote.Endpoint != tt.want {
				t.Fatalf("Endpoint = %q, want %q", remote.Endpoint, tt.want)
			}
			if strings.Contains(remote.Endpoint, "://") || !strings.HasPrefix(remote.Endpoint, "/") {
				t.Fatalf("Endpoint is not same-host relative: %q", remote.Endpoint)
			}
		})
	}
}

func TestToMuxConfigPreservesLocalAndBuildsRemoteDeterministically(t *testing.T) {
	cfg := validConfig(
		Server{Name: "local", Kind: KindLocal, Command: "shellbox-mcp", Args: []string{"--read-only"}, InheritEnv: []string{"Z_VAR", "A_VAR"}},
		Server{Name: "search", Kind: KindVectorSearch, Resource: []string{"main", "docs"}, Auth: AuthEnv, InheritEnv: []string{"DATABRICKS_CONFIG_FILE", "HOME"}},
	)
	var gotRemote Remote
	opts := Options{
		RemoteCommand: "db-mcp-bridge",
		RemoteArgs: func(remote Remote) ([]string, error) {
			gotRemote = remote
			return []string{"--kind", string(remote.Kind), "--resource", strings.Join(remote.Components, "."), "--auth", string(remote.Auth)}, nil
		},
	}
	got, err := ToMuxConfig(cfg, opts)
	if err != nil {
		t.Fatalf("ToMuxConfig() error = %v", err)
	}
	want := muxcfg.Config{Servers: []muxcfg.Server{
		{Name: "local", Command: "shellbox-mcp", Args: []string{"--read-only"}, Env: muxcfg.ServerEnv{Inherit: []string{"A_VAR", "Z_VAR"}}},
		{Name: "search", Command: "db-mcp-bridge", Args: []string{"--kind", "vector-search", "--resource", "main.docs", "--auth", "env"}, Env: muxcfg.ServerEnv{Inherit: []string{"DATABRICKS_CONFIG_FILE", "HOME"}}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToMuxConfig() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(gotRemote, Remote{Kind: KindVectorSearch, Endpoint: "/api/2.0/mcp/vector-search/main/docs", Components: []string{"main", "docs"}, Auth: AuthEnv}) {
		t.Fatalf("builder remote = %#v", gotRemote)
	}

	// Conversion must not normalize or alias caller-owned slices in place.
	got.Servers[0].Args[0] = "changed"
	got.Servers[0].Env.Inherit[0] = "changed"
	gotRemote.Components[0] = "changed"
	if cfg.Servers[0].Args[0] != "--read-only" || cfg.Servers[0].InheritEnv[0] != "Z_VAR" || cfg.Servers[1].Resource[0] != "main" {
		t.Fatalf("ToMuxConfig mutated or aliased its input: %#v", cfg)
	}

	again, err := cfg.MuxConfig(opts)
	if err != nil {
		t.Fatalf("MuxConfig() error = %v", err)
	}
	if again.Servers[0].Env.Inherit[0] != "A_VAR" {
		t.Fatalf("inherited env was not deterministically sorted: %#v", again.Servers[0].Env.Inherit)
	}
}

func TestToMuxConfigOptionsAndBuilderErrors(t *testing.T) {
	local := validConfig(Server{Name: "local", Kind: KindLocal, Command: "mcp"})
	if _, err := ToMuxConfig(local, Options{}); err != nil {
		t.Fatalf("local-only conversion should not require remote options: %v", err)
	}

	remote := validConfig(Server{Name: "remote", Kind: KindSQL, Auth: AuthEnv})
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"missing command", Options{RemoteArgs: func(Remote) ([]string, error) { return nil, nil }}, "bridge command"},
		{"command path", Options{RemoteCommand: "/bin/bridge", RemoteArgs: func(Remote) ([]string, error) { return nil, nil }}, "bare command"},
		{"reserved", Options{RemoteCommand: "bzmux", RemoteArgs: func(Remote) ([]string, error) { return nil, nil }}, "collides"},
		{"missing builder", Options{RemoteCommand: "bridge"}, "builder is required"},
		{"builder failure", Options{RemoteCommand: "bridge", RemoteArgs: func(Remote) ([]string, error) { return nil, errors.New("boom") }}, "boom"},
		{"builder nul", Options{RemoteCommand: "bridge", RemoteArgs: func(Remote) ([]string, error) { return []string{"bad\x00arg"}, nil }}, "NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ToMuxConfig(remote, tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ToMuxConfig() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestJSONContainsNoLiteralEnvironmentMapOrURLField(t *testing.T) {
	cfg := validConfig(Server{
		Name:       "sql",
		Kind:       KindSQL,
		Auth:       AuthEnv,
		InheritEnv: []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN"},
	})
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{`"env":`, `"set":`, `"url":`, "https://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("marshaled managed config contains forbidden literal %q: %s", forbidden, text)
		}
	}
}
