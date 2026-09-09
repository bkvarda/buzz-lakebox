# Operator runbook — `buzz-backend-databricks-lakebox`

For the person who owns a deployed agent when something looks wrong.
Everything here runs on **your** machine against **your** Databricks
profile; the provider binary is the only tool you need beyond the stock
`databricks` CLI.

`<id>` below is a sandbox id (e.g. `cherished-piglet-2474`). Every
subcommand resolves the sandbox for you when the profile has exactly one
— pass the id explicitly when it has several.

---

## 0. The one thing to internalize

**Buzz Desktop's "DEPLOYED" badge is a deployment receipt, not a health
signal.** It means "a deploy succeeded at some point", and it keeps
saying that after the sandbox has stopped, the agent has crashed, or the
storage has been wiped. The desktop also cannot invoke any lifecycle op —
Buzz's provider protocol has only `deploy` and `info` today (v2 ops are
deferred upstream; see `docs/UPSTREAM_BUZZ_GAPS.md` §1, §2, §5).

So: when an agent stops answering mentions, the desktop will not tell you
and will not fix it. **`status` is the source of truth, `start` is the
fix.**

```
buzz-backend-databricks-lakebox status        # sandbox state + agent liveness + log tail
buzz-backend-databricks-lakebox start         # bring a dead agent back
```

---

## 1. Triage: "my agent stopped answering"

```
buzz-backend-databricks-lakebox status
```

The JSON tells you which case you are in:

| `sandbox_status` | `acp_running` | What happened | Fix |
|---|---|---|---|
| `Running` | `true` | The agent is alive. The problem is elsewhere: relay membership, mention routing, or a mention sent while it was down (see §2). | `logs` |
| `Running` | `false` | The agent process died; the sandbox is fine. | `start` |
| `Stopped` | `false` | The sandbox stopped: manual `stop`, an opted-in idle timeout, or the platform's lifetime cap. Nothing inside relaunches the agent — there are no boot hooks (`docs/M05_PROBE_RESULTS.md` §5). | `start` |
| *(error `sandbox.status`)* | — | The sandbox no longer exists (deleted, or Beta storage loss). | §4 |

`status` exits non-zero when the agent is not running, so it composes in
scripts.

**Why did it stop?** Deploys set `--no-autostop` by default precisely
because relay WSS traffic does **not** count as sandbox activity: an idle
timeout kills a perfectly healthy, connected agent ~11–12 minutes after
your last SSH (`docs/M05_PROBE_RESULTS.md` §4). If you opted back in with
`provider_config.idle_timeout`, this is the expected cost, and `start` is
the accepted recovery.

---

## 2. Mentions sent while the agent was down are lost

buzz-acp subscribes to the relay from a **startup watermark** (`since =
start − 5s`), so mentions posted during an outage are never replayed —
the agent simply appears to ignore them. This is an upstream gap
(`docs/UPSTREAM_BUZZ_GAPS.md` §3), not something the provider can fix.

After any recovery, **re-send anything you sent while it was down.**

---

## 3. `!shutdown` and the stop channel

The desktop's only post-deploy control over a remote agent is the
relay-side `!shutdown` owner mention. Know what each one leaves behind:

| Action | buzz-acp | Sandbox | Billing | Recover with |
|---|---|---|---|---|
| `!shutdown` mention | exits | **keeps running** | yes | `start` |
| `stop` | dies with the sandbox | Stopped | no | `start` |
| `undeploy` | gone | **deleted** | no | redeploy from the desktop |

An `!shutdown` alone therefore stops the agent but not the bill. Follow it
with `stop` if you want the compute to go away too.

---

## 4. Sandbox gone: Beta storage loss / lifetime cap / manual delete

Lakebox is Beta: storage is explicitly not durable, and sandboxes can be
deleted out of band. Symptoms: `status` fails with `sandbox.status`, or
`databricks sandbox list` no longer shows the id.

1. Confirm: `databricks sandbox list -p <profile>`.
2. **Redeploy from Buzz Desktop.** The provider's state file still maps
   this agent to the dead id; the deploy's status probe rejects it and
   falls through to creating a fresh sandbox, then rewrites the mapping.
   Nothing manual is required.
3. The agent's `$HOME` is gone with it: any repos it had cloned and
   anything in `OUTBOX/` are not recoverable.

---

## 5. Orphaned / duplicate sandboxes

