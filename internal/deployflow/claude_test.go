package deployflow

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// claudeRequest is a minimal valid Claude deploy payload. DATABRICKS_HOST
// is present because DeployRequest.Validate() now requires an inference
// source for this runtime.
func claudeRequest() *payload.DeployRequest {
	return buildReq(reqOpts{
		agentCommand: "claude-code",
		envVars: map[string]string{
			"DATABRICKS_HOST":  "https://example.databricks.com",
			"DATABRICKS_TOKEN": "dapi-marker-secret",
		},
	})
}

// TestDeploy_ClaudeRuntime_InstallsAdapterInOrder pins where the adapter
// install sits in the flow: after the .deb (which carries buzz-acp itself)
// and before verification, with the gateway probe after the handshake.
func TestDeploy_ClaudeRuntime_InstallsAdapterInOrder(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_VERIFY_OUTPUT", `{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"name":"@agentclientprotocol/claude-agent-acp","version":"0.73.0"}}}`)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-claude-1")

	if _, err := h.dep.Deploy(claudeRequest()); err != nil {
		t.Fatalf("claude deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{
		"SSH:install-exec",
		"SSH:adapter-write", "SSH:adapter-exec",
		"SSH:verify-exec",
		"SSH:claude-inference-probe",
		"SSH:launch-exec",
	})
}

// TestDeploy_BuzzAgentRuntime_SkipsAdapterSteps guards against the adapter
// install leaking into the buzz-agent path — a 570 MB npm tree and a
// gateway probe that buzz-agent neither needs nor would pass.
func TestDeploy_BuzzAgentRuntime_SkipsAdapterSteps(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-buzz-1")

	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatalf("buzz-agent deploy failed: %v", err)
	}

	seq := strings.Join(callSequence(h.events()), ",")
	for _, unwanted := range []string{"adapter-write", "adapter-exec", "claude-inference-probe"} {
		if strings.Contains(seq, unwanted) {
			t.Errorf("buzz-agent deploy must not run %q; sequence: %s", unwanted, seq)
		}
	}
}

func TestDeploy_ClaudeAdapterFailure_IsDistinctlyCoded(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-claude-2")
	t.Setenv("FAKE_ADAPTER_EXIT", "1")

	_, err := h.dep.Deploy(claudeRequest())
	if err == nil {
		t.Fatal("expected the adapter install failure to fail the deploy")
	}
	if got := CodeOf(err); got != CodeAdapterExec {
		t.Fatalf("code = %q, want %q (error: %v)", got, CodeAdapterExec, err)
	}
}

