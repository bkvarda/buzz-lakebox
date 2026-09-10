package main

// --verify-mcp unit tests (issue #17).  They drive runVerifyMCP/verifyDirectChild
// in-process against the fake-child harness (TestHelperProcess re-exec pattern,
// GO_HELPER_PROCESS=1, knobs FAKE_TOOLS / FAKE_PROTOCOL_VERSION /
// FAKE_EXIT_ON_TOOLS_CALL / FAKE_ECHO_ENV_FILE).  Every case that expects a
// failure passes a SHORT context so a silent/never-replying child fails fast in
// CI rather than blocking childInitTimeout (30s).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

// fakeChildArgs are the re-exec args every fake child is spawned with.
var fakeChildArgs = []string{"-test.run=TestHelperProcess", "--"}

// fakeChildEnv builds the process env for a fake child: the harness knobs plus
// any extras, rendered as KEY=VALUE strings (what spawn wants).
func fakeChildEnv(tools string, extras map[string]string) []string {
	env := map[string]string{
		"GO_HELPER_PROCESS": "1",
		"FAKE_TOOLS":        tools,
	}
	for k, v := range extras {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// (1) Correct: known server exports its expected tools → exit 0.
func TestVerifyMCP_Direct_Correct(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := verifyDirectChild(ctx, "buzz-dev-mcp", testBin(t), fakeChildArgs,
		fakeChildEnv("shell,read_file,view_image,str_replace,todo,_Stop,_PostCompact",
			map[string]string{"FAKE_PROTOCOL_VERSION": "2024-11-05"}))
	if err != nil {
		t.Fatalf("expected success for a fully-exporting buzz-dev-mcp, got: %v", err)
	}
}

// (2) Silent (never replies): the injected SHORT ctx must fail fast, not block 30s.
func TestVerifyMCP_Direct_SilentTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	// FAKE_NEVER_REPLY makes the child read stdin but never answer.
	err := verifyDirectChild(ctx, "buzz-dev-mcp", testBin(t), fakeChildArgs,
		fakeChildEnv("shell", map[string]string{"FAKE_NEVER_REPLY": "1"}))
	if err == nil {
		t.Fatal("expected a timeout error for a silent child")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("silent child blocked %s — the injected short ctx was not honored", elapsed)
	}
}

// (3) Partial under-export: buzz-dev-mcp reports only "shell" → non-zero naming
// the missing tools (the initialized-but-under-exports case).
func TestVerifyMCP_Direct_PartialUnderExport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := verifyDirectChild(ctx, "buzz-dev-mcp", testBin(t), fakeChildArgs,
		fakeChildEnv("shell", nil))
	if err == nil {
		t.Fatal("expected failure when buzz-dev-mcp under-exports its known tools")
	}
	if !strings.Contains(err.Error(), "missing expected tool") {
		t.Fatalf("error should name the missing tool(s), got: %v", err)
	}
	// A specific missing name should appear.
	if !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("error should name a specific missing tool (read_file), got: %v", err)
	}
}

// (4) Exits immediately: child exits before answering → non-zero "child died".
func TestVerifyMCP_Direct_ExitsImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := verifyDirectChild(ctx, "buzz-dev-mcp", testBin(t), fakeChildArgs,
		fakeChildEnv("shell", map[string]string{"FAKE_EXIT_BEFORE_INIT": "1"}))
	if err == nil {
		t.Fatal("expected failure when the child exits before initialize")
	}
	if !strings.Contains(err.Error(), "died") && !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("error should indicate the child exited/handshake failed, got: %v", err)
	}
}

// (5) Empty catalog: initialize OK, tools/list returns [] → non-zero.
func TestVerifyMCP_Direct_EmptyCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := verifyDirectChild(ctx, "some-unknown-server", testBin(t), fakeChildArgs,
		fakeChildEnv("", nil)) // no tools
	if err == nil {
		t.Fatal("expected failure for an empty tool catalog")
	}
	if !strings.Contains(err.Error(), "empty tool catalog") {
		t.Fatalf("error should mention the empty catalog, got: %v", err)
	}
}

// (6) Unknown direct command + non-empty catalog → exit 0 (the "where available"
// tier: an unknown server needs only a non-empty catalog).
func TestVerifyMCP_Direct_UnknownCommandNonEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := verifyDirectChild(ctx, "operator-custom-mcp", testBin(t), fakeChildArgs,
		fakeChildEnv("arbitrary_tool_a,arbitrary_tool_b", nil))
	if err != nil {
		t.Fatalf("unknown command with a non-empty catalog should pass, got: %v", err)
	}
}

