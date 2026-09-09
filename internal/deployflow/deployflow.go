// Package deployflow orchestrates docs/PLAN.md §4.4's 11-step deploy flow:
// validate, preflight, reuse-or-create, PAT reset, install+verify runtime,
// provision the nest, hand over secrets, kill-then-launch, verify, and
// finally relax the autostop policy — with the §4.3 failure-teardown
// wrapper around every in-sandbox step. This is the internal/provider.DeployFunc
// implementation wired into main.go.
package deployflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/httpmcpbin"
	"github.com/IceRhymers/buzz-lakebox/internal/identity"
	"github.com/IceRhymers/buzz-lakebox/internal/install"
	"github.com/IceRhymers/buzz-lakebox/internal/lakebox"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/muxbin"
	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/redact"
	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
	"github.com/IceRhymers/buzz-lakebox/internal/sshx"
	"github.com/IceRhymers/buzz-lakebox/internal/state"
	"github.com/IceRhymers/buzz-lakebox/internal/version"
)

const (
	// deployTimeout bounds the whole flow well inside buzz's 600s
	// invocation budget (docs/CONTRACT.md §1).
	deployTimeout = 550 * time.Second

	// defaultWaitRunningTimeout bounds how long we wait for a
	// created/started sandbox to reach Running (docs/M05_PROBE_RESULTS.md:
	// create->Running ~1.1s, start ~20.5s observed live; generous margin
	// here for slower workspaces).
	defaultWaitRunningTimeout = 90 * time.Second
	defaultPollInterval       = 2 * time.Second

	// defaultVerifyDelay is N=10s (docs/PLAN.md §4.4 step 10,
	// docs/M05_PROBE_RESULTS.md §3: terminal failure lands ~1s after
	// launch, so 10s is ample).
	defaultVerifyDelay = 10 * time.Second

	// installVerifyTimeoutSeconds bounds the ACP initialize handshake
	// (docs/M05_PROBE_RESULTS.md §6: "<1s ... without touching the LLM").
	installVerifyTimeoutSeconds = 10

	// mcpVerifyTimeoutSeconds bounds the deploy-time MCP slot verification
	// (#17): a two-round-trip initialize+tools/list handshake plus child
	// cold-start (N children cold-start concurrently in mux mode). PROVISIONAL
	// at 15s — a touch above the ACP 10s to absorb cold-start — pending the
	// live #14/#16 two-real-servers measurement (plan Driver 3 / the
	// `// measured:` comment in cmd/bzmux/verify.go). A slow server is a
	// budget concern; a broken one fails regardless of budget.
	mcpVerifyTimeoutSeconds = 15

	// Log vocabulary verified live (docs/M05_PROBE_RESULTS.md §3).
	agentPoolReadyMarker = "agent_pool_ready"
	terminalErrorLine    = "initial relay connect failed with terminal error"

	verifyEnvFilePath = "$HOME/.buzz-backend/.env.verify"

	// teardownTimeout bounds teardown's OWN context (BUG 4 fix): teardown
	// must be able to run even when it is triggered by the deploy
	// timeout expiring, so it gets an independent budget rather than
	// inheriting the (possibly already-expired) deploy context.
	teardownTimeout = 60 * time.Second

	// maxErrorLogBytes bounds how much of any remote log/output is ever
	// interpolated into an error string (BUG 5 fix): an unbounded
	// acp.log (or other remote output) must never blow up an error
	// message.
	maxErrorLogBytes = 4096
)

// Deployer implements the deploy flow and satisfies
// internal/provider.DeployFunc via its Deploy method.
type Deployer struct {
	CLI *lakebox.CLI
	SSH *sshx.Client

	// Sleep is used for the step-10 post-launch wait; injectable so
	// tests avoid a real wall-clock wait (PLAN.md §7: no network/real
	// timing in CI).
	Sleep func(time.Duration)

	// VerifyDelay is N in docs/PLAN.md §4.4 step 10 (default 10s).
	VerifyDelay time.Duration

	// NewLaunchID returns the per-deploy identifier stamped into acp.log
	// so step 10 can scope its readiness check to THIS launch. Injectable
	// so tests get deterministic rendered output; nil uses random bytes.
	NewLaunchID func() string

	// WaitRunningTimeout/PollInterval bound polling a sandbox to Running
	// after create/start.
	WaitRunningTimeout time.Duration
	PollInterval       time.Duration

	// DeployTimeout bounds the whole deploy() flow (default
	// deployTimeout); injectable so tests can force the deploy context to
	// expire mid-provision, proving teardown still completes on its own
	// independent budget (BUG 4 regression coverage).
	DeployTimeout time.Duration

	// State is the provider-side npub→sandbox mapping — the ONLY
	// durable reuse key (see internal/state's package doc: the desktop
	// never sends backend_agent_id and Lakebox does not persist
	// caller-set names, so without this every redeploy orphans a
	// still-billing --no-autostop sandbox). nil disables persistence
	// (lookups skipped, nothing recorded); tests point it at a temp
	// file.
	State *state.Store
}

// New returns a Deployer with production defaults.
func New(cli *lakebox.CLI, ssh *sshx.Client) *Deployer {
	return &Deployer{
		CLI:                cli,
		SSH:                ssh,
		Sleep:              time.Sleep,
		VerifyDelay:        defaultVerifyDelay,
		WaitRunningTimeout: defaultWaitRunningTimeout,
		PollInterval:       defaultPollInterval,
		DeployTimeout:      deployTimeout,
		State:              state.NewDefault(),
	}
}

func (d *Deployer) sleep(dur time.Duration) {
	if d.Sleep != nil {
		d.Sleep(dur)
		return
	}
	time.Sleep(dur)
}

// Deploy implements internal/provider.DeployFunc.
func (d *Deployer) Deploy(req *payload.DeployRequest) (string, error) {
	id, err := d.deploy(req)
	if err != nil {
		// Belt-and-suspenders redaction at this package's own boundary
		// (docs/PLAN.md §5): internal/provider also redacts every
		// DeployFunc error, but deployflow must not depend on that when
		// invoked directly (operator `deploy --payload-file`).
		secrets := redact.SecretsFromPayload(req.Agent)
		return "", Redacted(CodeOf(err), redact.Redact(err.Error(), secrets))
	}
	return id, nil
}

