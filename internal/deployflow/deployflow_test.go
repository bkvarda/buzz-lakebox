package deployflow

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/identity"
	"github.com/IceRhymers/buzz-lakebox/internal/lakebox"
	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/skillconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/sshx"
	"github.com/IceRhymers/buzz-lakebox/internal/state"
)

// testNsec is a throwaway nostr private key used only for
// identity-derivation math in tests (never a live relay credential).
const testNsec = "nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5"

// --- fake `databricks` CLI shim -------------------------------------------
//
// The shim replays canned responses per subcommand (controlled by FAKE_*
// env vars set per test) and records every invocation to a shared log
// file: one block per call, terminated by a line containing only "---".
// CLI-level calls (version/current-user/register/list/create/start/
// status/delete/config) log a single "CLI:<name> <args>" line. Sandbox
// ssh calls log three lines: the step tag (parsed from the "# buzz-step:
// <tag>" comment internal/deployflow prepends to every command string —
// see step() in deployflow.go), the full command text base64'd (ARGS —
// must never contain a secret), and the stdin content base64'd (the only
// sanctioned path for secrets). Base64 keeps each multi-line script/stdin
// blob on one log line for easy parsing (PLAN.md §7: "RECORDS every
// invocation (args + stdin) to a log file the test reads").
const fakeShimScript = `#!/bin/sh
LOG="${FAKE_LOG:?FAKE_LOG must be set}"

case "$1" in
  version)
    echo "Databricks CLI v${FAKE_VERSION:-1.9.0}"
    { echo "CLI:version"; echo "---"; } >> "$LOG"
    exit 0
    ;;
  current-user)
    { echo "CLI:current-user"; echo "---"; } >> "$LOG"
    exit "${FAKE_CURRENT_USER_EXIT:-0}"
    ;;
  sandbox)
    case "$2" in
      register)
        { echo "CLI:register"; echo "---"; } >> "$LOG"
        exit "${FAKE_REGISTER_EXIT:-0}"
        ;;
      create)
        name="$3"
        { echo "CLI:create $name"; echo "---"; } >> "$LOG"
        if [ "${FAKE_CREATE_EXIT:-0}" != "0" ]; then
          exit "${FAKE_CREATE_EXIT}"
        fi
        printf '{"sandboxId":"%s","name":"%s","status":"%s"}' "${FAKE_CREATE_ID:-sandbox-created-1}" "$name" "${FAKE_CREATE_STATUS:-Running}"
        exit 0
        ;;
      list)
        { echo "CLI:list"; echo "---"; } >> "$LOG"
        if [ "${FAKE_LIST_EXIT:-0}" != "0" ]; then
          exit "${FAKE_LIST_EXIT}"
        fi
        printf '%s' "${FAKE_LIST_JSON:-[]}"
        exit 0
        ;;
      status)
        id="$3"
        { echo "CLI:status $id"; echo "---"; } >> "$LOG"
        if [ "${FAKE_STATUS_EXIT:-0}" != "0" ]; then
          exit "${FAKE_STATUS_EXIT}"
        fi
        status="${FAKE_STATUS_STATUS:-Running}"
        # FAKE_STATUS_FLIP_FILE: first call reports FAKE_STATUS_STATUS and
        # touches the file; later calls report Running — lets tests drive
        # the stopped -> started -> Running transition WaitRunning polls.
        if [ -n "${FAKE_STATUS_FLIP_FILE:-}" ]; then
          if [ -f "$FAKE_STATUS_FLIP_FILE" ]; then
            status="Running"
          else
            : > "$FAKE_STATUS_FLIP_FILE"
          fi
        fi
        printf '{"sandboxId":"%s","name":"fake","status":"%s"}' "$id" "$status"
        exit 0
        ;;
      start)
        id="$3"
        { echo "CLI:start $id"; echo "---"; } >> "$LOG"
        exit "${FAKE_START_EXIT:-0}"
        ;;
      stop)
        id="$3"
        { echo "CLI:stop $id"; echo "---"; } >> "$LOG"
        exit "${FAKE_STOP_EXIT:-0}"
        ;;
      delete)
        id="$3"
        { echo "CLI:delete $id"; echo "---"; } >> "$LOG"
        exit "${FAKE_DELETE_EXIT:-0}"
        ;;
      config)
        shift 2
        { echo "CLI:config $*"; echo "---"; } >> "$LOG"
        exit "${FAKE_CONFIG_EXIT:-0}"
        ;;
      ssh)
        cmd="$7"
        tag=$(printf '%s\n' "$cmd" | sed -n 's/^# buzz-step:\([a-zA-Z-]*\).*/\1/p' | head -n1)
        stdin_data=$(cat)
        args_b64=$(printf '%s' "$cmd" | base64 | tr -d '\n')
        stdin_b64=$(printf '%s' "$stdin_data" | base64 | tr -d '\n')
        {
          echo "SSH_TAG:$tag"
          echo "SSH_ARGS_B64:$args_b64"
          echo "SSH_STDIN_B64:$stdin_b64"
          echo "---"
        } >> "$LOG"

        case "$tag" in
          auth-probe)
            if [ "${FAKE_AUTH_PROBE_EXIT:-0}" != "0" ]; then
              if [ -n "${FAKE_AUTH_PROBE_CAUSE:-}" ]; then
                echo "BUZZ_PROBE_CAUSE=${FAKE_AUTH_PROBE_CAUSE}"
              fi
              exit "${FAKE_AUTH_PROBE_EXIT}"
            fi
            exit 0
            ;;
          verify-exec)
            if [ "${FAKE_VERIFY_EXEC_EXIT:-0}" != "0" ]; then
              exit "${FAKE_VERIFY_EXEC_EXIT}"
            fi
            printf '%s' "${FAKE_VERIFY_OUTPUT:-}"
            exit 0
            ;;
          install-exec)
            exit "${FAKE_INSTALL_EXIT:-0}"
            ;;
          adapter-exec)
            exit "${FAKE_ADAPTER_EXIT:-0}"
            ;;
          extra-bins-exec)
            exit "${FAKE_EXTRA_BIN_EXIT:-0}"
            ;;
          mux-selftest)
            exit "${FAKE_MUX_SELFTEST_EXIT:-0}"
            ;;
          mcp-verify)
            exit "${FAKE_MCP_VERIFY_EXIT:-0}"
            ;;
          prelaunch-kill)
            printf '%s' "${FAKE_PRELAUNCH_OUTPUT:-}"
            exit 0
            ;;
          claude-inference-probe)
            if [ -n "${FAKE_CLAUDE_PROBE_CAUSE:-}" ]; then
              printf 'BUZZ_CLAUDE_PROBE_CAUSE=%s\n' "${FAKE_CLAUDE_PROBE_CAUSE}"
              printf 'BUZZ_CLAUDE_PROBE_STATUS=%s\n' "${FAKE_CLAUDE_PROBE_STATUS:-401}"
              exit 1
            fi
            exit 0
            ;;
          launch-exec)
            exit "${FAKE_LAUNCH_EXIT:-0}"
            ;;
          verify-check)
            printf 'BUZZ_PGREP_RC=%s\n%s' "${FAKE_PGREP_EXIT:-0}" "${FAKE_ACP_LOG:-}"
            exit 0
            ;;
          status-check)
            printf 'BUZZ_PGREP_RC=%s\n%s' "${FAKE_PGREP_EXIT:-0}" "${FAKE_ACP_LOG:-}"
            exit 0
            ;;
          logs-tail)
            printf '%s' "${FAKE_ACP_LOG:-}"
            exit 0
            ;;
          undeploy-shred)
            exit "${FAKE_SHRED_EXIT:-0}"
            ;;
          start-launch)
            if [ "${FAKE_NO_LAUNCH_SH:-0}" = "1" ]; then
              echo "BUZZ_NO_LAUNCH_SH=1"
              exit 9
            fi
            exit "${FAKE_LAUNCH_EXIT:-0}"
            ;;
          *)
            exit 0
            ;;
        esac
        ;;
      *)
        exit 1
        ;;
    esac
    ;;
  *)
    exit 1
    ;;
esac
`