Reuse across redeploys is keyed by the provider's own state file
(`~/.local/state/buzz-lakebox/agents.json`), because the desktop never
echoes `backend_agent_id` back and Lakebox does not persist caller-set
sandbox names (`docs/UPSTREAM_BUZZ_GAPS.md` §4).

**If that file is lost**, the next deploy creates a *new* sandbox and the
old one keeps running with `--no-autostop` — billing forever, and
possibly double-responding on the relay.

Audit and clean up:

```
databricks sandbox list -p <profile>
cat ~/.local/state/buzz-lakebox/agents.json      # what the provider still tracks
buzz-backend-databricks-lakebox undeploy <id>    # for the stale one
```

An `identity.ambiguous` deploy error is the other face of this: two
sandboxes match one agent's name prefix. The provider refuses to guess —
running two harnesses on one key means both answer every mention. Delete
the stale one, then redeploy.

---

## 6. Retiring an agent

```
buzz-backend-databricks-lakebox undeploy <id>
```

Shreds the in-sandbox secret files (the env file holding the agent's
nsec, auth tag, and inference token), deletes the sandbox, then drops the
reuse mapping. It asks you to type the sandbox id back; `--yes` skips the
prompt and is **required** when stdin is not a terminal.

Notes:

- A stopped sandbox cannot be SSH'd into, so the shred is skipped and the
  secrets are destroyed with the sandbox's storage instead. The command
  says so on stderr.
- A failed shred never aborts the delete: a surviving sandbox is strictly
  worse (still billing, still holding the nsec).
- Also delete or rotate the agent in Buzz Desktop. Rotation is the remedy
  for any suspicion the key leaked — the nsec lived in Databricks-managed
  Beta infrastructure.

---

## 7. Reading a failure: error codes

Every `{"ok": false}` and every CLI error is shaped:

```
[<code>] <what went wrong> — remedy: <what to do> (sandbox <id>, databricks cli <version>)
```

Match on the code, not the prose.

