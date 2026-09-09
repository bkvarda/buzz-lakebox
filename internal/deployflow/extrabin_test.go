package deployflow

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// extraBinReq is a buzz-agent deploy carrying one valid extra_binaries entry.
func extraBinReq() *payload.DeployRequest {
	req := buildReq(reqOpts{})
	req.ProviderConfig.ExtraBinaries = []payload.ExtraBinary{
		{
			URL:    "https://example.com/dl/shellbox-mcp",
			SHA256: strings.Repeat("a", 64),
			Bin:    "shellbox-mcp",
		},
	}
	return req
}

// TestDeploy_ExtraBinaries_InstalledInOrder pins where the extra-binaries
// install sits: after the .deb install-exec and before runtime verification.
func TestDeploy_ExtraBinaries_InstalledInOrder(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-extrabin-1")

	if _, err := h.dep.Deploy(extraBinReq()); err != nil {
		t.Fatalf("extra-binaries deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{
		"SSH:install-exec",
		"SSH:extra-bins-write", "SSH:extra-bins-exec",
		"SSH:verify-exec",
		"SSH:launch-exec",
	})
}

// TestDeploy_ExtraBinaries_InstalledAfterAdapter pins the ordering invariant
// for an adapter-bearing runtime: the extra-binaries install must sit AFTER the
// ACP adapter install (Claude installs an npm adapter) and before verification.
// TestDeploy_ExtraBinaries_InstalledInOrder uses buzz-agent, which skips the
// adapter entirely, so it cannot catch a regression that reorders extra-bins
// relative to the adapter for Claude/Codex.
func TestDeploy_ExtraBinaries_InstalledAfterAdapter(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_VERIFY_OUTPUT", `{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"name":"@agentclientprotocol/claude-agent-acp","version":"0.73.0"}}}`)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-extrabin-claude-1")

	req := buildReq(reqOpts{
		agentCommand: "claude-code",
		envVars: map[string]string{
			"DATABRICKS_HOST":  "https://example.databricks.com",
			"DATABRICKS_TOKEN": "dapi-marker-secret",
		},
	})
	req.ProviderConfig.ExtraBinaries = []payload.ExtraBinary{
		{URL: "https://example.com/dl/shellbox-mcp", SHA256: strings.Repeat("a", 64), Bin: "shellbox-mcp"},
	}

	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatalf("claude + extra-binaries deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{
		"SSH:install-exec",
		"SSH:adapter-write", "SSH:adapter-exec",
		"SSH:extra-bins-write", "SSH:extra-bins-exec",
		"SSH:verify-exec",
		"SSH:claude-inference-probe",
		"SSH:launch-exec",
	})
}

// TestDeploy_NoExtraBinaries_SkipsSteps guards the regression-safe no-op path:
// a deploy with no extra_binaries must issue no extra-bins round trips at all.
func TestDeploy_NoExtraBinaries_SkipsSteps(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-extrabin-2")

	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatalf("deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	for _, unwanted := range []string{"SSH:extra-bins-write", "SSH:extra-bins-exec"} {
		assertNotContains(t, seq, unwanted)
	}
}

// TestDeploy_ExtraBinariesExecFailure_IsDistinctlyCoded pins that a failing
// extra-bins-exec surfaces CodeExtraBinExec.
func TestDeploy_ExtraBinariesExecFailure_IsDistinctlyCoded(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-extrabin-3")
	t.Setenv("FAKE_EXTRA_BIN_EXIT", "1")

	_, err := h.dep.Deploy(extraBinReq())
	if err == nil {
		t.Fatal("expected the extra-binaries install failure to fail the deploy")
	}
	if got := CodeOf(err); got != CodeExtraBinExec {
		t.Fatalf("code = %q, want %q (error: %v)", got, CodeExtraBinExec, err)
	}
}

// TestDeploy_ExtraBinariesScriptError_IsDistinctlyCoded pins that a render
// failure (a duplicate bin, which #18's validator does not dedup across
// entries) surfaces CodeExtraBinScript and never reaches the sandbox.
func TestDeploy_ExtraBinariesScriptError_IsDistinctlyCoded(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-extrabin-4")

	req := buildReq(reqOpts{})
	req.ProviderConfig.ExtraBinaries = []payload.ExtraBinary{
		{URL: "https://example.com/a", SHA256: strings.Repeat("a", 64), Bin: "dup"},
		{URL: "https://example.com/b", SHA256: strings.Repeat("b", 64), Bin: "dup"},
	}

	_, err := h.dep.Deploy(req)
	if err == nil {
		t.Fatal("expected a duplicate-bin render error to fail the deploy")
	}
	if got := CodeOf(err); got != CodeExtraBinScript {
		t.Fatalf("code = %q, want %q (error: %v)", got, CodeExtraBinScript, err)
	}
	// The failure must be pre-flight of the sandbox write: no extra-bins round
	// trip should have been issued.
	assertNotContains(t, callSequence(h.events()), "SSH:extra-bins-write")
}
