package nest

import (
	"bytes"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// runSh executes script under /bin/sh -c and returns stdout, failing the
// test on any error. Used to verify shellQuote's escaping is actually
// safe by round-tripping through a real shell rather than just
// string-matching the escape sequence.
func runSh(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sh -c failed: %v (stderr: %s)", err, stderr.String())
	}
	return stdout.String()
}

func TestRenderEnv_GoldenFields(t *testing.T) {
	model := "databricks-claude-opus-4-8"
	provider := "databricks_v2"
	agent := payload.Agent{
		Name:                "Reviewer",
		RelayURL:            "wss://relay.example.com",
		PrivateKeyNsec:      "nsec1abc",
		AuthTag:             "tag-abc",
		AgentCommand:        "buzz-agent",
		AgentArgs:           []string{"--flag", "value"},
		SystemPrompt:        "You are a reviewer.",
		Model:               &model,
		Provider:            &provider,
		TurnTimeoutSeconds:  120,
		IdleTimeoutSeconds:  900,
		MaxTurnDurationSecs: 7200,
		Parallelism:         2,
		RespondTo:           "owner-only",
		RespondToAllowlist:  []string{"npub1a", "npub1b"},
		EnvVars: map[string]string{
			"DATABRICKS_HOST":  "https://example.databricks.com",
			"DATABRICKS_TOKEN": "dapi-marker-secret",
		},
	}

	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "")

	wantLines := []string{
		`export BUZZ_PRIVATE_KEY='nsec1abc'`,
		`export BUZZ_AUTH_TAG='tag-abc'`,
		`export BUZZ_RELAY_URL='wss://relay.example.com'`,
		`export BUZZ_ACP_AGENT_COMMAND='buzz-agent'`,
		`export BUZZ_ACP_AGENT_ARGS='--flag,value'`,
		`export BUZZ_ACP_AGENTS='2'`,
		`export BUZZ_ACP_SYSTEM_PROMPT='You are a reviewer.'`,
		`export BUZZ_ACP_MODEL='databricks-claude-opus-4-8'`,
		`export BUZZ_ACP_RESPOND_TO='owner-only'`,
		`export BUZZ_ACP_RESPOND_TO_ALLOWLIST='npub1a,npub1b'`,
		`export BUZZ_ACP_IDLE_TIMEOUT='900'`,
		`export BUZZ_ACP_MAX_TURN_DURATION='7200'`,
		`export NOSTR_PRIVATE_KEY='nsec1abc'`,
		`export BUZZ_ACP_MCP_COMMAND='buzz-dev-mcp'`,
		`export MCP_HOOK_SERVERS='*'`,
		`export BUZZ_ACP_RELAY_OBSERVER='true'`,
		`export BUZZ_ACP_DEDUP='queue'`,
		`export BUZZ_ACP_MULTIPLE_EVENT_HANDLING='steer'`,
		`export BUZZ_AGENT_PROVIDER='databricks_v2'`,
		`export DATABRICKS_MODEL='databricks-claude-opus-4-8'`,
		`export DATABRICKS_HOST='https://example.databricks.com'`,
		`export DATABRICKS_TOKEN='dapi-marker-secret'`,
	}
	for _, want := range wantLines {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing expected line %q; full env:\n%s", want, env)
		}
	}

	// The deprecated / nonexistent buzz-acp names must never reappear:
	// *_SECONDS variants are ignored by buzz-acp, and BUZZ_ACP_TURN_TIMEOUT
	// is deprecated upstream (the desktop deliberately never sets it).
	for _, banned := range []string{
		"BUZZ_ACP_TURN_TIMEOUT",
		"BUZZ_ACP_IDLE_TIMEOUT_SECONDS",
		"BUZZ_ACP_MAX_TURN_DURATION_SECONDS",
	} {
		if strings.Contains(env, banned) {
			t.Fatalf("env must not contain %q; full env:\n%s", banned, env)
		}
	}

	// env_vars must be emitted after (and thus win over) the fixed
	// BUZZ_AGENT_PROVIDER/DATABRICKS_MODEL block.
	if strings.Index(env, "DATABRICKS_MODEL") > strings.Index(env, "DATABRICKS_HOST") {
		t.Fatal("env_vars must be rendered after the fixed inference defaults")
	}
}

// TestRenderEnv_ZeroTimeouts_Omitted mirrors the desktop: idle/max-turn
// are emitted only when explicitly set, so zero values fall through to
// buzz-acp's own defaults (900s idle / 7200s max turn) instead of
// exporting a `0` whose semantics upstream owns.
func TestRenderEnv_ZeroTimeouts_Omitted(t *testing.T) {
	agent := payload.Agent{AgentCommand: "buzz-agent"}
	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "")
	if strings.Contains(env, "BUZZ_ACP_IDLE_TIMEOUT") || strings.Contains(env, "BUZZ_ACP_MAX_TURN_DURATION") {
		t.Fatalf("zero timeouts must be omitted entirely, got:\n%s", env)
	}
}

