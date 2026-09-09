package payload

import (
	"fmt"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/skillconfig"

	"github.com/IceRhymers/buzz-lakebox/internal/install"
)

// capReq builds a deploy request whose provider_config carries the capability
// keys under test, mirroring ownerPATReq. The runtime "codex-acp" is one
// RuntimeFor accepts (see runtime.go).
func capReq(cfg ProviderConfig) DeployRequest {
	return DeployRequest{
		Op: "deploy",
		Agent: Agent{
			RelayURL: "wss://r", PrivateKeyNsec: "nsec1x", AuthTag: "t",
			AgentCommand: "codex-acp",
		},
		ProviderConfig: cfg,
	}
}

// validExtraBinary is a structurally-valid entry reused across the tests that
// need the structural checks to pass so a different check can be exercised.
func validExtraBinary() ExtraBinary {
	return ExtraBinary{
		URL:    "https://example.com/shellbox-mcp",
		SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Bin:    "shellbox-mcp",
	}
}

// TestValidate_CapabilityKeysRefusedUnderOwnerPAT pins the gate: either
// capability key is refused whenever the sandbox holds a workspace-owner
// credential, under BOTH inference_auth="sandbox" and keep_workspace_pat=true.
// The error must name the key and point at the inference_auth="env" remedy.
func TestValidate_CapabilityKeysRefusedUnderOwnerPAT(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ProviderConfig
		wantKey string
	}{
		{"extra_binaries under sandbox", ProviderConfig{InferenceAuth: "sandbox", ExtraBinaries: []ExtraBinary{validExtraBinary()}}, "extra_binaries"},
		{"mcp_servers under sandbox", ProviderConfig{InferenceAuth: "sandbox", McpServers: []string{"shellbox"}}, "mcp_servers"},
		{"extra_binaries under keep_workspace_pat", ProviderConfig{KeepWorkspacePAT: true, ExtraBinaries: []ExtraBinary{validExtraBinary()}}, "extra_binaries"},
		{"mcp_servers under keep_workspace_pat", ProviderConfig{KeepWorkspacePAT: true, McpServers: []string{"shellbox"}}, "mcp_servers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := capReq(tc.cfg).Validate()
			if err == nil {
				t.Fatalf("%s: capability key must be refused when the sandbox holds an owner credential", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("%s: rejection should name %s, got: %v", tc.name, tc.wantKey, err)
			}
			if !strings.Contains(err.Error(), `inference_auth="env"`) {
				t.Errorf("%s: rejection should point at the inference_auth=\"env\" remedy, got: %v", tc.name, err)
			}
		})
	}
}

// TestValidate_CapabilityKeysGateDeterministic pins that a payload setting
// BOTH keys always reports extra_binaries first.
func TestValidate_CapabilityKeysGateDeterministic(t *testing.T) {
	cfg := ProviderConfig{
		InferenceAuth: "sandbox",
		ExtraBinaries: []ExtraBinary{validExtraBinary()},
		McpServers:    []string{"shellbox"},
	}
	err := capReq(cfg).Validate()
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !strings.Contains(err.Error(), "extra_binaries") {
		t.Errorf("with both keys set the gate must report extra_binaries first, got: %v", err)
	}
}

// TestValidate_CapabilityKeysPermittedUnderEnv pins the asymmetry: under
// inference_auth="env" the owner-PAT gate does not fire, so the capability
// keys are permitted by that gate. (A structurally-valid extra_binaries entry
// is used so validateExtraBinaries does not fire either.)
func TestValidate_CapabilityKeysPermittedUnderEnv(t *testing.T) {
	env := map[string]string{"DATABRICKS_HOST": "https://mine.example", "DATABRICKS_TOKEN": "dapi-own"}
	req := capReq(ProviderConfig{
		InferenceAuth: "env",
		ExtraBinaries: []ExtraBinary{validExtraBinary()},
		McpServers:    []string{"shellbox"},
	})
	req.Agent.EnvVars = env
	if err := req.Validate(); err != nil {
		t.Errorf("env mode uses the owner's own credential and must permit the capability keys: %v", err)
	}
}

