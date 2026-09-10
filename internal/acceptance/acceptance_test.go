package acceptance_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/acceptance"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

func TestParsePlanValidGenericManifest(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "valid.json"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := acceptance.ParsePlan(data, acceptance.Options{
		AcceptInternalWorkspaceRisk: true,
		LookupEnv: env(map[string]string{
			acceptance.OptInEnvironment: "1",
		}),
	})
	if err != nil {
		t.Fatalf("ParsePlan() error = %v", err)
	}
	if got.Profile != "manual-test-profile" {
		t.Errorf("Profile = %q", got.Profile)
	}
	wantRuntimes := []acceptance.Runtime{
		{AgentCommand: "buzz-agent", Kind: payload.RuntimeBuzzAgent, SpawnCommand: "buzz-agent"},
		{AgentCommand: "claude-code", Kind: payload.RuntimeClaude, SpawnCommand: "claude-agent-acp"},
		{AgentCommand: "codex-acp", Kind: payload.RuntimeCodex, SpawnCommand: "codex-acp"},
	}
	if !reflect.DeepEqual(got.Runtimes, wantRuntimes) {
		t.Errorf("Runtimes = %#v, want %#v", got.Runtimes, wantRuntimes)
	}
	wantMCP := `{"schema":"buzz-managed-mcp","version":1,"servers":[{"name":"knowledge","kind":"genie","resource":["example_space"],"auth":"env"}]}`
	if got.MCPConfig != wantMCP {
		t.Errorf("MCPConfig = %q, want %q", got.MCPConfig, wantMCP)
	}
	wantSkills := `{"schema":"buzz-skills","version":1,"aitools":{"skills":["example-skill"],"collision_policy":"fail"}}`
	if got.SkillsConfig != wantSkills {
		t.Errorf("SkillsConfig = %q, want %q", got.SkillsConfig, wantSkills)
	}
}

func TestBuildPlanRequiresBothExplicitOptIns(t *testing.T) {
	t.Parallel()
	manifest := validManifest()
	tests := []struct {
		name    string
		options acceptance.Options
		wantErr error
	}{
		{
			name:    "neither",
			options: acceptance.Options{LookupEnv: env(nil)},
			wantErr: acceptance.ErrInternalWorkspaceRiskNotAccepted,
		},
		{
			name: "environment only",
			options: acceptance.Options{LookupEnv: env(map[string]string{
				acceptance.OptInEnvironment: "1",
			})},
			wantErr: acceptance.ErrInternalWorkspaceRiskNotAccepted,
		},
		{
			name: "caller boolean only",
			options: acceptance.Options{
				AcceptInternalWorkspaceRisk: true,
				LookupEnv:                   env(nil),
			},
			wantErr: acceptance.ErrEnvironmentOptInRequired,
		},
		{
			name: "wrong environment value",
			options: acceptance.Options{
				AcceptInternalWorkspaceRisk: true,
				LookupEnv: env(map[string]string{
					acceptance.OptInEnvironment: "true",
				}),
			},
			wantErr: acceptance.ErrEnvironmentOptInRequired,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := acceptance.BuildPlan(manifest, tt.options)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("BuildPlan() error = %v, want errors.Is(_, %v)", err, tt.wantErr)
			}
		})
	}
}

func TestBuildPlanRejectsCIRegardlessOfOptIns(t *testing.T) {
	t.Parallel()
	for _, variable := range []string{"GITHUB_ACTIONS", "CI"} {
		variable := variable
		t.Run(variable, func(t *testing.T) {
			t.Parallel()
			_, err := acceptance.BuildPlan(validManifest(), acceptance.Options{
				AcceptInternalWorkspaceRisk: true,
				LookupEnv: env(map[string]string{
					acceptance.OptInEnvironment: "1",
					variable:                    "TrUe",
				}),
			})
			if !errors.Is(err, acceptance.ErrCIForbidden) {
				t.Fatalf("BuildPlan() error = %v, want ErrCIForbidden", err)
			}
		})
	}
}