// (7) Children may select different protocol versions from the one offered.
// Databricks managed MCP currently does this for Skills while SQL echoes the
// offered version, and both are valid children of one multiplexer.
func TestVerifyMCP_Mux_AllowsNegotiatedProtocolVersions(t *testing.T) {
	writeMuxConfig(t, &muxcfg.Config{
		Servers: []muxcfg.Server{
			muxFakeSrv("buzz-dev-mcp", "shell,read_file,view_image,str_replace,todo,_Stop,_PostCompact", nil),
			muxFakeSrv("shellbox-mcp", "shell_create,shell_send,shell_read,shell_list,shell_resize,shell_kill",
				map[string]string{"FAKE_PROTOCOL_VERSION": "2025-03-26"}), // mismatch
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runVerifyMCP(ctx, ""); err != nil {
		t.Fatalf("negotiated child protocol versions should pass: %v", err)
	}
}

// (7b) Happy mux path: both known children fully export → exit 0 (proves the
// expected-names union is satisfied by a healthy multiplexer).
func TestVerifyMCP_Mux_HappyPath(t *testing.T) {
	writeMuxConfig(t, &muxcfg.Config{
		Servers: []muxcfg.Server{
			muxFakeSrv("buzz-dev-mcp", "shell,read_file,view_image,str_replace,todo,_Stop,_PostCompact", nil),
			muxFakeSrv("shellbox-mcp", "shell_create,shell_send,shell_read,shell_list,shell_resize,shell_kill", nil),
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runVerifyMCP(ctx, ""); err != nil {
		t.Fatalf("expected mux verify to pass for two fully-exporting known servers, got: %v", err)
	}
}

// (8) Direct child env: the child echoes its received env to a side-channel file;
// assert the direct child got the sourced BUZZ_* vars (direct mode passes the
// full inherited env). verifyDirectChild receives the env explicitly here (in
// production this is os.Environ() after the deploy script sources the env file).
func TestVerifyMCP_Direct_ChildReceivesSourcedEnv(t *testing.T) {
	echoFile := filepath.Join(t.TempDir(), "child-env.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env := fakeChildEnv("shell,read_file,view_image,str_replace,todo,_Stop,_PostCompact",
		map[string]string{
			"FAKE_ECHO_ENV_FILE": echoFile,
			"BUZZ_PRIVATE_KEY":   "MARKER-PRIVATE-KEY",
			"BUZZ_AUTH_TAG":      "MARKER-AUTH-TAG",
		})
	if err := verifyDirectChild(ctx, "buzz-dev-mcp", testBin(t), fakeChildArgs, env); err != nil {
		t.Fatalf("verify failed: %v", err)
	}

	data, err := os.ReadFile(echoFile)
	if err != nil {
		t.Fatalf("read echoed child env: %v", err)
	}
	got := string(data)
	for _, want := range []string{"BUZZ_PRIVATE_KEY=MARKER-PRIVATE-KEY", "BUZZ_AUTH_TAG=MARKER-AUTH-TAG"} {
		if !strings.Contains(got, want) {
			t.Fatalf("direct child did not receive sourced env %q; child env was:\n%s", want, got)
		}
	}
}

// --- mux-mode test helpers -------------------------------------------------

// muxFakeSrv builds a muxcfg.Server that runs the test binary as a fake MCP
// child under the given logical name (so knownServerTools lookups hit).
func muxFakeSrv(name, tools string, extras map[string]string) muxcfg.Server {
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
		Command: os.Args[0],
		Args:    fakeChildArgs,
		Env:     muxcfg.ServerEnv{Set: env},
	}
}

// writeMuxConfig writes cfg to $HOME/.buzz-backend/mcp-mux.json under a fresh
// temp HOME so resolveConfigPath (executable-relative first, then the $HOME
// fallback) finds it in-process.
func writeMuxConfig(t *testing.T, cfg *muxcfg.Config) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	bbDir := filepath.Join(tmpHome, ".buzz-backend")
	if err := os.MkdirAll(bbDir, 0o700); err != nil {
		t.Fatalf("mkdir .buzz-backend: %v", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bbDir, "mcp-mux.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