// envReq builds an env-mode request carrying the given extra_binaries, so the
// owner-PAT gate never fires and validateExtraBinaries is exercised in
// isolation.
func envReq(bins ...ExtraBinary) DeployRequest {
	req := capReq(ProviderConfig{InferenceAuth: "env", ExtraBinaries: bins})
	req.Agent.EnvVars = map[string]string{"DATABRICKS_HOST": "https://mine.example", "DATABRICKS_TOKEN": "dapi-own"}
	return req
}

// TestValidate_ExtraBinariesURL covers the url structural rule: https only,
// and with a host (a hostless "https://" must fail loud here, not at fetch).
func TestValidate_ExtraBinariesURL(t *testing.T) {
	for _, bad := range []string{"", "http://example.com/x", "ftp://example.com/x", "://nohost", "https://", "https:///path"} {
		eb := validExtraBinary()
		eb.URL = bad
		if err := envReq(eb).Validate(); err == nil {
			t.Errorf("extra_binaries.url %q must be rejected", bad)
		}
	}
}

// TestValidate_ExtraBinariesSHA256 covers the sha256 structural rule: required,
// lowercase 64-hex only.
func TestValidate_ExtraBinariesSHA256(t *testing.T) {
	cases := map[string]string{
		"missing":       "",
		"too short":     "0123456789abcdef",
		"non-hex chars": "z123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"uppercase":     "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF",
	}
	for name, sha := range cases {
		eb := validExtraBinary()
		eb.SHA256 = sha
		if err := envReq(eb).Validate(); err == nil {
			t.Errorf("extra_binaries.sha256 %q (%s) must be rejected", sha, name)
		}
	}
}

// TestValidate_ExtraBinariesBinTraversal covers the bare-filename rule: path
// separators, traversal names, empty, and leading-dash (option-injection)
// names are all rejected.
func TestValidate_ExtraBinariesBinTraversal(t *testing.T) {
	for _, bad := range []string{"foo/bar", "..", ".", "a\\b", "", "sub/../x", "-rf", "-n"} {
		eb := validExtraBinary()
		eb.Bin = bad
		if err := envReq(eb).Validate(); err == nil {
			t.Errorf("extra_binaries.bin %q must be rejected", bad)
		}
	}
}

