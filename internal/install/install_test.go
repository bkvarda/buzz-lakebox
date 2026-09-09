package install

import (
	"strings"
	"testing"
)

func TestBuildInstallScript_DefaultVersion(t *testing.T) {
	script, err := BuildInstallScript("")
	if err != nil {
		t.Fatalf("BuildInstallScript(\"\") error: %v", err)
	}
	if !strings.Contains(script, DefaultVersion) {
		t.Fatalf("script should reference the default pinned version %q", DefaultVersion)
	}
	if !strings.Contains(script, pinnedSHA256[DefaultVersion]) {
		t.Fatal("script should embed the pinned sha256")
	}
	if !strings.Contains(script, "releases/download/desktop-v0.5.23/Buzz_0.5.23_amd64.deb") {
		t.Fatal("script should map the semantic version to the desktop-prefixed release tag")
	}
}

func TestBuildInstallScript_AcceptsDesktopTagAlias(t *testing.T) {
	script, err := BuildInstallScript("desktop-v0.5.23")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "releases/download/desktop-v0.5.23/Buzz_0.5.23_amd64.deb") {
		t.Fatal("desktop tag alias should resolve to the pinned asset")
	}
}

func TestBuildInstallScript_UnknownVersionFailsLoud(t *testing.T) {
	_, err := BuildInstallScript("v99.99.99")
	if err == nil {
		t.Fatal("expected an error for an unpinned version")
	}
	if !strings.Contains(err.Error(), "v99.99.99") {
		t.Fatalf("error should name the requested version, got: %v", err)
	}
}

func TestBuildInstallScript_NoSetX(t *testing.T) {
	script, err := BuildInstallScript(DefaultVersion)
	if err != nil {
		t.Fatalf("BuildInstallScript error: %v", err)
	}
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "set -x" || strings.HasPrefix(trimmed, "set -ex") || strings.Contains(trimmed, "set -xe") {
			t.Fatalf("script must never enable shell tracing, found line: %q", line)
		}
	}
	if !strings.Contains(script, "set -eu") {
		t.Fatal("script should set -eu")
	}
	if !strings.Contains(script, "umask 077") {
		t.Fatal("script should set umask 077")
	}
}

func TestBuildInstallScript_SkipsWhenMarkerMatches(t *testing.T) {
	script, err := BuildInstallScript(DefaultVersion)
	if err != nil {
		t.Fatalf("BuildInstallScript error: %v", err)
	}
	if !strings.Contains(script, "already installed; skipping download") {
		t.Fatal("script should contain marker-match skip logic")
	}
	if !strings.Contains(script, `[ "$(cat "$MARKER")" = "$BUZZ_VERSION" ]`) {
		t.Fatal("script should compare the marker file content against the pinned version")
	}
}

func TestBuildInstallScript_SymlinksAllBinaries(t *testing.T) {
	script, err := BuildInstallScript(DefaultVersion)
	if err != nil {
		t.Fatalf("BuildInstallScript error: %v", err)
	}
	for _, bin := range BinNames {
		if !strings.Contains(script, `"$BIN_DIR/`+bin+`"`) {
			t.Fatalf("script should symlink %q into BIN_DIR", bin)
		}
	}
}

func TestBuildInstallScript_ChecksumVerification(t *testing.T) {
	script, err := BuildInstallScript(DefaultVersion)
	if err != nil {
		t.Fatalf("BuildInstallScript error: %v", err)
	}
	if !strings.Contains(script, "sha256sum -c -") {
		t.Fatal("script should verify sha256 via sha256sum -c")
	}
	if !strings.Contains(script, "curl -q -fL") {
		t.Fatal("script should fetch via curl, with -q FIRST so it ignores ~/.curlrc")
	}
	if !strings.Contains(script, "dpkg-deb -x") {
		t.Fatal("script should extract via dpkg-deb -x")
	}
}

func TestBuildVerifyCommand_CombinedScript(t *testing.T) {
	cmd, err := BuildVerifyCommand(`$HOME/.buzz-backend/.env.verify`, 10, VerifySpecFor("buzz-agent"))
	if err != nil {
		t.Fatalf("BuildVerifyCommand error for the canonical trusted path: %v", err)
	}
	if !strings.Contains(cmd, InitializeFrame) {
		t.Fatal("verify command should pipe the ACP initialize frame")
	}
	if !strings.Contains(cmd, "timeout 10") {
		t.Fatal("verify command should enforce a 10s timeout")
	}
	// envFile is a trusted "$HOME"-relative literal: it must be assigned
	// via a double-quoted literal (so "$HOME" expands), not
	// shellquote.Single (which would suppress that expansion and break
	// sourcing — the exact BUG 1 regression this pins).
	if !strings.Contains(cmd, `ENVF="$HOME/.buzz-backend/.env.verify"`) {
		t.Fatalf("verify command should assign the trusted env file path with $HOME expansion preserved, got: %q", cmd)
	}
	if !strings.Contains(cmd, `. "$ENVF"`) {
		t.Fatalf("verify command should source the env file via the expanded $ENVF variable, got: %q", cmd)
	}
	if !strings.Contains(cmd, `cat > "$ENVF"`) {
		t.Fatal("verify command should write its own stdin into the env file (single combined round trip)")
	}
	if !strings.Contains(cmd, `trap 'rm -f "$ENVF" "$OUTF"' EXIT`) {
		t.Fatal("verify command should clean up the real (expanded-path) env file AND the captured output file on exit via trap")
	}
	if !strings.Contains(cmd, "buzz-agent") {
		t.Fatal("verify command should invoke buzz-agent")
	}
}