// harness wires a Deployer at a fake databricks binary and exposes the
// recorded call log.
type harness struct {
	t       *testing.T
	logPath string
	dep     *Deployer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake databricks shim is a POSIX shell script")
	}
	dir := t.TempDir()
	binPath := filepath.Join(dir, "databricks")
	if err := os.WriteFile(binPath, []byte(fakeShimScript), 0o755); err != nil {
		t.Fatalf("write fake databricks: %v", err)
	}
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("FAKE_LOG", logPath)

	cli := &lakebox.CLI{Bin: binPath}
	ssh := &sshx.Client{Bin: binPath}
	dep := New(cli, ssh)
	dep.Sleep = func(time.Duration) {} // no real wall-clock waits in tests
	// Deterministic launch id so the fake acp.log fixture can carry this
	// deploy's launch marker (verifyLaunch scopes the readiness search to
	// the current launch; see testLaunchID / healthyLog).
	dep.NewLaunchID = func() string { return testLaunchID }
	dep.WaitRunningTimeout = 5 * time.Second
	dep.PollInterval = time.Millisecond
	// Never let tests touch the real ~/.local/state mapping (New() wires
	// the default store); point persistence at the test temp dir.
	dep.State = &state.Store{Path: filepath.Join(dir, "agents.json")}

	return &harness{t: t, logPath: logPath, dep: dep}
}