// TestValidate_ExtraBinariesBinCollision table-drives over EVERY reserved
// name: install.BinNames plus both install.AdapterBinNames() entries. Each
// must be refused because it would shadow a real binary in the launch PATH.
func TestValidate_ExtraBinariesBinCollision(t *testing.T) {
	var reserved []string
	reserved = append(reserved, install.BinNames...)
	reserved = append(reserved, install.AdapterBinNames()...)
	// "codex" is not installed by the provider but is deliberately preserved
	// (the image's /usr/local/bin ucode wrapper); a bin named "codex" must be
	// refused too so it cannot shadow that wrapper.
	reserved = append(reserved, "codex")
	for _, name := range reserved {
		eb := validExtraBinary()
		eb.Bin = name
		err := envReq(eb).Validate()
		if err == nil {
			t.Errorf("extra_binaries.bin %q must be rejected: it collides with an installed binary", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("collision rejection for %q should name the bin, got: %v", name, err)
		}
	}
}

// TestValidate_ExtraBinariesStructuralRunsInEveryMode pins that the structural
// checks are UNCONDITIONAL: a malformed entry is rejected even in env mode,
// where the owner-PAT gate does not fire.
func TestValidate_ExtraBinariesStructuralRunsInEveryMode(t *testing.T) {
	eb := validExtraBinary()
	eb.URL = "http://example.com/x"
	if err := envReq(eb).Validate(); err == nil {
		t.Error("structural checks must run in env mode too")
	}
}

// TestValidate_NoCapabilityKeysNoError is the regression guard: a request with
// neither capability key validates cleanly under an owner-PAT mode.
func TestValidate_NoCapabilityKeysNoError(t *testing.T) {
	req := capReq(ProviderConfig{InferenceAuth: "sandbox"})
	if err := req.Validate(); err != nil {
		t.Errorf("a request with neither capability key must validate: %v", err)
	}
}

// TestValidate_SingleValidExtraBinaryPasses is the positive regression: a
// single well-formed entry in env mode passes full Validate().
func TestValidate_SingleValidExtraBinaryPasses(t *testing.T) {
	if err := envReq(validExtraBinary()).Validate(); err != nil {
		t.Errorf("a valid extra_binaries entry in env mode must pass Validate(): %v", err)
	}
}

// TestCapabilityKeyNamesPassSecretWordFilter documents (#18 item 4) why these
// keys are Desktop-shaped-safe on the NAME axis: Buzz's validate_provider_config
// rejects any provider_config key whose name contains a secret-like word
// segment (token|key|secret|password|credential). None of our capability key
// names or the extra_binaries sub-field names contain such a segment, so they
// pass that name filter — the reason a payload is rejected on these keys is
// their ARRAY value (scalar-only), never their name.
func TestCapabilityKeyNamesPassSecretWordFilter(t *testing.T) {
	forbidden := []string{"token", "key", "secret", "password", "credential"}
	for _, name := range []string{"extra_binaries", "mcp_servers", "url", "sha256", "bin"} {
		for _, word := range forbidden {
			if strings.Contains(name, word) {
				t.Errorf("key name %q contains secret-word segment %q; it would be rejected by Buzz's validate_provider_config name filter", name, word)
			}
		}
	}
}

// mcpServersReq builds a deploy request that carries the given mcp_servers
// list without triggering the owner-PAT gate — the default InferenceAuth=""
// leaves OwnerPATInSandbox()=false — so validateMcpServers is exercised in
// isolation. Mirrors envReq for extra_binaries.
func mcpServersReq(servers []string) DeployRequest {
	return capReq(ProviderConfig{McpServers: servers})
}

// nServers returns a slice of n unique, structurally-valid server names of the
// form "srv-0", "srv-1", …, used to hit the count cap without touching other
// validation rules.
func nServers(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("srv-%d", i)
	}
	return out
}

