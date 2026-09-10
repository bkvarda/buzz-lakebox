package deployflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/install"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// mcpMuxReq returns a 2-entry mcp_servers DeployRequest (McpMux mode).
func mcpMuxReq() *payload.DeployRequest {
	req := buildReq(reqOpts{})
	req.ProviderConfig.McpServers = []string{"buzz-dev-mcp", "shellbox-mcp"}
	return req
}

// TestDeploy_McpMux_StepsInOrder pins the mux install sequence: mux-bin-write
// → mux-cfg-write → mcp-verify appear after install-exec and before
// verify-exec/launch-exec for a 2-entry mcp_servers payload. The former
// mux-selftest step is GONE — mux-mode mcp-verify (#17) is a superset of it and
// replaces it in the flow.
func TestDeploy_McpMux_StepsInOrder(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-1")

	if _, err := h.dep.Deploy(mcpMuxReq()); err != nil {
		t.Fatalf("mcp mux deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{
		"SSH:install-exec",
		"SSH:mux-bin-write",
		"SSH:mux-launcher-write",
		"SSH:mux-cfg-write",
		"SSH:mcp-verify",
		"SSH:verify-exec",
		"SSH:launch-exec",
	})
	// mux-selftest is replaced by mcp-verify in the deploy flow.
	assertNotContains(t, seq, "SSH:mux-selftest")
}

// TestDeploy_McpMux_StepsAfterExtraBins asserts mux steps appear after
// extra-bins steps when both are present, maintaining the correct ordering:
// install-exec → adapter (if any) → extra-bins → mux → verify-exec.
func TestDeploy_McpMux_StepsAfterExtraBins(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-2")

	req := mcpMuxReq()
	req.ProviderConfig.ExtraBinaries = []payload.ExtraBinary{
		{URL: "https://example.com/dl/shellbox-mcp", SHA256: strings.Repeat("a", 64), Bin: "shellbox-mcp"},
	}

	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatalf("mcp mux + extra-bins deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{
		"SSH:install-exec",
		"SSH:extra-bins-write", "SSH:extra-bins-exec",
		"SSH:mux-bin-write", "SSH:mux-launcher-write", "SSH:mux-cfg-write", "SSH:mcp-verify",
		"SSH:verify-exec",
		"SSH:launch-exec",
	})
	assertNotContains(t, seq, "SSH:mux-selftest")
}

// TestDeploy_McpMux_AbsentForNoMcpServers guards the byte-identical regression:
// a deploy with 0 mcp_servers entries (McpNone) must issue no mux OR mcp-verify
// round trips (the load-bearing Question B guard — McpNone stays byte-identical
// to pre-#17).
func TestDeploy_McpMux_AbsentForNoMcpServers(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-3")

	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatalf("deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	for _, unwanted := range []string{
		"SSH:mux-bin-write", "SSH:mux-launcher-write", "SSH:mux-cfg-write", "SSH:mux-selftest",
		"SSH:mcp-bin-write", "SSH:mcp-verify",
	} {
		assertNotContains(t, seq, unwanted)
	}
}

// TestDeploy_McpMux_AbsentForSingleMcpServer pins the intentional #17 change:
// a deploy with 1 mcp_servers entry (McpDirect) now DOES issue mcp-bin-write +
// mcp-verify (write the embedded bzmux, then verify the single command via a
// real initialize+tools/list handshake), but still issues NO mux-cfg-write or
// mux-selftest — bzmux runs in direct mode, so no mcp-mux.json is written.
func TestDeploy_McpMux_AbsentForSingleMcpServer(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-4")

	req := buildReq(reqOpts{})
	req.ProviderConfig.McpServers = []string{"shellbox-mcp"}

	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatalf("deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	// McpDirect DOES write bzmux and verify it...
	assertOrder(t, seq, []string{
		"SSH:install-exec",
		"SSH:mcp-bin-write",
		"SSH:mcp-verify",
		"SSH:verify-exec",
		"SSH:launch-exec",
	})
	// ...but never writes the mux config or runs the mux selftest (direct mode).
	for _, unwanted := range []string{"SSH:mux-launcher-write", "SSH:mux-cfg-write", "SSH:mux-selftest"} {
		assertNotContains(t, seq, unwanted)
	}
}

// TestDeploy_McpDirect_StepsInOrder pins the McpDirect sequence (#17):
// install-exec → mcp-bin-write → mcp-verify → verify-exec → launch-exec.
func TestDeploy_McpDirect_StepsInOrder(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-direct-1")

	req := buildReq(reqOpts{})
	req.ProviderConfig.McpServers = []string{"buzz-dev-mcp"}

	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatalf("mcp direct deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{
		"SSH:install-exec",
		"SSH:mcp-bin-write",
		"SSH:mcp-verify",
		"SSH:verify-exec",
		"SSH:launch-exec",
	})
}