// event is one parsed log block: either a top-level CLI invocation or an
// in-sandbox ssh call.
type event struct {
	kind     string // "CLI" or "SSH"
	cliLine  string // full "CLI:<...>" line, CLI events only
	sshTag   string // SSH events only
	argsB64  string // SSH events only
	stdinB64 string // SSH events only
}

func (h *harness) events() []event {
	h.t.Helper()
	data, err := os.ReadFile(h.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		h.t.Fatalf("read log: %v", err)
	}
	var events []event
	var cur event
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case line == "---":
			if cur.kind != "" {
				events = append(events, cur)
			}
			cur = event{}
		case strings.HasPrefix(line, "CLI:"):
			cur = event{kind: "CLI", cliLine: strings.TrimPrefix(line, "CLI:")}
		case strings.HasPrefix(line, "SSH_TAG:"):
			cur.kind = "SSH"
			cur.sshTag = strings.TrimPrefix(line, "SSH_TAG:")
		case strings.HasPrefix(line, "SSH_ARGS_B64:"):
			cur.argsB64 = strings.TrimPrefix(line, "SSH_ARGS_B64:")
		case strings.HasPrefix(line, "SSH_STDIN_B64:"):
			cur.stdinB64 = strings.TrimPrefix(line, "SSH_STDIN_B64:")
		}
	}
	return events
}

func (e event) args(t *testing.T) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(e.argsB64)
	if err != nil {
		t.Fatalf("decode args for event %+v: %v", e, err)
	}
	return string(b)
}

func (e event) stdin(t *testing.T) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(e.stdinB64)
	if err != nil {
		t.Fatalf("decode stdin for event %+v: %v", e, err)
	}
	return string(b)
}

// callSequence renders each event as a short label for order assertions:
// "CLI:<name>" (first word only, dropping ids/args) or "SSH:<tag>".
func callSequence(events []event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		if e.kind == "CLI" {
			name := strings.Fields(e.cliLine)
			if len(name) == 0 {
				out = append(out, "CLI:")
				continue
			}
			out = append(out, "CLI:"+name[0])
		} else {
			out = append(out, "SSH:"+e.sshTag)
		}
	}
	return out
}

// assertOrder checks that want appears, in order (not necessarily
// contiguous), as a subsequence of got.
func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, w := range want {
		found := false
		for ; i < len(got); i++ {
			if got[i] == w {
				found = true
				i++
				break
			}
		}
		if !found {
			t.Fatalf("expected %q to appear in order within call sequence %v (want subsequence %v)", w, got, want)
		}
	}
}

func assertNotContains(t *testing.T, got []string, unwanted string) {
	t.Helper()
	for _, g := range got {
		if g == unwanted {
			t.Fatalf("call sequence %v unexpectedly contains %q", got, unwanted)
		}
	}
}

// --- test payload builder --------------------------------------------------

type reqOpts struct {
	nsec             string
	profile          string
	idleTimeout      string
	keepWorkspacePAT bool
	buzzVersion      string
	envVars          map[string]string
	authTag          string
	inferenceAuth    string
	// agentCommand selects the agent runtime; empty means buzz-agent, so
	// every pre-existing test keeps its original payload unchanged.
	agentCommand string
}

func buildReq(o reqOpts) *payload.DeployRequest {
	nsec := o.nsec
	if nsec == "" {
		nsec = testNsec
	}
	authTag := o.authTag
	if authTag == "" {
		authTag = "auth-tag-marker-value"
	}
	agentCommand := o.agentCommand
	if agentCommand == "" {
		agentCommand = "buzz-agent"
	}
	return &payload.DeployRequest{
		Op: "deploy",
		Agent: payload.Agent{
			Name:           "Reviewer",
			RelayURL:       "wss://relay.example.com",
			PrivateKeyNsec: nsec,
			AuthTag:        authTag,
			AgentCommand:   agentCommand,
			EnvVars:        o.envVars,
		},
		ProviderConfig: payload.ProviderConfig{
			Profile:          o.profile,
			IdleTimeout:      o.idleTimeout,
			KeepWorkspacePAT: o.keepWorkspacePAT,
			BuzzVersion:      o.buzzVersion,
			InferenceAuth:    o.inferenceAuth,
		},
	}
}