func (d *Deployer) deploy(req *payload.DeployRequest) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.deployTimeoutOrDefault())
	defer cancel()

	agent := req.Agent

	// Step 1: validate (defense-in-depth; internal/provider already
	// validates before invoking DeployFunc, but deployflow may also be
	// invoked directly by the operator `deploy --payload-file` path).
	if err := req.Validate(); err != nil {
		return "", failf(CodeValidation, "%w", err)
	}

	profile := req.ProviderConfig.Profile
	if profile == "" {
		profile = version.DefaultProfile
	}

	// Step 2: preflight. CachedVersion (not Version) so the fetch shares
	// lakebox.CLI's sync.Once cache (BUG 9 fix: no duplicate `databricks
	// version` subprocess). The returned version is what wrap() — the
	// single stamping boundary for the §4.3 sandbox-id + CLI-version
	// error annotations — stamps onto every error below.
	version, err := d.CLI.CachedVersion(ctx)
	if err != nil {
		return "", failf(CodeCLIVersionUnknown, "preflight: could not determine databricks CLI version: %w", err)
	}
	meets, err := lakebox.MeetsMinVersion(version)
	if err != nil {
		return "", d.wrap("", version, failf(CodeCLIVersionUnknown, "preflight: %w", err))
	}
	if !meets {
		return "", d.wrap("", version, failf(CodeCLIVersionOld, "preflight: databricks CLI %s is below the minimum supported %s", version, lakebox.MinCLIVersion))
	}
	if _, err := d.CLI.CurrentUser(ctx, profile); err != nil {
		return "", d.wrap("", version, failf(CodeProfileUnresolved, "preflight: profile %q does not resolve: %w", profile, err))
	}
	if err := d.CLI.SandboxRegister(ctx, profile); err != nil {
		return "", d.wrap("", version, failf(CodeSandboxRegister, "preflight: sandbox register: %w", err))
	}

	// Step 3: reuse-or-create, keyed on the agent's npub identity.
	npub, err := identity.NsecToNpub(agent.PrivateKeyNsec)
	if err != nil {
		return "", d.wrap("", version, failf(CodeIdentityDerive, "derive identity from private_key_nsec: %w", err))
	}
	prefix, err := identity.PrefixFor(npub)
	if err != nil {
		return "", d.wrap("", version, failf(CodeIdentityDerive, "derive sandbox name prefix: %w", err))
	}

	var sandboxID string
	var freshlyCreated bool

	// Step 3a: provider-side state lookup — the PRIMARY reuse key. The
	// desktop never echoes backend_agent_id back in deploy payloads, and
	// Lakebox does not persist caller-set sandbox names (live-verified
	// 2026-07-26: list/status return the id as "name"), so the name-prefix
	// match below cannot find a sandbox created by an earlier process.
	// Without this lookup, every redeploy would orphan the previous
	// sandbox — still running, --no-autostop, billing forever. A stale
	// mapping (sandbox deleted out-of-band) fails the status probe and
	// falls through to the legacy match / create path.
	stateKey := state.Key(profile, npub)
	if d.State != nil {
		if entry, ok, serr := d.State.Lookup(stateKey); serr != nil {
			return "", d.wrap("", version, failf(CodeStateRead, "read sandbox state file: %w", serr))
		} else if ok {
			if sb, perr := d.CLI.SandboxStatus(ctx, profile, entry.SandboxID); perr == nil {
				sandboxID = sb.ID
				if !strings.EqualFold(sb.Status, lakebox.StatusRunning) {
					if serr := d.CLI.SandboxStart(ctx, profile, sandboxID); serr != nil {
						return "", d.wrap(sandboxID, version, failf(CodeSandboxStart, "sandbox start: %w", serr))
					}
					if werr := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); werr != nil {
						return "", d.wrap(sandboxID, version, failf(CodeSandboxWait, "waiting for reused sandbox to reach Running: %w", werr))
					}
				}
			}
		}
	}

	if sandboxID == "" {
		var merr error
		sandboxID, freshlyCreated, merr = d.matchOrCreate(ctx, profile, prefix, npub, agent.Name)
		if merr != nil {
			// sandboxID may carry a partial id (e.g. a matched sandbox
			// that failed to start) for wrap's annotation, or be empty.
			return "", d.wrap(sandboxID, version, merr)
		}
	}

	// Persist the mapping BEFORE provisioning: a mapping that cannot be
	// written means every future redeploy leaks a sandbox, so fail fast
	// (the teardown wrapper below cleans up a freshly created sandbox on
	// provisioning failure, and a torn-down id left in the state file is
	// self-healing — the next deploy's status probe rejects it).
	if d.State != nil {
		if rerr := d.State.Record(stateKey, state.Entry{SandboxID: sandboxID, Profile: profile}); rerr != nil {
			if freshlyCreated {
				d.teardown(profile, sandboxID, freshlyCreated)
			}
			return "", d.wrap(sandboxID, version, failf(CodeStateWrite, "persist sandbox mapping (required to reuse this sandbox on redeploy): %w", rerr))
		}
	}

	// Steps 4-11: provisioning, wrapped by failure teardown. A
	// *postVerifyFailure means launch verification already succeeded
	// (BUG 6 fix): the agent is healthy, so destructive teardown must be
	// skipped entirely — only a genuinely unhealthy/unverified deploy
	// gets torn down. A *preMutationFailure (R2: the zero-token auth
	// probe) means nothing has been written to the sandbox THIS
	// invocation: a reused sandbox is left completely alone (it may carry
	// a healthy agent from a prior deploy), while a freshly created one is
	// still deleted — via a scoped delete, not full teardown, since there
	// is nothing to shred.
	if err := d.provision(ctx, profile, sandboxID, freshlyCreated, req); err != nil {
		var pvf *postVerifyFailure
		var pmf *preMutationFailure
		switch {
		case errors.As(err, &pvf):
			// Healthy agent; never tear down.
		case errors.As(err, &pmf):
			if freshlyCreated {
				d.deleteFreshSandbox(profile, sandboxID)
			}
		default:
			d.teardown(profile, sandboxID, freshlyCreated)
		}
		return "", d.wrap(sandboxID, version, err)
	}

	return sandboxID, nil
}

// matchOrCreate is the legacy step-3 path when the state file has no
// usable mapping: match on the npub-derived name prefix (kept for
// sandboxes created before the naming regression and for services that
// persist names again), else create fresh.
func (d *Deployer) matchOrCreate(ctx context.Context, profile, prefix, npub, agentName string) (string, bool, error) {
	sandboxes, _, err := d.CLI.SandboxList(ctx, profile)
	if err != nil {
		return "", false, failf(CodeSandboxList, "sandbox list: %w", err)
	}

	var matches []lakebox.Sandbox
	for _, sb := range sandboxes {
		if strings.HasPrefix(sb.Name, prefix) {
			matches = append(matches, sb)
		}
	}

	var sandboxID string
	var freshlyCreated bool

	switch len(matches) {
	case 0:
		name, nerr := identity.SandboxName(npub, agentName)
		if nerr != nil {
			return "", false, failf(CodeIdentityDerive, "derive sandbox name: %w", nerr)
		}
		sb, cerr := d.CLI.SandboxCreate(ctx, profile, name)
		if cerr != nil {
			return "", false, failf(CodeSandboxCreate, "sandbox create: %w", cerr)
		}
		sandboxID = sb.ID
		freshlyCreated = true
		if !strings.EqualFold(sb.Status, lakebox.StatusRunning) {
			if werr := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); werr != nil {
				d.teardown(profile, sandboxID, freshlyCreated)
				return "", false, failf(CodeSandboxWait, "waiting for freshly created sandbox to reach Running: %w", werr)
			}
		}
	case 1:
		sandboxID = matches[0].ID
		if !strings.EqualFold(matches[0].Status, lakebox.StatusRunning) {
			if serr := d.CLI.SandboxStart(ctx, profile, sandboxID); serr != nil {
				return sandboxID, false, failf(CodeSandboxStart, "sandbox start: %w", serr)
			}
			if werr := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); werr != nil {
				return sandboxID, false, failf(CodeSandboxWait, "waiting for reused sandbox to reach Running: %w", werr)
			}
		}
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return "", false, failf(CodeIdentityAmbiguous,
			"ambiguous identity: %d sandboxes match prefix %q (%s); refusing to guess",
			len(matches), prefix, strings.Join(ids, ", "),
		)
	}

	return sandboxID, freshlyCreated, nil
}

func (d *Deployer) deployTimeoutOrDefault() time.Duration {
	if d.DeployTimeout > 0 {
		return d.DeployTimeout
	}
	return deployTimeout
}

func (d *Deployer) waitTimeout() time.Duration {
	if d.WaitRunningTimeout > 0 {
		return d.WaitRunningTimeout
	}
	return defaultWaitRunningTimeout
}

func (d *Deployer) pollInterval() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return defaultPollInterval
}

func (d *Deployer) verifyDelay() time.Duration {
	if d.VerifyDelay > 0 {
		return d.VerifyDelay
	}
	return defaultVerifyDelay
}

// wrap embeds the sandbox id and CLI version (each when known) in err,
// per docs/PLAN.md §4.3: "Every {ok:false} error embeds the sandbox id
// (when one exists) and the recorded CLI version." This is the SINGLE
// stamping boundary for BOTH annotations (review round 2, refining
// BUG 9): lakebox.wrapErr no longer stamps a version, so every error
// leaving deploy() — lakebox-originated or not (sshx install/verify
// failures, identity errors, ambiguous-identity, semver parse) — gets
// exactly one "(databricks cli X)" here, from the version CachedVersion
// already fetched at preflight (no extra subprocess). Failures before
// preflight fetched a version stamp what exists: empty → omitted.
func (d *Deployer) wrap(sandboxID, version string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case sandboxID == "" && version == "":
		return err
	case sandboxID == "":
		return fmt.Errorf("%w (databricks cli %s)", err, version)
	case version == "":
		return fmt.Errorf("%w (sandbox %s)", err, sandboxID)
	default:
		return fmt.Errorf("%w (sandbox %s, databricks cli %s)", err, sandboxID, version)
	}
}

// postVerifyFailure marks an error that occurred AFTER launch
// verification already succeeded (today, only setAutostopPolicy) — BUG 6
// fix. deploy() must not run destructive teardown for these: the agent
// is healthy, and docs/PLAN.md §4.3's teardown intent ("never strand a
// secret-bearing env file or a runaway sandbox") is about UNHEALTHY
// deploys, not a healthy one whose autostop policy merely failed to
// apply. Pre-verify failures are unaffected and keep exact prior
// behavior.
type postVerifyFailure struct {
	err error
}

func (e *postVerifyFailure) Error() string { return e.err.Error() }
func (e *postVerifyFailure) Unwrap() error { return e.err }