// TestBuildVerifyCommand_RejectsHostilePaths pins the round-2 defensive
// guard: envFile is interpolated unescaped inside a double-quoted shell
// assignment (required so "$HOME" expands), so any character that could
// carry shell syntax through that context — a closing double quote, a
// backtick, $( ) command substitution, backslash, whitespace — must be
// rejected outright. BuildVerifyCommand accepts trusted static literals
// only.
func TestBuildVerifyCommand_RejectsHostilePaths(t *testing.T) {
	hostile := []string{
		`$HOME/x"; rm -rf /; echo "`,         // embedded double quote breaks out of the assignment
		"$HOME/`touch /tmp/pwned`",           // backtick command substitution
		`$HOME/$(touch /tmp/pwned)`,          // $() command substitution
		`$HOME/.buzz-backend/.env verify`,    // whitespace
		`$HOME\.buzz-backend\.env.verify`,    // backslash
		`$HOME/.buzz-backend/.env.verify'x`,  // single quote
		"$HOME/.buzz-backend/.env\nrm -rf /", // newline
		"",                                   // empty
	}
	for _, path := range hostile {
		if cmd, err := BuildVerifyCommand(path, 10, VerifySpecFor("buzz-agent")); err == nil {
			t.Fatalf("BuildVerifyCommand(%q) should have been rejected, got script: %q", path, cmd)
		}
	}
}

// TestBuildVerifyCommand_CodexHoldsStdinOpen pins a fix for what would have
// been a total codex runtime failure, presented as a handshake error.
//
// The handshake is `printf FRAME | timeout N BIN`, so stdin closes the
// instant the frame is written. claude-agent-acp answers and exits 0 on
// that EOF in ~355ms, which is what the original shape was measured
// against. codex-acp does NOT: measured against the real published
// adapter, `printf FRAME | codex-acp` exits 0 having written ZERO bytes,
// while the same frame with stdin held open for even one second returns
// the full initialize reply (690 bytes, agentInfo present).
//
// Without the hold, installAndVerify's AgentInfoMarker scan would find
// nothing and EVERY codex deploy would die at CodeRuntimeVerify — whose
// remedy text would then send the operator to look at inference variables
// for a problem that has nothing to do with them.
func TestBuildVerifyCommand_CodexHoldsStdinOpen(t *testing.T) {
	cmd, err := BuildVerifyCommand(`$HOME/.buzz-backend/.env.verify`, 10, VerifySpecFor("codex-acp"))
	if err != nil {
		t.Fatal(err)
	}
	// A brace group, so the pipeline still has exactly one stdin source.
	if !strings.Contains(cmd, "; sleep 2; } | timeout 10") {
		t.Errorf("codex verify must hold stdin open after the frame, got:\n%s", cmd)
	}
}

// TestBuildVerifyCommand_NonCodexUnchanged is the other half: the stdin
// hold is per-runtime precisely so it does not perturb the two runtimes
// that were already verified working. A global sleep would have added
// latency to every deploy to fix one runtime's problem.
func TestBuildVerifyCommand_NonCodexUnchanged(t *testing.T) {
	for _, spawnCommand := range []string{"claude-agent-acp", "buzz-agent"} {
		cmd, err := BuildVerifyCommand(`$HOME/.buzz-backend/.env.verify`, 10, VerifySpecFor(spawnCommand))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cmd, "sleep") {
			t.Errorf("%s answers on stdin EOF and must not pay for the codex hold:\n%s", spawnCommand, cmd)
		}
		if !strings.Contains(cmd, "printf '%s\\n' '"+InitializeFrame+"' | timeout 10") {
			t.Errorf("%s verify pipeline changed shape:\n%s", spawnCommand, cmd)
		}
	}
}

// TestAdapterSpecs_VerifyStdinHoldDeclared makes the requirement explicit
// for any future adapter: a new ACP adapter must state whether it answers a
// stdin that closes with the frame, because getting it wrong ships a
// runtime that installs cleanly and never handshakes.
func TestAdapterSpecs_VerifyStdinHoldDeclared(t *testing.T) {
	want := map[string]int{"claude-agent-acp": 0, "codex-acp": 2}
	for spawnCommand, spec := range adapterSpecs {
		expected, known := want[spawnCommand]
		if !known {
			t.Errorf("adapter %q has no declared stdin-hold expectation; measure it against the real adapter and add it here", spawnCommand)
			continue
		}
		if spec.VerifyStdinHoldSeconds != expected {
			t.Errorf("adapter %q VerifyStdinHoldSeconds = %d, want %d", spawnCommand, spec.VerifyStdinHoldSeconds, expected)
		}
		if got := VerifySpecFor(spawnCommand).StdinHoldSeconds; got != expected {
			t.Errorf("adapter %q spec does not reach VerifySpec: got %d, want %d", spawnCommand, got, expected)
		}
	}
}
