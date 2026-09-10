package operatorconfig_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/operatorconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/skillconfig"
)

func TestValidateMCPJSONReturnsCanonicalDocumentAndSummary(t *testing.T) {
	t.Parallel()
	input := `{
		"schema": "buzz-managed-mcp",
		"version": 1,
		"servers": [
			{"args":["--quiet"],"command":"tool","kind":"local","name":"local","inherit_env":["PATH"]},
			{"resource":["space_1"],"auth":"env","kind":"genie","name":"knowledge"}
		]
	}`

	got, err := operatorconfig.Validate(operatorconfig.Input{MCPJSON: input})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	wantJSON := `{"schema":"buzz-managed-mcp","version":1,"servers":[{"name":"local","kind":"local","command":"tool","args":["--quiet"],"inherit_env":["PATH"]},{"name":"knowledge","kind":"genie","resource":["space_1"],"auth":"env"}]}`
	if got.CompactJSON != wantJSON {
		t.Errorf("CompactJSON = %q, want %q", got.CompactJSON, wantJSON)
	}
	if got.Kind != operatorconfig.KindMCP {
		t.Errorf("Kind = %q, want %q", got.Kind, operatorconfig.KindMCP)
	}
	if got.Summary.Skills != nil {
		t.Errorf("Skills summary = %#v, want nil", got.Summary.Skills)
	}
	wantSummary := &operatorconfig.MCPSummary{
		Schema:            mcpconfig.CurrentSchema,
		Version:           mcpconfig.CurrentVersion,
		ServerCount:       2,
		LocalServerCount:  1,
		RemoteServerCount: 1,
		Servers: []operatorconfig.MCPServerSummary{
			{Name: "local", Kind: mcpconfig.KindLocal},
			{Name: "knowledge", Kind: mcpconfig.KindGenie},
		},
	}
	if !reflect.DeepEqual(got.Summary.MCP, wantSummary) {
		t.Errorf("MCP summary = %#v, want %#v", got.Summary.MCP, wantSummary)
	}
}

func TestValidateSkillsFileReturnsCanonicalDocumentAndSummary(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "skills.json")
	input := `{
		"aitools": {
			"collision_policy": "replace-managed",
			"experimental": true,
			"skills": ["sql", "catalog.reader"]
		},
		"version": 1,
		"schema": "buzz-skills"
	}`
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := operatorconfig.Validate(operatorconfig.Input{SkillsFile: path})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	wantJSON := `{"schema":"buzz-skills","version":1,"aitools":{"skills":["sql","catalog.reader"],"experimental":true,"collision_policy":"replace-managed"}}`
	if got.CompactJSON != wantJSON {
		t.Errorf("CompactJSON = %q, want %q", got.CompactJSON, wantJSON)
	}
	if got.Kind != operatorconfig.KindSkills {
		t.Errorf("Kind = %q, want %q", got.Kind, operatorconfig.KindSkills)
	}
	if got.Summary.MCP != nil {
		t.Errorf("MCP summary = %#v, want nil", got.Summary.MCP)
	}
	wantSummary := &operatorconfig.SkillsSummary{
		Schema:            skillconfig.CurrentSchema,
		Version:           skillconfig.CurrentVersion,
		Enabled:           true,
		Skills:            []string{"sql", "catalog.reader"},
		Experimental:      true,
		CollisionPolicy:   skillconfig.CollisionReplaceManaged,
		UsesDefaultSkills: false,
	}
	if !reflect.DeepEqual(got.Summary.Skills, wantSummary) {
		t.Errorf("Skills summary = %#v, want %#v", got.Summary.Skills, wantSummary)
	}
}

func TestValidateSkillsSummaryDefaultsAndDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		wantJSON string
		want     operatorconfig.SkillsSummary
	}{
		{
			name:     "aitools omitted disables synchronization",
			input:    `{"schema":"buzz-skills","version":1}`,
			wantJSON: `{"schema":"buzz-skills","version":1}`,
			want: operatorconfig.SkillsSummary{
				Schema: skillconfig.CurrentSchema, Version: skillconfig.CurrentVersion,
			},
		},
		{
			name:     "empty aitools uses defaults and fail closed policy",
			input:    `{"schema":"buzz-skills","version":1,"aitools":{}}`,
			wantJSON: `{"schema":"buzz-skills","version":1,"aitools":{}}`,
			want: operatorconfig.SkillsSummary{
				Schema: skillconfig.CurrentSchema, Version: skillconfig.CurrentVersion,
				Enabled: true, UsesDefaultSkills: true, CollisionPolicy: skillconfig.CollisionFail,
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := operatorconfig.Validate(operatorconfig.Input{SkillsJSON: tt.input})
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if got.CompactJSON != tt.wantJSON {
				t.Errorf("CompactJSON = %q, want %q", got.CompactJSON, tt.wantJSON)
			}
			if got.Summary.Skills == nil || !reflect.DeepEqual(*got.Summary.Skills, tt.want) {
				t.Errorf("Skills summary = %#v, want %#v", got.Summary.Skills, tt.want)
			}
		})
	}
}

