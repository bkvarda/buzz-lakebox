package payload

import (
	"fmt"
	"strings"
	"testing"
)

const validDeployJSON = `{
  "op": "deploy",
  "request_id": "11111111-1111-1111-1111-111111111111",
  "agent": {
    "name": "Reviewer",
    "relay_url": "wss://relay.example.com",
    "private_key_nsec": "nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5",
    "auth_tag": "tag-abc",
    "agent_command": "buzz-agent",
    "agent_args": ["--flag"],
    "system_prompt": "You are a reviewer.",
    "model": "databricks-claude-opus-4-8",
    "provider": "databricks_v2",
    "turn_timeout_seconds": 120,
    "idle_timeout_seconds": 900,
    "max_turn_duration_seconds": 7200,
    "parallelism": 1,
    "respond_to": "owner-only",
    "respond_to_allowlist": ["npub1abc"],
    "env_vars": {"FOO": "bar"}
  },
  "provider_config": {
    "profile": "tanner-west",
    "idle_timeout": "1h",
    "keep_workspace_pat": false,
    "buzz_version": "0.4.24"
  }
}`

func TestParseDeployRequest_GoldenValid(t *testing.T) {
	req, err := ParseDeployRequest([]byte(validDeployJSON))
	if err != nil {
		t.Fatalf("ParseDeployRequest error: %v", err)
	}
	if req.Op != "deploy" {
		t.Fatalf("Op = %q, want deploy", req.Op)
	}
	if req.Agent.Name != "Reviewer" {
		t.Fatalf("Agent.Name = %q", req.Agent.Name)
	}
	if req.Agent.Model == nil || *req.Agent.Model != "databricks-claude-opus-4-8" {
		t.Fatalf("Agent.Model = %v", req.Agent.Model)
	}
	if req.Agent.Provider == nil || *req.Agent.Provider != "databricks_v2" {
		t.Fatalf("Agent.Provider = %v", req.Agent.Provider)
	}
	if req.ProviderConfig.Profile != "tanner-west" {
		t.Fatalf("ProviderConfig.Profile = %q", req.ProviderConfig.Profile)
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error on golden valid payload: %v", err)
	}
}

func TestParseDeployRequest_NullableFieldsAcceptNull(t *testing.T) {
	body := `{"op":"deploy","agent":{"name":"a","relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","agent_command":"buzz-agent","model":null,"provider":null}}`
	req, err := ParseDeployRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseDeployRequest error: %v", err)
	}
	if req.Agent.Model != nil {
		t.Fatalf("Agent.Model = %v, want nil", req.Agent.Model)
	}
	if req.Agent.Provider != nil {
		t.Fatalf("Agent.Provider = %v, want nil", req.Agent.Provider)
	}
}

func TestParseDeployRequest_TolerantOfUnknownFields(t *testing.T) {
	body := `{
	  "op":"deploy",
	  "totally_new_top_level_field": 42,
	  "agent":{
	    "name":"a","relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t",
	    "agent_command":"buzz-agent",
	    "some_future_agent_field": {"nested": true}
	  },
	  "provider_config": {"some_future_config_field": "x"}
	}`
	req, err := ParseDeployRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseDeployRequest should tolerate unknown fields, got error: %v", err)
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}

