// Failure taxonomy (docs/PLAN.md §6 M3: "every failure mode → distinct
// actionable ok:false").
//
// Every error this package returns to a caller carries a stable machine
// code and exactly one canonical operator remedy, rendered as:
//
//	[<code>] <what went wrong> — remedy: <what to do about it>
//
// The code is the part a human (or, later, a protocol-v2 desktop) can
// match on: it never changes when the surrounding prose does, and no two
// distinct failure modes share one. The remedy lives in a single table
// here rather than at each call site, so the same failure always
// suggests the same fix no matter which step raised it.
package deployflow

import (
	"errors"
	"fmt"
)

// Code identifies one distinct failure mode. Values are stable
// identifiers — treat them as API: rename only with a deliberate
// migration, since operators and runbooks match on them
// (docs/RUNBOOK.md).
type Code string

const (
	// Preflight — nothing has been provisioned yet (docs/PLAN.md §4.4 step 2).
	CodeValidation        Code = "validation"
	CodeCLIVersionUnknown Code = "preflight.cli_version_unknown"
	CodeCLIVersionOld     Code = "preflight.cli_too_old"
	CodeProfileUnresolved Code = "preflight.profile"
	CodeSandboxRegister   Code = "preflight.sandbox_register"

	// Identity + reuse (docs/PLAN.md §4.1).
	CodeIdentityDerive    Code = "identity.derive"
	CodeIdentityAmbiguous Code = "identity.ambiguous"
	CodeStateRead         Code = "state.read"
	CodeStateWrite        Code = "state.write"

	// Sandbox lifecycle CLI calls (docs/PLAN.md §4.4 step 3).
	CodeSandboxList   Code = "sandbox.list"
	CodeSandboxCreate Code = "sandbox.create"
	CodeSandboxStart  Code = "sandbox.start"
	CodeSandboxWait   Code = "sandbox.wait_running"
	CodeSandboxStatus Code = "sandbox.status"
	CodeSandboxStop   Code = "sandbox.stop"
	CodeSandboxDelete Code = "sandbox.delete"

	// In-sandbox provisioning (docs/PLAN.md §4.4 steps 4-9).
	CodePATReset Code = "provision.pat_reset"
	// CodeSandboxAuth is provision.sandbox_auth (not preflight.*): it fires
	// against an already-established sandbox, in the step-4 slot the PAT
	// reset would otherwise occupy, when provider_config.inference_auth is
	// "sandbox" (docs/PLAN.md zero-token design).
	CodeSandboxAuth   Code = "provision.sandbox_auth"
	CodeInstallScript Code = "install.script"
	CodeInstallWrite  Code = "install.write"
	CodeInstallExec   Code = "install.exec"
	CodeBuzzSkill     Code = "install.buzz_skill"
	CodeSkillsConfig  Code = "install.skills_config"
	CodeSkillsExec    Code = "install.skills_exec"
	// Adapter install — the npm-installed ACP adapter the Claude runtime
	// spawns. Only reached for runtimes that need one; buzz-agent ships in
	// the .deb and skips these entirely.
	CodeAdapterScript Code = "install.adapter_script"
	CodeAdapterWrite  Code = "install.adapter_write"
	CodeAdapterExec   Code = "install.adapter_exec"
	CodeRuntimeVerify Code = "install.runtime_verify"
	// CodeClaudeInference is the Claude-only deploy-time gateway probe. It
	// is what makes "a successful deploy means an agent that can run a
	// session" true for this runtime: the ACP handshake behind
	// CodeRuntimeVerify never touches the LLM, so without this a bad
	// endpoint, a rejected credential, or a gateway that does not serve
	// the anthropic route all deploy "healthy" and die at first mention.
	CodeClaudeInference Code = "install.claude_inference"
	// CodeCodexInference is the codex twin of CodeClaudeInference, and
	// exists separately rather than as a shared code because its remedies
	// differ: a different gateway surface, a different config mechanism
	// (a generated config.toml rather than env vars), and a distinct
	// fail-closed consequence — a codex agent with no config falls back to
	// the image's ~/.codex symlink and its baked workspace credential.
	CodeCodexInference Code = "install.codex_inference"
	// Extra-binaries install — the #15 capability channel. Only reached when
	// provider_config.extra_binaries is non-empty; a deploy without it never
	// emits these. Script is the render/validation failure (e.g. a duplicate
	// bin), write/exec mirror the .deb and adapter write/exec pairs.
	CodeExtraBinScript Code = "install.extra_bin_script"
	CodeExtraBinWrite  Code = "install.extra_bin_write"
	CodeExtraBinExec   Code = "install.extra_bin_exec"
	// MCP multiplexer install — the #16 Increment 2 capability channel. Only
	// reached when provider_config.mcp_servers has ≥2 entries (McpMux mode);
	// a deploy with 0 or 1 entries never emits these.
	// Write covers both binary and config file write failures. Exec is
	// reserved for chmod/setup step failures (currently inlined; kept for
	// RUNBOOK completeness). Selftest fires when `bzmux --selftest` exits
	// non-zero at deploy time (prevents shipping a live-bitten agent).
	CodeMuxWrite    Code = "install.mux_write"
	CodeMuxExec     Code = "install.mux_exec"
	CodeMuxSelftest Code = "install.mux_selftest"
	// CodeMcpVerify fires when the deploy-time MCP slot verification (#17)
	// fails: bzmux --verify-mcp runs a real initialize+tools/list handshake
	// against whatever BUZZ_ACP_MCP_COMMAND resolves to (McpDirect: the single
	// command; McpMux: the multiplexer's children) and asserts a non-empty tool
	// catalog plus the expected tool names where the server is known. It closes
	// the silent-failure class where a deploy passes ACP verification but the
	// agent is permanently tool-less. CodeMuxSelftest stays reserved (the
	// --selftest subcommand + its RUNBOOK remedy remain valid); mux-mode
	// mcp-verify is a superset of it and replaces it in the deploy flow.
	CodeMcpVerify     Code = "install.mcp_verify"
	CodeEnvWrite      Code = "provision.env_write"
	CodePrelaunchKill Code = "launch.prelaunch_kill"
	// CodeStaleAgent fires when a previous buzz-acp is still alive after
	// the prelaunch kill's bounded wait. Launching over it would be worse
	// than failing: launch.sh refuses to relaunch while one is alive, and
	// acp.log is append-only, so the previous deploy's readiness line
	// would satisfy this deploy's verification while the OLD runtime kept
	// serving with the OLD environment.
	CodeStaleAgent  Code = "launch.stale_agent"
	CodeLaunchWrite Code = "launch.write"
	CodeLaunchExec  Code = "launch.exec"

	// Launch verification (docs/PLAN.md §4.4 step 10).
	CodeVerifySSH         Code = "verify.unreachable"
	CodeVerifyUnparseable Code = "verify.unparseable"
	CodeVerifyProcessDead Code = "verify.process_dead"
	CodeVerifyRelayDenied Code = "verify.relay_denied"
	CodeVerifyNotReady    Code = "verify.pool_not_ready"

	// Post-verification (docs/PLAN.md §4.4 step 11).
	CodeAutostopConfig Code = "autostop.config"

	// Operator lifecycle subcommands (docs/PLAN.md §6 M2).
	CodeNotDeployed Code = "lifecycle.not_deployed"
	CodeLogsRead    Code = "lifecycle.logs_read"
	CodeStatusProbe Code = "lifecycle.status_probe"
	// CodeStatusUnparseable is Status's own parse-failure code, distinct
	// from CodeVerifyUnparseable: that code's remedy tells the operator
	// to run `status` and `logs`, which is circular when the failure
	// came FROM status itself.
	CodeStatusUnparseable Code = "lifecycle.status_unparseable"
)