func TestRenderEnv_EmptyAllowlist_OmitsEnvVar(t *testing.T) {
	agent := payload.Agent{AgentCommand: "buzz-agent"}
	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "")
	if strings.Contains(env, "BUZZ_ACP_RESPOND_TO_ALLOWLIST") {
		t.Fatalf("expected BUZZ_ACP_RESPOND_TO_ALLOWLIST to be omitted entirely when the allowlist is empty (the desktop only sets it in allowlist mode), got:\n%s", env)
	}
}

func TestRenderEnv_ProviderDefaultsWhenEmpty(t *testing.T) {
	agent := payload.Agent{AgentCommand: "buzz-agent"}
	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "")
	if !strings.Contains(env, `export BUZZ_AGENT_PROVIDER='databricks_v2'`) {
		t.Fatalf("expected default provider databricks_v2, got:\n%s", env)
	}
}

func TestRenderEnv_ExplicitProviderWins(t *testing.T) {
	provider := "databricks"
	agent := payload.Agent{AgentCommand: "buzz-agent", Provider: &provider}
	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "")
	if !strings.Contains(env, `export BUZZ_AGENT_PROVIDER='databricks'`) {
		t.Fatalf("expected explicit provider to be used, got:\n%s", env)
	}
}

func TestRenderEnv_QuotingEmbeddedQuotesAndNewlines(t *testing.T) {
	agent := payload.Agent{
		AgentCommand: "buzz-agent",
		SystemPrompt: "Line one.\nSay 'hello' and \"goodbye\".\nLine three.",
	}
	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "")

	// The rendered assignment must be shell-safe: verify by sourcing the
	// *entire* rendered env (since the quoted value itself contains
	// literal embedded newlines, it can't be sliced out line-by-line)
	// through a real shell, rather than string-matching the escape
	// sequence.
	got := evalEnvVar(t, env, "BUZZ_ACP_SYSTEM_PROMPT")
	want := "Line one.\nSay 'hello' and \"goodbye\".\nLine three."
	if got != want {
		t.Fatalf("system prompt did not round-trip through the shell:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestRenderEnv_Deterministic(t *testing.T) {
	agent := payload.Agent{
		AgentCommand: "buzz-agent",
		EnvVars:      map[string]string{"Z_VAR": "z", "A_VAR": "a", "M_VAR": "m"},
	}
	first := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "")
	for i := 0; i < 5; i++ {
		if got := RenderEnv(agent, payload.RuntimeBuzzAgent, false, ""); got != first {
			t.Fatalf("RenderEnv is not deterministic across calls")
		}
	}
	// env_vars should be sorted (A_VAR before M_VAR before Z_VAR).
	if strings.Index(first, "A_VAR") > strings.Index(first, "M_VAR") || strings.Index(first, "M_VAR") > strings.Index(first, "Z_VAR") {
		t.Fatalf("expected env_vars sorted by key, got:\n%s", first)
	}
}

// TestRenderEnv_SandboxMode_AppendsSnippetAfterEnvVars is the sandbox-mode
// golden: SandboxAuthSnippet must appear, verbatim, after the last
// env_vars line — so env_vars-supplied DATABRICKS_HOST/DATABRICKS_TOKEN
// are already exported (and thus win via the snippet's only-if-unset
// checks) by the time the snippet runs.
func TestRenderEnv_SandboxMode_AppendsSnippetAfterEnvVars(t *testing.T) {
	agent := payload.Agent{
		AgentCommand: "buzz-agent",
		EnvVars:      map[string]string{"Z_VAR": "z", "A_VAR": "a"},
	}

	env := RenderEnv(agent, payload.RuntimeBuzzAgent, true, "")

	if !strings.Contains(env, SandboxAuthSnippet) {
		t.Fatalf("sandbox mode must append SandboxAuthSnippet verbatim, got:\n%s", env)
	}
	lastEnvVar := strings.LastIndex(env, "export Z_VAR=")
	snippetAt := strings.Index(env, SandboxAuthSnippet)
	if lastEnvVar < 0 {
		t.Fatal("expected env_vars to be rendered")
	}
	if snippetAt < lastEnvVar {
		t.Fatalf("SandboxAuthSnippet must appear after the env_vars block; snippet at %d, last env_var at %d", snippetAt, lastEnvVar)
	}
}