// preMutationFailure marks an error that occurred BEFORE any in-sandbox
// mutation this invocation performed (today, only the zero-token auth
// probe — R2 of the zero-token design) — mirrors postVerifyFailure
// above, at the opposite end of provisioning. The probe only reads: it
// greps for the PAT-stub marker, sources a mktemp-materialized copy of
// nest.SandboxAuthSnippet, and runs `databricks current-user me`; it
// never writes to nest.EnvFilePath or anywhere else in the sandbox
// (Critic implementation note 1). deploy() must therefore never run
// destructive teardown against a REUSED sandbox for this failure: the
// sandbox may carry a perfectly healthy env-mode agent from a previous
// deploy, and teardown's shred+pkill would destroy its env file and kill
// it over a merely-failed env→sandbox switch — violating teardown's own
// "written this invocation" contract (see teardown's doc comment below).
// A FRESHLY CREATED sandbox is still deleted (via deleteFreshSandbox,
// never shredded — nothing was written to it): it has zero value with a
// broken baked credential and would otherwise bill forever.
type preMutationFailure struct {
	err error
}

func (e *preMutationFailure) Error() string { return e.err.Error() }
func (e *preMutationFailure) Unwrap() error { return e.err }

// provision runs docs/PLAN.md §4.4 steps 4-11 against an established
// (Running) sandbox.
func (d *Deployer) provision(ctx context.Context, profile, sandboxID string, freshlyCreated bool, req *payload.DeployRequest) error {
	agent := req.Agent
	sandboxAuth := req.ProviderConfig.SandboxInferenceAuth()
	// Resolved once and threaded through the whole flow. Validate() has
	// already rejected an unknown agent_command, so the lookup cannot fail
	// here; default to buzz-agent rather than panicking if a future caller
	// reaches provision() without validating.
	rt := runtimeOf(agent)
	// Resolve the single BUZZ_ACP_MCP_COMMAND value from
	// provider_config.mcp_servers (#16) before rendering the env. McpNone
	// leaves the runtime's own default in place (byte-identical to pre-#16);
	// McpDirect points it straight at the one entry (introducing it for
	// claude — #14); McpMux points it at "bzmux" (the multiplexer). Increment 2
	// completes this path: installMux (called from installAndVerify) writes the
	// bzmux binary + mcp-mux.json config + runs a deploy-time self-test.
	mcpCommand, err := resolveMcpCommand(req.ProviderConfig)
	if err != nil {
		return err
	}
	envContent := nest.RenderEnv(agent, rt, sandboxAuth, mcpCommand)

	// Step 4: either the PAT reset (default/env mode; the first in-sandbox
	// action of every deploy, unless the owner opted out) or, in zero-token
	// sandbox mode, the auth probe in its place — sandbox mode needs the
	// sandbox's baked creator-identity ~/.databrickscfg intact, so the PAT
	// reset must never run at all for it (inference_auth:"sandbox"
	// supersedes keep_workspace_pat the same way RenderLaunchScript's stub
	// skip does).
	switch {
	case sandboxAuth:
		if err := d.authProbe(ctx, profile, sandboxID); err != nil {
			return err
		}
	case !req.ProviderConfig.KeepWorkspacePAT:
		if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
			step("pat-reset", "set -eu; umask 077; cat > \"$HOME/.databrickscfg\""),
			strings.NewReader(nest.PATStub),
		); err != nil {
			return failf(CodePATReset, "PAT reset: %w", err)
		}
	}

	// Step 5: install pinned .deb + sha256 + runtime verification.
	// install-write (below) already `mkdir -p "$HOME/.buzz-backend"`, and
	// launch.sh (step 9) `mkdir -p`s the full nest working-dir set, so the
	// former standalone "nest working dirs" SSH round trip (step 6) was a
	// redundant extra round trip and has been removed (C5 cleanup).
	if err := d.installAndVerify(ctx, profile, sandboxID, rt, req.ProviderConfig, envContent); err != nil {
		return err
	}

	// Step 7: secret handover — env file via stdin only, 0600 at rest.
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("env-write", fmt.Sprintf(`set -eu; umask 077; cat > %s && chmod 600 %s`, dquote(nest.EnvFilePath), dquote(nest.EnvFilePath))),
		strings.NewReader(envContent),
	); err != nil {
		return failf(CodeEnvWrite, "write env file: %w", err)
	}

	// Step 8: update-in-place guard — kill any existing buzz-acp process
	// group before relaunching (tolerate no-match). Pattern is
	// '[b]uzz-acp', NOT 'buzz-acp' (BUG 2 fix): the literal pattern
	// 'buzz-acp' also appears in this pkill invocation's OWN argv (as
	// seen by the remote shell), so pkill -f would SIGTERM its own
	// invoking shell and abort the happy path with a non-zero exit. The
	// bracket idiom's regex still matches the real "buzz-acp" process
	// name but the argv text "[b]uzz-acp" does not match itself.
	//
	// The kill is followed by a bounded WAIT for the process to actually
	// go away, because SIGTERM starts a graceful drain rather than an
	// immediate exit: buzz-acp shuts down each pooled agent in turn,
	// waiting on every child. Returning before that finishes used to lose
	// a race with step 9 — launch.sh exits early when a buzz-acp is still
	// alive ("already running; not relaunching"), and acp.log is appended
	// to rather than truncated, so step 10 would then read the PREVIOUS
	// deploy's readiness line and pass. The result was a deploy reporting
	// ok:true while the old runtime kept serving with the old environment
	// — worst on exactly the redeploy that switches runtime or rotates a
	// credential, which is the case that most needs to take effect.
	out, err := d.SSH.Run(ctx, profile, sandboxID, step("prelaunch-kill", prelaunchKillScript()))
	if err != nil {
		return failf(CodePrelaunchKill, "kill existing buzz-acp process group: %w", err)
	}
	if strings.Contains(out, prelaunchStillAliveMarker) {
		return failf(CodeStaleAgent,
			"a previous buzz-acp did not exit within %ds of SIGTERM; refusing to launch over it: %s",
			prelaunchKillWaitSeconds, remoteText(strings.TrimSpace(out)))
	}

	// Step 9: write + run launch.sh, stamped with a per-deploy launch id so
	// step 10 can tell this launch's readiness line from a previous one's.
	launchID := d.newLaunchID()
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("launch-write", fmt.Sprintf(`set -eu; umask 077; cat > %s && chmod 700 %s`, dquote(nest.LaunchScriptPath), dquote(nest.LaunchScriptPath))),
		strings.NewReader(nest.RenderLaunchScript(req.ProviderConfig.KeepWorkspacePAT, sandboxAuth, launchID, len(req.ProviderConfig.ExtraBinaries) > 0)),
	); err != nil {
		return failf(CodeLaunchWrite, "write launch.sh: %w", err)
	}
	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("launch-exec", fmt.Sprintf(`sh %s`, dquote(nest.LaunchScriptPath)))); err != nil {
		return failf(CodeLaunchExec, "run launch.sh: %w", err)
	}

	// Step 10: verify after N=VerifyDelay, then (only on success) set
	// the autostop policy.
	if err := d.verifyLaunch(ctx, profile, sandboxID, launchID); err != nil {
		return err
	}

	// Step 11: autostop policy, set LAST. A failure here is a
	// postVerifyFailure (BUG 6): launch already verified healthy, so the
	// caller must not tear anything down for this specific failure. The
	// message deliberately does NOT interpolate the sandbox id itself:
	// deploy()'s wrap() appends "(sandbox <id>, databricks cli <v>)" to
	// every error, and embedding the id here too duplicated it (review
	// round 2) — the remedy names a <sandbox-id> placeholder that wrap's
	// annotation resolves.
	if err := d.setAutostopPolicy(ctx, profile, sandboxID, req.ProviderConfig); err != nil {
		return &postVerifyFailure{err: failf(CodeAutostopConfig,
			"the agent launched and verified successfully, but the autostop policy could not be set: %w; the sandbox remains on the default 10-minute idle autostop (the sandbox id is in this error's trailing annotation)",
			err,
		)}
	}

	return nil
}

