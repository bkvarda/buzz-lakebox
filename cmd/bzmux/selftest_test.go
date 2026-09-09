package main

// Selftest integration tests.  Each test runs the selftest path end-to-end by
// spawning the test binary itself as a subprocess (GO_HELPER_PROCESS=1 +
// GO_RUN_SELFTEST=1 → TestHelperProcess → runSelftest()).  The subprocess
// looks up mcp-mux.json via the $HOME/.buzz-backend/ fallback, so each test
// writes the config there using a temp HOME.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

// runST spawns the test binary as a selftest subprocess, writes cfg to
// $tmpHome/.buzz-backend/mcp-mux.json so resolveConfigPath finds it, and
// returns combined stderr+stdout and the exit code (0 = success).
func runST(t *testing.T, cfg *muxcfg.Config) (output string, exitCode int) {
	t.Helper()

	tmpHome := t.TempDir()
	bbDir := filepath.Join(tmpHome, ".buzz-backend")
	if err := os.MkdirAll(bbDir, 0700); err != nil {
		t.Fatalf("mkdir .buzz-backend: %v", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bbDir, "mcp-mux.json"), data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	exe := testBin(t)
	cmd := exec.Command(exe, "-test.run=TestHelperProcess", "--")
	cmd.Env = []string{
		"GO_HELPER_PROCESS=1",
		"GO_RUN_SELFTEST=1",
		"HOME=" + tmpHome,
		"PATH=" + os.Getenv("PATH"),
	}

	// Run with timeout.
	done := make(chan struct{})
	var combinedOut []byte
	go func() {
		defer close(done)
		combinedOut, _ = cmd.CombinedOutput()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
		}
		t.Fatal("selftest subprocess timed out after 30s")
	}

	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return string(combinedOut), code
}

// fakeSrvST builds a muxcfg.Server for selftest subprocess tests.
// Unlike fakeSrv (which calls testBin(t) at build time), here the exe path
// must be resolved at call time since we're inside a helper.
func fakeSrvST(t *testing.T, name, tools string, extras map[string]string) muxcfg.Server {
	t.Helper()
	env := map[string]string{
		"GO_HELPER_PROCESS": "1",
		"FAKE_TOOLS":        tools,
		"FAKE_CHILD_NAME":   name,
	}
	for k, v := range extras {
		env[k] = v
	}
	return muxcfg.Server{
		Name:    name,
		Command: testBin(t),
		Args:    []string{"-test.run=TestHelperProcess", "--"},
		Env:     muxcfg.ServerEnv{Set: env},
	}
}

// ---------------------------------------------------------------------------
// TestSelftest_PassesDisjointPair
// ---------------------------------------------------------------------------
func TestSelftest_PassesDisjointPair(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrvST(t, "child-a", "search,list", nil),
			fakeSrvST(t, "child-b", "create,delete", nil),
		},
	}
	out, code := runST(t, cfg)
	if code != 0 {
		t.Fatalf("selftest expected exit 0 for disjoint pair; got exit %d\noutput: %s", code, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("selftest output should contain 'OK'; got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// TestSelftest_FailsOnCollision
// Two children advertising the same tool name → exit non-zero with a clear
// message naming both servers and the colliding tool.
// ---------------------------------------------------------------------------
func TestSelftest_FailsOnCollision(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrvST(t, "srv-x", "shared_tool", nil),
			fakeSrvST(t, "srv-y", "other,shared_tool", nil),
		},
	}
	out, code := runST(t, cfg)
	if code == 0 {
		t.Fatalf("selftest expected non-zero exit on collision; got exit 0\noutput: %s", out)
	}
	for _, want := range []string{"srv-x", "srv-y", "shared_tool"} {
		if !strings.Contains(out, want) {
			t.Errorf("collision output missing %q:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// TestSelftest_FailsOnDoubleUnderscore
// A child advertising a tool name containing __ → exit non-zero.
// ---------------------------------------------------------------------------
func TestSelftest_FailsOnDoubleUnderscore(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrvST(t, "srv-a", "good_tool", nil),
			fakeSrvST(t, "srv-b", "bad__tool", nil),
		},
	}
	out, code := runST(t, cfg)
	if code == 0 {
		t.Fatalf("selftest expected non-zero exit for __ tool name; got exit 0\noutput: %s", out)
	}
	if !strings.Contains(out, "__") {
		t.Errorf("selftest output should mention '__'; got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// TestSelftest_FailsOnBudgetOverflow
// A child advertising a tool name that exceeds the 64-byte qualified limit →
// exit non-zero (len("bzmux__") + 58 = 65 > 64).
// ---------------------------------------------------------------------------
func TestSelftest_FailsOnBudgetOverflow(t *testing.T) {
	longName := strings.Repeat("x", 58) // 7+58 = 65 > 64
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrvST(t, "srv-a", "ok_tool", nil),
			fakeSrvST(t, "srv-b", longName, nil),
		},
	}
	out, code := runST(t, cfg)
	if code == 0 {
		t.Fatalf("selftest expected non-zero exit for budget overflow; got exit 0\noutput: %s", out)
	}
}

// ---------------------------------------------------------------------------
// TestSelftest_NilAgentOut_ChildNotification
// A child that emits a notification during init must not panic in selftest
// mode (where p.agentOut is nil — no agent is connected).
// ---------------------------------------------------------------------------
func TestSelftest_NilAgentOut_ChildNotification(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrvST(t, "child-a", "toolA", map[string]string{
				"FAKE_SEND_NOTIFICATION": "1",
			}),
		},
	}
	out, code := runST(t, cfg)
	if code != 0 {
		t.Fatalf("selftest should exit 0 even when child emits a notification during init; got exit %d\noutput: %s", code, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("selftest output should contain 'OK'; got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// TestSelftest_AllowsNegotiatedProtocolVersion
// A child may select a different supported protocolVersion than the client
// offered; that negotiated result is not a transport failure.
// ---------------------------------------------------------------------------
func TestSelftest_AllowsNegotiatedProtocolVersion(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrvST(t, "srv-a", "toolA", nil), // default pv "2024-11-05"
			fakeSrvST(t, "srv-b", "toolB", map[string]string{
				"FAKE_PROTOCOL_VERSION": "2025-03-26", // mismatch
			}),
		},
	}
	out, code := runST(t, cfg)
	if code != 0 {
		t.Fatalf("selftest should accept a child-selected protocol version; got exit %d\noutput: %s", code, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("selftest output should contain OK; got:\n%s", out)
	}
}