| Code | Remedy |
|---|---|
| `validation` | fix the deploy payload field named above and redeploy |
| `preflight.cli_version_unknown` | install the Databricks CLI and make sure it is on PATH (`databricks version` must work); run `buzz-backend-databricks-lakebox doctor` |
| `preflight.cli_too_old` | upgrade the Databricks CLI, then rerun `buzz-backend-databricks-lakebox doctor` |
| `preflight.profile` | check ~/.databrickscfg for the profile and re-authenticate (`databricks auth login -p <profile>`) |
| `preflight.sandbox_register` | confirm the workspace is in a Lakebox-enabled region (us-west-2 verified) and that your profile may use sandboxes |
| `identity.derive` | the agent's private_key_nsec is not a valid bech32 nsec — re-mint the agent's key in Buzz Desktop and redeploy |
| `identity.ambiguous` | delete the stale sandbox(es) listed above with `databricks sandbox delete <id>`, then redeploy |
| `state.read` | inspect (or delete) ~/.local/state/buzz-lakebox/agents.json — a corrupt mapping file blocks reuse; deleting it costs only orphan detection, which `databricks sandbox list` can replace |
| `state.write` | make ~/.local/state/buzz-lakebox writable — without a persisted mapping every redeploy orphans a still-billing sandbox |
| `sandbox.list` | verify sandbox access with `databricks sandbox list -p <profile>` |
| `sandbox.create` | check the workspace's sandbox quota and region gating with `databricks sandbox list -p <profile>` |
| `sandbox.start` | retry; if the sandbox is mid-transition (Stopping), wait for it to settle and redeploy |
| `sandbox.wait_running` | check the sandbox with `databricks sandbox status <id>` — it may be stuck mid-transition; retry once it settles |
| `sandbox.status` | confirm the sandbox still exists with `databricks sandbox list -p <profile>`; if it was deleted, redeploy to create a new one |
| `sandbox.stop` | retry, or stop it directly with `databricks sandbox stop <id>` |
| `sandbox.delete` | delete it manually with `databricks sandbox delete <id> --auto-approve` — an undeleted sandbox keeps billing |
| `provision.pat_reset` | retry; if it persists the sandbox may be unreachable over SSH — check `databricks sandbox ssh <id> -- true` |
| `provision.sandbox_auth` | sandbox-mode only (`inference_auth: "sandbox"`); three disambiguated causes — see [Zero-token inference auth](#zero-token-inference-auth-inference_auth-sandbox) below for the full remedy per cause |
| `install.script` | pass a known `provider_config.buzz_version` (the error lists the pinned versions this build ships sha256s for) |
| `install.write` | check sandbox SSH reachability with `databricks sandbox ssh <id> -- true` |
| `install.exec` | read the install output above: a sha256 mismatch means the pinned release changed; a fetch failure means the sandbox lost egress to GitHub |
| `install.buzz_skill` | check sandbox SSH reachability and that `$HOME/.buzz` is writable; the embedded Buzz CLI skill could not be refreshed |
| `install.skills_config` | fix `provider_config.skills_config`; only the versioned `buzz-skills` schema and safe skill names are accepted |
| `install.skills_exec` | check `databricks aitools` availability and output; remove unmanaged name collisions or choose `replace-managed` only for provider-marked skills |
| `install.adapter_script` | pass a known `provider_config.claude_adapter_version` (the error lists the adapter versions this build ships a pinned package-lock.json for) |
| `install.adapter_write` | check sandbox SSH reachability with `databricks sandbox ssh <id> -- true` |
| `install.adapter_exec` | read the npm output above: an integrity mismatch means the registry served different bytes than the pinned lockfile — do NOT retry, report it; anything else is usually lost sandbox egress to registry.npmjs.org, which is safe to retry |
| `install.runtime_verify` | the installed agent runtime could not complete an ACP initialize handshake — check `logs` and the inference env for that runtime (buzz-agent: BUZZ_AGENT_PROVIDER / DATABRICKS_HOST / DATABRICKS_TOKEN; claude: ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / DATABRICKS_HOST; codex: CODEX_HOME and the generated `$HOME/.buzz-backend/codex/config.toml`) |
| `install.claude_inference` | the agent installed and handshook, but could not reach the AI Gateway: confirm the workspace serves `{host}/ai-gateway/anthropic/v1/messages` and that the credential is accepted there — in `inference_auth: "env"` check env_vars DATABRICKS_HOST/DATABRICKS_TOKEN, in `"sandbox"` mode retry or fall back to env mode for this agent |
| `install.codex_inference` | same, for the codex runtime: confirm the workspace serves `{host}/ai-gateway/codex/v1/responses` and that the credential is accepted there — in `inference_auth: "env"` check env_vars DATABRICKS_HOST/DATABRICKS_TOKEN, in `"sandbox"` mode retry or fall back to env mode. An "unset" cause means no `config.toml` was generated at all, which is the fail-closed path working |
| `install.extra_bin_script` | the extra-binaries install script could not be rendered — a `provider_config.extra_binaries` entry is malformed or two entries share the same `bin`; fix the payload and redeploy |
| `install.extra_bin_write` | check sandbox SSH reachability with `databricks sandbox ssh <id> -- true` |
| `install.extra_bin_exec` | read the install output above: a sha256 mismatch means the pinned URL served different bytes than `provider_config.extra_binaries[].sha256` (do NOT retry — re-pin the sha256 or the URL); a fetch failure means the sandbox lost egress to the download host |
| `install.mux_write` | check sandbox SSH reachability with `databricks sandbox ssh <id> -- true`; the MCP multiplexer binary or config could not be written to the sandbox |
| `install.mux_exec` | check sandbox SSH reachability and that `$HOME/.buzz-backend/bin` is writable; re-running the deploy usually fixes a transient permission failure |
| `install.mux_selftest` | the MCP multiplexer failed its deploy-time self-test — check each entry in `provider_config.mcp_servers` is a valid, installed MCP command on the sandbox; run `databricks sandbox ssh <id> -- $HOME/.buzz-backend/bin/bzmux --selftest` for the full error |
| `install.mcp_verify` | the MCP server the agent will use failed a deploy-time initialize+tools/list check — the error text distinguishes a server slow to start (raise the verify budget) from one that is broken or exports the wrong/too-few tools (fix the command): confirm `BUZZ_ACP_MCP_COMMAND` (resolved from `provider_config.mcp_servers`) names a valid, installed MCP server that returns its expected tools, and run `databricks sandbox ssh <id> -- $HOME/.buzz-backend/bin/bzmux --verify-mcp --command <cmd>` for the full error |
| `provision.env_write` | check sandbox SSH reachability and that $HOME is writable in the sandbox |
| `launch.prelaunch_kill` | check sandbox SSH reachability with `databricks sandbox ssh <id> -- true` |
| `launch.stale_agent` | a previous buzz-acp was still shutting down and did not exit — run `status <sandbox-id>` to confirm, then `stop <sandbox-id>` followed by a redeploy; if it persists the old process is wedged and the sandbox needs a restart |
| `launch.write` | check sandbox SSH reachability and that $HOME is writable in the sandbox |
| `launch.exec` | run `logs <sandbox-id>` for the agent's own output, then `start <sandbox-id>` to retry the launch |
| `verify.unreachable` | the sandbox stopped responding right after launch — run `status <sandbox-id>`, then `start <sandbox-id>` |
| `verify.unparseable` | run `status <sandbox-id>` and `logs <sandbox-id>` to see the agent's real state before redeploying |
| `verify.process_dead` | run `logs <sandbox-id>` for the crash output; the acp.log tail is included above |
| `verify.relay_denied` | mint or register a relay-member key for this agent in Buzz Desktop and redeploy — this key is not a member of the target relay, or the payload's auth_tag is missing/stale (the relay denies a member key with an empty auth tag the same way) |
| `verify.pool_not_ready` | run `logs <sandbox-id>`; the agent started but never reported a ready pool within the verification window |
| `autostop.config` | run `databricks sandbox config <sandbox-id> --no-autostop` manually, or redeploy — the agent itself is healthy |
| `lifecycle.not_deployed` | deploy this sandbox first (from Buzz Desktop, or `deploy --payload-file`) |
| `lifecycle.logs_read` | confirm the sandbox is Running with `status <sandbox-id>` — a stopped sandbox has no readable log |
| `lifecycle.status_probe` | confirm sandbox SSH reachability with `databricks sandbox ssh <id> -- true` |
| `lifecycle.status_unparseable` | run `logs <sandbox-id>` for the agent's raw output, or inspect the sandbox directly with `databricks sandbox ssh <id> -- true` |

The table is kept honest by `TestRunbook_DocumentsEveryCode` — a new code
without a row here fails CI.

### What never appears in these outputs

Secrets. The provider scrubs payload-derived values (nsec, auth tag,
every `env_vars` value) from every error, and additionally scrubs
credential-shaped `NAME=value` pairs and bare `nsec1…` tokens out of any
log tail it renders — including `logs` and `status`, which read the
agent's own output. Diagnostics that are *not* secret (relay URL, the
agent's `pubkey=…`) are left intact. If you need a genuinely raw log,
read it in place: `databricks sandbox ssh <id> -- cat '$HOME/.buzz-backend/acp.log'`.

---

## 7b. Claude Code runtime

Deploy an agent on this runtime with `agent_command: "claude-code"` (aliases: `claude-agent-acp`, `claude-code-acp`, `claudecode`). It is installed as an npm ACP adapter, not from the Buzz `.deb`.

**`install.adapter_exec` — adapter install failed.** Read the npm output in the error. An *integrity mismatch* means the registry served different bytes than the committed lockfile pins: **do not retry**, report it. Anything else is usually lost sandbox egress to `registry.npmjs.org`, which is safe to retry. A cold install takes ~6 s and ~570 MB, so a long hang is a network problem, not a slow install.

**`install.runtime_verify` on a claude deploy — adapter not spawnable.** buzz-acp spawns the agent by bare name, so the adapter must be at `$HOME/.buzz-backend/bin/claude-agent-acp`. Check it:

```sh
databricks sandbox ssh <id> -p <profile> -- 'ls -l $HOME/.buzz-backend/bin/claude-agent-acp && $HOME/.buzz-backend/bin/claude-agent-acp --help >/dev/null 2>&1; echo rc=$?'
```

**`install.claude_inference` with cause "unset" — no endpoint, and deliberately no token.** This is the fail-closed path doing its job, not a bug: with no `DATABRICKS_HOST` the provider emits **neither** `ANTHROPIC_BASE_URL` nor `ANTHROPIC_AUTH_TOKEN`, because Claude Code falls back to `https://api.anthropic.com` when the base URL is unset and the sandbox has open egress there — so emitting the token alone would send a live workspace PAT to a third party. Fix the payload: supply `env_vars.DATABRICKS_HOST` (with `DATABRICKS_TOKEN`), or set `provider_config.inference_auth: "sandbox"`, or supply `ANTHROPIC_BASE_URL` **and** `ANTHROPIC_AUTH_TOKEN` together.

**`install.claude_inference` / `install.codex_inference` with cause "malformed" — the credential was refused before use.** The token contains a character outside `A-Za-z0-9._~+/=-`. The probes hand the bearer to `curl` through a config file rather than argv, and a config file is *parsed*: a quote or newline in the value would let it inject further `curl` directives, so anything outside the charset fails closed. This is usually not an attack — the most common cause is a trailing space or a CRLF line ending on the `token =` line of the sandbox's `~/.databrickscfg`, or stray quoting in `env_vars`. Inspect the credential's exact bytes; the endpoint is not the problem here.

**`install.codex_inference` with cause "unset" — no config was generated, and deliberately so.** Same fail-closed design as the claude case above, with a different fallback to avoid: the codex runtime takes its endpoint from a generated `config.toml` under a provider-owned `CODEX_HOME`, and that file is written **only** when the provider derived the host itself. With no host it writes nothing — because if `CODEX_HOME` pointed nowhere, codex would read `~/.codex/config.toml`, which in a Lakebox sandbox is a **symlink to the image's baked gateway config** whose `auth.command` reads the workspace PAT straight out of `~/.databrickscfg`. The agent would work, on an owner-level credential, outside every provider gate. Fix the payload the same way: supply `env_vars.DATABRICKS_HOST` (with `DATABRICKS_TOKEN`), or set `provider_config.inference_auth: "sandbox"`.

**`install.claude_inference` with cause "auth" — the credential was refused (401/403).** In `env` mode the supplied `DATABRICKS_TOKEN` is not authorized for the gateway (it needs CAN QUERY on the endpoints). In `sandbox` mode the sandbox's baked credential was rejected — fall back to `env` mode for that agent. Note the probe deliberately does **not** fail on other statuses: any answer other than 401/403 already proves the endpoint is reachable and the credential accepted.

**`launch.stale_agent` — a previous buzz-acp would not exit.** The provider SIGTERMs the old agent and waits up to 15 s (buzz-acp drains its pool on shutdown), then escalates to SIGKILL. Reaching this code means one survived both. Run `status <id>`, then `stop <id>` and redeploy; if it persists the process is wedged and the sandbox needs a restart. The provider refuses to launch over it on purpose: `launch.sh` will not relaunch while an agent is alive, and `acp.log` is append-only, so proceeding would let the *previous* deploy's readiness line verify this one while the old runtime kept serving with the old environment.

**Model selection is not configurable on this runtime.** `agent.model` is ignored. A Databricks gateway model id (`databricks-claude-*`) gets rewritten by the Anthropic SDK into a canonical id the gateway does not serve, which fails every turn — so the provider emits no model variable and the adapter uses its own default.

**Codex: the "model metadata not found" warning is expected, and is not a bug.** The first agent message of **every** codex session reads:

```
Warning: Model metadata for `databricks-gpt-5-3-codex` not found.
Defaulting to fallback metadata; this can degrade performance and cause issues.
```

It arrives as transcript text, not stderr, so it is visible in the desktop. It is structurally unavoidable and was not introduced by this provider: the AI Gateway accepts **only** `databricks-`-prefixed model ids, while codex's own metadata table contains **only** unprefixed ids — the two sets are disjoint by construction. Setting `model_context_window`/`model_max_output_tokens` explicitly does not suppress it (retested). Sessions complete normally (`stopReason: end_turn`). Do not file it; do not "fix" it by changing the model id, which will instead produce a 404 from the gateway.

**The provider refuses some `env_vars` names, and the two rules differ.** Under `inference_auth: "sandbox"` ONLY, it refuses the credential pair — `DATABRICKS_HOST`, `DATABRICKS_TOKEN`, `DATABRICKS_CONFIG_FILE`, `DATABRICKS_CLIENT_ID`/`_SECRET`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` — because that is the only mode where the provider derives the sandbox's baked owner PAT for you, and these decide where it gets sent. Under `inference_auth: "sandbox"` **or** `keep_workspace_pat: true`, it additionally refuses names that decide what code runs beside that credential, or that make the provider's own stack read it: `BUZZ_ACP_AGENT_COMMAND`/`_ARGS`/`BUZZ_ACP_MCP_COMMAND`; `BUZZ_ACP_BASE_PROMPT_FILE`/`_SYSTEM_PROMPT_FILE`/`_HEARTBEAT_PROMPT_FILE` (buzz-acp reads these paths itself and prepends the contents to every prompt it sends to the inference endpoint — point one at `~/.databrickscfg` and the token leaves in the prompt body); `BUZZ_ACP_RELAY_OBSERVER` (the desktop's only health signal); proxy and CA-bundle variables; `PATH`/`SHELL`/`BUZZ_SHELL`/`HOME`/`BASH_ENV`/`ENV`/`IFS`; and the `NODE_*`/`LD_*`/`DYLD_*`/`npm_config_*`/`PYTHON*`/`GIT_*` loader prefixes. The codex-runtime set (`CODEX_PATH`, `CODEX_HOME`, `CODEX_CONFIG`, `MODEL_PROVIDER`, `DEFAULT_AUTH_REQUEST`, `INITIAL_AGENT_MODE`, `CODEX_API_KEY`, `OPENAI_API_KEY`, `APP_SERVER_LOGS`) is refused under the same condition. **If you use `keep_workspace_pat: true` you can still supply your own `DATABRICKS_HOST`/`DATABRICKS_TOKEN`** — that mode derives nothing, so the token in the agent's environment is yours. Note the baked PAT is still on disk in that mode, which is why the code-selection names stay refused. Keys colliding with the provider's own shell scratch namespace (`buzz_*`, `BUZZ_PROBE_*`, `ENVF`, `OUTF`) are always rejected, in every mode. Under `inference_auth: "env"` the code-selection names are all permitted — the token is your own — but `CODEX_PATH` will still break the runtime outright: the adapter spawns it as `<path> app-server`, and the image's `/usr/local/bin/codex` is a `ucode` wrapper that accepts no arguments.

**Codex: inspecting the generated config.** The runtime's endpoint lives in a file the provider regenerates on every launch, not in an environment variable:

```
databricks sandbox ssh <id> -p <profile> -- 'cat "$HOME/.buzz-backend/codex/config.toml"'
```

An **absent** file is meaningful, not broken: it means the provider could not derive a host and deliberately declined to write one (see `install.codex_inference` cause "unset" in §7). Do not hand-write one to work around that — it will be removed on the next launch, and the reason it is missing is that something upstream of it is misconfigured.

**Resource and disk notes.** Each adapter process is ~115 MB RSS at idle, so `parallelism: N` costs roughly `N × 115 MB` on the sandbox's 8 GiB before session working set. Claude Code also writes conversation transcripts under `~/.claude/`. If a long-lived `--no-autostop` sandbox runs low on space:

```sh
databricks sandbox ssh <id> -p <profile> -- 'du -sh ~/.claude ~/.buzz-backend; df -h $HOME | tail -1'
```

Switching an agent *away* from the claude runtime deliberately leaves `~/.buzz-backend/npm-claude` (~570 MB), `~/.claude`, and the `bin/claude-agent-acp` symlink in place — they are harmless and re-used if the agent switches back. Delete them by hand if you need the space.

## 8. Fresh-machine walkthrough

For a new operator machine, in order:

```
# 1. Databricks CLI present, recent, and authenticated to a Lakebox region
databricks version
databricks auth login -p <profile>

# 2. Install the provider (from a source checkout)
make install PROFILE=<profile>

# 3. Environment check — non-destructive, no deploy
buzz-backend-databricks-lakebox doctor

# 4. Confirm Buzz Desktop can see it
#    (providers are PATH-discovered; the profile picker should list
#     "Databricks Lakebox")
echo '{"op":"info"}' | buzz-backend-databricks-lakebox

# 5. Deploy an agent from Buzz Desktop, then verify from here
buzz-backend-databricks-lakebox status
```

`doctor` is the fast triage for steps 1–3: it checks CLI presence and
minimum version, that the profile resolves, and that the sandbox command
family is reachable, and prints a JSON summary.

---

## 9. Live acceptance checks

CI covers the flow against a fake `databricks` shim; these are the checks
that need real infrastructure. Use a **throwaway nostr key** for anything
that is not the member-key case.

**Deploy (PLAN §6 M1)**

1. **Non-member throwaway key, fresh create** — PLAN §6 M1 1(a)

   A fresh-create verify failure runs §4.3 failure teardown (secret shred,
   then sandbox delete), so there is no sandbox left to inspect after the
   deploy returns. The in-sandbox checks below are therefore observed
   **during** the deploy: run the deploy in the background, wait for its
   sandbox id to appear in `databricks sandbox list`, then probe over SSH
   once the env file exists (the window between env-write and teardown is
   roughly 15 s).

   - `deploy` returns `{"ok": false}` with code `verify.relay_denied`
   - a sandbox was created for the attempt (the error text names its id)
     and is **absent** from `databricks sandbox list` once teardown's
     delete settles — nothing to clean up manually
   - mid-deploy: `$HOME/.buzz-backend/bin/` holds all five symlinked
     binaries (`buzz-acp`, `buzz`, `buzz-agent`, `buzz-dev-mcp`,
     `git-credential-nostr`); the deploy proceeding past `install-exec`
     to launch is the evidence the `buzz-agent` ACP-`initialize`
     handshake verified
   - mid-deploy: `stat -c %a $HOME/.buzz-backend/env` → `600`
   - mid-deploy: in-sandbox `databricks current-user me` **fails** (PAT reset)
   - `acp.log` contains `buzz-acp starting: relay=` and
     `initial relay connect failed with terminal error` — both readable
     from the log tail the deploy embeds in its error text
   - buzz-acp exited within ~10 s: the deploy's own N=10 s process check
     is what routes the failure to `verify.relay_denied` (a dead process
     plus the terminal-error line), so the error code doubles as this
     check's pass signal

2. **Member test key, fresh create** — 1(b)
   - `{"ok": true, "agent_id": …}` in < 180 s — **record the wall-clock number**
   - at N=10 s: `pgrep -f '[b]uzz-acp'` matches a non-zombie pid
   - `acp.log` contains `agent_pool_ready agents=N`
   - `acp.log` contains **no** terminal-error line
   - `status <id>` → `acp_running: true`

3. **Redeploy the same agent** — item 2
   - same `agent_id`; no second sandbox in `databricks sandbox list`
   - **no .deb re-download**: `stat -c %Y $HOME/.buzz-backend/dist/.installed_version` is **identical** before and after. (The install script's download branch does `rm -rf "$DIST_DIR"` and rewrites the marker — see `internal/install/install.go` — while the skip branch only reads it, so an unchanged mtime is exact proof the skip path was taken.)
   - exactly one buzz-acp **process group**: `ps -o pgid= -p $(pgrep -f '[b]uzz-acp' | tr '\n' ',' | sed 's/,$//') | tr -d ' ' | sort -u | wc -l` → `1`

4. **Induced mid-deploy failure on a fresh create** — item 3
   - **Mechanism:** while the deploy is inside `install-exec` (the `.deb` fetch — the widest window), run **once** on the operator machine: `pkill -f 'sandbox ssh'`. This kills the deploy's `databricks sandbox ssh` child while leaving the control plane reachable, which is what teardown needs — teardown runs on its own `context.Background()` with a 60 s timeout, independent of the deploy context. Do **not** disable host networking or firewall the workspace: those also break `sandbox delete` and orphan the very sandbox you are trying to prove gets torn down. Fire it once, not in a loop — teardown spawns its own `sandbox ssh` for the secret shred, and a repeated pkill would hit that too (harmless, the result is discarded, but know why).
   - `{"ok": false}` naming the sandbox id
   - the sandbox absent from `databricks sandbox list` afterwards
   - **Fallback:** if teardown could not reach the API, delete the sandbox manually per §5 and record the manual step in `docs/ACCEPTANCE.md` as `LIVE (teardown failed; manual cleanup)`. That is a finding, not a pass.

Two places this procedure is a proxy rather than the literal PLAN clause, so a
LIVE row is not over-read:

- PLAN 1(a) says buzz-acp "exits 1"; the pgrep-absence check above is a
  stand-in, because a detached process's exit code is not observable to the
  operator.
- PLAN §4.4 step 5 symlinks five binaries (`buzz-acp`, `buzz`, `buzz-agent`,
  `buzz-dev-mcp`, `git-credential-nostr`); check 1 above only confirms two of
  them (`buzz-acp`, `buzz-agent`).

**Lifecycle (PLAN §6 M2)**

```
buzz-backend-databricks-lakebox stop <id>       # -> Stopped, agent dead
buzz-backend-databricks-lakebox start <id>      # -> Running + agent alive in <= 60s, no redeploy
databricks sandbox ssh <id> -- databricks current-user me   # must still FAIL (PAT stub survived the restart)
buzz-backend-databricks-lakebox status <id>     # stable JSON
buzz-backend-databricks-lakebox undeploy <id>   # -> gone from sandbox list
```

**PAT opt-out (PLAN §6 M3)**

Deploy with `provider_config.keep_workspace_pat: true`, then:

```
databricks sandbox ssh <id> -- databricks current-user me   # must SUCCEED, as the workspace creator
```

This is the one acceptance CI cannot express — it depends on the image's
baked credential. CI does prove the two decisions behind it: with the
opt-out, the provider neither writes the stub during deploy nor
re-asserts it from `launch.sh` on any later relaunch.

---

## Zero-token inference auth (inference_auth: "sandbox")

Setting `provider_config.inference_auth` to `"sandbox"` opts an agent into
zero-token inference: instead of resetting the sandbox's baked
creator-identity PAT to a stub, the provider leaves `~/.databrickscfg`
alone and derives `DATABRICKS_HOST`/`DATABRICKS_TOKEN` from it.

**How derivation works.** The env file the provider writes at deploy time
carries a static POSIX-sh snippet, appended after any explicit
`env_vars`: if `DATABRICKS_TOKEN` is unset and `~/.databrickscfg` is
readable, it awk-parses the `[DEFAULT]` section for `host`/`token` and
exports each **only if currently unset** — so explicit `env_vars` always
win. This snippet re-runs on **every** launch, not just the first:
`launch.sh` sources the env file both at deploy time and on every
operator `start`, so a PAT rotated by the platform between a sandbox stop
and start is picked up automatically, by construction — there is nothing
to re-derive manually.

**Unknown baked-PAT lifetime.** How long the sandbox image's baked PAT
stays valid is not documented upstream and hasn't been characterized here
beyond what a single stop/start cycle shows (see `docs/ACCEPTANCE.md`'s
rotation-tolerance row for what was actually observed live). Treat it as
an unknown, not a guarantee.

**Deploy-time probe and its three causes.** Because sandbox mode skips
the PAT reset, deploy instead runs an auth probe that exercises the same
derivation snippet the launch env will use, then validates the derived
credential with `databricks current-user me`. A probe failure surfaces as
`[provision.sandbox_auth]` with one of three disambiguated causes:

- **(a) stub marker present** — `~/.databrickscfg` still carries the
  provider's own stub marker, meaning this sandbox was previously
  deployed in **env mode**: the baked PAT it once had is already gone and
  unrestorable. Remedy: `databricks sandbox delete <id>`, then redeploy
  fresh in sandbox mode. (If `env_vars`-supplied
  `DATABRICKS_HOST`/`DATABRICKS_TOKEN` are present in the payload, they'd
  take precedence over derivation anyway — the stub is still the blocker
  for the *derived* path.)
- **(b) cfg missing or unparseable** — the derivation snippet itself
  couldn't extract both a host and a token from `~/.databrickscfg`
  (missing file, missing `[DEFAULT]` section, or a format the awk parser
  doesn't tolerate). Remedy: inspect the cfg directly —
  `databricks sandbox ssh <id> -- cat ~/.databrickscfg` — and compare
  against the parser's tolerances (section-scoped, whitespace-tolerant,
  values taken verbatim).
- **(c) derived credential rejected** — the snippet produced a host and
  token, but `databricks current-user me` failed using them. The baked
  PAT is invalid, expired, or the workspace was unreachable (this can
  also be a transient network/gateway error — the CLI's own error text is
  included in the remedy). Remedy: retry if it looks transient;
  otherwise the baked credential needs the platform to refresh it, or
  fall back to env mode for this agent.

A probe failure never tears down a sandbox that was already running a
healthy agent — only a **freshly created** sandbox with a broken baked
PAT gets deleted (it has zero value and would otherwise bill forever). A
reused sandbox's existing agent and env file survive a failed switch
attempt untouched.

**Triage: agent alive but silent.** If `status` shows the agent running
in sandbox mode but it never responds to mentions, check `logs` for a
401/403 from the AI Gateway. That's silent mid-life PAT expiry — the
baked credential died after deploy succeeded, with nothing in between to
detect it. This is the **accepted residual risk** of zero-token mode:
remedy is to redeploy (which re-runs the probe against the current cfg)
or switch the agent back to env mode.