// TestDeploy_McpDirect_VerifyFailure asserts a failed mcp-verify in direct mode
// surfaces CodeMcpVerify and fails the deploy.
func TestDeploy_McpDirect_VerifyFailure(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-direct-fail")
	t.Setenv("FAKE_MCP_VERIFY_EXIT", "1")

	req := buildReq(reqOpts{})
	req.ProviderConfig.McpServers = []string{"buzz-dev-mcp"}

	_, err := h.dep.Deploy(req)
	if err == nil {
		t.Fatal("expected mcp-verify failure to fail the deploy")
	}
	if got := CodeOf(err); got != CodeMcpVerify {
		t.Fatalf("code = %q, want %q (error: %v)", got, CodeMcpVerify, err)
	}
	if !strings.Contains(err.Error(), "install.mcp_verify") {
		t.Fatalf("error should render the mcp_verify code + remedy, got: %v", err)
	}
}

// TestResolveMcpCommand_McpMux asserts resolveMcpCommand returns the
// provider-owned environment launcher for a 2-entry payload.
func TestResolveMcpCommand_McpMux(t *testing.T) {
	cfg := payload.ProviderConfig{McpServers: []string{"buzz-dev-mcp", "shellbox-mcp"}}
	got, err := resolveMcpCommand(cfg)
	if err != nil {
		t.Fatalf("McpMux: unexpected error: %v", err)
	}
	if got != install.MuxLaunchName {
		t.Fatalf("McpMux: got %q, want %q", got, install.MuxLaunchName)
	}
}

// TestResolveMcpCommand_McpDirect asserts resolveMcpCommand returns the single
// entry for a 1-entry mcp_servers payload (McpDirect mode).
func TestResolveMcpCommand_McpDirect(t *testing.T) {
	cfg := payload.ProviderConfig{McpServers: []string{"shellbox-mcp"}}
	got, err := resolveMcpCommand(cfg)
	if err != nil {
		t.Fatalf("McpDirect: unexpected error: %v", err)
	}
	if got != "shellbox-mcp" {
		t.Fatalf("McpDirect: got %q, want %q", got, "shellbox-mcp")
	}
}