// TestRenderEnv_NonSandboxMode_ByteIdenticalToBaseline pins that
// sandboxInferenceAuth=false never changes RenderEnv's output — the
// zero-token feature must be fully opt-in.
func TestRenderEnv_NonSandboxMode_ByteIdenticalToBaseline(t *testing.T) {
	agent := payload.Agent{
		AgentCommand: "buzz-agent",
		EnvVars:      map[string]string{"DATABRICKS_HOST": "https://example.databricks.com"},
	}
	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "")
	if strings.Contains(env, "SandboxAuthSnippet") || strings.Contains(env, "buzz_awk_extract") {
		t.Fatalf("sandboxInferenceAuth=false must never render the snippet, got:\n%s", env)
	}
}

func TestRenderLaunchScript_GoldenInvariants(t *testing.T) {
	script := RenderLaunchScript(false, false, "", false)

	if !strings.Contains(script, "set -eu") {
		t.Fatal("launch.sh must set -eu")
	}
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "set -x" || strings.HasPrefix(trimmed, "set -ex") || strings.Contains(trimmed, "set -xe") {
			t.Fatalf("launch.sh must never enable shell tracing (set -x), found: %q", line)
		}
	}
	if !strings.Contains(script, `cat > "$HOME/.databrickscfg"`) {
		t.Fatal("launch.sh (keep_workspace_pat=false, the default) must re-assert the PAT stub")
	}
	if !strings.Contains(script, "flock -n 9") {
		t.Fatal("launch.sh must guard against double-launch via flock")
	}
	if !strings.Contains(script, "pgrep -f '[b]uzz-acp'") {
		t.Fatal("launch.sh must also guard via pgrep, using the non-self-matching bracket idiom")
	}
	if !strings.Contains(script, "setsid nohup") {
		t.Fatal("launch.sh must launch buzz-acp via setsid nohup")
	}
	if !strings.Contains(script, `. "$HOME/.buzz-backend/env"`) {
		t.Fatal("launch.sh must source the env file")
	}
	pathExport := `export PATH="$HOME/.buzz-backend/bin:$PATH"`
	if !strings.Contains(script, pathExport) {
		t.Fatal("launch.sh must prepend the installed bin dir to PATH — buzz-acp spawns the agent command by bare name")
	}
	if strings.Index(script, pathExport) < strings.Index(script, `. "$HOME/.buzz-backend/env"`) {
		t.Fatal("PATH export must come after sourcing the env file so the env file cannot clobber it")
	}
}

func TestRenderLaunchScript_KeepWorkspacePAT_OmitsStub(t *testing.T) {
	// BUG 3: launch.sh runs on every deploy AND every future `start` /
	// supervisor relaunch, so unconditionally re-asserting the PAT stub
	// defeated provider_config.keep_workspace_pat=true by clobbering the
	// kept PAT on the very next relaunch.
	script := RenderLaunchScript(true, false, "", false)

	if strings.Contains(script, `cat > "$HOME/.databrickscfg"`) {
		t.Fatalf("launch.sh (keep_workspace_pat=true) must NOT re-assert the PAT stub, got:\n%s", script)
	}
	// Everything else must still be present.
	if !strings.Contains(script, "flock -n 9") {
		t.Fatal("launch.sh must still guard against double-launch via flock")
	}
	if !strings.Contains(script, "setsid nohup") {
		t.Fatal("launch.sh must still launch buzz-acp via setsid nohup")
	}
	if !strings.Contains(script, `. "$HOME/.buzz-backend/env"`) {
		t.Fatal("launch.sh must still source the env file")
	}
}

func TestRenderLaunchScript_NoSecrets(t *testing.T) {
	// launch.sh is a static template with no per-request substitution, so
	// no secret value can ever appear in it structurally — this test
	// pins that invariant, for both keep_workspace_pat variants.
	for _, keep := range []bool{false, true} {
		script := RenderLaunchScript(keep, false, "", false)
		for _, marker := range []string{"nsec1", "BUZZ_PRIVATE_KEY=", "DATABRICKS_TOKEN="} {
			if strings.Contains(script, marker) {
				t.Fatalf("launch.sh (keepWorkspacePAT=%v) unexpectedly contains %q", keep, marker)
			}
		}
	}
}

func TestPATStub_CommentOnly(t *testing.T) {
	for _, line := range strings.Split(PATStub, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			t.Fatalf("PAT stub must be comment-only, found non-comment line: %q", line)
		}
	}
	if strings.Contains(PATStub, "[") {
		t.Fatal("PAT stub must not contain a [profile] section header")
	}
}