// resolveMcpCommand maps provider_config.mcp_servers to the single
// BUZZ_ACP_MCP_COMMAND value nest.RenderEnv should emit, or "" to leave the
// runtime's own default in place (McpNone). See payload.ProviderConfig.McpMode.
//
// The fail-loud guard on McpDirect is deliberate. buzz-agent spawns its MCP
// child on session/new, NOT on the initialize verify handshake (Spike 1), so
// an empty-but-set command would NOT be caught by deploy-time verification —
// it would surface only at the first live mention. A validated single
// mcp_servers entry is always non-empty (validateMcpServers rejects ""), so
// this is defense-in-depth for a caller that reaches provision() without
// validating (the same posture runtimeOf's default takes); it fails at deploy
// time with a clear message rather than shipping an agent that silently
// renders an empty BUZZ_ACP_MCP_COMMAND and can never answer.
//
// McpMux (2+ entries): Increment 2 delivers the bzmux binary and its
// mcp-mux.json config via installMux (called from installAndVerify). This
// function returns payload.MuxBinaryName ("bzmux") for this mode so
// BUZZ_ACP_MCP_COMMAND points at the multiplexer after installMux has placed
// it on PATH. The deploy-time self-test (mux-selftest) ensures bzmux boots
// before the agent is launched — preventing the live-bitten failure class
// where a deploy passes ACP verification but the agent is permanently tool-less.
func resolveMcpCommand(cfg payload.ProviderConfig) (string, error) {
	switch cfg.McpMode() {
	case payload.McpDirect:
		cmd := cfg.McpDirectCommand()
		if cmd == "" {
			return "", failf(CodeValidation,
				"provider_config.mcp_servers has a single entry but it resolved to an empty MCP command; a single-entry mcp_servers must name a non-empty command")
		}
		return cmd, nil
	case payload.McpMux:
		// Increment 2: return the multiplexer's bare name. MuxBinDir equals
		// the .deb BinDir ($HOME/.buzz-backend/bin) which is on the sandbox
		// PATH, so "bzmux" resolves without an absolute path. installMux
		// writes the binary and config before this command is ever used.
		return payload.MuxBinaryName, nil
	default: // payload.McpNone
		return "", nil
	}
}

// authProbeCauseMarkerPrefix is the line authProbeScript echoes to stdout
// right before exiting non-zero, so the Go side can tell apart the three
// causes a zero-token auth probe can fail for (docs/PLAN.md zero-token
// design). Kept distinct from parsePgrepCheck's BUZZ_PGREP_RC marker
// (different probe, different vocabulary).
const authProbeCauseMarkerPrefix = "BUZZ_PROBE_CAUSE="

// authProbe implements the zero-token deploy-time auth probe (R1): it
// exercises the SAME derivation snippet the launch env will use, then
// validates the derived credential — never the CLI's own independent
// ~/.databrickscfg reading — with `databricks current-user me`. It is a
// preMutationFailure on any failure (R2): the probe never writes to
// nest.EnvFilePath or anywhere else in the sandbox (Critic note 1), only
// ever reading and running a mktemp-materialized copy of its own stdin.
//
// The SSH round trip shares provision()'s ctx, which is deploy()'s own
// ctx bounded by deployTimeoutOrDefault() (Critic note 5) — identical to
// every neighboring in-sandbox step in this flow; there is no separate,
// tighter timeout to add here.
func (d *Deployer) authProbe(ctx context.Context, profile, sandboxID string) error {
	out, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("auth-probe", authProbeScript()),
		strings.NewReader(nest.SandboxAuthSnippet),
	)
	if err == nil {
		return nil
	}
	cause := authProbeCause(out)
	msg := authProbeCauseMessage(cause)
	return &preMutationFailure{err: failf(CodeSandboxAuth,
		"zero-token auth probe failed: %s (probe output: %s)",
		msg, remoteText(strings.TrimSpace(out)),
	)}
}

// authProbeScript is the static (no payload interpolation beyond the
// trusted nest.PATStubMarker constant) remote script the auth probe ships
// over RunWithStdin's stdin-as-cmd-text channel — the snippet itself
// travels as this call's actual stdin, per authProbe above. In order:
//
//  1. grep the provider's own stub marker in ~/.databrickscfg — present
//     means this sandbox was already deployed in env mode and its baked
//     PAT is gone (cause "stub");
//  2. else materialize the piped-in snippet to a mktemp file, source it,
//     then rm it — NEVER nest.EnvFilePath (Critic note 1) — and require
//     both DATABRICKS_HOST and DATABRICKS_TOKEN to come out non-empty
//     (cause "parse" otherwise): this is the exact parser launch.sh will
//     use, so a parse failure here is a parse failure there too;
//  3. else run `databricks current-user me` with those derived values
//     already exported into this very shell's environment — the CLI
//     prefers env over cfg, so this validates the DERIVED credential
//     itself, not the CLI's own independent cfg parse (cause "credential"
//     otherwise).
//
// Each failure branch echoes a BUZZ_PROBE_CAUSE=<cause> marker line
// before exiting non-zero, so authProbeCause can disambiguate them
// Go-side.
func authProbeScript() string {
	return fmt.Sprintf(`set -eu
if grep -qF %s "$HOME/.databrickscfg" 2>/dev/null; then
  echo "%sstub"
  exit 1
fi
BUZZ_PROBE_TMP=$(mktemp)
cat > "$BUZZ_PROBE_TMP"
# shellcheck disable=SC1090
. "$BUZZ_PROBE_TMP"
rm -f "$BUZZ_PROBE_TMP"
if [ -z "${DATABRICKS_HOST:-}" ] || [ -z "${DATABRICKS_TOKEN:-}" ]; then
  echo "%sparse"
  exit 1
fi
if ! databricks current-user me >/dev/null 2>&1; then
  echo "%scredential"
  exit 1
fi
`, shellquote.Single(nest.PATStubMarker), authProbeCauseMarkerPrefix, authProbeCauseMarkerPrefix, authProbeCauseMarkerPrefix)
}

// authProbeCause scans out (as authProbe received it — stdout captured
// regardless of the remote command's exit code) for the FIRST
// BUZZ_PROBE_CAUSE=<cause> line and returns <cause>, or "" when none is
// found (e.g. the ssh transport itself failed before the script ever
// ran, rather than the script's own logic failing).
func authProbeCause(out string) string {
	return probeCause(out, authProbeCauseMarkerPrefix)
}

// probeCause scans out for the FIRST <prefix><cause> line and returns
// <cause>, or "" when none is found. Shared by every in-sandbox probe that
// disambiguates its own failure modes by echoing a marker before exiting
// non-zero, so they all parse their causes the same way.
func probeCause(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return strings.TrimPrefix(trimmed, prefix)
		}
	}
	return ""
}

// authProbeCauseMessage renders a human diagnosis for one of the probe's
// three disambiguated causes (docs/PLAN.md zero-token design; Critic
// note 2).
//
// It used to take the agent's env_vars and, for cause "stub", add a note
// that explicit env_vars credentials would take precedence over derivation
// anyway. That branch is gone because the combination it described can no
// longer reach this code: payload.validateOwnerPATEnvVars now rejects
// env_vars.DATABRICKS_HOST/DATABRICKS_TOKEN outright under
// inference_auth="sandbox", since supplying one of the pair let a payload
// couple its own endpoint to the sandbox's owner-level token. A diagnosis
// for an unreachable state is worse than none — it would tell an operator
// that a rejected configuration was merely redundant.
func authProbeCauseMessage(cause string) string {
	switch cause {
	case "stub":
		return "sandbox was previously deployed in env mode; its baked PAT is unrestorable — delete the sandbox and redeploy fresh in sandbox mode"
	case "parse":
		return "~/.databrickscfg missing or unparseable by the derivation snippet"
	case "credential":
		// Hedged (Critic note 2): `databricks current-user me` can also
		// fail on a transient network/gateway error, not only an
		// invalid/expired PAT; the CLI's own error text reaches the
		// operator via the probe output appended by authProbe.
		return "derived credential rejected: the baked PAT is invalid, expired, or the workspace was unreachable"
	default:
		return "auth probe failed before it could report which of its three causes applied"
	}
}

// deleteFreshSandbox implements the freshly-created half of R2's
// pre-mutation failure semantics: when the auth probe fails against a
// sandbox created THIS invocation, nothing has been written to it (the
// probe never touches nest.EnvFilePath — see preMutationFailure's doc
// comment), so there is nothing to shred; only the sandbox itself, which
// has zero value with a broken baked credential and would otherwise bill
// forever, needs reclaiming. Deliberately narrower than teardown: no
// shred step, no pkill branch — see teardown's own doc comment for why
// running its "written this invocation" shred here would be wrong.
//
// Uses its own independent context, for the same BUG-4 reason teardown
// does (a deploy-deadline-expired ctx must not silently no-op cleanup).
func (d *Deployer) deleteFreshSandbox(profile, sandboxID string) {
	if sandboxID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	_ = d.CLI.SandboxDelete(ctx, profile, sandboxID)
}