// AllCodes is every code this package can emit, in flow order. It is
// what docs/RUNBOOK.md's table is generated from and what the taxonomy
// tests iterate — a new code that is not listed here has no remedy
// coverage, which the tests fail on.
var AllCodes = []Code{
	CodeValidation, CodeCLIVersionUnknown, CodeCLIVersionOld, CodeProfileUnresolved, CodeSandboxRegister,
	CodeIdentityDerive, CodeIdentityAmbiguous, CodeStateRead, CodeStateWrite,
	CodeSandboxList, CodeSandboxCreate, CodeSandboxStart, CodeSandboxWait, CodeSandboxStatus, CodeSandboxStop, CodeSandboxDelete,
	CodePATReset, CodeSandboxAuth, CodeInstallScript, CodeInstallWrite, CodeInstallExec, CodeBuzzSkill, CodeSkillsConfig, CodeSkillsExec,
	CodeAdapterScript, CodeAdapterWrite, CodeAdapterExec, CodeRuntimeVerify, CodeClaudeInference, CodeCodexInference,
	CodeExtraBinScript, CodeExtraBinWrite, CodeExtraBinExec,
	CodeMuxWrite, CodeMuxExec, CodeMuxSelftest, CodeMcpVerify,
	CodeEnvWrite, CodePrelaunchKill, CodeStaleAgent, CodeLaunchWrite, CodeLaunchExec,
	CodeVerifySSH, CodeVerifyUnparseable, CodeVerifyProcessDead, CodeVerifyRelayDenied, CodeVerifyNotReady,
	CodeAutostopConfig,
	CodeNotDeployed, CodeLogsRead, CodeStatusProbe, CodeStatusUnparseable,
}