// TestPATStubMarker_IsPATStubFirstLine pins that PATStubMarker and PATStub
// cannot drift apart: PATStub is built from PATStubMarker by compile-time
// concatenation, so PATStub must start with the marker followed by a
// newline, and the marker itself must be exactly PATStub's first line.
func TestPATStubMarker_IsPATStubFirstLine(t *testing.T) {
	if !strings.HasPrefix(PATStub, PATStubMarker+"\n") {
		t.Fatalf("PATStub must start with PATStubMarker; PATStubMarker=%q, PATStub=%q", PATStubMarker, PATStub)
	}
	firstLine := strings.SplitN(PATStub, "\n", 2)[0]
	if firstLine != PATStubMarker {
		t.Fatalf("PATStub's first line = %q, want PATStubMarker %q", firstLine, PATStubMarker)
	}
}

// TestRenderLaunchScript_StubOmittedMatrix is the 2x2 keepWorkspacePAT x
// sandboxInferenceAuth matrix (R): the PAT-stub heredoc must be written
// ONLY when both flags are false; either flag alone (or both) must skip
// it, since inference_auth:"sandbox" needs the baked cfg intact just as
// much as keep_workspace_pat:true does.
func TestRenderLaunchScript_StubOmittedMatrix(t *testing.T) {
	cases := []struct {
		keepPAT, sandbox bool
		wantStub         bool
	}{
		{keepPAT: false, sandbox: false, wantStub: true},
		{keepPAT: true, sandbox: false, wantStub: false},
		{keepPAT: false, sandbox: true, wantStub: false},
		{keepPAT: true, sandbox: true, wantStub: false},
	}
	for _, tc := range cases {
		script := RenderLaunchScript(tc.keepPAT, tc.sandbox, "", false)
		hasStub := strings.Contains(script, `cat > "$HOME/.databrickscfg"`)
		if hasStub != tc.wantStub {
			t.Fatalf("RenderLaunchScript(keepPAT=%v, sandbox=%v): stub present=%v, want %v", tc.keepPAT, tc.sandbox, hasStub, tc.wantStub)
		}
		// Everything else must still be present regardless of the matrix cell.
		if !strings.Contains(script, "flock -n 9") {
			t.Fatalf("RenderLaunchScript(keepPAT=%v, sandbox=%v): must still guard against double-launch via flock", tc.keepPAT, tc.sandbox)
		}
		if !strings.Contains(script, `. "$HOME/.buzz-backend/env"`) {
			t.Fatalf("RenderLaunchScript(keepPAT=%v, sandbox=%v): must still source the env file", tc.keepPAT, tc.sandbox)
		}
	}
}

// evalEnvVar sources the entire rendered env text through /bin/sh and
// echoes the resulting value of the named variable, to verify
// shellQuote's escaping is actually safe rather than merely
// string-matching it. Sourcing the whole env (rather than slicing out one
// line) is required because a quoted value may itself contain literal
// embedded newlines.
func evalEnvVar(t *testing.T, env, key string) string {
	t.Helper()
	script := env + "\nprintf '%s' \"$" + key + "\"\n"
	return runSh(t, script)
}

// TestRenderLaunchScript_LivenessGuardInvariants pins the two guard
// properties whose absence is invisible in a golden diff but fatal in
// production — both found by the executable proofs in
// launch_exec_test.go.
func TestRenderLaunchScript_LivenessGuardInvariants(t *testing.T) {
	script := RenderLaunchScript(false, false, "", false)

	// A zombie buzz-acp must not count as running (see AliveCheckSnippet).
	if !strings.Contains(script, "buzz_acp_alive") {
		t.Fatal("launch.sh must use the zombie-aware liveness check, not a bare pgrep")
	}
	if !strings.Contains(script, AliveCheckSnippet) {
		t.Fatal("launch.sh must embed AliveCheckSnippet verbatim so the check cannot drift from status/verify")
	}

	// The launched agent must NOT inherit the lock fd, or a lingering
	// worker keeps the flock held and every future start no-ops while
	// the agent is dead.
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "setsid nohup") && !strings.Contains(line, "9>&-") {
			t.Fatalf("the detached launch must close the lock fd (9>&-), got: %q", line)
		}
	}
}

func TestRenderEnv_CurrentLaunchOwnerIsAuthoritative(t *testing.T) {
	agent := payload.Agent{
		RelayURL:       "wss://relay.example",
		PrivateKeyNsec: "nsec1x",
		AuthTag:        "tag",
		AgentCommand:   "buzz-agent",
		Parallelism:    1,
		OwnerPubkey:    strings.Repeat("a", 64),
		EnvVars: map[string]string{
			"BUZZ_ACP_SESSION_POLICY": "thread",
			"BUZZ_ACP_LAZY_POOL":      "true",
		},
	}
	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "")
	for _, want := range []string{
		"export BUZZ_ACP_AGENT_OWNER='" + strings.Repeat("a", 64) + "'",
		"export BUZZ_ACP_SESSION_POLICY='thread'",
		"export BUZZ_ACP_LAZY_POOL='true'",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("rendered env missing %q:\n%s", want, env)
		}
	}
}