// installAndVerify runs docs/PLAN.md §4.4 step 5: fetch/verify/extract the
// pinned .deb (skipped in-script when the version marker already
// matches), then the ACP initialize runtime-verification handshake
// (docs/M05_PROBE_RESULTS.md §6) as a SINGLE combined round trip (BUG 1 +
// efficiency fix): the agent env content travels over this one
// RunWithStdin call's stdin, is written to a temp file, sourced, and used
// to run the handshake, all inside install.BuildVerifyCommand's script,
// which also removes the temp file on exit via trap. This replaces what
// were previously two round trips (write the temp env file, then a
// separate exec that sourced it) — the old split was also the
// deploy-breaking bug: the write step double-quoted the path (expanding
// $HOME) while the exec step single-quoted it (not expanding), so
// sourcing always failed under `set -eu` and the exec's own trap never
// removed the real file either.
// The .deb install is unconditional for EVERY runtime: buzz-acp itself
// ships in it, and launch.sh runs it by absolute path. Runtimes that also
// need an ACP adapter (currently only Claude) get an additional pair of
// round trips after it, and a gateway reachability probe after the
// handshake.
func (d *Deployer) installAndVerify(ctx context.Context, profile, sandboxID string, rt payload.Runtime, cfg payload.ProviderConfig, envContent string) error {
	script, err := install.BuildInstallScript(cfg.BuzzVersion)
	if err != nil {
		return failf(CodeInstallScript, "install: %w", err)
	}

	const installScriptPath = "$HOME/.buzz-backend/install.sh"
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("install-write", fmt.Sprintf(`set -eu; umask 077; mkdir -p "$HOME/.buzz-backend"; cat > %s`, dquote(installScriptPath))),
		strings.NewReader(script),
	); err != nil {
		return failf(CodeInstallWrite, "install: write install script: %w", err)
	}
	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("install-exec", fmt.Sprintf(`sh %s`, dquote(installScriptPath)))); err != nil {
		return failf(CodeInstallExec, "install: %w", err)
	}

	if err := d.installBuzzSkill(ctx, profile, sandboxID); err != nil {
		return err
	}
	if cfg.HasSkills() {
		if err := d.installDatabricksSkills(ctx, profile, sandboxID, cfg); err != nil {
			return err
		}
	}

	if spec, ok := install.AdapterSpecFor(rt.SpawnCommand()); ok {
		if err := d.installACPAdapter(ctx, profile, sandboxID, spec, adapterVersionFor(rt, cfg)); err != nil {
			return err
		}
	}

	// Extra binaries (#15): fetch/verify/place the operator's pinned single-file
	// executables into the appended extra-bin PATH dir. Sequenced AFTER the
	// adapter and BEFORE runtime verification, and ONLY when present — a deploy
	// with no extra_binaries issues no round trip at all (regression-safe).
	if len(cfg.ExtraBinaries) > 0 {
		if err := d.installExtraBinaries(ctx, profile, sandboxID, cfg.ExtraBinaries); err != nil {
			return err
		}
	}

	// MCP multiplexer (#16 Increment 2): write the embedded bzmux binary and
	// its mcp-mux.json config, then run a deploy-time self-test. Only on the
	// McpMux (≥2 entries) branch — never perturbs McpNone/McpDirect paths
	// (byte-identical guard locked by TestDeploy_McpMux_AbsentFor*).
	if cfg.McpMode() == payload.McpMux {
		if err := d.installMux(ctx, profile, sandboxID, cfg, envContent); err != nil {
			return err
		}
	}

	// MCP direct verification (#17): for a single-entry mcp_servers (McpDirect),
	// write the embedded bzmux and run a deploy-time initialize+tools/list check
	// against the resolved command. Sequenced beside the other capability
	// installs, BEFORE the ACP verify-exec. Never perturbs McpNone (byte-
	// identical) or McpMux (which verifies via installMux's mux-mode step).
	if cfg.McpMode() == payload.McpDirect {
		if err := d.installMcpDirect(ctx, profile, sandboxID, cfg.McpDirectCommand(), envContent); err != nil {
			return err
		}
	}

	// Runtime verification: ACP initialize handshake with the agent env
	// sourced (docs/M05_PROBE_RESULTS.md §6), env content shipped via
	// stdin only. The binary differs per runtime; the frame and the
	// success marker do not.
	spec := install.VerifySpecFor(rt.SpawnCommand())
	verifyCmd, err := install.BuildVerifyCommand(verifyEnvFilePath, installVerifyTimeoutSeconds, spec)
	if err != nil {
		return failf(CodeRuntimeVerify, "install verification: %w", err)
	}
	out, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("verify-exec", verifyCmd),
		strings.NewReader(envContent),
	)
	if err != nil {
		return failf(CodeRuntimeVerify, "install verification: %s ACP initialize handshake failed: %w", rt.SpawnCommand(), err)
	}
	if !strings.Contains(out, install.AgentInfoMarker) {
		return failf(CodeRuntimeVerify, "install verification: %s ACP initialize response did not contain %q: %s", rt.SpawnCommand(), install.AgentInfoMarker, remoteText(strings.TrimSpace(out)))
	}

	switch rt {
	case payload.RuntimeClaude:
		if err := d.claudeInferenceProbe(ctx, profile, sandboxID, envContent); err != nil {
			return err
		}
	case payload.RuntimeCodex:
		if err := d.codexInferenceProbe(ctx, profile, sandboxID, envContent); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) installBuzzSkill(ctx context.Context, profile, sandboxID string) error {
	write := fmt.Sprintf(`set -eu
umask 077
skill=%s
version=%s
mkdir -p "$(dirname "$skill")"
tmp="${skill}.tmp.$$"
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
chmod 600 "$tmp"
mv -f "$tmp" "$skill"
printf '%%s' %s > "$version"
chmod 600 "$version"
trap - EXIT
%s`, dquote(nest.BuzzSkillPath), dquote(nest.BuzzSkillVersionPath), shellquote.Single(nest.BuzzSkillVersion), nest.BuzzSkillLinkScript)
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("buzz-skill-write", write),
		bytes.NewReader(nest.BuzzSkillMD),
	); err != nil {
		return failf(CodeBuzzSkill, "Buzz CLI skill install: %w", err)
	}
	return nil
}

func (d *Deployer) installDatabricksSkills(ctx context.Context, profile, sandboxID string, cfg payload.ProviderConfig) error {
	script, err := install.BuildSkillsInstallScript(cfg.Skills)
	if err != nil {
		return failf(CodeSkillsConfig, "Databricks skills: %w", err)
	}
	const scriptPath = "$HOME/.buzz-backend/install-skills.sh"
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("skills-write", fmt.Sprintf(`set -eu; umask 077; cat > %s && chmod 700 %s`, dquote(scriptPath), dquote(scriptPath))),
		strings.NewReader(script),
	); err != nil {
		return failf(CodeSkillsExec, "Databricks skills: write installer: %w", err)
	}
	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("skills-exec", dquote(scriptPath))); err != nil {
		return failf(CodeSkillsExec, "Databricks skills: %w", err)
	}
	return nil
}

// adapterVersionFor picks the per-runtime adapter version override from
// provider_config. A runtime with no override field yields "", which
// BuildAdapterInstallScript reads as "use the spec's pinned default".
func adapterVersionFor(rt payload.Runtime, cfg payload.ProviderConfig) string {
	switch rt {
	case payload.RuntimeClaude:
		return cfg.ClaudeAdapterVersion
	case payload.RuntimeCodex:
		return cfg.CodexAdapterVersion
	default:
		return ""
	}
}

// installACPAdapter installs the npm ACP adapter a runtime spawns, as its
// own write+exec pair so a failure is attributable to the adapter rather
// than to the .deb.
//
// The script path is shared across runtimes even though each adapter has
// its own npm tree: it is written immediately before it is executed and
// never read again, so it carries no runtime-specific state worth
// partitioning.
func (d *Deployer) installACPAdapter(ctx context.Context, profile, sandboxID string, spec install.AdapterSpec, adapterVersion string) error {
	script, err := install.BuildAdapterInstallScript(spec.BinName, adapterVersion)
	if err != nil {
		return failf(CodeAdapterScript, "%s adapter install: %w", spec.Label, err)
	}
	const adapterScriptPath = "$HOME/.buzz-backend/install-adapter.sh"
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("adapter-write", fmt.Sprintf(`set -eu; umask 077; mkdir -p "$HOME/.buzz-backend"; cat > %s`, dquote(adapterScriptPath))),
		strings.NewReader(script),
	); err != nil {
		return failf(CodeAdapterWrite, "%s adapter install: write install script: %w", spec.Label, err)
	}
	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("adapter-exec", fmt.Sprintf(`sh %s`, dquote(adapterScriptPath)))); err != nil {
		return failf(CodeAdapterExec, "%s adapter install: %w", spec.Label, err)
	}
	return nil
}

