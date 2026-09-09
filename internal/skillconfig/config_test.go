package skillconfig

import (
	"strings"
	"testing"
)

func TestParseValidVersionedConfig(t *testing.T) {
	cfg, err := Parse([]byte(`{
		"schema":"buzz-skills",
		"version":1,
		"aitools":{
			"skills":["sql-helper","Catalog.Read_Only"],
			"experimental":true,
			"collision_policy":"replace-managed"
		}
	}`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.AITools == nil || len(cfg.AITools.Skills) != 2 || !cfg.AITools.Experimental {
		t.Fatalf("Parse() = %#v", cfg)
	}
	if got := cfg.AITools.EffectiveCollisionPolicy(); got != CollisionReplaceManaged {
		t.Fatalf("EffectiveCollisionPolicy() = %q", got)
	}
}

func TestOptionalAIToolsAndFailDefault(t *testing.T) {
	cfg, err := Parse([]byte(`{"schema":"buzz-skills","version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AITools != nil {
		t.Fatalf("omitted aitools should disable sync, got %#v", cfg.AITools)
	}

	cfg, err = Parse([]byte(`{"schema":"buzz-skills","version":1,"aitools":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AITools.EffectiveCollisionPolicy(); got != CollisionFail {
		t.Fatalf("omitted policy = %q, want %q", got, CollisionFail)
	}
}

func TestParseRejectsNonContractAndEnvironmentSpecificFields(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"unknown top-level", `{"schema":"buzz-skills","version":1,"url":"https://evil.example"}`, "unknown field"},
		{"aitools url", `{"schema":"buzz-skills","version":1,"aitools":{"url":"https://evil.example"}}`, "unknown field"},
		{"profile", `{"schema":"buzz-skills","version":1,"aitools":{"profile":"prod"}}`, "unknown field"},
		{"token", `{"schema":"buzz-skills","version":1,"aitools":{"token":"dapi-secret"}}`, "unknown field"},
		{"env", `{"schema":"buzz-skills","version":1,"aitools":{"env":{"HOME":"x"}}}`, "unknown field"},
		{"wrong schema", `{"schema":"https://evil/schema","version":1}`, "schema"},
		{"wrong version", `{"schema":"buzz-skills","version":2}`, "version"},
		{"missing schema", `{"version":1}`, "schema"},
		{"multiple", `{"schema":"buzz-skills","version":1} {}`, "multiple JSON values"},
		{"junk", `{"schema":"buzz-skills","version":1} nope`, "trailing data"},
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

func TestValidateSkillIdentifiers(t *testing.T) {
	valid := []string{"a", "A0", "sql-helper", "catalog.reader", "under_score", strings.Repeat("x", 64)}
	if err := (Config{Schema: CurrentSchema, Version: CurrentVersion, AITools: &AITools{Skills: valid}}).Validate(); err != nil {
		t.Fatalf("valid identifiers rejected: %v", err)
	}

	tests := []struct {
		name string
		want string
	}{
		{"", "safe identifier"},
		{"-leading", "safe identifier"},
		{"../escape", "safe identifier"},
		{"a/b", "safe identifier"},
		{"has space", "safe identifier"},
		{"$(touch-pwned)", "safe identifier"},
		{"${HOME}", "safe identifier"},
		{"semi;colon", "safe identifier"},
		{"quote'", "safe identifier"},
		{"line\nbreak", "safe identifier"},
		{"é", "safe identifier"},
		{strings.Repeat("x", 65), "1-64"},
		{"buzz-cli", "reserved"},
		{"BUZZ-CLI", "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Config{Schema: CurrentSchema, Version: CurrentVersion, AITools: &AITools{Skills: []string{tt.name}}}).Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateLimitsDuplicatesAndPolicies(t *testing.T) {
	tooMany := make([]string, MaxSkills+1)
	for i := range tooMany {
		tooMany[i] = "skill" + strings.Repeat("x", i/10) + string(rune('a'+i%10))
	}
	tests := []struct {
		name string
		ai   AITools
		want string
	}{
		{"too many", AITools{Skills: tooMany}, "maximum"},
		{"duplicate", AITools{Skills: []string{"one", "one"}}, "duplicates"},
		{"case folded duplicate", AITools{Skills: []string{"One", "oNe"}}, "case-insensitively"},
		{"unknown policy", AITools{CollisionPolicy: "overwrite"}, "must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Config{Schema: CurrentSchema, Version: CurrentVersion, AITools: &tt.ai}).Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}

	for _, policy := range []CollisionPolicy{"", CollisionFail, CollisionReplaceManaged} {
		if err := (AITools{CollisionPolicy: policy}).Validate(); err != nil {
			t.Errorf("valid policy %q rejected: %v", policy, err)
		}
	}
}