func TestParseDeployRequest_CurrentLaunchIsAuthoritative(t *testing.T) {
	body := `{
	  "op":"deploy",
	  "agent":{
	    "relay_url":"wss://relay.example","private_key_nsec":"nsec1x","auth_tag":"tag",
	    "agent_command":"stale-command","agent_args":["stale"],
	    "system_prompt":"stale prompt","parallelism":99,
	    "env_vars":{"STALE":"must-not-return","BUZZ_ACP_SESSION_POLICY":"channel"},
	    "launch":{
	      "command":"buzz-agent","args":["--fresh"],
	      "policy_env":{"BUZZ_ACP_AGENTS":"4","BUZZ_ACP_SYSTEM_PROMPT":"fresh prompt","BUZZ_ACP_SESSION_POLICY":"thread","BUZZ_ACP_LAZY_POOL":"true","BUZZ_ACP_RESPOND_TO":"allowlist","BUZZ_ACP_RESPOND_TO_ALLOWLIST":"owner-a,owner-b","BUZZ_ACP_AGENT_COMMAND":"spoof"},
	      "env":{"USER_KEY":"user-value","BUZZ_ACP_SESSION_POLICY":"user-thread","BUZZ_ACP_RESPOND_TO":"anyone","BUZZ_PRIVATE_KEY":"spoof","BUZZ_ACP_RELAY_OBSERVER":"false"},
	      "owner_pubkey":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	    }
	  }
	}`
	req, err := ParseDeployRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Agent.AgentCommand != "buzz-agent" || len(req.Agent.AgentArgs) != 1 || req.Agent.AgentArgs[0] != "--fresh" {
		t.Fatalf("launch command/args not authoritative: %#v", req.Agent)
	}
	if req.Agent.SystemPrompt != "fresh prompt" || req.Agent.Parallelism != 4 {
		t.Fatalf("launch policy fields not projected: prompt=%q parallelism=%d", req.Agent.SystemPrompt, req.Agent.Parallelism)
	}
	if req.Agent.OwnerPubkey != strings.Repeat("a", 64) {
		t.Fatalf("owner pubkey not projected: %q", req.Agent.OwnerPubkey)
	}
	if _, ok := req.Agent.EnvVars["STALE"]; ok {
		t.Fatal("legacy env_vars must not be re-merged when launch is present")
	}
	if got := req.Agent.EnvVars["BUZZ_ACP_SESSION_POLICY"]; got != "user-thread" {
		t.Fatalf("launch.env must win over policy_env, got %q", got)
	}
	if got := req.Agent.EnvVars["USER_KEY"]; got != "user-value" {
		t.Fatalf("launch.env missing user value: %q", got)
	}
	if req.Agent.RespondTo != "anyone" || len(req.Agent.RespondToAllowlist) != 0 {
		t.Fatalf("launch response policy not projected: respond_to=%q allowlist=%v", req.Agent.RespondTo, req.Agent.RespondToAllowlist)
	}
	for _, key := range []string{"BUZZ_ACP_AGENT_COMMAND", "BUZZ_PRIVATE_KEY", "BUZZ_ACP_RELAY_OBSERVER", "BUZZ_ACP_RESPOND_TO", "BUZZ_ACP_RESPOND_TO_ALLOWLIST"} {
		if _, ok := req.Agent.EnvVars[key]; ok {
			t.Fatalf("provider-owned key %s must be stripped from lower tiers", key)
		}
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("current launch should validate: %v", err)
	}
}

func TestParseDeployRequest_CurrentLaunchProjectsAllowlist(t *testing.T) {
	body := `{"op":"deploy","agent":{"relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","launch":{"command":"buzz-agent","env":{},"policy_env":{"BUZZ_ACP_RESPOND_TO":"allowlist","BUZZ_ACP_RESPOND_TO_ALLOWLIST":"owner-a,owner-b"}}}}`
	req, err := ParseDeployRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Agent.RespondTo != "allowlist" || len(req.Agent.RespondToAllowlist) != 2 || req.Agent.RespondToAllowlist[0] != "owner-a" || req.Agent.RespondToAllowlist[1] != "owner-b" {
		t.Fatalf("response policy = %q %v, want projected allowlist", req.Agent.RespondTo, req.Agent.RespondToAllowlist)
	}
}

func TestParseDeployRequest_CurrentLaunchDefaultsRespondToOwnerOnly(t *testing.T) {
	body := `{"op":"deploy","agent":{"relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","respond_to":"stale","respond_to_allowlist":["stale"],"launch":{"command":"buzz-agent","env":{},"policy_env":{}}}}`
	req, err := ParseDeployRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Agent.RespondTo != "owner-only" || len(req.Agent.RespondToAllowlist) != 0 {
		t.Fatalf("response policy = %q %v, want owner-only and empty", req.Agent.RespondTo, req.Agent.RespondToAllowlist)
	}
}

func TestParseDeployRequest_CurrentLaunchRequiresCommand(t *testing.T) {
	body := `{"op":"deploy","agent":{"relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","agent_command":"buzz-agent","launch":{"env":{"A":"b"}}}}`
	if _, err := ParseDeployRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), "agent.launch.command") {
		t.Fatalf("expected launch command error, got %v", err)
	}
}

func TestParseDeployRequest_MalformedJSON(t *testing.T) {
	cases := []string{
		``,
		`{`,
		`not json`,
		`{"op": "deploy", "agent": "should be an object"}`,
	}
	for _, body := range cases {
		if _, err := ParseDeployRequest([]byte(body)); err == nil {
			t.Fatalf("ParseDeployRequest(%q) should have failed", body)
		}
	}
}