// installExtraBinaries installs the operator's pinned extra_binaries (#15) as
// its own write+exec pair, mirroring installACPAdapter so a failure is
// attributable to this step rather than to the .deb or the adapter. The
// payload structs are mapped to install-local ones at the boundary, keeping
// internal/install free of an internal/payload dependency (P2).
func (d *Deployer) installExtraBinaries(ctx context.Context, profile, sandboxID string, ebs []payload.ExtraBinary) error {
	specs := make([]install.ExtraBinaryInstall, len(ebs))
	for i, eb := range ebs {
		specs[i] = install.ExtraBinaryInstall{URL: eb.URL, SHA256: eb.SHA256, Bin: eb.Bin}
	}
	script, err := install.BuildExtraBinariesInstallScript(specs)
	if err != nil {
		return failf(CodeExtraBinScript, "extra binaries install: %w", err)
	}
	const extraBinScriptPath = "$HOME/.buzz-backend/install-extra-bins.sh"
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("extra-bins-write", fmt.Sprintf(`set -eu; umask 077; mkdir -p "$HOME/.buzz-backend"; cat > %s`, dquote(extraBinScriptPath))),
		strings.NewReader(script),
	); err != nil {
		return failf(CodeExtraBinWrite, "extra binaries install: write install script: %w", err)
	}
	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("extra-bins-exec", fmt.Sprintf(`sh %s`, dquote(extraBinScriptPath)))); err != nil {
		return failf(CodeExtraBinExec, "extra binaries install: %w", err)
	}
	return nil
}

// installMux installs the embedded bzmux MCP multiplexer binary and its
// mcp-mux.json configuration into the sandbox (issue #16 Increment 2). It is
// modeled on installExtraBinaries and is called only when
// cfg.McpMode() == payload.McpMux (≥2 mcp_servers entries).
//
// Steps mirror the extrabin idiom (write bytes over stdin, never argv):
//   - mux-bin-write: write muxbin.Binary to install.MuxBinPath, chmod 755.
//   - mux-cfg-write: write BuildMuxConfigJSON output to install.MuxConfigPath, chmod 600.
//   - mux-selftest:  source envContent so BUZZ_* vars reach bzmux's children,
//     export the same PATH launch.sh uses, and run the ABSOLUTE install.MuxBinPath
//     with --selftest — prevents shipping a live-bitten agent (Critic #8).
//
// Per-server env is least-privilege: legacy buzz-dev-mcp receives only its
// Buzz relay identity variables; typed remote bridge children receive only
// DATABRICKS_HOST/DATABRICKS_TOKEN; other local children receive only their
// validated explicit inherit list.
func (d *Deployer) installMux(ctx context.Context, profile, sandboxID string, cfg payload.ProviderConfig, envContent string) error {
	var muxCfg muxcfg.Config
	if cfg.HasManagedMCP() {
		var err error
		muxCfg, err = mcpconfig.ToMuxConfig(cfg.MCP, mcpconfig.Options{
			RemoteCommand: "bzhttpmcp",
			RemoteArgs: func(remote mcpconfig.Remote) ([]string, error) {
				args := []string{"--kind", string(remote.Kind)}
				if remote.Kind == mcpconfig.KindSkills {
					for i := 0; i < len(remote.Components); i += 2 {
						args = append(args, "--schema", remote.Components[i]+"."+remote.Components[i+1])
					}
				} else {
					for _, component := range remote.Components {
						args = append(args, "--resource", component)
					}
				}
				return args, nil
			},
		})
		if err != nil {
			return failf(CodeMuxWrite, "managed MCP: build mux config: %w", err)
		}
		// The bridge deliberately supports env auth only in this release. It
		// receives exactly the host/token pair and no Buzz identity secrets.
		for i := range muxCfg.Servers {
			if cfg.MCP.Servers[i].Kind != mcpconfig.KindLocal {
				muxCfg.Servers[i].Env.Inherit = []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN"}
			}
		}
	} else {
		servers := make([]muxcfg.Server, len(cfg.McpServers))
		for i, name := range cfg.McpServers {
			srv := muxcfg.Server{Name: name, Command: name}
			if name == "buzz-dev-mcp" {
				srv.Env.Inherit = []string{"BUZZ_PRIVATE_KEY", "BUZZ_AUTH_TAG", "BUZZ_RELAY_URL", "NOSTR_PRIVATE_KEY"}
			}
			servers[i] = srv
		}
		muxCfg = muxcfg.Config{Servers: servers}
	}

	cfgBytes, err := install.BuildMuxConfigJSON(muxCfg)
	if err != nil {
		return failf(CodeMuxWrite, "mcp multiplexer: build config: %w", err)
	}

	// Step mux-bin-write: write the embedded bzmux binary over stdin and
	// chmod 755 in the same command, so binary bytes never appear in argv.
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("mux-bin-write", fmt.Sprintf(
			`set -eu; umask 077; mkdir -p %s; cat > %s && chmod 755 %s`,
			dquote(install.MuxBinDir), dquote(install.MuxBinPath), dquote(install.MuxBinPath),
		)),
		bytes.NewReader(muxbin.Binary),
	); err != nil {
		return failf(CodeMuxWrite, "mcp multiplexer: write binary: %w", err)
	}

	if cfg.HasManagedMCP() {
		if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
			step("http-mcp-bin-write", fmt.Sprintf(
				`set -eu; umask 077; mkdir -p %s; cat > %s && chmod 755 %s`,
				dquote(install.MuxBinDir), dquote(install.HTTPMCPBinPath), dquote(install.HTTPMCPBinPath),
			)),
			bytes.NewReader(httpmcpbin.Binary),
		); err != nil {
			return failf(CodeMuxWrite, "managed MCP: write Streamable HTTP bridge: %w", err)
		}
	}

	// Step mux-cfg-write: write the JSON config over stdin and chmod 600
	// (mirrors the env-file write pattern — secrets in Set values never
	// reach argv or logs). set -eu + umask 077 prevent the file from being
	// briefly world-readable before chmod 600 applies.
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("mux-cfg-write", fmt.Sprintf(
			`set -eu; umask 077; cat > %s && chmod 600 %s`,
			dquote(install.MuxConfigPath), dquote(install.MuxConfigPath),
		)),
		bytes.NewReader(cfgBytes),
	); err != nil {
		return failf(CodeMuxWrite, "mcp multiplexer: write config: %w", err)
	}

	// Step mcp-verify (#17, mux mode): replaces the former mux-selftest step.
	// It reuses the exact env-sourcing idiom (transient verifyEnvFilePath,
	// trap-rm on EXIT, `set -a` source so BUZZ_* relay secrets reach bzmux's
	// children, launch.sh PATH export, ABSOLUTE MuxBinPath) but runs
	// `bzmux --verify-mcp` (no --command) instead of `--selftest`. mux-mode
	// verify is a SUPERSET of the selftest — same collision/`__`/budget +
	// protocolVersion checks — PLUS the expected-names union of the known
	// children, so it catches the initialized-but-under-exports / wrong-server
	// case the selftest misses. env travels over stdin only; no secret is
	// interpolated into the command string, and a failed check leaves no
	// secret env file behind.
	if err := d.mcpVerify(ctx, profile, sandboxID, install.McpVerifyModeMux, "", envContent); err != nil {
		return err
	}

	return nil
}

// mcpVerify runs the deploy-time MCP slot verification step (#17): it ships
// envContent over stdin to the BuildMcpVerifyCommand script (which sources the
// env into the transient verifyEnvFilePath, exports the launch.sh PATH, and
// runs the ABSOLUTE bzmux --verify-mcp under a timeout), and maps any failure
// to CodeMcpVerify. mode is install.McpVerifyModeMux (no slotCommand) or
// install.McpVerifyModeDirect (slotCommand = the resolved single command).
func (d *Deployer) mcpVerify(ctx context.Context, profile, sandboxID, mode, slotCommand, envContent string) error {
	verifyCmd, err := install.BuildMcpVerifyCommand(verifyEnvFilePath, mcpVerifyTimeoutSeconds, install.MuxBinPath, mode, slotCommand)
	if err != nil {
		return failf(CodeMcpVerify, "mcp verify: %w", err)
	}
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("mcp-verify", verifyCmd),
		strings.NewReader(envContent),
	); err != nil {
		return failf(CodeMcpVerify,
			"mcp verify: the MCP server the agent will use failed a deploy-time initialize+tools/list check: %w",
			err)
	}
	return nil
}