// TestValidate_McpServers is the structural-validation table for
// provider_config.mcp_servers, mirroring the extra_binaries suite. Each
// sub-test exercises exactly one rule; passing cases confirm the happy path
// at each boundary.
func TestValidate_McpServers(t *testing.T) {
	// --- passing cases ---
	t.Run("empty list ok", func(t *testing.T) {
		if err := mcpServersReq(nil).Validate(); err != nil {
			t.Fatalf("empty mcp_servers must validate: %v", err)
		}
	})
	t.Run("single valid entry ok", func(t *testing.T) {
		if err := mcpServersReq([]string{"shellbox-mcp"}).Validate(); err != nil {
			t.Fatalf("single valid entry must validate: %v", err)
		}
	})
	t.Run("entries at the count limit ok", func(t *testing.T) {
		if err := mcpServersReq(nServers(MaxMcpServers)).Validate(); err != nil {
			t.Fatalf("%d entries (at the cap) must validate: %v", MaxMcpServers, err)
		}
	})

	// --- rejection cases ---
	t.Run("count cap exceeded", func(t *testing.T) {
		err := mcpServersReq(nServers(MaxMcpServers + 1)).Validate()
		if err == nil {
			t.Fatalf("%d entries (one over cap) must be rejected", MaxMcpServers+1)
		}
		if !strings.Contains(err.Error(), "provider_config.mcp_servers") {
			t.Errorf("rejection must name provider_config.mcp_servers, got: %v", err)
		}
	})
	t.Run("duplicate entry rejected", func(t *testing.T) {
		err := mcpServersReq([]string{"buzz-dev-mcp", "buzz-dev-mcp"}).Validate()
		if err == nil {
			t.Fatal("duplicate mcp_servers entry must be rejected")
		}
		if !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("rejection must name 'duplicate', got: %v", err)
		}
	})
	t.Run("dot traversal rejected", func(t *testing.T) {
		err := mcpServersReq([]string{"."}).Validate()
		if err == nil {
			t.Fatal("mcp_servers entry '.' must be rejected")
		}
	})
	t.Run("dotdot traversal rejected", func(t *testing.T) {
		err := mcpServersReq([]string{".."}).Validate()
		if err == nil {
			t.Fatal("mcp_servers entry '..' must be rejected")
		}
	})
	t.Run("path separator rejected", func(t *testing.T) {
		// A name with a "/" must be rejected by the bare-name charset — the
		// entry is invoked by bare command, so a path is never a valid name.
		err := mcpServersReq([]string{"some/server"}).Validate()
		if err == nil {
			t.Fatal("mcp_servers entry with a path separator must be rejected")
		}
		if !strings.Contains(err.Error(), "some/server") {
			t.Errorf("rejection must name the offending entry, got: %v", err)
		}
	})
	t.Run("leading dash rejected", func(t *testing.T) {
		err := mcpServersReq([]string{"-x"}).Validate()
		if err == nil {
			t.Fatal("mcp_servers entry '-x' (leading dash) must be rejected")
		}
		if !strings.Contains(err.Error(), "-x") {
			t.Errorf("rejection must name the offending entry, got: %v", err)
		}
	})
	t.Run("double-underscore name rejected", func(t *testing.T) {
		// buzz-agent uses "__" as its server/tool qualified-name separator and
		// hard-rejects a server name containing it, failing the whole session.
		// A single "_" is still allowed by the charset; only the doubled
		// sequence is illegal.
		err := mcpServersReq([]string{"bad__name"}).Validate()
		if err == nil {
			t.Fatal("mcp_servers entry containing '__' must be rejected")
		}
		if !strings.Contains(err.Error(), "__") {
			t.Errorf("rejection must name the '__' separator, got: %v", err)
		}
		// A single underscore must remain valid.
		if err := mcpServersReq([]string{"buzz_dev_mcp"}).Validate(); err != nil {
			t.Errorf("a single-underscore name must validate: %v", err)
		}
	})
	t.Run("reserved bzmux name rejected", func(t *testing.T) {
		err := mcpServersReq([]string{MuxBinaryName}).Validate()
		if err == nil {
			t.Fatalf("mcp_servers entry %q (the multiplexer's reserved name) must be rejected", MuxBinaryName)
		}
		if !strings.Contains(err.Error(), MuxBinaryName) {
			t.Errorf("rejection must name %q, got: %v", MuxBinaryName, err)
		}
	})
}

// TestValidate_ExtraBinariesBinRejectsMuxBinaryName pins that the MCP
// multiplexer's reserved name (MuxBinaryName = "bzmux") cannot be used as an
// extra_binaries bin name. Such an entry would shadow the embedded multiplexer
// in the $HOME/.buzz-backend/bin dir that launch.sh prepends to PATH, breaking
// any mcp_servers deploy that routes through it. This mirrors the "codex" guard
// in TestValidate_ExtraBinariesBinCollision.
func TestValidate_ExtraBinariesBinRejectsMuxBinaryName(t *testing.T) {
	eb := validExtraBinary()
	eb.Bin = MuxBinaryName
	err := envReq(eb).Validate()
	if err == nil {
		t.Fatalf("extra_binaries.bin %q must be rejected: it collides with the MCP multiplexer's reserved name", MuxBinaryName)
	}
	if !strings.Contains(err.Error(), MuxBinaryName) {
		t.Errorf("rejection must name %q, got: %v", MuxBinaryName, err)
	}
}