// TestResolveMcpCommand_McpNone asserts resolveMcpCommand returns "" for a
// 0-entry mcp_servers payload (McpNone mode).
func TestResolveMcpCommand_McpNone(t *testing.T) {
	cfg := payload.ProviderConfig{}
	got, err := resolveMcpCommand(cfg)
	if err != nil {
		t.Fatalf("McpNone: unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("McpNone: got %q, want empty", got)
	}
}

// TestDeploy_McpMux_VerifyCommandIsAbsoluteAndSourcesEnv is a regression guard
// for the BLOCKER where bare "bzmux" was command-not-found in a non-interactive
// SSH shell whose PATH does not include BinDir.  It asserts that the mcp-verify
// step command (a) uses the absolute install.MuxBinPath rather than bare
// "bzmux", (b) sources the transient verifyEnvFilePath so BUZZ_* relay secrets
// reach bzmux's children, (c) does NOT write the permanent nest.EnvFilePath
// (Critic note 1: the probe/install path must leave no secret env file behind),
// and (d) passes --timeout (#17). CI catches regressions here before a
// production deploy ever runs.
func TestDeploy_McpMux_VerifyCommandIsAbsoluteAndSourcesEnv(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-6")

	if _, err := h.dep.Deploy(mcpMuxReq()); err != nil {
		t.Fatalf("mcp mux deploy: %v", err)
	}

	var cmdStr string
	var found bool
	for _, ev := range h.events() {
		if ev.kind == "SSH" && ev.sshTag == "mcp-verify" {
			cmdStr = ev.args(t)
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mcp-verify SSH event not found in call log")
	}

	// Must use the absolute path — NOT bare "bzmux".
	if !strings.Contains(cmdStr, install.MuxBinPath) {
		t.Errorf("mcp-verify command must contain absolute path %q (not bare 'bzmux'):\n%s",
			install.MuxBinPath, cmdStr)
	}
	// Must source the TRANSIENT verify env file so BUZZ_* vars reach children.
	if !strings.Contains(cmdStr, verifyEnvFilePath) {
		t.Errorf("mcp-verify command must reference the transient env file %q (for BUZZ_* secrets):\n%s",
			verifyEnvFilePath, cmdStr)
	}
	// Must NOT write the permanent env file (Critic note 1): a failed verify
	// must leave no secret env file on disk.
	if strings.Contains(cmdStr, nest.EnvFilePath) {
		t.Errorf("mcp-verify command must NOT reference the permanent env file %q "+
			"(use the transient verifyEnvFilePath instead):\n%s",
			nest.EnvFilePath, cmdStr)
	}
	// Must remove the transient file via a trap so it never lingers.
	if !strings.Contains(cmdStr, "trap") || !strings.Contains(cmdStr, "rm -f") {
		t.Errorf("mcp-verify command must trap-remove the transient env file:\n%s", cmdStr)
	}
	// Must pass the --timeout bound (#17).
	if !strings.Contains(cmdStr, "--timeout") {
		t.Errorf("mcp-verify command must pass --timeout:\n%s", cmdStr)
	}
	// Mux mode: no --command.
	if strings.Contains(cmdStr, "--command") {
		t.Errorf("mux-mode mcp-verify command must NOT pass --command:\n%s", cmdStr)
	}
}

// TestDeploy_McpMux_VerifyFailure asserts that a failing mux-mode mcp-verify
// surfaces CodeMcpVerify and fails the deploy.
func TestDeploy_McpMux_VerifyFailure(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-5")
	t.Setenv("FAKE_MCP_VERIFY_EXIT", "1")

	_, err := h.dep.Deploy(mcpMuxReq())
	if err == nil {
		t.Fatal("expected mcp-verify failure to fail the deploy")
	}
	if got := CodeOf(err); got != CodeMcpVerify {
		t.Fatalf("code = %q, want %q (error: %v)", got, CodeMcpVerify, err)
	}
}

func TestDeploy_ManagedMCPWritesTypedBridgeAndLeastPrivilegeConfig(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	req := buildReq(reqOpts{envVars: map[string]string{
		"DATABRICKS_HOST":  "https://workspace.example",
		"DATABRICKS_TOKEN": "dapi-own",
	}})
	req.ProviderConfig.MCP = mcpconfig.Config{
		Schema: mcpconfig.CurrentSchema, Version: mcpconfig.CurrentVersion,
		Servers: []mcpconfig.Server{
			{Name: "warehouse", Kind: mcpconfig.KindSQL, Auth: mcpconfig.AuthEnv},
			{Name: "tools", Kind: mcpconfig.KindMCPService, Resource: []string{"system", "ai", "tools"}, Auth: mcpconfig.AuthEnv},
		},
	}
	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatal(err)
	}

	var bridgeWritten, launcherChecked, configChecked bool
	for _, e := range h.events() {
		if e.kind != "SSH" {
			continue
		}
		switch e.sshTag {
		case "http-mcp-bin-write":
			bridgeWritten = len(e.stdin(t)) > 4 && strings.HasPrefix(e.stdin(t), "\x7fELF")
		case "mux-launcher-write":
			launcher := e.stdin(t)
			launcherChecked = strings.Contains(launcher, nest.EnvFilePath) && strings.Contains(launcher, install.MuxBinPath) && !strings.Contains(launcher, "dapi-own")
		case "mux-cfg-write":
			var cfg muxcfg.Config
			if err := json.Unmarshal([]byte(e.stdin(t)), &cfg); err != nil {
				t.Fatal(err)
			}
			if len(cfg.Servers) != 2 {
				t.Fatalf("managed mux servers=%d", len(cfg.Servers))
			}
			for _, srv := range cfg.Servers {
				if srv.Command != "bzhttpmcp" {
					t.Fatalf("managed command=%q", srv.Command)
				}
				if !reflect.DeepEqual(srv.Env.Inherit, []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN"}) {
					t.Fatalf("managed inherit=%v", srv.Env.Inherit)
				}
				if len(srv.Env.Set) != 0 {
					t.Fatalf("managed server serialized literal env: %v", srv.Env.Set)
				}
			}
			if got := strings.Join(cfg.Servers[1].Args, " "); got != "--kind mcp-service --resource system --resource ai --resource tools" {
				t.Fatalf("typed args=%q", got)
			}
			configChecked = true
		}
	}
	if !bridgeWritten || !launcherChecked || !configChecked {
		t.Fatalf("bridgeWritten=%v launcherChecked=%v configChecked=%v", bridgeWritten, launcherChecked, configChecked)
	}
}