func testPrefix(t *testing.T) string {
	t.Helper()
	npub, err := identity.NsecToNpub(testNsec)
	if err != nil {
		t.Fatalf("NsecToNpub: %v", err)
	}
	prefix, err := identity.PrefixFor(npub)
	if err != nil {
		t.Fatalf("PrefixFor: %v", err)
	}
	return prefix
}

// testLaunchID is the fixed per-deploy launch id the test Deployer stamps,
// standing in for the random one production uses.
const testLaunchID = "testlaunch01"

// healthyLog is what verify-check returns for a launch that succeeded.
//
// Note it carries no launch marker: scoping happens SERVER-SIDE (awk, in
// acpLivenessProbeFor), so what reaches Go is already only the region after
// this launch's marker. The fixture therefore represents the scoped region,
// not the raw file.
const healthyLog = "buzz-acp starting: relay=wss://x pubkey=abc\nagent_pool_ready agents=1\n"

// staleScopedLog is what verify-check returns when this deploy's launch
// marker is absent from acp.log — awk finds nothing to scope to and emits
// nothing. It means launch.sh did not start an agent, which is the
// stale-agent case: an earlier deploy's readiness line may well still be in
// the file, but it is not in THIS launch's region, so it cannot satisfy
// this deploy's verification.
const staleScopedLog = ""
const agentInfoOutput = `{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"name":"buzz-agent","version":"0.1.0"}}}`

func setHappyPathEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FAKE_VERSION", "1.9.0")
	t.Setenv("FAKE_VERIFY_OUTPUT", agentInfoOutput)
	t.Setenv("FAKE_ACP_LOG", healthyLog)
}

// --- (a) happy-path fresh deploy --------------------------------------------

func TestDeploy_HappyPath_FreshCreate(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-new-1")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	req := buildReq(reqOpts{})
	agentID, err := h.dep.Deploy(req)
	if err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}
	if agentID != "sandbox-new-1" {
		t.Fatalf("agent_id = %q, want sandbox-new-1", agentID)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{
		"CLI:version", "CLI:current-user", "CLI:register", "CLI:list", "CLI:create",
		"SSH:pat-reset", "SSH:install-write", "SSH:install-exec",
		"SSH:buzz-skill-write", "SSH:verify-exec",
		"SSH:env-write", "SSH:prelaunch-kill",
		"SSH:launch-write", "SSH:launch-exec",
		"SSH:verify-check",
		"CLI:config",
	})
	// config --no-autostop must be strictly last.
	if seq[len(seq)-1] != "CLI:config" {
		t.Fatalf("expected CLI:config to be the last call, got sequence %v", seq)
	}
	assertNotContains(t, seq, "CLI:delete")

	// config call used --no-autostop by default.
	for _, e := range h.events() {
		if e.kind == "CLI" && strings.HasPrefix(e.cliLine, "config") {
			if !strings.Contains(e.cliLine, "--no-autostop") {
				t.Fatalf("expected default autostop policy --no-autostop, got %q", e.cliLine)
			}
		}
	}
}

func TestDeploy_HappyPath_NoSecretInArgv(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	req := buildReq(reqOpts{
		authTag: "MARKER-AUTH-TAG-abcdefgh",
		envVars: map[string]string{"DATABRICKS_TOKEN": "MARKER-DATABRICKS-TOKEN-abcdefgh"},
	})
	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	secrets := []string{testNsec, "MARKER-AUTH-TAG-abcdefgh", "MARKER-DATABRICKS-TOKEN-abcdefgh"}
	var sawSecretInStdin bool
	for _, e := range h.events() {
		if e.kind != "SSH" {
			continue
		}
		args := e.args(t)
		for _, secret := range secrets {
			if strings.Contains(args, secret) {
				t.Fatalf("secret %q leaked into ARGV for ssh step %q: %q", secret, e.sshTag, args)
			}
		}
		if strings.Contains(e.stdin(t), secrets[0]) {
			sawSecretInStdin = true
		}
	}
	if !sawSecretInStdin {
		t.Fatal("expected the nsec to travel via stdin at least once (env-write step)")
	}
}

// --- (b) idempotent redeploy: one match, Stopped -> start, update-in-place --