func TestValidate_ManagedMCP(t *testing.T) {
	req := capReq(ProviderConfig{InferenceAuth: "env"})
	req.ProviderConfig.MCP = mcpconfig.Config{
		Schema: mcpconfig.CurrentSchema, Version: mcpconfig.CurrentVersion,
		Servers: []mcpconfig.Server{{Name: "sql", Kind: mcpconfig.KindSQL, Auth: mcpconfig.AuthEnv}},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("valid managed MCP rejected: %v", err)
	}
	if req.ProviderConfig.McpMode() != McpMux || !req.ProviderConfig.HasManagedMCP() {
		t.Fatal("managed MCP must use the mux so typed bridge args are preserved")
	}

	req.ProviderConfig.McpServers = []string{"legacy"}
	if err := req.Validate(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestValidate_ManagedMCPRefusedWithOwnerCredential(t *testing.T) {
	req := capReq(ProviderConfig{InferenceAuth: "sandbox"})
	req.ProviderConfig.MCP = mcpconfig.Config{
		Schema: mcpconfig.CurrentSchema, Version: mcpconfig.CurrentVersion,
		Servers: []mcpconfig.Server{{Name: "sql", Kind: mcpconfig.KindSQL, Auth: mcpconfig.AuthSandboxProfile}},
	}
	if err := req.Validate(); err == nil || !strings.Contains(err.Error(), "provider_config.mcp") {
		t.Fatalf("owner credential must refuse managed MCP: %v", err)
	}
}

func TestParseDeployRequest_MCPConfigScalarJSON(t *testing.T) {
	managed := `{"schema":"buzz-managed-mcp","version":1,"servers":[{"name":"warehouse","kind":"sql","auth":"env"}]}`
	body := fmt.Sprintf(`{"op":"deploy","agent":{"relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","agent_command":"codex-acp"},"provider_config":{"inference_auth":"env","mcp_config":%q}}`, managed)
	req, err := ParseDeployRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.ProviderConfig.MCP.Servers) != 1 || req.ProviderConfig.MCP.Servers[0].Kind != mcpconfig.KindSQL {
		t.Fatalf("scalar MCP JSON not parsed: %#v", req.ProviderConfig.MCP)
	}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseDeployRequest_MCPConfigRejectsLiteralHost(t *testing.T) {
	managed := `{"schema":"buzz-managed-mcp","version":1,"servers":[],"url":"https://workspace.example"}`
	body := fmt.Sprintf(`{"op":"deploy","agent":{},"provider_config":{"mcp_config":%q}}`, managed)
	if _, err := ParseDeployRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected custom host rejection, got %v", err)
	}
}

func TestParseDeployRequest_SkillsConfig(t *testing.T) {
	skills := `{"schema":"buzz-skills","version":1,"aitools":{"skills":["bundles","sql"],"collision_policy":"replace-managed"}}`
	body := fmt.Sprintf(`{"op":"deploy","agent":{"relay_url":"wss://r","private_key_nsec":"nsec1x","auth_tag":"t","agent_command":"buzz-agent"},"provider_config":{"inference_auth":"env","skills_config":%q}}`, skills)
	req, err := ParseDeployRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !req.ProviderConfig.HasSkills() || len(req.ProviderConfig.Skills.AITools.Skills) != 2 {
		t.Fatalf("skills config not parsed: %#v", req.ProviderConfig.Skills)
	}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_SkillsConfigRefusesOwnerCredential(t *testing.T) {
	req := capReq(ProviderConfig{InferenceAuth: "sandbox", Skills: skillconfig.Config{Schema: skillconfig.CurrentSchema, Version: 1, AITools: &skillconfig.AITools{}}})
	if err := req.Validate(); err == nil || !strings.Contains(err.Error(), "skills_config") {
		t.Fatalf("expected skills owner-credential rejection, got %v", err)
	}
}
