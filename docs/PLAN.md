# PLAN — `buzz-backend-databricks`

> Consensus plan, 2026-07-24, produced via ralplan: independent planner draft → independent critic review (13 findings, all resolved, final verdict ACCEPT) → coordinator synthesis with source verification against `block/buzz` @ `3bd3a014c6` (`desktop/src-tauri/src/commands/agents_deploy.rs`, `agents.rs`). Sources: `README.md`, `docs/BUZZ_AGENT_SESSION_ARCHITECTURE.md` (lane A), `docs/OMNIGENT_DATABRICKS_SANDBOX_PATTERNS.md` (lane B), `docs/LAKEBOX_LIVE_PROBE_RESULTS.md` (lane C). Unverified details are marked OPEN QUESTION; owner calls are in §9.
>
> **Updated 2026-07-24 (post-M0.5):** the M0.5 probe session ran live on a sanitized test workspace — results in `docs/M05_PROBE_RESULTS.md` (lane D). All four §9 owner decisions are resolved and every M0.5-gated unknown is settled; the affected sections below are updated in place and marked *(M0.5)*.

## 0. Verified deploy-payload contract (closes the draft's envelope open question)

Verified directly against buzz source (coordinator, commit `3bd3a014c6`):

- **Payload fields (exhaustive):** `name`, `relay_url`, `private_key_nsec`, `auth_tag`, `agent_command`, `agent_args`, `system_prompt`, `model`, `provider`, `turn_timeout_seconds`, `idle_timeout_seconds`, `max_turn_duration_seconds`, `parallelism`, `respond_to`, `respond_to_allowlist`, `env_vars`.
- **There is NO `backend_agent_id` in the payload and there will not be in v1.** Buzz persists whatever `agent_id` string the provider returns, but never sends it back on redeploy.
- **Redeploy contract** (doc comment on `deploy_to_provider`): "calling deploy on an already-deployed agent sends the same payload again. Providers are expected to handle this as an update-in-place or no-op — the protocol does not include an explicit undeploy operation (deferred to v2)."

Consequence: **idempotency is entirely the provider's responsibility**, keyed on identity derived from the payload itself (§4.1).

---

## 1. Goals / non-goals