// installMcpDirect implements the McpDirect half of #17: it writes the embedded
// bzmux binary (mcp-bin-write, mirroring mux-bin-write — bzmux is inert unless
// invoked, since BUZZ_ACP_MCP_COMMAND still points at the direct command) and
// then runs the mcp-verify step in direct mode against the resolved single
// command. Gated by the caller on cfg.McpMode() == payload.McpDirect.
func (d *Deployer) installMcpDirect(ctx context.Context, profile, sandboxID, slotCommand, envContent string) error {
	// Step mcp-bin-write: write the embedded bzmux binary over stdin and chmod
	// 755 in the same command, so binary bytes never appear in argv (mirrors
	// mux-bin-write).
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("mcp-bin-write", fmt.Sprintf(
			`set -eu; umask 077; mkdir -p %s; cat > %s && chmod 755 %s`,
			dquote(install.MuxBinDir), dquote(install.MuxBinPath), dquote(install.MuxBinPath),
		)),
		bytes.NewReader(muxbin.Binary),
	); err != nil {
		return failf(CodeMuxWrite, "mcp multiplexer: write binary: %w", err)
	}

	return d.mcpVerify(ctx, profile, sandboxID, install.McpVerifyModeDirect, slotCommand, envContent)
}

// claudeInferenceProbe closes the gap between "the agent process answers
// ACP" and docs/PLAN.md §1's promise that a successful deploy means an
// agent that can run a session. The initialize handshake never touches the
// LLM, so without this probe a wrong endpoint, a credential the gateway
// rejects, or a workspace that does not serve the anthropic route all
// deploy "healthy" and fail at the first real mention.
//
// It deliberately does NOT reuse authProbe's `databricks current-user me`
// check. That call needs workspace-identity permissions, but the README
// recommends a least-privilege service-principal token scoped to CAN QUERY
// on gateway endpoints — which would fail it while being perfectly valid
// for inference. The probe has to exercise the endpoint that actually has
// to work.
//
// The env content travels over stdin, is sourced, and is removed by a trap,
// exactly like the verify handshake; no secret is ever interpolated into
// the command string or argv.
func (d *Deployer) claudeInferenceProbe(ctx context.Context, profile, sandboxID, envContent string) error {
	// Mirrors authProbe's shape deliberately: the script signals failure by
	// exiting non-zero, so the error is the NORMAL failure path and the
	// cause marker must be parsed from the output it still returned —
	// returning early on err would make every diagnosis below dead code.
	out, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("claude-inference-probe", claudeInferenceProbeScript()),
		strings.NewReader(envContent),
	)
	if err == nil {
		return nil
	}
	cause := probeCause(out, claudeProbeCauseMarkerPrefix)
	if cause == "" {
		// No marker: the transport itself failed before the script ran.
		return failf(CodeClaudeInference, "claude inference probe: %w", err)
	}
	return failf(CodeClaudeInference, "claude inference probe: %s (probe output: %s)",
		claudeProbeCauseMessage(cause, out), remoteText(strings.TrimSpace(out)))
}

// codexInferenceProbe is the codex twin of claudeInferenceProbe: it closes
// the same gap between "the agent process answers an ACP handshake" and
// "the agent can actually reach an LLM", which neither install verification
// nor launch verification touches.
func (d *Deployer) codexInferenceProbe(ctx context.Context, profile, sandboxID, envContent string) error {
	// Same shape as authProbe and claudeInferenceProbe: the script signals
	// failure by exiting non-zero, so the error is the NORMAL failure path
	// and the cause marker must be parsed from the output it still
	// returned — returning early on err would make the diagnosis below
	// dead code.
	out, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("codex-inference-probe", codexInferenceProbeScript()),
		strings.NewReader(envContent),
	)
	if err == nil {
		return nil
	}
	cause := probeCause(out, codexProbeCauseMarkerPrefix)
	if cause == "" {
		// No marker: the transport itself failed before the script ran.
		return failf(CodeCodexInference, "codex inference probe: %w", err)
	}
	return failf(CodeCodexInference, "codex inference probe: %s (probe output: %s)",
		codexProbeCauseMessage(cause, out), remoteText(strings.TrimSpace(out)))
}

// verifyLaunch implements docs/PLAN.md §4.4 step 10's pass signal, waiting
// VerifyDelay before checking. The process-liveness check and the acp.log
// read are combined into a SINGLE SSH round trip (BUG 2 + BUG 5 + efficiency
// fix): a bare `pgrep -f buzz-acp` (or a separate `cat acp.log` call) would
// self-match this very invocation's own argv over `databricks sandbox ssh
// ... -- <cmd>`, so a dead agent could still pass the liveness check;
// pgrep now uses the non-self-matching '[b]uzz-acp' bracket idiom, its
// exit code is captured and echoed rather than relied on as this call's
// own exit code, and the log is bounded to the last 4KB server-side via
// `tail -c` so it can never blow up an error message (BUG 5).
// launchID identifies the launch this call is verifying. When non-empty,
// the readiness signal must appear AFTER that launch's delimiter in
// acp.log — the log is append-only, so an earlier deploy's readiness line
// is otherwise indistinguishable from this one's and would let a launch
// that never actually happened verify as healthy.
func (d *Deployer) verifyLaunch(ctx context.Context, profile, sandboxID, launchID string) error {
	d.sleep(d.verifyDelay())

	out, err := d.SSH.Run(ctx, profile, sandboxID, step("verify-check", acpLivenessProbeFor(launchID)))
	if err != nil {
		return failf(CodeVerifySSH, "verify: could not check buzz-acp process/log: %w", err)
	}

	rc, logOut, perr := parsePgrepCheck(out)
	if perr != nil {
		// Inconclusive, not conclusively dead: fail the deploy with a
		// distinct diagnosis rather than pretending the process check
		// itself returned rc=1 (review round 2 — an unparseable output
		// must not masquerade as a confirmed-dead agent).
		return failf(CodeVerifyUnparseable, "verify: could not parse verification output: %w (output: %s)", perr, remoteText(strings.TrimSpace(out)))
	}
	logOut = remoteText(logOut)
	if rc != 0 {
		// A dead process whose log carries the terminal relay error IS
		// the non-member signature — buzz-acp exits ~1s after the denial
		// (docs/M05_PROBE_RESULTS.md §3), so by N=10s it is always dead.
		// Checking process liveness alone here would make relay_denied
		// unreachable in exactly the scenario it classifies.
		if strings.Contains(logOut, terminalErrorLine) {
			return failf(CodeVerifyRelayDenied,
				"verify: relay connection failed (%q); this nostr key is very likely not a member of the target relay",
				terminalErrorLine,
			)
		}
		return failf(CodeVerifyProcessDead, "verify: buzz-acp process not found %s after launch (acp.log: %s)", d.verifyDelay(), strings.TrimSpace(logOut))
	}

	if strings.Contains(logOut, terminalErrorLine) {
		return failf(CodeVerifyRelayDenied,
			"verify: relay connection failed (%q); this nostr key is very likely not a member of the target relay",
			terminalErrorLine,
		)
	}
	// logOut is ALREADY scoped to this launch when launchID is set:
	// acpLivenessProbeFor selects the post-marker region server-side, where
	// the whole log is available. An empty log here therefore means the
	// marker was never written — i.e. launch.sh decided not to spawn (a
	// previous agent was still holding the guard), which is exactly the
	// stale-agent case this scoping exists to catch.
	// Empty here means the marker itself is absent (awk keeps the marker
	// line, so a launch that stamped always yields at least that) — i.e.
	// launch.sh reached its guards and declined to spawn.
	if launchID != "" && strings.TrimSpace(logOut) == "" {
		return failf(CodeVerifyNotReady,
			"verify: acp.log carries no output for this deploy's launch within %s — launch.sh did not start an agent (a previous one may still have been shutting down)",
			d.verifyDelay())
	}
	if !strings.Contains(logOut, agentPoolReadyMarker) {
		return failf(CodeVerifyNotReady, "verify: acp.log did not contain %q within %s; log: %s", agentPoolReadyMarker, d.verifyDelay(), strings.TrimSpace(logOut))
	}
	return nil
}

// parsePgrepCheck splits verify-check's output into the pgrep exit code
// (from its "BUZZ_PGREP_RC=<n>" marker line) and the remaining (already
// tail -c 4096 bounded) acp.log content. Lines are scanned IN ORDER and
// the FIRST line carrying the marker wins (review round 2): the remote
// echo always precedes the log tail, so first-match-in-order tolerates
// any stdout preamble (e.g. a shell profile banner) AND is
// collision-safe against the marker text appearing inside the log
// content itself. Everything after the marker line is the log. When NO
// line carries a parseable marker, a distinct error is returned rather
// than a fabricated rc=1 — an inconclusive check must not masquerade as
// a confirmed-dead agent (it still fails the deploy, with the raw
// output in the diagnosis).
func parsePgrepCheck(out string) (rc int, log string, err error) {
	const prefix = "BUZZ_PGREP_RC="
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		n, aerr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, prefix)))
		if aerr != nil {
			return 0, "", fmt.Errorf("malformed %s marker line %q: %w", prefix, trimmed, aerr)
		}
		return n, strings.Join(lines[i+1:], "\n"), nil
	}
	return 0, "", fmt.Errorf("no %s marker line found in verification output", prefix)
}