// remedies is the single source of truth for what an operator should do
// about each failure mode. Every Code declared above must appear here —
// TestTaxonomy_EveryCodeHasARemedy enforces it.
var remedies = map[Code]string{
	CodeValidation:        "fix the deploy payload field named above and redeploy",
	CodeCLIVersionUnknown: "install the Databricks CLI and make sure it is on PATH (`databricks version` must work); run `buzz-backend-databricks-lakebox doctor`",
	CodeCLIVersionOld:     "upgrade the Databricks CLI, then rerun `buzz-backend-databricks-lakebox doctor`",
	CodeProfileUnresolved: "check ~/.databrickscfg for the profile and re-authenticate (`databricks auth login -p <profile>`)",
	CodeSandboxRegister:   "confirm the workspace is in a Lakebox-enabled region (us-west-2 verified) and that your profile may use sandboxes",

	CodeIdentityDerive:    "the agent's private_key_nsec is not a valid bech32 nsec — re-mint the agent's key in Buzz Desktop and redeploy",
	CodeIdentityAmbiguous: "delete the stale sandbox(es) listed above with `databricks sandbox delete <id>`, then redeploy",
	CodeStateRead:         "inspect (or delete) ~/.local/state/buzz-lakebox/agents.json — a corrupt mapping file blocks reuse; deleting it costs only orphan detection, which `databricks sandbox list` can replace",
	CodeStateWrite:        "make ~/.local/state/buzz-lakebox writable — without a persisted mapping every redeploy orphans a still-billing sandbox",

	CodeSandboxList:   "verify sandbox access with `databricks sandbox list -p <profile>`",
	CodeSandboxCreate: "check the workspace's sandbox quota and region gating with `databricks sandbox list -p <profile>`",
	CodeSandboxStart:  "retry; if the sandbox is mid-transition (Stopping), wait for it to settle and redeploy",
	CodeSandboxWait:   "check the sandbox with `databricks sandbox status <id>` — it may be stuck mid-transition; retry once it settles",
	CodeSandboxStatus: "confirm the sandbox still exists with `databricks sandbox list -p <profile>`; if it was deleted, redeploy to create a new one",
	CodeSandboxStop:   "retry, or stop it directly with `databricks sandbox stop <id>`",
	CodeSandboxDelete: "delete it manually with `databricks sandbox delete <id> --auto-approve` — an undeleted sandbox keeps billing",

	CodePATReset:      "retry; if it persists the sandbox may be unreachable over SSH — check `databricks sandbox ssh <id> -- true`",
	CodeSandboxAuth:   "the error text names which of three causes the auth probe hit: (a) stub marker present — this sandbox was previously deployed in env mode and its baked PAT is unrestorable; `databricks sandbox delete <id> --auto-approve` then redeploy fresh in sandbox mode; (b) ~/.databrickscfg missing/unparseable — inspect it with `databricks sandbox ssh <id> -- cat ~/.databrickscfg`; (c) derived credential rejected — retry (may be transient), or fall back to `inference_auth: \"env\"` for this agent",
	CodeInstallScript: "pass a known `provider_config.buzz_version` (the error lists the pinned versions this build ships sha256s for)",
	CodeInstallWrite:  "check sandbox SSH reachability with `databricks sandbox ssh <id> -- true`",
	CodeInstallExec:   "read the install output above: a sha256 mismatch means the pinned release changed; a fetch failure means the sandbox lost egress to GitHub",
	CodeBuzzSkill:     "check sandbox SSH reachability and that $HOME/.buzz is writable; the embedded Buzz CLI skill could not be refreshed",
	CodeSkillsConfig:  "fix provider_config.skills_config; only the versioned buzz-skills schema and safe skill names are accepted",
	CodeSkillsExec:    "check `databricks aitools` availability and output above; remove unmanaged name collisions or choose replace-managed only for provider-marked skills",
	CodeAdapterScript: "pass a known `provider_config.claude_adapter_version` / `codex_adapter_version` — the error names which adapter it hit and lists the versions this build ships a pinned package-lock.json for",
	CodeAdapterWrite:  "check sandbox SSH reachability with `databricks sandbox ssh <id> -- true`",
	CodeAdapterExec:   "read the npm output above: an integrity mismatch means the registry served different bytes than the pinned lockfile — do NOT retry, report it; anything else is usually lost sandbox egress to registry.npmjs.org, which is safe to retry",
	CodeRuntimeVerify: "the installed agent runtime could not complete an ACP initialize handshake — check `logs` and the inference env for that runtime (buzz-agent: BUZZ_AGENT_PROVIDER / DATABRICKS_HOST / DATABRICKS_TOKEN; claude: ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / DATABRICKS_HOST; codex: CODEX_HOME and the generated $HOME/.buzz-backend/codex/config.toml)",
	CodeClaudeInference: "the agent installed and handshook, but could not reach the AI Gateway: confirm the workspace serves `{host}/ai-gateway/anthropic/v1/messages` and that the credential is accepted there — " +
		"in `inference_auth: \"env\"` check env_vars DATABRICKS_HOST/DATABRICKS_TOKEN, in `\"sandbox\"` mode retry or fall back to env mode for this agent",
	CodeCodexInference: "the agent installed and handshook, but could not reach the AI Gateway: confirm the workspace serves `{host}/ai-gateway/codex/v1/responses` and that the credential is accepted there — " +
		"in `inference_auth: \"env\"` check env_vars DATABRICKS_HOST/DATABRICKS_TOKEN, in `\"sandbox\"` mode retry or fall back to env mode for this agent. " +
		"An \"unset\" cause means no config.toml was generated at all, which is the fail-closed path working: the agent was deliberately NOT launched",
	CodeExtraBinScript: "the extra-binaries install script could not be rendered — a `provider_config.extra_binaries` entry is malformed or two entries share the same `bin`; fix the payload and redeploy",
	CodeExtraBinWrite:  "check sandbox SSH reachability with `databricks sandbox ssh <id> -- true`",
	CodeExtraBinExec:   "read the install output above: a sha256 mismatch means the pinned URL served different bytes than `provider_config.extra_binaries[].sha256` (do NOT retry — re-pin the sha256 or the URL); a fetch failure means the sandbox lost egress to the download host",
	CodeMuxWrite:       "check sandbox SSH reachability with `databricks sandbox ssh <id> -- true`; the MCP multiplexer binary or config could not be written to the sandbox",
	CodeMuxExec:        "check sandbox SSH reachability and that $HOME/.buzz-backend/bin is writable; re-running the deploy usually fixes a transient permission failure",
	CodeMuxSelftest:    "the MCP multiplexer failed its deploy-time self-test — check each entry in `provider_config.mcp_servers` is a valid, installed MCP command on the sandbox; run `databricks sandbox ssh <id> -- $HOME/.buzz-backend/bin/bzmux --selftest` for the full error",
	CodeMcpVerify:      "the MCP server the agent will use failed a deploy-time initialize+tools/list check — the error text distinguishes a server that is slow to start (raise the verify budget) from one that is broken or exports the wrong/too-few tools (fix the command): confirm `BUZZ_ACP_MCP_COMMAND` (resolved from `provider_config.mcp_servers`) names a valid, installed MCP server that returns its expected tools, and run `databricks sandbox ssh <id> -- $HOME/.buzz-backend/bin/bzmux --verify-mcp --command <cmd>` for the full error",
	CodeEnvWrite:       "check sandbox SSH reachability and that $HOME is writable in the sandbox",
	CodePrelaunchKill:  "check sandbox SSH reachability with `databricks sandbox ssh <id> -- true`",
	CodeStaleAgent:     "a previous buzz-acp was still shutting down and did not exit — run `status <sandbox-id>` to confirm, then `stop <sandbox-id>` followed by a redeploy; if it persists the old process is wedged and the sandbox needs a restart",
	CodeLaunchWrite:    "check sandbox SSH reachability and that $HOME is writable in the sandbox",
	CodeLaunchExec:     "run `logs <sandbox-id>` for the agent's own output, then `start <sandbox-id>` to retry the launch",

	CodeVerifySSH:         "the sandbox stopped responding right after launch — run `status <sandbox-id>`, then `start <sandbox-id>`",
	CodeVerifyUnparseable: "run `status <sandbox-id>` and `logs <sandbox-id>` to see the agent's real state before redeploying",
	CodeVerifyProcessDead: "run `logs <sandbox-id>` for the crash output; the acp.log tail is included above",
	CodeVerifyRelayDenied: "mint or register a relay-member key for this agent in Buzz Desktop and redeploy — this key is not a member of the target relay, or the payload's auth_tag is missing/stale (the relay denies a member key with an empty auth tag the same way)",
	CodeVerifyNotReady:    "run `logs <sandbox-id>`; the agent started but never reported a ready pool within the verification window",

	CodeAutostopConfig: "run `databricks sandbox config <sandbox-id> --no-autostop` manually, or redeploy — the agent itself is healthy",

	CodeNotDeployed:       "deploy this sandbox first (from Buzz Desktop, or `deploy --payload-file`)",
	CodeLogsRead:          "confirm the sandbox is Running with `status <sandbox-id>` — a stopped sandbox has no readable log",
	CodeStatusProbe:       "confirm sandbox SSH reachability with `databricks sandbox ssh <id> -- true`",
	CodeStatusUnparseable: "run `logs <sandbox-id>` for the agent's raw output, or inspect the sandbox directly with `databricks sandbox ssh <id> -- true`",
}