// TestDeploy_ClaudeInferenceProbeFailure_IsDistinctlyCoded covers the gap
// the probe exists to close: the agent installs and handshakes fine, and
// only the actual gateway call fails.
func TestDeploy_ClaudeInferenceProbeFailure_IsDistinctlyCoded(t *testing.T) {
	for _, tc := range []struct{ cause, wantIn string }{
		{"unset", "NOT launched"},
		{"auth", "HTTP 401"},
		{"unreachable", "network egress"},
	} {
		t.Run(tc.cause, func(t *testing.T) {
			h := newHarness(t)
			setHappyPathEnv(t)
			t.Setenv("FAKE_LIST_JSON", "[]")
			t.Setenv("FAKE_CREATE_ID", "sandbox-claude-3")
			t.Setenv("FAKE_CLAUDE_PROBE_CAUSE", tc.cause)

			_, err := h.dep.Deploy(claudeRequest())
			if err == nil {
				t.Fatal("expected the inference probe failure to fail the deploy")
			}
			if got := CodeOf(err); got != CodeClaudeInference {
				t.Fatalf("code = %q, want %q (error: %v)", got, CodeClaudeInference, err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("cause %q should produce a specific diagnosis containing %q, got: %v", tc.cause, tc.wantIn, err)
			}
		})
	}
}

// TestVerifyLaunch_RejectsStaleReadiness is the regression guard for the
// append-only acp.log hazard: a readiness line from a PREVIOUS deploy must
// not satisfy this one. Without the per-launch marker, a launch that never
// actually happened (because a draining agent was still holding the guard)
// would verify as healthy and the deploy would report ok:true while the old
// runtime kept serving with the old environment.
func TestVerifyLaunch_RejectsStaleReadiness(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-stale-1")
	t.Setenv("FAKE_ACP_LOG", staleScopedLog)

	_, err := h.dep.Deploy(buildReq(reqOpts{}))
	if err == nil {
		t.Fatal("a readiness line from a previous launch must not verify this deploy")
	}
	if got := CodeOf(err); got != CodeVerifyNotReady {
		t.Fatalf("code = %q, want %q (error: %v)", got, CodeVerifyNotReady, err)
	}
	if !strings.Contains(err.Error(), "did not start an agent") {
		t.Fatalf("diagnosis should explain that no agent was launched, got: %v", err)
	}
}

// TestAcpLivenessProbe_ScopesToLaunchServerSide pins WHERE the scoping
// happens, which is the whole point of doing it in awk. Selecting the
// post-marker region before the byte bound is applied is what keeps a
// chatty startup from pushing the marker out of a pre-truncated window and
// failing a healthy deploy — the marker is always older than the readiness
// line it scopes, so truncate-then-search would be strictly more fragile
// than the unscoped check it replaced.
func TestAcpLivenessProbe_ScopesToLaunchServerSide(t *testing.T) {
	scoped := acpLivenessProbeFor("abc123")
	if !strings.Contains(scoped, "awk -v m=") {
		t.Fatal("a scoped probe must select the launch region server-side, where the whole log is available")
	}
	if !strings.Contains(scoped, nest.LaunchEpochPrefix+"abc123") {
		t.Fatal("the scoped probe must carry this launch's marker")
	}
	awkIdx := strings.Index(scoped, "awk -v m=")
	tailIdx := strings.LastIndex(scoped, "tail -c 4096")
	if awkIdx < 0 || tailIdx < awkIdx {
		t.Fatalf("the byte bound must be applied AFTER the region is selected (awk=%d tail=%d)", awkIdx, tailIdx)
	}

	// Unscoped form is byte-identical to the pre-change probe, so Status
	// and Start keep their existing behavior.
	unscoped := acpLivenessProbeFor("")
	if strings.Contains(unscoped, "awk -v m=") {
		t.Fatal("an unscoped probe must not filter the log")
	}
	if unscoped != acpLivenessProbe() {
		t.Fatal("acpLivenessProbe() must equal the unscoped form")
	}
}

// TestVerifyLaunch_RejectsReadinessOutsideThisLaunch is the case the
// server-side scoping exists for and the one a naive implementation gets
// wrong: an agent that stamped its launch and then died before becoming
// ready, while an OLDER readiness line is still in the file. Because awk
// emits only the post-marker region, that older line never reaches Go.
func TestVerifyLaunch_RejectsReadinessOutsideThisLaunch(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-stale-2")
	// The scoped region: this launch stamped, then logged a crash — with
	// no readiness line of its own.
	t.Setenv("FAKE_ACP_LOG", "buzz-acp starting: relay=wss://x pubkey=abc\npanic: adapter exited\n")

	_, err := h.dep.Deploy(buildReq(reqOpts{}))
	if err == nil {
		t.Fatal("a launch that never reported readiness must not verify")
	}
	if got := CodeOf(err); got != CodeVerifyNotReady {
		t.Fatalf("code = %q, want %q (error: %v)", got, CodeVerifyNotReady, err)
	}
}

// TestDeploy_StaleAgentBlocksLaunch drives the CodeStaleAgent path end to
// end: a previous buzz-acp that will not die must fail the deploy rather
// than let launch.sh no-op over it.
func TestDeploy_StaleAgentBlocksLaunch(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-stale-3")
	t.Setenv("FAKE_PRELAUNCH_OUTPUT", prelaunchStillAliveMarker)

	_, err := h.dep.Deploy(buildReq(reqOpts{}))
	if err == nil {
		t.Fatal("a buzz-acp that survived the kill must fail the deploy")
	}
	if got := CodeOf(err); got != CodeStaleAgent {
		t.Fatalf("code = %q, want %q (error: %v)", got, CodeStaleAgent, err)
	}
	// Nothing past the guard may run: launching over a live agent is the
	// failure this code exists to prevent.
	seq := strings.Join(callSequence(h.events()), ",")
	if strings.Contains(seq, "launch-exec") {
		t.Fatalf("launch must not proceed past a stale agent; sequence: %s", seq)
	}
}

// TestPrelaunchKill_WaitsForDeath pins the shape of the kill script rather
// than just its presence: the wait, the SIGKILL escalation, and the
// zombie-aware liveness check are each load-bearing, and a bare pkill would
// pass a "does it contain pkill" assertion while reintroducing the race.
func TestPrelaunchKill_WaitsForDeath(t *testing.T) {
	script := prelaunchKillScript()
	for _, want := range []string{
		"pkill -f '[b]uzz-acp'",
		"buzz_acp_alive",
		"pkill -9 -f '[b]uzz-acp'",
		prelaunchStillAliveMarker,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("prelaunch kill script missing %q", want)
		}
	}
	// The literal (unbracketed) pattern would match this command's own
	// argv and SIGTERM the invoking shell.
	if strings.Contains(script, "pkill -f 'buzz-acp'") {
		t.Error("must use the bracket idiom so pkill does not match its own argv")
	}
	// Reporting via marker rather than exit status keeps a genuine
	// transport failure distinguishable from "the process would not die".
	if !strings.Contains(script, "exit 0") {
		t.Error("script should exit 0 and report liveness via the marker")
	}
}