func TestValidateAcceptsEachInputMode(t *testing.T) {
	t.Parallel()
	mcp := `{"schema":"buzz-managed-mcp","version":1,"servers":[]}`
	skills := `{"schema":"buzz-skills","version":1}`
	dir := t.TempDir()
	mcpPath := filepath.Join(dir, "mcp.json")
	skillsPath := filepath.Join(dir, "skills.json")
	if err := os.WriteFile(mcpPath, []byte(mcp), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillsPath, []byte(skills), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		in   operatorconfig.Input
		kind operatorconfig.Kind
	}{
		{"MCP JSON", operatorconfig.Input{MCPJSON: mcp}, operatorconfig.KindMCP},
		{"MCP file", operatorconfig.Input{MCPFile: mcpPath}, operatorconfig.KindMCP},
		{"skills JSON", operatorconfig.Input{SkillsJSON: skills}, operatorconfig.KindSkills},
		{"skills file", operatorconfig.Input{SkillsFile: skillsPath}, operatorconfig.KindSkills},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := operatorconfig.Validate(tt.in)
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if got.Kind != tt.kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tt.kind)
			}
		})
	}
}

func TestValidateRejectsMissingAndAmbiguousInputs(t *testing.T) {
	t.Parallel()
	validMCP := `{"schema":"buzz-managed-mcp","version":1,"servers":[]}`
	validSkills := `{"schema":"buzz-skills","version":1}`
	missingFile := filepath.Join(t.TempDir(), "must-not-be-read")

	tests := []struct {
		name string
		in   operatorconfig.Input
		want error
	}{
		{"missing", operatorconfig.Input{}, operatorconfig.ErrMissingInput},
		{"inline and file of same kind", operatorconfig.Input{MCPJSON: validMCP, MCPFile: missingFile}, operatorconfig.ErrAmbiguousInput},
		{"both inline kinds", operatorconfig.Input{MCPJSON: validMCP, SkillsJSON: validSkills}, operatorconfig.ErrAmbiguousInput},
		{"both file kinds", operatorconfig.Input{MCPFile: missingFile, SkillsFile: missingFile}, operatorconfig.ErrAmbiguousInput},
		{"three inputs", operatorconfig.Input{MCPJSON: validMCP, MCPFile: missingFile, SkillsJSON: validSkills}, operatorconfig.ErrAmbiguousInput},
		{"all inputs", operatorconfig.Input{MCPJSON: validMCP, MCPFile: missingFile, SkillsJSON: validSkills, SkillsFile: missingFile}, operatorconfig.ErrAmbiguousInput},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := operatorconfig.Validate(tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Validate() error = %v, want errors.Is(_, %v)", err, tt.want)
			}
			if !reflect.DeepEqual(got, operatorconfig.Result{}) {
				t.Errorf("Validate() result = %#v, want zero value", got)
			}
		})
	}
}

func TestValidateDelegatesStrictSchemaParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      operatorconfig.Input
		wantErr string
	}{
		{
			"MCP unknown field",
			operatorconfig.Input{MCPJSON: `{"schema":"buzz-managed-mcp","version":1,"servers":[],"profile":"prod"}`},
			`validate MCP config: decode managed MCP config: json: unknown field "profile"`,
		},
		{
			"MCP trailing value",
			operatorconfig.Input{MCPJSON: `{"schema":"buzz-managed-mcp","version":1,"servers":[]} {}`},
			"validate MCP config: decode managed MCP config: multiple JSON values",
		},
		{
			"MCP wrong schema",
			operatorconfig.Input{MCPJSON: `{"schema":"buzz-skills","version":1,"servers":[]}`},
			`validate MCP config: schema must be "buzz-managed-mcp"`,
		},
		{
			"skills unknown field",
			operatorconfig.Input{SkillsJSON: `{"schema":"buzz-skills","version":1,"host":"https://example.invalid"}`},
			`validate skills config: decode skills config: json: unknown field "host"`,
		},
		{
			"skills trailing value",
			operatorconfig.Input{SkillsJSON: `{"schema":"buzz-skills","version":1} {}`},
			"validate skills config: decode skills config: multiple JSON values",
		},
		{
			"skills wrong version",
			operatorconfig.Input{SkillsJSON: `{"schema":"buzz-skills","version":2}`},
			"validate skills config: version must be 1",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := operatorconfig.Validate(tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, operatorconfig.Result{}) {
				t.Errorf("Validate() result = %#v, want zero value", got)
			}
		})
	}
}

func TestValidateFileErrorIdentifiesKindAndPath(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "absent.json")
	got, err := operatorconfig.Validate(operatorconfig.Input{SkillsFile: path})
	if err == nil {
		t.Fatal("Validate() error = nil, want file error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Validate() error = %v, want errors.Is(_, os.ErrNotExist)", err)
	}
	if !strings.Contains(err.Error(), `read skills config file "`+path+`"`) {
		t.Errorf("Validate() error = %q, want kind and path", err)
	}
	if !reflect.DeepEqual(got, operatorconfig.Result{}) {
		t.Errorf("Validate() result = %#v, want zero value", got)
	}
}

func TestValidateDoesNotMutateProcessEnvironment(t *testing.T) {
	const key = "BUZZ_OPERATORCONFIG_HERMETIC_TEST"
	t.Setenv(key, "sentinel")

	_, err := operatorconfig.Validate(operatorconfig.Input{MCPJSON: `{"schema":"buzz-managed-mcp","version":1,"servers":[]}`})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := os.Getenv(key); got != "sentinel" {
		t.Errorf("environment value = %q, want sentinel", got)
	}
}