// Failure is the typed error every deployflow entry point returns. It
// renders as "[code] cause — remedy: ...", and unwraps to its cause so
// errors.Is/As on the underlying error keeps working.
type Failure struct {
	Code Code
	Err  error

	// rendered marks an Err whose message is already the finished
	// "[code] … — remedy: …" text (see Redacted): re-rendering it would
	// duplicate the code and the remedy.
	rendered bool
}

func (f *Failure) Error() string {
	if f.rendered {
		return f.Err.Error()
	}
	remedy, ok := remedies[f.Code]
	if !ok || remedy == "" {
		return fmt.Sprintf("[%s] %v", f.Code, f.Err)
	}
	return fmt.Sprintf("[%s] %v — remedy: %s", f.Code, f.Err, remedy)
}

// Redacted rebuilds an error from an already-rendered (and scrubbed)
// message while preserving its taxonomy code. Deploy renders and
// redacts its error text at the package boundary; without this the
// scrubbed error would lose its code and every caller would be back to
// substring-matching prose.
//
// msg MUST already be the fully-rendered text that code's own Error()
// would have produced (i.e. the output of failf(code, ...).Error(),
// possibly then scrubbed) — Redacted has no way to verify this and sets
// rendered unconditionally, so the returned *Failure's Error() returns
// msg verbatim regardless of code. Passing a mismatched pair (msg
// rendered from a DIFFERENT code, or hand-written prose) silently
// produces a *Failure whose .Code a caller can match on while the
// printed text says something else entirely — a footgun for whoever
// reads the mismatch later, since nothing here catches the disagreement.
func Redacted(code Code, msg string) error {
	if code == "" {
		return errors.New(msg)
	}
	return &Failure{Code: code, Err: errors.New(msg), rendered: true}
}

func (f *Failure) Unwrap() error { return f.Err }

// failf builds a *Failure whose cause is fmt.Errorf(format, a...), so
// call sites keep using %w to chain the underlying error.
func failf(code Code, format string, a ...any) error {
	return &Failure{Code: code, Err: fmt.Errorf(format, a...)}
}

// CodeOf reports the taxonomy code carried by err (searching wrapped
// errors), or "" when err did not originate here. Tests and the runbook
// use it; a protocol-v2 desktop could too.
func CodeOf(err error) Code {
	var f *Failure
	if errors.As(err, &f) {
		return f.Code
	}
	return ""
}