// TestClaudeProbeCauseMessage_StatusIsValidated pins that remote-derived
// text cannot ride into an operator-facing error unbounded. The status is
// interpolated into a message that does NOT go through remoteText, and the
// Claude runtime ships a shell that can write the sandbox user's startup
// files — so anything but curl's own three-digit code is reported as
// "unknown" rather than echoed.
func TestClaudeProbeCauseMessage_StatusIsValidated(t *testing.T) {
	hostile := "BUZZ_CLAUDE_PROBE_STATUS=401 " + strings.Repeat("A", 5000) + " nsec1leak"
	msg := claudeProbeCauseMessage("auth", hostile)

	if strings.Contains(msg, "AAAA") {
		t.Fatalf("unbounded remote text reached the error message: %.200q", msg)
	}
	if strings.Contains(msg, "nsec1leak") {
		t.Fatalf("unscrubbed remote text reached the error message: %.200q", msg)
	}
	if !strings.Contains(msg, "HTTP unknown") {
		t.Fatalf("a malformed status should degrade to \"unknown\", got: %.200q", msg)
	}

	// The well-formed case still reports the real code.
	if got := claudeProbeCauseMessage("auth", "BUZZ_CLAUDE_PROBE_STATUS=403\n"); !strings.Contains(got, "HTTP 403") {
		t.Fatalf("a valid status should be reported verbatim, got: %q", got)
	}
}

// TestClaudeInferenceProbeScript_TrapsSecretTempFile pins that the file
// holding the agent's real env (nsec, auth tag, Databricks token) is
// reclaimed even when sourcing aborts under set -e. Its structural twin
// install.BuildVerifyCommand traps; nothing else ever shreds this path.
func TestClaudeInferenceProbeScript_TrapsSecretTempFile(t *testing.T) {
	script := claudeInferenceProbeScript()
	// BUZZ_PROBE_HDR joined this list when the bearer moved out of curl's
	// argv into a config file; it is secret-bearing too, so the same trap
	// must reclaim it.
	if !strings.Contains(script, `trap 'rm -f "$BUZZ_PROBE_TMP" "$BUZZ_PROBE_ERR" "$BUZZ_PROBE_HDR"' EXIT`) {
		t.Fatal("probe must trap-remove its secret-bearing temp files; a plain rm is skipped when sourcing aborts")
	}
	trapIdx := strings.Index(script, "trap 'rm -f")
	catIdx := strings.Index(script, `cat > "$BUZZ_PROBE_TMP"`)
	if trapIdx < 0 || catIdx < 0 || trapIdx > catIdx {
		t.Fatalf("the trap must be armed BEFORE any secret is written (trap=%d cat=%d)", trapIdx, catIdx)
	}
	// The token must never reach a log or an error. The response body is
	// discarded outright; curl's stderr is captured to a trapped temp file
	// (bounded to 200 chars server-side, then scrubbed by remoteText) so a
	// genuine "unreachable" carries a diagnosis without echoing the request.
	if !strings.Contains(script, "-o /dev/null") {
		t.Error("probe must discard curl's response body")
	}
	if !strings.Contains(script, `"$BUZZ_PROBE_ERR"`) {
		t.Error("curl's stderr capture must be trapped too")
	}
	if strings.Contains(script, "Authorization: Bearer") && strings.Contains(script, "set -x") {
		t.Error("tracing would echo the Authorization header")
	}
}