// remoteText prepares output that came back from inside a sandbox (an
// acp.log tail, a command's stdout) for rendering into an error, a
// status payload, or the operator's terminal: bounded, then scrubbed of
// anything credential-shaped.
//
// The scrub is what the payload-keyed redaction in Deploy cannot do:
// the operator lifecycle commands have no payload to derive secrets
// from, and a crashing agent that echoes its environment would
// otherwise put a live DATABRICKS_TOKEN in the terminal (and in
// whatever issue the owner pastes it into). Deploy still redacts its
// own payload's secrets on top of this — remoteText is the floor.
func remoteText(s string) string {
	return redact.Log(truncate(s, maxErrorLogBytes))
}

// truncate bounds s to at most max bytes (BUG 5 fix: no remote log/output
// may be interpolated into an error string unbounded), appending a marker
// when it cuts content off.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// setAutostopPolicy implements docs/PLAN.md §4.4 step 11: default
// --no-autostop, or --idle-timeout <v> when provider_config.idle_timeout
// is set.
func (d *Deployer) setAutostopPolicy(ctx context.Context, profile, sandboxID string, cfg payload.ProviderConfig) error {
	opts := lakebox.SandboxConfigOptions{NoAutostop: true}
	if cfg.IdleTimeout != "" {
		opts = lakebox.SandboxConfigOptions{IdleTimeout: cfg.IdleTimeout}
	}
	return d.CLI.SandboxConfig(ctx, profile, sandboxID, opts)
}

// teardown implements docs/PLAN.md §4.3 failure teardown: best-effort
// shred of any env files written this invocation, then either delete a
// sandbox freshly created this invocation, or (for a reused sandbox) kill
// the buzz-acp process group and leave the sandbox alone. All best-effort
// — errors here are swallowed since the caller already has the real
// failure to report, and teardown itself failing must not mask it
// (docs/PLAN.md §4.3: "a human can recover if teardown itself fails").
//
// deploy() never calls this for a *preMutationFailure (the zero-token
// auth probe): that failure mode wrote nothing "this invocation" for
// EITHER shred or kill to be about — see preMutationFailure's doc comment
// and deleteFreshSandbox, its own narrower (shred-free, freshly-created-
// only) cleanup path.
//
// teardown uses its OWN independent context (BUG 4 fix), rather than the
// caller's deploy ctx: when a provisioning step fails BECAUSE the deploy
// deadline expired, the same (already-dead) ctx would make every SSH/CLI
// call here no-op immediately, silently leaking the secret-bearing env
// file and a fresh sandbox. Teardown must be able to run even after the
// deploy deadline.
func (d *Deployer) teardown(profile, sandboxID string, freshlyCreated bool) {
	if sandboxID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()

	_, _ = d.SSH.Run(ctx, profile, sandboxID, step("teardown-shred", secretShredCommand()))

	if freshlyCreated {
		_ = d.CLI.SandboxDelete(ctx, profile, sandboxID)
		return
	}
	// '[b]uzz-acp', not 'buzz-acp' (BUG 2 fix, applied here too for the
	// identical self-match reason as the step-8 prelaunch-kill above):
	// otherwise this teardown pkill could SIGTERM its own invoking shell
	// before ever reaching the real buzz-acp process, defeating the
	// "reused sandbox unhealthy — kill the lingering agent" teardown
	// intent.
	_, _ = d.SSH.Run(ctx, profile, sandboxID, step("teardown-pkill", `pkill -f '[b]uzz-acp' 2>/dev/null; true`))
}

// acpLivenessProbe is the one remote command behind both deploy's
// step-10 verification and the operator `status`: report whether a
// non-zombie buzz-acp is alive (as the parseable BUZZ_PGREP_RC marker
// line — see parsePgrepCheck) and echo a bounded acp.log tail, in a
// single round trip.
//
// The exit code is echoed rather than used as the command's own status
// because a dead agent must still produce a readable log tail rather
// than a failed SSH call.
func acpLivenessProbe() string {
	return acpLivenessProbeFor("")
}

// acpLivenessProbeFor is acpLivenessProbe scoped to one launch.
//
// When launchID is empty the log is the last 4KB, unchanged — that is what
// Status and Start use, since neither has a launch of its own to scope to.
//
// When launchID is set, the log region is selected SERVER-SIDE, from the
// last occurrence of that launch's marker, before the 4KB bound is applied.
// Doing it here rather than in Go is what keeps the check from becoming
// strictly more fragile than the one it replaces: the marker is written
// just before the agent spawns, so it is always OLDER than the readiness
// line it scopes. Truncating first and then looking for the marker would
// mean a chatty startup (several pooled agents, each logging) could push
// the marker out of the window while the readiness line remained — failing
// a perfectly healthy deploy. awk sees the whole file, so only the
// post-marker region is ever subject to the byte bound.
//
// launchID is a hex string generated by newLaunchID and is never payload
// data. It is shell-quoted into the awk -v assignment regardless, so the
// safety here rests on shellquote plus the value's provenance — NOT on any
// validation, which no caller performs.
func acpLivenessProbeFor(launchID string) string {
	logCmd := `tail -c 4096 "$HOME/.buzz-backend/acp.log" 2>/dev/null || true`
	if launchID != "" {
		marker := nest.LaunchEpochPrefix + launchID
		// Buffer from the last marker occurrence, print at EOF. Always
		// exits 0 so a missing marker (an agent that never launched)
		// yields empty output rather than a non-zero status the caller
		// would misread as a transport failure.
		//
		// The marker LINE ITSELF is kept in the buffer rather than
		// skipped. That is what lets the caller distinguish "this launch
		// stamped but has not logged anything yet" (buffer holds just the
		// marker) from "this launch never happened" (buffer empty) —
		// dropping it would collapse both into empty output and produce a
		// confidently wrong diagnosis for a launch that did occur.
		logCmd = fmt.Sprintf(
			`awk -v m=%s 'index($0, m) { buf = $0 "\n"; found = 1; next } found { buf = buf $0 "\n" } END { printf "%%s", buf; exit 0 }' `+
				`"$HOME/.buzz-backend/acp.log" 2>/dev/null | tail -c 4096 || true`,
			shellquote.Single(marker),
		)
	}
	return nest.AliveCheckSnippet + "\n" +
		`buzz_acp_alive; echo "BUZZ_PGREP_RC=$?"; ` + logCmd
}

// secretShredCommand removes every secret-bearing file this provider
// writes into a sandbox: the agent env file (nsec, auth tag, inference
// token) and the transient verification env file. Shared by failure
// teardown and Undeploy so neither can drift into shredding less than
// the other. Always exits 0 — a missing file is the expected case on
// most paths and must not fail the caller's own error reporting.
func secretShredCommand() string {
	// verifyEnvFilePath+".out" is the verify handshake's captured
	// stdout+stderr. It is produced with the agent's full env exported
	// under `set -a`, so a runtime that echoes its environment on startup
	// puts secrets in it. BuildVerifyCommand traps it on exit, but a
	// transport drop mid-command leaves it behind — and this function's
	// contract is to reclaim every secret-bearing file the provider
	// writes, not only the ones whose own cleanup usually works.
	paths := []string{nest.EnvFilePath, verifyEnvFilePath, verifyEnvFilePath + ".out"}
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, `shred -u %s 2>/dev/null || rm -f %s 2>/dev/null; `, dquote(p), dquote(p))
	}
	b.WriteString("true")
	return b.String()
}

// step prepends an inert shell comment tagging cmd with a short
// identifier (e.g. "install-exec"). It has no effect on execution — sh
// ignores comment lines — and exists purely so tests can assert
// call-order against a fake `databricks` CLI shim without depending on
// exact command text (PLAN.md §7: "assert full call ORDER").
func step(tag, cmd string) string {
	return "# buzz-step:" + tag + "\n" + cmd
}

// dquote double-quotes s for interpolation into a POSIX shell command
// string, preserving "$HOME"-style shell expansion inside s (these are
// always fixed path constants, never secrets) while still protecting
// against word splitting/globbing.
//
// dquote is for TRUSTED static "$HOME"-relative literals ONLY — never
// payload/untrusted data. Untrusted or secret-adjacent data must go
// through internal/shellquote.Single instead (or, better, over stdin per
// internal/sshx's doc comment).
func dquote(s string) string {
	return `"` + s + `"`
}