**Goals**
- Ship a single executable, `buzz-backend-databricks`, discoverable by Buzz Desktop on `PATH` as `BackendKind::Provider` (lane A §3b: providers are PATH-discovered `buzz-backend-<id>` binaries; no desktop changes needed).
- Implement provider protocol v1: `{op:"deploy"}` and `{op:"info"}` over one-shot stdin/stdout JSON (lane A §3b: the only ops today; the rest "deferred to v2").
- On deploy: reuse-or-create a Databricks Sandbox keyed on the agent's npub (§4.1), neutralize the baked creator PAT, install pinned Buzz Linux binaries **and a working agent runtime** into persistent `$HOME`, hand over the payload as an 0600 env file via SSH stdin, launch `buzz-acp` detached, verify, and only then relax the autostop policy. Return `{ok:true, agent_id:"<sandbox-id>"}`.
- **A "successful" deploy means an agent that can run a session**, not just a live harness process: the runtime named by `agent_command` must exist in the sandbox (v0: `buzz-agent`, shipped in the .deb — lane A §1; lane C "Buzz-stack proof").
- Fail safe: any partial-deploy failure tears down what this invocation created and never strands secrets or a forever-running sandbox (§4.3).
- Interim lifecycle as CLI subcommands (`status`, `stop`, `start`, `logs`, `undeploy`, `doctor`) mapping 1:1 onto future v2 ops (lane C gap #1).
- Survive the autostop model: `$HOME` persists, everything else is wiped, all processes die, nothing relaunches buzz-acp (lane C "Persistence semantics").

**Non-goals (v0)**
- No changes to `block/buzz`.
- No goose runtime support (§4.2, Open Decision 4). Payloads requesting it are rejected with an actionable error, not half-deployed. **Claude Code is supported as of v0.1** (issue #2), and **Codex as of v0.1** (issue #3).
- No relay-side orchestration, multi-agent packing, or custom images (no image/CPU/RAM/disk knobs exist — lane C "API/CLI surface").
- No Windows provider binary.
- No in-sandbox workspace OAuth (omnigent's flow, lane B §b step 5) — buzz agents talk to the relay, not Databricks Apps; we only neutralize the baked PAT.

---

## 2. Language + toolchain

**Go (1.22+), stdlib-heavy, cobra for CLI, goreleaser for releases, `CGO_ENABLED=0`.**

- The work is JSON on stdio, subprocess orchestration (`databricks sandbox ...`), and small shell-script templating — no SDK or async runtime required (§3.1).
- Single static binary, trivial cross-compile: the provider runs on the *owner's* machine (overwhelmingly macOS — Buzz Desktop is Tauri). `GOOS/GOARCH` covers darwin/arm64+amd64 and linux/arm64+amd64 from one free-tier `ubuntu-latest` runner; Rust would force macOS runners (10× minute multiplier).
- npub derivation from `private_key_nsec` (§4.1) is bech32 decode + secp256k1 pubkey — mature Go libraries (`btcec`, `btcutil/bech32`), no cgo.
- `databricks-sdk-go` exists if we ever switch to direct REST; Go forecloses nothing.

CI: `golangci-lint`, `go vet`, `go test`, 4-target build, goreleaser to GitHub Releases with checksums.

---

## 3. Architecture

### 3.1 Transport: shell out to the stock `databricks` CLI (not raw REST)

All Lakebox operations go through `databricks sandbox <cmd> --json -p <profile>` subprocesses.

- Stock public CLI (≥ v1.8.0) ships the full `sandbox` group: `register/create/list/status/ssh/config/start/stop/delete/ssh-key` — verified live (lane C "API/CLI surface").
- **SSH is the clincher**: exec rides a gateway on port 2222 and "the CLI handles ProxyCommand wiring itself" (lane C). Reimplementing that against an undocumented Beta surface is pure risk. Omnigent made the same call (lane B §a).
- Churn resistance: the REST namespace still carries the old codename (`/api/2.0/lakebox/...`) and the product already renamed once (databricks/cli#5487, lane C). The CLI is the supported abstraction over that churn.
- Auth free: CLI resolves `~/.databrickscfg` profiles; provider threads `provider_config.profile` as `-p` (README "Auth").

Version handling (critique #11): `doctor` and deploy preflight **gate** on a minimum CLI version (resolved at M0 from the CLI changelog — the first release with the `sandbox` group; no live infra needed), and every deploy/`status`/error output **records** the `databricks version` string in use, so `--json` shape drift is diagnosable from a single report.

### 3.2 Binary layout

```
cmd/buzz-backend-databricks/main.go   # dispatch: no argv → provider mode (stdin JSON); argv → operator subcommands
internal/provider/                    # stdin/stdout protocol: envelope, op routing, response shapes (frozen at M0 against invoke_provider)
internal/payload/                     # deploy-payload structs per §0 field list + validation
internal/identity/                    # nsec → npub derivation; sandbox-name keying (§4.1)
internal/lakebox/                     # typed wrapper over `databricks sandbox ...` subprocesses
internal/sshx/                        # in-sandbox exec: run, runWithStdin (secret path), script runner
internal/install/                     # pinned .deb fetch/sha256/extract + runtime verification scripts
internal/nest/                        # ~/.buzz layout, env-file, launch.sh templates
internal/deployflow/                  # §4 orchestration incl. failure teardown
internal/redact/                      # secret redaction on every log/error path
```

### 3.3 Mode dispatch

- **Provider mode** (no argv): read one JSON object from stdin, dispatch on `op`:
  - `info` → `{name, version, description, ...}` (lane A: `commands/agent_providers.rs`).
  - `deploy` → §4 → `{ok:true, agent_id}` | `{ok:false, error}`. Must finish well inside buzz's 600 s timeout (lane A §3b).
  - Unknown op → structured error (forward-compat: v2 ops map onto the subcommand internals below).
  - Exact response envelope field names, error shape, and exit-code expectations are **frozen at M0 by reading `backend.rs::invoke_provider`** (critique #5) — a code-search task, no infra needed. §0 already froze the request side.
- **Operator mode** (argv): `status | stop | start | logs | undeploy | doctor | deploy --payload-file <f>` (the last for testing without the desktop).
  - `start` = `sandbox start` → wait Running (~20.5 s, lane C) → run `launch.sh` (which re-asserts the PAT stub — §5). This is the autostop/lifetime-cap recovery path.
  - `undeploy` = best-effort in-sandbox secret shred → `sandbox delete --auto-approve`.
  - `logs` = one-shot tail only; no `-f` follow mode in v0 (critique #12 — operators can `sandbox ssh -- tail -f` themselves).

---

## 4. Deploy flow

Budget: 600 s (lane A §3b). Measured: create→Running 1.1 s, start 20.5 s (lane C); the ~65 MB .deb download dominates and is skipped on redeploy.

### 4.1 Identity and idempotent reuse (resolves draft blocker #1)

Since the payload never carries a prior sandbox id (§0), the provider derives a **deterministic identity from the payload**:

- Derive the agent's **npub** from `private_key_nsec` (stable across redeploys; unique per agent — buzz mints per-agent keys, lane A §4).
- Sandbox name = `buzz-<npub12>-<slug>` where `<npub12>` = first 12 chars of the npub data part (the identity key) and `<slug>` = sanitized display name (cosmetic only — never identity; two agents named "Reviewer" must not collide, critique #10; lane B's hostname-collision gotcha is the cautionary tale).
- **Reuse-or-create**: `sandbox list --json` (verified endpoint/command, lane C), match on the `buzz-<npub12>-` prefix:
  - 0 matches → create.
  - 1 match → reuse: if `Stopped`, `start` and wait `Running`; then update-in-place per §4.4 step ordering (refresh env file, reinstall binaries only if pinned version changed, kill old process group, relaunch). This satisfies buzz's "update-in-place or no-op" redeploy contract (§0).
  - \>1 matches → **fail** with all sandbox ids listed and manual-recovery guidance. Never guess: picking wrong risks two harnesses on one key double-responding (lane C "Buzz-stack proof" note a) plus a stranded nsec copy.
- The returned `agent_id` is the sandbox id — buzz persists it for display, but the provider never depends on getting it back.

### 4.2 Runtime provisioning (resolves draft blocker #2)

buzz-acp spawns **agent subprocesses** via `BUZZ_ACP_AGENT_COMMAND` (lane A §2 stage B — "default `goose acp`; codex/claude via npm ACP adapters"). The sandbox image has Node 22 and Python 3.12. A deploy that only installs the harness ships an agent that dies on its first mention.

  **Correction (probe S1, docs/M3_CODEX_PROBE_RESULTS.md):** this section previously stated the image has "no goose/claude/codex". It ships **both** `/usr/local/bin/claude` and `/usr/local/bin/codex` — but they are Databricks `ucode` wrappers that take no arguments and launch an interactive TUI, not ACP servers. They are unusable for this purpose, so the conclusion stands while the premise did not. This is direct evidence for the alias-canonicalization rule below, which was previously justified only from upstream source.

- **Supported runtimes.** Payload validation (step 1 below, before any provisioning) rejects an `agent_command` outside this table with an actionable error naming the alternatives.
- **Canonicalization is load-bearing, not tidiness.** Every alias resolves to one spawn command per runtime (`payload.Runtime.SpawnCommand()`), and `internal/install` symlinks exactly that one name. Combined with `launch.sh`'s `PATH` prepend, this is what keeps the image's `ucode` wrappers unreachable. For codex the adapter independently resolves its own bundled binary via `createRequire` with no `PATH` lookup at all (probe S10), so the wrapper is doubly out of reach.

| Runtime | `agent_command` | How it gets into the sandbox | Inference |
|---|---|---|---|
| `buzz-agent` | `buzz-agent` | ships in the same pinned .deb (verified Linux x86_64 build, lane C "Buzz-stack proof") | `BUZZ_AGENT_PROVIDER=databricks_v2` + `DATABRICKS_*` |
| `claude` | `claude-agent-acp`, `claude-code-acp`, `claude-code`, `claudecode` | `npm ci` of `@agentclientprotocol/claude-agent-acp` against a committed lockfile, symlinked into the existing bin dir (~6s, 570 MB — measured, docs/M2_CLAUDE_PROBE_RESULTS.md) | `ANTHROPIC_BASE_URL` → the workspace AI Gateway + `ANTHROPIC_AUTH_TOKEN`, derived by `nest.ClaudeEnvSnippet` |
| `codex` | `codex-acp`, `codex` | `npm ci` of `@agentclientprotocol/codex-acp` against a committed lockfile, symlinked as `codex-acp` only (4.4s, 362 MB — measured, docs/M3_CODEX_PROBE_RESULTS.md) | a `config.toml` generated at launch under a provider-owned `CODEX_HOME`, pointing `base_url` at `{host}/ai-gateway/codex/v1` with `wire_api = "responses"` and `env_key = "DATABRICKS_TOKEN"`, written by `nest.CodexEnvSnippet`. Receives `BUZZ_ACP_MCP_COMMAND=buzz-dev-mcp` (matching upstream's discovery entry): its tool calls run under the adapter's no-network sandbox, so the MCP subprocess is how it reaches the relay |

  The Claude runtime deliberately emits **no model variable**: a gateway model id is rewritten by the Anthropic SDK into a canonical id the gateway does not serve, failing every turn. `agent.model` is therefore ignored for this runtime and the adapter's own default is used.

  The Codex runtime also ignores `agent.model`, for a different reason: its model is written into the generated `config.toml` as a **fixed constant** with no payload interpolation, because that file is produced by a heredoc inside a shell-sourced env file and any payload-derived value would be the injection surface `payload.Agent.Validate()` exists to close. The gain would be nil — the gateway serves only `databricks-`-prefixed ids, and codex emits a "model metadata not found" warning for every one of them regardless (probe S6/G5).

  Codex takes `/responses` and **only** `/responses`. API-type support on this gateway is per-**model**, not per-surface: `/chat/completions` returns HTTP 400 for `databricks-gpt-5-3-codex` (probe G2). The `wire_api = "chat"` fallback contemplated in issue #3 cannot work with any codex-class model, so there is one code path rather than two.
- Install verification *(M0.5)*: `buzz-agent --version` does not exist (config validation precedes arg parsing; exit 2 demanding `BUZZ_AGENT_PROVIDER`). The verification is an **ACP `initialize` handshake**: with provider env set, send one `initialize` frame on stdin and expect the `agentInfo` result — validates binary + config in <1 s without touching the LLM (lane D §6). `session/new` additionally exercises gateway auth (it performs model discovery) and is the right preflight when inference env is present.
- **v0.1: goose/claude/codex** via npm ACP adapters — feasible (npm egress open, Node 22 present, lane C) but brings runtime *config bridge* obligations (§4.5) and provider API keys; scope is Open Decision 4.

### 4.3 Failure teardown (resolves draft blocker #3)

Wrapped around every step below:

- Failure after a **create performed in this invocation**: best-effort shred of the env file (if written) → `sandbox delete --auto-approve`. No orphaned sandbox, no stranded nsec, no runaway bill.
- Failure on a **reused** sandbox: shred the freshly written env file, kill the buzz-acp process group, leave the sandbox (it belongs to a previous successful deploy).
- Every `{ok:false}` error **embeds the sandbox id** (when one exists) and the recorded CLI version, so a human can recover if teardown itself fails.
- **Autostop policy is configured last** (step 10), after launch verification: an orphan that escapes teardown is at worst a default 10-minute-idle sandbox (lane C lifecycle), never a forever-running one.

### 4.4 Step order

1. **Parse + validate payload** (stdin; structs per §0). Reject unsupported `agent_command` here (§4.2) — before anything is provisioned. `provider_config` is already secret-free by buzz's own validation (lane A §3b).
2. **Preflight**: CLI present + version gate + version recorded (§3.1); profile resolves (`databricks current-user me -p <profile>`); `sandbox register -p <profile>` (idempotent, machine-level key — lane C gap #6). Region-gated-preview guidance on 4xx (lane C: us-west-2 confirmed).
3. **Reuse-or-create** per §4.1. Create → Running ~1.1 s; reuse of Stopped → start ~20.5 s (lane C).
4. **PAT reset — first in-sandbox action** (critique #4a): before any other in-sandbox command executes, overwrite `~/.databrickscfg` (baked creator-identity PAT, verified `current-user me` → creator, lane C "Environment") with a comment-only stub, unless `provider_config.keep_workspace_pat: true`. Rationale: the very next step runs a network-fetched install script; nothing between Running and here needs the PAT (SSH auth is local profile + gateway; the .deb comes from GitHub). Omnigent precedent for a full reset: lane B §b step 5. Ships in **M1**, not M3 (critique #4c).
5. **Install pinned Buzz .deb into `$HOME`** (persists across stop/start; everything else is wiped — lane C): in-sandbox `curl -fL` from the pinned GitHub release URL, verify pinned **sha256** (M1 scope, critique #4c), `dpkg-deb -x` into `$HOME/.buzz-backend/dist`, symlink `buzz-acp`, `buzz`, `buzz-agent`, `buzz-dev-mcp`, `git-credential-nostr` into `$HOME/.buzz-backend/bin`. Verified feasible end-to-end (lane C egress + "Buzz-stack proof"). Skip when the pinned version is already present. Then **runtime verification** (§4.2). Pinned version is a build-time default overridable via `provider_config.buzz_version` (OPEN QUESTION: payload carries no version field per §0, so matching the desktop's version automatically is impossible in v1 — pin + override is the design, not a stopgap).
6. **Provision the nest**: `$HOME/.buzz`, `$HOME/.buzz/REPOS`, `$HOME/.buzz/OUTBOX` (buzz-acp's system prompt names cwd, `OUTBOX/`, `{cwd}/REPOS/`; local agents run with cwd `~/.buzz` — lane A §4). Lane A also lists "persona/team dirs + runtime config bridges" (critique #9); per-item disposition:
   - *Persona/team material*: projected into the payload as `system_prompt` and env (`BUZZ_ACP_SYSTEM_PROMPT`, `BUZZ_ACP_TEAM_INSTRUCTIONS` — lane A §4 env list); no remote dirs needed. **Verify at the M0 source pass** that provider deploys have no other persona/team file dependency; if one exists, add it here.
   - *Runtime config bridges* (`config_bridge/{goose,claude,codex}.rs`): only needed for external runtimes. `buzz-agent` and `claude` are configured entirely via env (`BUZZ_AGENT_PROVIDER`/`BUZZ_AGENT_MODEL`; `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`), so neither needs a file. **Codex does**, and the mechanism is `CODEX_HOME` — *not* `~/.codex/config.toml`.

     This corrects an earlier instruction in this document to write `~/.codex/config.toml` directly. In a Lakebox sandbox that path is a **symlink to `/run/lakebox/codex-config.toml`** (probe S2): writing it would follow the symlink, clobber image-managed state, and be silently discarded on the next stop/start when `/run` is regenerated. `nest.CodexEnvSnippet` sets `CODEX_HOME` to a provider-owned directory under `$HOME/.buzz-backend/` and generates the file there, at launch, from values it derived itself. The same probe also **confirms** the previously-unverified restore-on-start behaviour behind step 9's PAT re-assert: `~/.databrickscfg` is likewise a symlink into `/run`, so it genuinely is regenerated on every start.
7. **Secret handover**: render the env file — `BUZZ_PRIVATE_KEY`, `BUZZ_AUTH_TAG`, `BUZZ_RELAY_URL`, `BUZZ_ACP_AGENT_COMMAND/ARGS`, `BUZZ_ACP_AGENTS` (parallelism), `BUZZ_ACP_SYSTEM_PROMPT`, `BUZZ_ACP_MODEL`, `BUZZ_ACP_RESPOND_TO(_ALLOWLIST)`, the three timeout vars, `NOSTR_PRIVATE_KEY` + `GIT_CONFIG_*` (git-credential-nostr, lane A §4), **inference auth for buzz-agent** *(M0.5, lane D §2)* — `BUZZ_AGENT_PROVIDER` (`databricks_v2` for AI Gateway), `DATABRICKS_HOST`, `DATABRICKS_TOKEN`, `DATABRICKS_MODEL` (mapped from payload `provider`/`model` + `env_vars`; buzz-agent consumes the static-bearer env path, not `~/.databrickscfg`, so this coexists with the step-4 PAT reset) — plus merged `env_vars`. Ship it **over SSH stdin only, never argv**, into `$HOME/.buzz-backend/env` with `umask 077` (README op table; lane C gap #3). *(M0.5)* **The stdin transport is VERIFIED**: `printf ... | databricks sandbox ssh <id> -- 'cat > /tmp/x && sha256sum /tmp/x'` arrived byte-identical (lane D §1). This is the primary transport; the raw-ssh ProxyCommand fallback stays documented but is not needed.
8. **Update-in-place guard**: kill any existing buzz-acp process group (redeploy semantics, §4.1; prevents double-responding harnesses).
9. **Launch**: write `$HOME/.buzz-backend/launch.sh` — `set +x`; **re-assert the PAT stub** (critique #4b — covers the unprobed "does start restore the baked file?" question by construction); source env; `cd $HOME/.buzz`; flock/pidfile + pgrep guard against double-launch; `setsid nohup buzz-acp >> $HOME/.buzz-backend/acp.log 2>&1 &`. Run it. `setsid nohup` survival across SSH disconnect is verified (lane C). `launch.sh` is the single relaunch entrypoint used by deploy, `start`, and any future supervisor.
10. **Verify, then set autostop policy** *(M0.5)*: success signal = `pgrep -f buzz-acp` alive after **N=10 s** (terminal relay failure lands ~1 s after launch — lane D §3; 10 s is ample) **and** `acp.log` contains `agent_pool_ready agents=N` without the terminal line `initial relay connect failed with terminal error`. Log vocabulary is now known (lane D §3): startup `buzz-acp starting: relay=… pubkey=…`; healthy `agent_pool_ready agents=N`; fatal `initial relay connect failed with terminal error` → exit 1, no crash-loop. Only after verification: **default `sandbox config <id> --no-autostop`** (Open Decision 3 resolved — lane D §4 proved relay WSS traffic does not prevent idle autostop, so the default idle timeout kills healthy agents ~11–12 min after the last SSH); `provider_config.idle_timeout` (1m–24h, lane C) opts into autostop for owners who accept `start`-based manual recovery.
11. **Respond**: `{ok:true, agent_id:"<sandbox-id>"}`.

Downtime tolerance: buzz-acp is stateless and replays unprocessed @mentions via a `since` filter (lane A §5), so relaunch-after-death catches up — this is what makes the autostop/Beta-storage model tolerable (lane C persistence).

---

## 5. Security

**nsec handling.** The payload contains `private_key_nsec` (§0); today it lives in the OS keyring and deploy fails closed without it (lane A §4). Shipping it into Databricks-managed infra is a **deliberate, owner-acknowledged trust-boundary crossing** (lane C hit this live: the probe's classifier blocked a real nsec). Controls: SSH-stdin-only transport (never argv; probed at M0.5), `set +x`/`umask 077` remote scripts, 0600 at rest in a single-user microVM (uid 10086), redaction on every provider log/error path, per-agent keys only (compromise = impersonating that one agent on the relay; remedy = rotate/delete in desktop + `undeploy`), `undeploy` and every failure-teardown path shred the env file, and Beta storage is treated as both unreliable *and* potentially outliving intent (lane C) — hence explicit shredding.

**PAT-in-image.** The image bakes a creator-identity PAT in `~/.databrickscfg` — anything in the sandbox can act as the owner on the workspace (lane C "Environment"; README key facts). Handling: reset to a stub as the **first in-sandbox action** of every deploy (§4.4 step 4), re-asserted by `launch.sh` on every start (§4.4 step 9) so the unverified restore-on-start behavior is moot. `keep_workspace_pat: true` opts out (Open Decision 2). Reset + re-assert ship in M1; M3 adds only the opt-out flag's acceptance test and audits. Least-privilege upgrade path (post-v0, unverified — do not promise): a narrowly-scoped service-principal credential supplied via `env_vars` replacing the stub.

**Provider-side least privilege.** Runs on the owner's machine with the owner's chosen profile; needs only the sandbox command family + `current-user me`. The machine-level SSH key is shared across all sandboxes (lane C gap #6) — documented, not fixable at our layer. `provider_config` is validated secret-free by buzz itself (lane A §3b).

**`inference_auth: "sandbox"` (issue #10).** This mode intentionally retains the creator-identity credential the PAT reset above exists to remove: for as long as the baked cfg is valid, the agent can act as the owner across the whole workspace, not just call the AI Gateway. Compensating controls: the value is enum-validated; the create-form schema now defaults new Buzz agents to an explicitly persisted `"sandbox"`, while the wire default remains `""`/`"env"` so existing agents and raw payloads keep the PAT-reset behavior unchanged. Current Buzz launch blocks may include Databricks host/token fields used for local model selection; sandbox mode drops that projected pair before deriving its own all-or-nothing Sandbox pair. A deploy-time probe exercises the derivation snippet itself and fails loud with a disambiguated `provision.sandbox_auth` rather than deploying a silently broken agent; the bare-`dapi` redaction pattern added alongside this change scrubs a derived token from anything logged.

**`inference_auth: "sandbox"` + the `claude` runtime is a materially wider grant than `sandbox` + `buzz-agent`, and ships with documentation only.** Stated plainly because it is a deliberate choice, not an oversight. Three individually parity-justified decisions compose into one capability: the Claude runtime omits the MCP tool gate (it needs none — Claude Code ships its own built-in tools, *including a shell*), buzz-acp auto-approves every `session/request_permission` regardless of permission mode, and `sandbox` mode hands the agent a live workspace-owner credential. The result is a shell, auto-approving every tool call, holding the owner's PAT. Note specifically that **`BUZZ_ACP_PERMISSION_MODE` is not a lever here** — buzz-acp approves permission requests before the mode is ever consulted, so setting it to `default` adds round trips and restricts nothing; no future reviewer should mistake it for a control. Where buzz-agent needed `buzz-dev-mcp` wired in before it could act at all, Claude Code arrives able to act. The compensating controls listed above still apply, and one is added by this change (below), but none of them narrows what an agent can *do* once running. Owners who want the narrower grant should use `inference_auth: "env"` with a least-privilege service-principal token scoped to `CAN QUERY` on the gateway endpoints.

**Credential-egress defense for the `claude` runtime (both modes).** Claude Code falls back to `https://api.anthropic.com` when `ANTHROPIC_BASE_URL` is unset, and a Lakebox sandbox has open egress to that host (verified live: HTTP 401 in 147 ms with a genuine Anthropic error body — docs/M2_CLAUDE_PROBE_RESULTS.md). Since `ANTHROPIC_AUTH_TOKEN` is sent as `Authorization: Bearer` regardless of destination, an otherwise-innocuous missing `DATABRICKS_HOST` would have shipped a live workspace PAT to a third party, on a deploy that reported healthy — neither the install handshake nor launch verification touches the LLM. Two independent defenses: `payload.DeployRequest.Validate()` rejects a Claude deploy that names no inference source at all (fail loud, before provisioning), and `nest.ClaudeEnvSnippet` derives `ANTHROPIC_AUTH_TOKEN` **only inside the branch where a base URL is known** (fail closed, at runtime). The second is what covers `sandbox` mode, where the host is derived in-sandbox and no payload-time check can see it. The runtime gate is specifically *"did this provider derive the endpoint"*, not *"is some endpoint set"*: in `sandbox` mode the token is the baked creator-identity PAT, so attaching it to an owner-named third-party endpoint would forward an owner-level credential the payload never had to carry. Bring-your-own-endpoint therefore requires bring-your-own-token (`ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` together), and both are then left untouched.

**`inference_auth: "sandbox"` + the `codex` runtime carries the same grant as `claude`, reached differently.** Codex's tool calls start under the adapter's default `AgentMode` — `workspaceWrite`, `writableRoots: []`, `networkAccess: false`, `approvalPolicy: "on-request"` (`docs/M3_CODEX_PROBE_RESULTS.md` S10). That is narrower *on paper* than Claude Code's unrestricted shell. It is **not** a confinement control, and must not be cited as one: `on-request` converts every sandbox denial into a `session/request_permission`, and buzz-acp auto-approves every one — the same mechanism, and the same caveat, this section already records for `BUZZ_ACP_PERMISSION_MODE`. The operative rule is *whatever codex asks for, it gets*, so the effective posture is the same unconfined one, and everything said above about a shell holding the owner's PAT applies unchanged.

Two related non-claims. **Nothing here asserts anything about network posture.** Codex's tool-path egress depends on buzz-acp's `CODEX_CONFIG` injection, which returns `None` — silently leaving that path without network — when the relay URL cannot be parsed, while this provider validates `agent.relay_url` only as non-empty; and upstream documents the mechanism in **macOS Seatbelt** terms against a Linux sandbox. It is not falsifiable from here. **And the generated `config.toml` deliberately omits `sandbox_mode` and `approval_policy`**, because the adapter applies an `AgentMode` preset per session that supersedes the file (S7 measured a `read-only` config permitting writes). Emitting them would place text in a generated file that reads like a control, is reviewed as one, and constrains nothing.

**Credential-egress defense for the `codex` runtime (both modes).** Structurally the same commitment as the Claude one below — never attach a credential to an endpoint this provider did not derive — but it needs two mechanisms rather than one, because the Claude lever is unavailable. Claude's defense works by *withholding*: `nest.ClaudeEnvSnippet` simply does not export `ANTHROPIC_AUTH_TOKEN` for an endpoint it did not choose. Codex's token arrives via `env_key = "DATABRICKS_TOKEN"`, which is already exported — by `SandboxAuthSnippet` in sandbox mode, **ungated on the host** — so there is nothing to withhold. Instead:

1. *Fail closed at runtime, statefully.* `nest.CodexEnvSnippet` writes its `config.toml` only inside the branch where it derived the host, and — because a file in a persistent `$HOME` survives redeploys in a way a per-process env var cannot — **unconditionally removes any previous one first, with the removal's success checked rather than assumed**. Without that, a redeploy into a reused sandbox whose regenerated `~/.databrickscfg` carries a token but no host would leave the previous deploy's file in place and send the current token to the previous host. It also exports `CODEX_HOME` even when it writes nothing, because otherwise codex falls back to `~/.codex/config.toml` — the image's baked gateway config, whose `auth.command` reads the workspace PAT straight out of `~/.databrickscfg`.
2. *Fail loud at the payload boundary.* Two rules, gated on different conditions because they answer different questions. **Where does the credential go** — `DATABRICKS_HOST`/`DATABRICKS_TOKEN`, the other Databricks auth variables, and the claude pair — is refused under `inference_auth: "sandbox"` only, since that is the sole mode in which the provider derives the baked owner PAT on the payload's behalf. Under `keep_workspace_pat` with `"env"` the provider derives nothing, so refusing them there would block the only working buzz-agent configuration while preventing nothing. **What code runs beside the credential** — `BUZZ_ACP_AGENT_COMMAND`/`_ARGS`/`BUZZ_ACP_MCP_COMMAND`, proxy and CA variables, `PATH`/`HOME`, the `NODE_*`/`LD_*`/`DYLD_*`/`npm_config_*`/`PYTHON*`/`GIT_*` loader prefixes, and the codex-runtime set — is refused whenever an owner PAT is reachable at all, i.e. `"sandbox"` **or** `keep_workspace_pat: true`. The invariant behind both, worth stating because three separate mistakes came from not having it written down: `agent.env_vars` render LAST and win over everything the provider emits, so the question is never "does an adapter read this" but "does this decide where the credential goes, or what code sees it". The provider's OWN emitted variables are the most direct lever of all — `BUZZ_ACP_AGENT_COMMAND=sh` with `BUZZ_ACP_AGENT_ARGS=-c,…` needs no loader trick and nothing on disk, and it also defeats the spawn-command canonicalization the ucode-wrapper defense rests on.

**`agent.env_vars` is trusted input whenever the sandbox holds an owner credential, and the key-charset check is not a content filter.** Its values can redirect inference (`DATABRICKS_HOST`, proxy variables), load attacker-controlled code into the agent process (`NODE_OPTIONS` with an `--import=data:text/javascript,…` URL executes arbitrary JavaScript inside the adapter with `DATABRICKS_TOKEN` in `process.env`, before any adapter code runs and with nothing written to disk; `LD_PRELOAD` does the same for native children), or widen the session policy (`INITIAL_AGENT_MODE: agent-full-access` suppresses every `session/request_permission`). A denylist built from "what the adapter reads" is structurally incapable of covering that class — none of those are adapter settings — which is why the loader prefixes are refused wholesale rather than enumerated.

**One consequence of giving codex `buzz-dev-mcp` belongs here.** Before that, a codex agent had no working outbound channel from its tool path at all — the adapter's default `AgentMode` sets `networkAccess: false`, and buzz-acp's `CODEX_CONFIG` injection is what opens it for the MCP subprocess (which upstream confirms is sandboxed alongside the shell, not privileged around it). The MCP server is what gives a codex agent a relay channel, which makes the residual below concretely reachable for codex rather than contingent on that injection working on Linux. It does not change the risk class — this section already states it — but the two facts should sit next to each other rather than a reader taking the earlier no-network observation as a mitigation.

**Scope, stated plainly so this section is not over-trusted: the two defenses above stop the PROVIDER from silently forwarding the owner credential. They do not stop a determined agent owner from obtaining it.** An owner can simply prompt their own agent to print `$DATABRICKS_TOKEN`, and the answer comes back over the relay as ordinary transcript text — no tool-path network egress required. That is consistent with what this section already says about the claude runtime ("none of them narrows what an agent can *do* once running") and with `config_schema`'s own warning that a sandbox-mode agent "can act AS YOU across the whole workspace". The defenses are anti-silent-forwarding controls, not a boundary against the payload author.


---

## 6. Milestones

**M0 — Scaffold + CI + contract freeze**
- *Precondition, not deliverable* (critique #5): freeze the wire contract against buzz source with cited line numbers — request payload (already done, §0), **response envelope + error shape + exit-code semantics from `backend.rs::invoke_provider`**, `info` shape from `agent_providers.rs`, persona/team + buzz-agent env-var checks from §4.4 step 6. Pure code-search; no infra.
- Deliverables: Go module per §3.2; `info` op; `doctor` (CLI presence/min-version — resolved from CLI changelog at M0 — profile check, sandbox-group reachability, version recording); redaction package; identity derivation (nsec→npub) with test vectors. CI: lint + tests + 4-target cross-compile + goreleaser dry-run.
- **Accept**: `echo '{"op":"info"}' | buzz-backend-databricks` matches the frozen contract byte-shape; unknown op returns the frozen error shape; contract-freeze doc committed with buzz file/line citations; CI green on free tier; `doctor` correctly reports a missing/old CLI (PATH-manipulation test).

**M0.5 — Probe session — ✅ COMPLETE 2026-07-24** (critique #6, #7, #8; results: `docs/M05_PROBE_RESULTS.md`, lane D)
- SSH-stdin passthrough: **verified**, byte-identical — primary transport selected, fallback unneeded (lane D §1).
- WSS-vs-idle: **autostop fires anyway** — continuous relay WSS traffic did not count as activity; sandbox stopped ~11–12 min after last SSH disconnect (lane D §4). Resolved Open Decision 3 → default `--no-autostop`.
- buzz-acp with a non-member key: **exit 1 in ~1 s**, no crash-loop; full log vocabulary captured (lane D §3) → defines M1's pass signal, N=10 s.
- buzz-agent verification = ACP `initialize` handshake (no `--version` — lane D §6). Boot-hook survey: **no hooks exist** (no cron/at; PID 1 is `sandbox-daemon`, systemd not booted — lane D §5) → the optional M3 supervisor is struck.
- Bonus probe (owner question): **AI Gateway inference proven end-to-end** in-sandbox — direct `POST {host}/ai-gateway/anthropic/v1/messages` 200 in 1.44 s, full buzz-agent ACP round-trip via `databricks_v2` in 3.5 s using `DATABRICKS_HOST`/`DATABRICKS_TOKEN` env (lane D §2).

**M1 — Working deploy (live, happy path)**
- Full §4 flow **including PAT reset + sha256 pin** (critique #4c), failure teardown, update-in-place, runtime verification. `deploy --payload-file` entrypoint.
- **Accept** (all observable, per M0.5-calibrated signals — lane D §3; throwaway nsec only):
  1. Fresh deploy, split by key class *(M0.5)*:
     - **(a) non-member throwaway key**: deploy's launch verification correctly reports the documented failure — buzz-acp exits 1 within ~10 s and `acp.log` contains `initial relay connect failed with terminal error` (plus the `buzz-acp starting: relay=…` startup line proving relay contact was attempted). Everything before launch must still hold: `{ok:false}` from verification, sandbox named `buzz-<npub12>-<slug>` exists, binaries + `buzz-agent` ACP-initialize-verified in `$HOME`, env file 0600, in-sandbox `databricks current-user me` **fails** (PAT reset).
     - **(b) member test key** (a real minted test agent): `{ok:true, agent_id}` < 180 s; `pgrep -f buzz-acp` alive at N=10 s; `acp.log` contains `agent_pool_ready agents=N` and no terminal-error line.
  2. Second identical deploy → same `agent_id`, no second sandbox in `sandbox list`, no .deb re-download, exactly one buzz-acp process group after (update-in-place proven).
  3. Induced mid-deploy failure (kill network during step 5) on a fresh create → `{ok:false}` containing the sandbox id, and the sandbox absent from `sandbox list` afterward (teardown proven).

**M2 — Lifecycle — code complete; live acceptance pending (docs/ACCEPTANCE.md, docs/RUNBOOK.md §9)**
- `status` (sandbox state + in-sandbox liveness + one-shot log tail + CLI version), `stop`, `start` (start → wait Running → `launch.sh`, PAT stub re-asserted), `logs` (one-shot), `undeploy` (shred → delete → forget the reuse mapping; typed-id confirmation, `--yes` for unattended use).
- All five are operator CLI subcommands: Buzz's provider protocol still has no v2 lifecycle ops, so the desktop cannot invoke them (`docs/UPSTREAM_BUZZ_GAPS.md` §5).
- **Accept**: `stop` → Stopped, process dead; `start` → Running and buzz-acp process group present ≤ 60 s without re-running deploy, and `current-user me` still fails in-sandbox; `undeploy` → sandbox gone from `sandbox list`; `status` emits stable JSON. Live checklist: `docs/RUNBOOK.md` §9.

**M3 — Hardening — code complete; live acceptance pending (docs/ACCEPTANCE.md, docs/RUNBOOK.md §9)**
- `keep_workspace_pat` opt-out matrix (CI proves the provider neither writes the stub at deploy nor re-asserts it from `launch.sh` on any later relaunch; the live half needs a real image); redaction audit (marker-secret fuzz across every deploy failure path, plus `redact.Log` credential-shaped scrubbing for the lifecycle paths, which have no payload to key on); double-launch guard proof; error taxonomy (`internal/deployflow/taxonomy.go`: stable code + single canonical remedy on every error, table kept in sync with the runbook by test); ~~optional in-sandbox supervisor~~ **struck** *(M0.5: no boot hooks exist — lane D §5; provider `start` is the only recovery path)*; operator runbook (`docs/RUNBOOK.md`: triage, autostop/lifetime-cap recovery, `!shutdown` interplay, Beta-storage-loss playbook, orphan cleanup, fresh-machine walkthrough, error-code reference).
- **Two silent-death defects found by making the guard proof executable** (both fixed, both regression-tested in `internal/nest/launch_exec_test.go`):
  1. *Zombie counted as alive.* buzz-acp is launched detached, so a crash reparents it to the sandbox's PID 1 (`sandbox-daemon`, not a reaping init — lane D §5). A `<defunct>` process still matches `pgrep -f`, so `status` would report a dead agent as running and `launch.sh` would refuse to relaunch it forever. All three liveness checks now share `nest.AliveCheckSnippet`, which skips `Z`-state pids.
  2. *Lock fd inherited by the agent.* `exec 9>"$LOCK"` was inherited by buzz-acp and every worker it spawns, so the flock stayed held for as long as ANY of them lived: a crashed agent with one lingering worker made every later `start` exit early with "already running (flock held)". The detached launch now closes fd 9 (`9>&-`); the lock serializes concurrent `launch.sh` runs only, and the alive check guards against a live agent.
- **Accept**: deploy with `keep_workspace_pat:true` → in-sandbox `current-user me` succeeds as creator *(live-only)*; marker-secret fuzz shows zero leakage in stdout/stderr/logs; deploy-twice yields exactly one process group; fresh-machine runbook walkthrough succeeds.
- Deferred post-M3 (critique #12): `logs -f`, recurring live canary, **goose** runtime. *(claude shipped in v0.1 via issue #2; codex via issue #3.)*

**v0.1 Codex runtime (issue #3) — code complete; live acceptance pending (docs/ACCEPTANCE.md, docs/RUNBOOK.md §9)**
- Gateway surface, adapter behaviour, and sandbox-image facts established by a live probe session before implementation: `docs/M3_CODEX_PROBE_RESULTS.md` (G1–G5 gateway, S1–S12 sandbox + adapter source). Four assumptions in issue #3 were overturned by that evidence and are corrected in §4.2/§4.4 above.
- Ships: `payload.RuntimeCodex` + alias canonicalization; a per-runtime adapter spec table in `internal/install` (replacing five hardcoded claude constants) with a committed `codex-acp@1.1.7` lockfile; per-runtime `payload.EnvShape` capabilities in `internal/nest` (replacing three `!= RuntimeClaude` branches that would silently have given codex buzz-agent's wiring); `nest.CodexEnvSnippet`; the `install.codex_inference` probe; and `payload.validateCodexInferenceSource`.
- **Not yet verified live:** an end-to-end provider deploy of a codex agent. The probe drove the adapter directly against the real gateway (`stopReason: end_turn`), but the provider's own deploy path has not been run against a real sandbox with a real relay. Two things to confirm there specifically: that buzz-acp's `CODEX_CONFIG` network injection actually opens egress for codex's tool path on **Linux** (upstream documents the mechanism in macOS Seatbelt terms — `docs/UPSTREAM_BUZZ_GAPS.md`), and that the S6 metadata warning remains cosmetic in a real session transcript.
- **Accept**: deploy with `agent_command: codex-acp` in both `inference_auth` modes → adapter installed, ACP handshake passes, `install.codex_inference` passes, agent answers a relay mention; a deploy naming any adapter env var under `inference_auth: "sandbox"` is rejected at the payload boundary.

**#16 Increment 2 — `bzmux` MCP multiplexer — code complete, live acceptance pending**
- `provider_config.mcp_servers` with ≥2 entries now deploys the embedded `bzmux` MCP multiplexer + a 0600 `mcp-mux.json` per-child config (no wire-field change; `mcp_servers []string` is unchanged — see `docs/CONTRACT.md §3`). Install sequence at deploy time: `mux-bin-write` → `mux-cfg-write` → `mux-selftest`. All CI-verifiable acceptance criteria (plan §6 of `.omc/plans/16-bzmux-multiplexer.md`) are green.
- Ships: `cmd/bzmux` (the stdio JSON-RPC multiplexer); `internal/muxbin` (embedded linux/amd64 binary + staleness guard); `internal/muxcfg` (frozen shared schema); `internal/install` `BuildMuxConfigJSON`/`BuildMuxInstallScript`; `internal/deployflow` `resolveMcpCommand` McpMux flip + `installMux` sequencing + three new taxonomy codes (`install.mux_write`, `install.mux_exec`, `install.mux_selftest`).
- **Not yet verified live:** an end-to-end provider deploy of a buzz-agent with `mcp_servers: [buzz-dev-mcp, shellbox-mcp]`. Live acceptance is infrastructure-gated (requires two real registered MCP commands on the sandbox) and feeds #17. Record in `docs/ACCEPTANCE.md` when observed.
- **Accept (live, infrastructure-gated):** deploy with two `mcp_servers` entries → both tool sets appear in one merged catalog; agent answers a relay mention; `_Stop` fan-out fires correctly to all children.

**#17 — deploy-time MCP slot verification (`bzmux --verify-mcp`) — code complete, live acceptance pending**
- The deploy now verifies whatever `BUZZ_ACP_MCP_COMMAND` resolves to with a real MCP handshake (`initialize` → `notifications/initialized` → `tools/list`), asserting a **non-empty tool catalog** always and **known tool names where the server is known** (`buzz-dev-mcp`, `shellbox-mcp`). It covers `McpDirect` (single command) and `McpMux` (multiplexer) with the same verification contract, and leaves `McpNone` byte-identical to pre-#17. No wire-shape change (`docs/CONTRACT.md §3`).
- Implemented as a new `bzmux --verify-mcp` mode reusing bzmux's own MCP client: **direct** (`--command <cmd>`) spawns the single command with the full inherited env; **mux** (no `--command`) reads `mcp-mux.json`, spawns the children directly, and runs the selftest's full check set (collision/`__`/64-byte budget + protocolVersion) **plus** the expected-names union — a superset of `--selftest`. In the deploy flow mux-mode `mcp-verify` **replaces** `mux-selftest`; `McpDirect` gains `mcp-bin-write` → `mcp-verify`. `child.initialize` gained a `context` parameter (two callers preserve the historical 30s).
- Ships: `cmd/bzmux/verify.go` (`runVerifyMCP` + `knownServerTools`), the `initialize(ctx,…)` refactor, `internal/install/mcpverify.go` (`BuildMcpVerifyCommand`), `internal/deployflow` `mcpVerify`/`installMcpDirect` wiring + the new `install.mcp_verify` taxonomy code (`CodeMuxSelftest` kept reserved). Budget `mcpVerifyTimeoutSeconds` is **provisional 15s** pending the live measurement.
- **Not yet verified live:** deploy an MCP entry that starts-then-exits → `install.mcp_verify` fail + remedy; deploy real `mcp_servers: [buzz-dev-mcp, shellbox-mcp]` → `mcp-verify` passes end-to-end and records the measured MCP cold-start/handshake latency to finalize the budget. Infrastructure-gated; record in `docs/ACCEPTANCE.md` when observed.

---

## 7. Testing strategy

**Unit (CI, no network)**
- Payload parse/validate golden fixtures using the §0 field list verbatim (incl. unsupported-runtime rejection, malformed, missing-secret).
- Identity: nsec→npub test vectors; sandbox-name derivation + prefix matching (0/1/many cases).
- Template golden files: launch.sh (asserts PAT re-assert, flock guard, `set +x`, no secret in argv), install script (sha256 pin), env file.
- Redaction: property-style — marker secrets in every payload field never reach rendered errors/logs.
- `lakebox` wrapper + full `deployflow` against a **fake `databricks` PATH shim** replaying lane-C-verified `--json` shapes (status `{sandboxId, status, gatewayHost, name, idleTimeout, noAutostop}`, create, list, start-during-stopping 4xx). Asserts call ordering (PAT-reset-first, autostop-last), reuse branching, and **failure-path teardown call sequences** for a failure injected at each step.

**Live (manual / `workflow_dispatch`-gated; region-gated Beta on a public repo — never default PR CI)**
- M0.5 probe session; M1/M2/M3 acceptance scripts. Throwaway keys only; real owner nsec and relay-membership credentials never enter CI.

---

## 8. Risks & mitigations

| Risk | Grounding | Mitigation |
|---|---|---|
| Preview API/CLI churn (renamed once; Beta) | lane C naming resolution | CLI not REST; min-version gate + version recorded in all outputs; pin at M0 |
| Duplicate sandboxes / double-responding harness on redeploy | §0 (no id round-trip); lane C proof note a | Deterministic npub-keyed reuse (§4.1); fail-loud on ambiguity; pgid kill before relaunch; flock guard |
| Partial deploy strands nsec / runaway sandbox | critique #3 | Teardown wrapper (§4.3); autostop-policy-last; sandbox id in every error |
| Agent deploys but can't run sessions (no runtime) | lane A §2B; lane C env | v0 buzz-agent-only + pre-provision rejection + in-sandbox runtime verification (§4.2) |
| ~~SSH-stdin transport unproven~~ resolved | lane D §1 | Verified byte-identical at M0.5; fallback documented, unneeded |
| Autostopped/lifetime-capped/Beta-wiped sandbox = silently dead agent; v1 has no status op | lane C persistence, gaps #1/#2; lane D §4 (autostop fires despite live relay traffic) | Default `--no-autostop` eliminates the autostop cause; silent death remains possible via Beta storage deletion/lifetime cap; relay observer frames are the only health signal (lane A §6). `start` subcommand is the recovery path; mention replay bounds damage (lane A §5) |
| Baked creator PAT grants agent owner powers | lane C environment | Reset first-in-sandbox-action + re-assert on every start (§4.4); opt-out explicit (Open Decision 2) |
| nsec in Databricks Beta infra | lane C gap #3 | §5 controls; per-agent keys; owner sign-off (Open Decision 1) |
| Fixed 4vCPU/8GiB/10GB too small for heavy work | lane C gap #4 | Document; disk check in `status`; no knobs exist |
| Region gating | README; lane C | Preflight/`doctor` explicit guidance |
| 600 s deploy timeout | lane A §3b; lane C timings | Only the .deb download is slow; cached on redeploy; bounded curl + one retry |
| buzz payload/envelope drift | lane A §3b (out-of-tree provider exists) | Contract frozen at M0 with citations; tolerate unknown fields; version-tagged `info` |

---

## 9. DECISIONS — all resolved (owner approval recorded 2026-07-24)

1. **nsec trust boundary — CONFIRMED.** Per-agent minted keys may ship into Databricks-managed Beta storage; rotation-on-suspicion is the remedy. §5 controls apply.
2. **Baked creator PAT — RESET in env/wire-default mode; retained by the explicit new-agent schema default.** The provider overwrites `~/.databrickscfg` with a stub (§4.4 step 4) whenever `inference_auth` is omitted or `env`; the agent then cannot act as the owner via ambient CLI credentials, and inference auth is passed explicitly as `DATABRICKS_HOST`/`DATABRICKS_TOKEN` in the agent env file. New Buzz agents instead persist `inference_auth: "sandbox"` from the provider schema and retain the creator credential with the controls above. `keep_workspace_pat: true` remains a separate documented operator opt-in. Least-privilege path: `env` with a service-principal token scoped to CAN QUERY on just the gateway endpoints.
3. **Compute posture — default `--no-autostop`** *(M0.5, lane D §4)*. The WSS-vs-idle probe showed the idle timer keys on SSH/gateway activity only: a healthily connected harness is autostopped ~11–12 min after the last SSH disconnect, and no boot hook exists to relaunch it (lane D §5). Default idle-timeout is therefore guaranteed silent death, not free reliability. `provider_config.idle_timeout` opts into autostop for owners who accept the 24/7 4vCPU/8GiB bill trade against `start`-based manual recovery.
4. **v0 supported runtime set — `buzz-agent` only.** goose/claude/codex rejected with a clear error until v0.1. **Superseded:** claude shipped in v0.1 ([#2](https://github.com/IceRhymers/buzz-lakebox/issues/2)) and codex in v0.1 ([#3](https://github.com/IceRhymers/buzz-lakebox/issues/3)); goose remains unsupported.

*(Remaining engineering-level open questions: §3.1 exact min CLI version — M0; §4.4 step 5 version pinning is by design since the payload carries no version — §0; §4.4 step 6 persona/env verification — M0 source pass. The M0.5-gated questions — §4.2 verification invocation, step-7 transport, step-10 signals/N — are resolved above.)*