func TestValidate_MissingSecrets(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{
			"missing private_key_nsec",
			`{"agent":{"relay_url":"wss://r","auth_tag":"t","agent_command":"buzz-agent"}}`,
		},
		{
			"empty private_key_nsec",
			`{"agent":{"relay_url":"wss://r","private_key_nsec":"","auth_tag":"t","agent_command":"buzz-agent"}}`,
		},
		{
			"missing relay_url",
			`{"agent":{"private_key_nsec":"nsec1x","auth_tag":"t","agent_command":"buzz-agent"}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := ParseDeployRequest([]byte(c.json))
			if err != nil {
				t.Fatalf("ParseDeployRequest error: %v", err)
			}
			if err := req.Validate(); err == nil {
				t.Fatalf("Validate() should have failed for %s", c.name)
			}
		})
	}
}

func TestValidate_UnsupportedRuntimeRejected(t *testing.T) {
	// "codex" has moved OUT of this list: it is now an accepted alias.
	// Bare "claude" stays, because upstream excludes it too — see
	// TestRuntimeFor_BareClaudeRejected for why the two are not symmetric.
	for _, runtime := range []string{"goose", "claude", "python foo.py", ""} {
		body := `{"agent":{"relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","agent_command":"` + runtime + `"}}`
		req, err := ParseDeployRequest([]byte(body))
		if err != nil {
			t.Fatalf("ParseDeployRequest error: %v", err)
		}
		err = req.Validate()
		if err == nil {
			t.Fatalf("Validate() should reject agent_command %q", runtime)
		}
		// The error used to point at issue #3 as "the only runtime still
		// outstanding". Codex now ships, so it names the accepted set
		// instead — a link to a closed issue is worse than no link.
		if !strings.Contains(err.Error(), "supported:") {
			t.Fatalf("error for agent_command %q should list the accepted commands, got: %v", runtime, err)
		}
	}
}

func TestValidate_SupportedRuntimeAccepted(t *testing.T) {
	body := `{"agent":{"relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","agent_command":"buzz-agent"}}`
	req, err := ParseDeployRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseDeployRequest error: %v", err)
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() should accept buzz-agent, got: %v", err)
	}
}

// TestValidate_EnvVarsKeyNames guards against a CRITICAL bug: internal/nest's
// RenderEnv writes env_vars KEYS raw (unquoted) into a file that is later
// `.`-sourced by a shell (see nest.go's emit()). An env_vars key containing
// shell metacharacters or a newline would achieve arbitrary command
// execution in that shell — right after it has exported the agent's nsec,
// auth tag, and DATABRICKS_TOKEN. Agent.Validate() must reject any key that
// isn't a valid POSIX shell/environment variable name.
func TestValidate_EnvVarsKeyNames(t *testing.T) {
	baseAgent := func(envVars map[string]string) Agent {
		return Agent{
			RelayURL:       "wss://r",
			PrivateKeyNsec: "nsec1x",
			AuthTag:        "t",
			AgentCommand:   SupportedAgentCommand,
			EnvVars:        envVars,
		}
	}

	accepted := []string{"PATH", "DATABRICKS_TOKEN", "_UNDERSCORE_LEAD", "A1"}
	for _, key := range accepted {
		t.Run("accepted/"+key, func(t *testing.T) {
			agent := baseAgent(map[string]string{key: "value"})
			if err := agent.Validate(); err != nil {
				t.Fatalf("Validate() should accept env_vars key %q, got: %v", key, err)
			}
		})
	}

	rejected := []string{
		"X=1$(id)",
		"FOO\nBAR",
		"FOO BAR",
		"FOO;id",
		"FOO-BAR",
		"1LEADINGDIGIT",
		"",
		"FOO`id`",
	}
	for _, key := range rejected {
		t.Run(fmt.Sprintf("rejected/%q", key), func(t *testing.T) {
			agent := baseAgent(map[string]string{key: "value"})
			err := agent.Validate()
			if err == nil {
				t.Fatalf("Validate() should reject env_vars key %q", key)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", key)) {
				t.Fatalf("error should name the offending key %q, got: %v", key, err)
			}
			if strings.Contains(err.Error(), "value") {
				t.Fatalf("error must not echo the key's VALUE, got: %v", err)
			}
		})
	}
}

// TestValidate_InferenceAuth covers provider_config.inference_auth: the
// allowed values ""/"env"/"sandbox" must validate, and anything else
// (including a case variant like "SANDBOX") must be rejected — matching is
// exact-case only, deliberately, so a typo never silently falls back to a
// different auth mode.
func TestValidate_InferenceAuth(t *testing.T) {
	baseAgentJSON := `"agent":{"relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","agent_command":"buzz-agent"}`

	valid := []string{"", "env", "sandbox"}
	for _, value := range valid {
		t.Run(fmt.Sprintf("valid/%q", value), func(t *testing.T) {
			body := fmt.Sprintf(`{%s,"provider_config":{"inference_auth":%q}}`, baseAgentJSON, value)
			req, err := ParseDeployRequest([]byte(body))
			if err != nil {
				t.Fatalf("ParseDeployRequest error: %v", err)
			}
			if req.ProviderConfig.InferenceAuth != value {
				t.Fatalf("ProviderConfig.InferenceAuth = %q, want %q", req.ProviderConfig.InferenceAuth, value)
			}
			if err := req.Validate(); err != nil {
				t.Fatalf("Validate() should accept inference_auth %q, got: %v", value, err)
			}
			wantSandbox := value == "sandbox"
			if got := req.ProviderConfig.SandboxInferenceAuth(); got != wantSandbox {
				t.Fatalf("SandboxInferenceAuth() = %v, want %v for inference_auth %q", got, wantSandbox, value)
			}
		})
	}

	rejected := []string{"oauth", "SANDBOX", "Env", "sandbox "}
	for _, value := range rejected {
		t.Run(fmt.Sprintf("rejected/%q", value), func(t *testing.T) {
			body := fmt.Sprintf(`{%s,"provider_config":{"inference_auth":%q}}`, baseAgentJSON, value)
			req, err := ParseDeployRequest([]byte(body))
			if err != nil {
				t.Fatalf("ParseDeployRequest error: %v", err)
			}
			err = req.Validate()
			if err == nil {
				t.Fatalf("Validate() should reject inference_auth %q", value)
			}
			if !strings.Contains(err.Error(), "provider_config.inference_auth") {
				t.Fatalf("error should name provider_config.inference_auth, got: %v", err)
			}
		})
	}
}

// TestValidate_EnvVarsAllValidKeysStillPasses is a regression check: a
// payload with only valid env_vars keys must still validate cleanly.
func TestValidate_EnvVarsAllValidKeysStillPasses(t *testing.T) {
	agent := Agent{
		RelayURL:       "wss://r",
		PrivateKeyNsec: "nsec1x",
		AuthTag:        "t",
		AgentCommand:   SupportedAgentCommand,
		EnvVars: map[string]string{
			"PATH":             "/usr/bin",
			"DATABRICKS_TOKEN": "dapi-xyz",
			"_UNDERSCORE_LEAD": "1",
			"A1":               "2",
		},
	}
	if err := agent.Validate(); err != nil {
		t.Fatalf("Validate() should accept a payload with only valid env_vars keys, got: %v", err)
	}
}

// TestProviderConfig_McpMode exercises the McpMode() helper: 0 entries →
// McpNone, 1 entry → McpDirect, ≥2 entries → McpMux. Callers (nest.RenderEnv,
// deployflow) branch on this intent value rather than re-deriving it from
// len() at each site.
func TestProviderConfig_McpMode(t *testing.T) {
	cases := []struct {
		name     string
		servers  []string
		wantMode McpMode
	}{
		{"no entries is McpNone", nil, McpNone},
		{"one entry is McpDirect", []string{"shellbox-mcp"}, McpDirect},
		{"two entries is McpMux", []string{"a", "b"}, McpMux},
		{"many entries is still McpMux", []string{"a", "b", "c"}, McpMux},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ProviderConfig{McpServers: tc.servers}
			if got := cfg.McpMode(); got != tc.wantMode {
				t.Errorf("McpMode() = %v, want %v (for %d entries)", got, tc.wantMode, len(tc.servers))
			}
		})
	}
}

// TestProviderConfig_McpDirectCommand exercises McpDirectCommand(): returns the
// single entry when McpMode() == McpDirect, and "" in McpNone or McpMux.
func TestProviderConfig_McpDirectCommand(t *testing.T) {
	cases := []struct {
		name    string
		servers []string
		want    string
	}{
		{"no entries returns empty string", nil, ""},
		{"one entry returns that entry", []string{"shellbox-mcp"}, "shellbox-mcp"},
		{"two entries returns empty string", []string{"a", "b"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ProviderConfig{McpServers: tc.servers}
			if got := cfg.McpDirectCommand(); got != tc.want {
				t.Errorf("McpDirectCommand() = %q, want %q (for %d entries)", got, tc.want, len(tc.servers))
			}
		})
	}
}

func TestParseDeployRequest_CurrentLaunchAbsentValuesDoNotFallBackToLegacy(t *testing.T) {
	body := `{"op":"deploy","agent":{"relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","agent_command":"stale","system_prompt":"stale","model":"stale","provider":"stale","parallelism":99,"idle_timeout_seconds":88,"max_turn_duration_seconds":77,"env_vars":{"STALE":"yes"},"launch":{"command":"buzz-agent","args":[],"env":{},"policy_env":{}}}}`
	req, err := ParseDeployRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Agent.SystemPrompt != "" || req.Agent.Model != nil || req.Agent.Provider != nil || req.Agent.Parallelism != 1 || req.Agent.IdleTimeoutSeconds != 0 || req.Agent.MaxTurnDurationSecs != 0 || len(req.Agent.EnvVars) != 0 {
		t.Fatalf("launch absence resurrected legacy fields: %#v", req.Agent)
	}
}