func TestDeploy_IdempotentRedeploy_ReuseStopped(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	prefix := testPrefix(t)
	t.Setenv("FAKE_LIST_JSON", `[{"sandboxId":"sandbox-existing-1","name":"`+prefix+`reviewer","status":"Stopped"}]`)
	t.Setenv("FAKE_STATUS_STATUS", "Running")

	req := buildReq(reqOpts{})
	agentID, err := h.dep.Deploy(req)
	if err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}
	if agentID != "sandbox-existing-1" {
		t.Fatalf("agent_id = %q, want sandbox-existing-1", agentID)
	}

	seq := callSequence(h.events())
	assertNotContains(t, seq, "CLI:create")
	assertOrder(t, seq, []string{
		"CLI:version", "CLI:current-user", "CLI:register", "CLI:list",
		"CLI:start", "CLI:status",
		"SSH:pat-reset", "SSH:install-write", "SSH:install-exec",
		"SSH:buzz-skill-write", "SSH:verify-exec",
		"SSH:env-write", "SSH:prelaunch-kill",
		"SSH:launch-write", "SSH:launch-exec",
		"SSH:verify-check",
		"CLI:config",
	})
}

func TestDeploy_IdempotentRedeploy_ReuseAlreadyRunning_NoStart(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	prefix := testPrefix(t)
	t.Setenv("FAKE_LIST_JSON", `[{"sandboxId":"sandbox-existing-2","name":"`+prefix+`reviewer","status":"Running"}]`)

	req := buildReq(reqOpts{})
	agentID, err := h.dep.Deploy(req)
	if err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}
	if agentID != "sandbox-existing-2" {
		t.Fatalf("agent_id = %q", agentID)
	}
	seq := callSequence(h.events())
	assertNotContains(t, seq, "CLI:create")
	assertNotContains(t, seq, "CLI:start")
}

// --- (c) ambiguous match: 2 matches, fail, no mutating calls after list ----

func TestDeploy_AmbiguousMatches_NoMutatingCallsAfterList(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_VERSION", "1.9.0")
	prefix := testPrefix(t)
	t.Setenv("FAKE_LIST_JSON", `[{"sandboxId":"sandbox-a","name":"`+prefix+`one","status":"Running"},{"sandboxId":"sandbox-b","name":"`+prefix+`two","status":"Running"}]`)

	req := buildReq(reqOpts{})
	_, err := h.dep.Deploy(req)
	if err == nil {
		t.Fatal("expected an error for ambiguous identity match")
	}
	if !strings.Contains(err.Error(), "sandbox-a") || !strings.Contains(err.Error(), "sandbox-b") {
		t.Fatalf("error should list both ambiguous sandbox ids, got: %v", err)
	}

	seq := callSequence(h.events())
	want := []string{"CLI:version", "CLI:current-user", "CLI:register", "CLI:list"}
	if len(seq) != len(want) {
		t.Fatalf("expected no calls after list, got sequence %v", seq)
	}
	for i, w := range want {
		if seq[i] != w {
			t.Fatalf("sequence[%d] = %q, want %q (full: %v)", i, seq[i], w, seq)
		}
	}
}

// --- (d) failure injection at mutating steps + teardown --------------------