func TestBuildPlanAllowsExplicitFalseCIValues(t *testing.T) {
	t.Parallel()
	_, err := acceptance.BuildPlan(validManifest(), acceptance.Options{
		AcceptInternalWorkspaceRisk: true,
		LookupEnv: env(map[string]string{
			acceptance.OptInEnvironment: "1",
			"GITHUB_ACTIONS":            "false",
			"CI":                        "0",
		}),
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
}

func TestParseRejectsMissingProfileRuntimesAndResources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		json string
		want string
	}{
		{
			name: "profile",
			json: `{"schema":"buzz-internal-acceptance","version":1,"runtimes":["buzz-agent"],"resources":{"mcp_config":"{\"schema\":\"buzz-managed-mcp\",\"version\":1,\"servers\":[]}"}}`,
			want: "profile is required",
		},
		{
			name: "runtimes",
			json: `{"schema":"buzz-internal-acceptance","version":1,"profile":"local","resources":{"mcp_config":"{\"schema\":\"buzz-managed-mcp\",\"version\":1,\"servers\":[]}"}}`,
			want: "runtimes must contain",
		},
		{
			name: "resources object",
			json: `{"schema":"buzz-internal-acceptance","version":1,"profile":"local","runtimes":["buzz-agent"]}`,
			want: "resources is required",
		},
		{
			name: "resources entries",
			json: `{"schema":"buzz-internal-acceptance","version":1,"profile":"local","runtimes":["buzz-agent"],"resources":{}}`,
			want: "resources must contain",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := acceptance.Parse([]byte(tt.json))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseRejectsSchemaVersionUnknownAndTrailingData(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		json string
		want string
	}{
		{"schema", `{"schema":"other","version":1,"profile":"local","runtimes":["buzz-agent"],"resources":{"mcp_config":"x"}}`, "schema must be"},
		{"version", `{"schema":"buzz-internal-acceptance","version":2,"profile":"local","runtimes":["buzz-agent"],"resources":{"mcp_config":"x"}}`, "version must be"},
		{"unknown", `{"schema":"buzz-internal-acceptance","version":1,"profile":"local","runtimes":["buzz-agent"],"resources":{"mcp_config":"x"},"host":"https://example.invalid"}`, "unknown field"},
		{"nested unknown", `{"schema":"buzz-internal-acceptance","version":1,"profile":"local","runtimes":["buzz-agent"],"resources":{"mcp_config":"x","token":"secret"}}`, "unknown field"},
		{"second value", `{"schema":"buzz-internal-acceptance","version":1,"profile":"local","runtimes":["buzz-agent"],"resources":{"mcp_config":"x"}} {}`, "multiple JSON values"},
		{"trailing", `{"schema":"buzz-internal-acceptance","version":1,"profile":"local","runtimes":["buzz-agent"],"resources":{"mcp_config":"x"}} trailing`, "trailing data"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := acceptance.Parse([]byte(tt.json))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseUsesExistingRuntimeParserAndRejectsDuplicates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		runtimes []string
		want     string
	}{
		{"unsupported", []string{"goose"}, "not a supported agent command"},
		{"exact case", []string{"Buzz-Agent"}, "not a supported agent command"},
		{"canonical duplicate", []string{"claude-code", "claude-agent-acp"}, "duplicates the runtime"},
		{"literal duplicate", []string{"codex-acp", "codex-acp"}, "duplicates the runtime"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			manifest := validManifest()
			manifest.Runtimes = tt.runtimes
			err := manifest.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateDelegatesToExistingConfigParsers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		resources acceptance.Resources
		want      string
	}{
		{
			name: "MCP unknown field",
			resources: acceptance.Resources{
				MCPConfig: `{"schema":"buzz-managed-mcp","version":1,"servers":[],"host":"https://example.invalid"}`,
			},
			want: "resources.mcp_config: decode managed MCP config: json: unknown field",
		},
		{
			name: "MCP malformed resource",
			resources: acceptance.Resources{
				MCPConfig: `{"schema":"buzz-managed-mcp","version":1,"servers":[{"name":"x","kind":"genie","resource":["../../escape"],"auth":"env"}]}`,
			},
			want: "not a safe identifier segment",
		},
		{
			name: "skills unknown field",
			resources: acceptance.Resources{
				SkillsConfig: `{"schema":"buzz-skills","version":1,"token":"not-allowed"}`,
			},
			want: "resources.skills_config: decode skills config: json: unknown field",
		},
		{
			name: "skills unsafe name",
			resources: acceptance.Resources{
				SkillsConfig: `{"schema":"buzz-skills","version":1,"aitools":{"skills":["../escape"]}}`,
			},
			want: "not a safe identifier",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			manifest := validManifest()
			manifest.Resources = &tt.resources
			err := manifest.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestManifestCannotStoreHostOrToken(t *testing.T) {
	t.Parallel()
	typeOfManifest := reflect.TypeOf(acceptance.Manifest{})
	typeOfResources := reflect.TypeOf(acceptance.Resources{})
	for _, typ := range []reflect.Type{typeOfManifest, typeOfResources} {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name + " " + typ.Field(i).Tag.Get("json"))
			if strings.Contains(name, "host") || strings.Contains(name, "token") {
				t.Errorf("%s unexpectedly exposes host/token field %s", typ, typ.Field(i).Name)
			}
		}
	}

	manifest := validManifest()
	manifest.Profile = "https://workspace.example.invalid"
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "not a workspace host") {
		t.Fatalf("Validate() host-shaped profile error = %v", err)
	}
}

func TestBuildPlanRevalidatesDirectlyConstructedManifest(t *testing.T) {
	t.Parallel()
	manifest := validManifest()
	manifest.Resources = nil
	_, err := acceptance.BuildPlan(manifest, enabledOptions())
	if err == nil || !strings.Contains(err.Error(), "resources is required") {
		t.Fatalf("BuildPlan() error = %v", err)
	}
}

func validManifest() acceptance.Manifest {
	return acceptance.Manifest{
		Schema:   acceptance.CurrentSchema,
		Version:  acceptance.CurrentVersion,
		Profile:  "manual-test-profile",
		Runtimes: []string{"buzz-agent"},
		Resources: &acceptance.Resources{
			MCPConfig: `{"schema":"buzz-managed-mcp","version":1,"servers":[]}`,
		},
	}
}

func enabledOptions() acceptance.Options {
	return acceptance.Options{
		AcceptInternalWorkspaceRisk: true,
		LookupEnv: env(map[string]string{
			acceptance.OptInEnvironment: "1",
		}),
	}
}

func env(values map[string]string) func(string) (string, bool) {
	copyOfValues := make(map[string]string, len(values))
	for key, value := range values {
		copyOfValues[key] = value
	}
	return func(key string) (string, bool) {
		value, ok := copyOfValues[key]
		return value, ok
	}
}