func TestDeploy_InstallFails_FreshCreate_TearsDownAndDeletes(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_VERSION", "1.8.0")
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-fresh-fail")
	t.Setenv("FAKE_CREATE_STATUS", "Running")
	t.Setenv("FAKE_INSTALL_EXIT", "1")

	req := buildReq(reqOpts{authTag: "MARKER-SHOULD-NOT-LEAK-1234"})
	_, err := h.dep.Deploy(req)
	if err == nil {
		t.Fatal("expected install failure to surface as an error")
	}
	if !strings.Contains(err.Error(), "sandbox-fresh-fail") {
		t.Fatalf("error should embed the sandbox id, got: %v", err)
	}
	// deployflow.wrap is the single stamping boundary for BOTH the
	// sandbox id and CLI version (docs/PLAN.md §4.3), so even this
	// sshx-originated (non-lakebox) install failure carries the version
	// fetched at preflight — and exactly once.
	if !strings.Contains(err.Error(), "databricks cli 1.8.0") {
		t.Fatalf("error should embed the CLI version, got: %v", err)
	}
	if strings.Count(err.Error(), "databricks cli") != 1 {
		t.Fatalf("error should carry exactly one CLI version stamp, got: %v", err)
	}
	if strings.Contains(err.Error(), "MARKER-SHOULD-NOT-LEAK-1234") {
		t.Fatalf("error leaked a planted marker secret: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{"CLI:create", "SSH:install-exec", "SSH:teardown-shred", "CLI:delete"})
	// Nothing past install should have run.
	assertNotContains(t, seq, "SSH:env-write")
	assertNotContains(t, seq, "CLI:config")
}

func TestDeploy_LaunchVerifyFails_TerminalError_ReusedSandbox_KillsNoDelete(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_VERSION", "1.9.0")
	t.Setenv("FAKE_VERIFY_OUTPUT", agentInfoOutput)
	prefix := testPrefix(t)
	t.Setenv("FAKE_LIST_JSON", `[{"sandboxId":"sandbox-reused-fail","name":"`+prefix+`reviewer","status":"Running"}]`)
	t.Setenv("FAKE_ACP_LOG", "buzz-acp starting: relay=wss://x pubkey=abc\nWARN buzz_acp::relay: initial relay connect failed with terminal error: Auth failed: restricted: not a relay member\n")

	req := buildReq(reqOpts{})
	_, err := h.dep.Deploy(req)
	if err == nil {
		t.Fatal("expected launch verification to fail on the terminal-error line")
	}
	if !strings.Contains(err.Error(), "sandbox-reused-fail") || !strings.Contains(err.Error(), "1.9.0") {
		t.Fatalf("error should embed sandbox id + cli version, got: %v", err)
	}
	if !strings.Contains(err.Error(), "relay") {
		t.Fatalf("error should carry the documented relay-membership guidance, got: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{"SSH:verify-check", "SSH:teardown-shred", "SSH:teardown-pkill"})
	assertNotContains(t, seq, "CLI:delete")
	assertNotContains(t, seq, "CLI:config")
}

// TestDeploy_LaunchVerifyFails_TerminalError_DeadProcess_ClassifiesRelayDenied
// pins the real-world non-member signature: buzz-acp exits ~1s after the
// relay denial (docs/M05_PROBE_RESULTS.md §3), so at N=10s the process is
// dead AND the log carries the terminal-error line. That must classify as
// verify.relay_denied, not the generic verify.process_dead — the prior
// liveness-first ordering made relay_denied unreachable in exactly the
// scenario it was built for (found live: sandbox expert-motmot-2453,
// 2026-07-26).
func TestDeploy_LaunchVerifyFails_TerminalError_DeadProcess_ClassifiesRelayDenied(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_VERSION", "1.9.0")
	t.Setenv("FAKE_VERIFY_OUTPUT", agentInfoOutput)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-relay-denied")
	t.Setenv("FAKE_CREATE_STATUS", "Running")
	t.Setenv("FAKE_PGREP_EXIT", "1")
	t.Setenv("FAKE_ACP_LOG", "buzz-acp starting: relay=wss://x pubkey=abc\nWARN buzz_acp::relay: initial relay connect failed with terminal error: Auth failed: restricted: not a relay member\n")

	req := buildReq(reqOpts{})
	_, err := h.dep.Deploy(req)
	if err == nil {
		t.Fatal("expected launch verification to fail on the terminal-error line")
	}
	if !strings.Contains(err.Error(), "verify.relay_denied") {
		t.Fatalf("dead process + terminal-error log must classify as verify.relay_denied, got: %v", err)
	}
	if strings.Contains(err.Error(), "verify.process_dead") {
		t.Fatalf("must not fall through to the generic process_dead diagnosis, got: %v", err)
	}

	// Fresh create + verify failure: full teardown, including delete.
	seq := callSequence(h.events())
	assertOrder(t, seq, []string{"CLI:create", "SSH:verify-check", "SSH:teardown-shred", "CLI:delete"})
	assertNotContains(t, seq, "CLI:config")
}

// TestDeploy_ConfigFails_FreshCreate_NoTeardown_AgentHealthy pins BUG 6:
// a step-11 (setAutostopPolicy) failure happens strictly AFTER launch
// verification already succeeded, so the agent is healthy — deploy must
// not run destructive teardown (no delete, no pkill, no env shred) for
// this failure, and the error must explain the autostop remedy.
func TestDeploy_ConfigFails_FreshCreate_NoTeardown_AgentHealthy(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-config-fail")
	t.Setenv("FAKE_CREATE_STATUS", "Running")
	t.Setenv("FAKE_CONFIG_EXIT", "1")

	req := buildReq(reqOpts{})
	_, err := h.dep.Deploy(req)
	if err == nil {
		t.Fatal("expected config failure to surface as an error")
	}
	if !strings.Contains(err.Error(), "sandbox-config-fail") {
		t.Fatalf("error should embed the sandbox id, got: %v", err)
	}
	if !strings.Contains(err.Error(), "autostop") {
		t.Fatalf("error should mention the autostop remedy, got: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "verified") {
		t.Fatalf("error should state the agent launched/verified successfully, got: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{"CLI:config"})
	assertNotContains(t, seq, "CLI:delete")
	assertNotContains(t, seq, "SSH:teardown-shred")
	assertNotContains(t, seq, "SSH:teardown-pkill")
}

// TestDeploy_ContextExpiresMidProvision_TeardownStillRunsIndependently
// pins BUG 4: when a provisioning step fails BECAUSE the deploy deadline
// expired, teardown (shred + delete) must still complete — it must not
// silently no-op by inheriting the same (already-dead) deploy context.
// DeployTimeout is set to a few milliseconds, and Sleep (the step-10
// post-launch wait) is overridden to really sleep well past that
// deadline, so by the time verifyLaunch's SSH call runs, the deploy ctx
// is genuinely expired — exactly the scenario that used to make teardown
// silently no-op.
func TestDeploy_ContextExpiresMidProvision_TeardownStillRunsIndependently(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-ctx-expired")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	// DeployTimeout must be generous enough for the ~13 real subprocess
	// spawns in steps 1-9 to complete (observed comfortably under 1s
	// locally), but the step-10 Sleep override sleeps well past it so the
	// ctx is genuinely expired by the time verifyLaunch's SSH call runs.
	h.dep.DeployTimeout = 2 * time.Second
	h.dep.Sleep = func(time.Duration) { time.Sleep(3 * time.Second) }

	req := buildReq(reqOpts{})
	_, err := h.dep.Deploy(req)
	if err == nil {
		t.Fatal("expected deploy to fail once its context deadline expires mid-provision")
	}
	if !strings.Contains(err.Error(), "sandbox-ctx-expired") {
		t.Fatalf("error should embed the sandbox id, got: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{"CLI:create", "SSH:teardown-shred", "CLI:delete"})
}

// --- (e) idle_timeout config variant ----------------------------------------

func TestDeploy_IdleTimeoutVariant(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	req := buildReq(reqOpts{idleTimeout: "2h"})
	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	var configLine string
	for _, e := range h.events() {
		if e.kind == "CLI" && strings.HasPrefix(e.cliLine, "config") {
			configLine = e.cliLine
		}
	}
	if !strings.Contains(configLine, "--idle-timeout 2h") {
		t.Fatalf("expected --idle-timeout 2h, got config call %q", configLine)
	}
	if strings.Contains(configLine, "--no-autostop") {
		t.Fatalf("idle_timeout variant must not also pass --no-autostop, got %q", configLine)
	}
}

// --- (f) keep_workspace_pat skips the PAT stub step -------------------------

func TestDeploy_KeepWorkspacePAT_SkipsPATReset(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	req := buildReq(reqOpts{keepWorkspacePAT: true})
	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	seq := callSequence(h.events())
	assertNotContains(t, seq, "SSH:pat-reset")
	// The rest of the flow should still proceed normally.
	assertOrder(t, seq, []string{"SSH:install-write", "SSH:env-write", "SSH:launch-exec", "CLI:config"})
}

// --- validation still runs when deployflow is invoked directly -------------

func TestDeploy_RejectsUnsupportedRuntimeBeforeAnyCLICall(t *testing.T) {
	h := newHarness(t)
	req := buildReq(reqOpts{})
	req.Agent.AgentCommand = "goose"

	_, err := h.dep.Deploy(req)
	if err == nil {
		t.Fatal("expected validation to reject agent_command \"goose\"")
	}
	if !strings.Contains(err.Error(), "goose") {
		t.Fatalf("error should name the rejected runtime, got: %v", err)
	}
	if len(h.events()) != 0 {
		t.Fatalf("expected no CLI/ssh calls before validation, got %v", callSequence(h.events()))
	}
}

// --- parsePgrepCheck (verify-check output parsing) ---------------------------

func TestParsePgrepCheck_MarkerAfterPreamble(t *testing.T) {
	// A stdout preamble (e.g. a shell profile banner) before the marker
	// line must not break parsing: the FIRST marker line in order wins,
	// and only what follows it is the log.
	out := "some shell profile banner\nBUZZ_PGREP_RC=0\nagent_pool_ready agents=1\n"
	rc, log, err := parsePgrepCheck(out)
	if err != nil {
		t.Fatalf("parsePgrepCheck error: %v", err)
	}
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if !strings.Contains(log, "agent_pool_ready") {
		t.Fatalf("log should contain the content after the marker, got %q", log)
	}
	if strings.Contains(log, "banner") {
		t.Fatalf("preamble before the marker must not leak into the log, got %q", log)
	}
}

func TestParsePgrepCheck_FirstMarkerWins_CollisionSafe(t *testing.T) {
	// The echo always precedes the log tail, so the first marker in order
	// is authoritative; marker-shaped text INSIDE the log content must be
	// treated as log, not re-parsed.
	out := "BUZZ_PGREP_RC=1\nlog line quoting BUZZ_PGREP_RC=0 as content\n"
	rc, log, err := parsePgrepCheck(out)
	if err != nil {
		t.Fatalf("parsePgrepCheck error: %v", err)
	}
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (first marker line must win)", rc)
	}
	if !strings.Contains(log, "BUZZ_PGREP_RC=0") {
		t.Fatalf("marker-shaped log content should be preserved as log, got %q", log)
	}
}

func TestParsePgrepCheck_MissingMarker_DistinctError(t *testing.T) {
	// No marker anywhere → a distinct parse error, NOT a fabricated rc=1:
	// an inconclusive check must not masquerade as a confirmed-dead agent
	// (rc=1 routes to the process-dead message; the parse error routes to
	// its own "could not parse verification output" diagnosis upstream).
	_, _, err := parsePgrepCheck("just some log lines\nno marker here\n")
	if err == nil {
		t.Fatal("expected a distinct error when no BUZZ_PGREP_RC marker line exists")
	}
	if !strings.Contains(err.Error(), "BUZZ_PGREP_RC") {
		t.Fatalf("error should name the missing marker, got: %v", err)
	}
}

// TestResolveMcpCommand pins the provider_config.mcp_servers → BUZZ_ACP_MCP_COMMAND
// mapping (issue #16 Increment 1): 0 entries leave the runtime default (""),
// 1 entry points straight at that command, and 2+ entries are REFUSED at deploy
// time until the multiplexer ships (Increment 2) — never emitted as a "bzmux"
// command that does not exist on the sandbox.
func TestResolveMcpCommand(t *testing.T) {
	t.Run("McpNone yields empty, no error", func(t *testing.T) {
		got, err := resolveMcpCommand(payload.ProviderConfig{})
		if err != nil {
			t.Fatalf("McpNone must not error: %v", err)
		}
		if got != "" {
			t.Fatalf("McpNone must resolve to \"\", got %q", got)
		}
	})
	t.Run("McpDirect points at the single command", func(t *testing.T) {
		got, err := resolveMcpCommand(payload.ProviderConfig{McpServers: []string{"shellbox-mcp"}})
		if err != nil {
			t.Fatalf("McpDirect must not error: %v", err)
		}
		if got != "shellbox-mcp" {
			t.Fatalf("McpDirect must resolve to the single entry, got %q", got)
		}
	})
	t.Run("McpMux resolves to bzmux (Increment 2 landed)", func(t *testing.T) {
		got, err := resolveMcpCommand(payload.ProviderConfig{McpServers: []string{"buzz-dev-mcp", "shellbox-mcp"}})
		if err != nil {
			t.Fatalf("McpMux must not error after Increment 2: %v", err)
		}
		if got != payload.MuxBinaryName {
			t.Fatalf("McpMux must resolve to %q, got %q", payload.MuxBinaryName, got)
		}
	})
}

func TestDeploy_BuzzSkillWrittenFromEmbeddedBytes(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatal(err)
	}
	for _, e := range h.events() {
		if e.kind != "SSH" || e.sshTag != "buzz-skill-write" {
			continue
		}
		if got := e.stdin(t); got != strings.TrimSuffix(string(nest.BuzzSkillMD), "\n") {
			t.Fatalf("skill stdin differed from embedded bytes after the shell shim's trailing-newline trim: got %d bytes, want %d", len(got), len(nest.BuzzSkillMD))
		}
		args := e.args(t)
		for _, want := range []string{nest.BuzzSkillPath, nest.BuzzSkillVersionPath, "mv -f", nest.BuzzSkillLinkScript} {
			if !strings.Contains(args, want) {
				t.Fatalf("skill install command missing %q", want)
			}
		}
		return
	}
	t.Fatal("deploy did not write the Buzz CLI skill")
}

func TestDeploy_DatabricksSkillsRunAfterBuzzSkill(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	req := buildReq(reqOpts{})
	req.ProviderConfig.Skills = skillconfig.Config{Schema: skillconfig.CurrentSchema, Version: 1, AITools: &skillconfig.AITools{Skills: []string{"sql"}}}
	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatal(err)
	}
	assertOrder(t, callSequence(h.events()), []string{"SSH:install-exec", "SSH:buzz-skill-write", "SSH:skills-write", "SSH:skills-exec", "SSH:verify-exec"})
	for _, e := range h.events() {
		if e.kind == "SSH" && e.sshTag == "skills-write" {
			script := e.stdin(t)
			for _, want := range []string{"databricks aitools install --skills-only", "--skills 'sql'", "buzz-cli", "replace-managed"} {
				if !strings.Contains(script, want) {
					t.Fatalf("skills script missing %q", want)
				}
			}
			return
		}
	}
	t.Fatal("skills installer was not written")
}
