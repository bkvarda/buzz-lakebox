# buzz-lakebox

A [Buzz](https://github.com/block/buzz) agent-backend provider that runs agent sessions in ephemeral **Databricks Sandboxes** (Lakebox) instead of on the owner's machine.

## How it works

Buzz Desktop discovers `buzz-backend-<id>` executables on `PATH` (`BackendKind::Provider`) and hands them a one-shot JSON payload over stdin. This repo builds **`buzz-backend-databricks-lakebox`**: on `deploy` it provisions a Databricks Sandbox, installs the Buzz harness into the sandbox's persistent `$HOME`, ships the agent's identity/env over SSH stdin, and launches `buzz-acp` as a detached background process. Three agent runtimes are supported: **`buzz-agent`** (ships in the pinned Buzz `.deb`), **Claude Code** (`agent_command: claude-code`, installed as the `@agentclientprotocol/claude-agent-acp` ACP adapter), and **Codex** (`agent_command: codex-acp`, installed as the `@agentclientprotocol/codex-acp` ACP adapter), all taking inference from the workspace AI Gateway. The agent then talks to the Buzz relay outbound-only over WSS — no inbound connectivity to the sandbox is required for sessions.

```
Buzz Desktop ──stdin JSON {op:"deploy",...}──> buzz-backend-databricks-lakebox
                                                  │  /api/2.0/lakebox/* (create/config)
                                                  │  ssh (install binaries, ship env)
                                                  ▼
                                        Databricks Sandbox (microVM)
                                          └─ buzz-acp ──WSS──> Buzz relay ──> agent runtime
```

## Inference auth: zero-token for new Buzz agents or bring a scoped token

The provider schema defaults **new Buzz-created agents** to `provider_config.inference_auth="sandbox"`, the only current zero-manual-token path. Buzz persists that value explicitly. The provider drops the Databricks inference host/token fields Buzz required for local model selection, then leaves the sandbox's baked creator-identity `~/.databrickscfg` in place and derives a fresh `DATABRICKS_HOST`/`DATABRICKS_TOKEN` pair from it at every launch. Stated bluntly: **the agent can act as you across the entire workspace**, not just call the AI Gateway. Arbitrary managed MCP and skill sync are refused beside that credential.

Wire compatibility is intentionally different from the create-form default: an omitted or empty `inference_auth` still means `env`. Existing agents and hand-written payloads therefore never gain creator-identity access merely by upgrading or redeploying.

For a narrower grant, set `inference_auth="env"`, mint a personal access token or preferably a scoped service-principal token, and set `DATABRICKS_HOST`/`DATABRICKS_TOKEN` in the agent's environment variables (see [Setting up an agent in Buzz Desktop](#setting-up-an-agent-in-buzz-desktop) below). The token needs only the permissions the agent requires, such as CAN QUERY on selected gateway endpoints, and the sandbox's baked creator credential is neutralized. See [`docs/RUNBOOK.md`](docs/RUNBOOK.md#zero-token-inference-auth-inference_auth-sandbox) for derivation, rotation tolerance, and failure recovery.

## Provider protocol

| Op | Behavior |
|---|---|
| `deploy` | Reuse-or-create a sandbox → install pinned Buzz binaries into `$HOME` → write deploy-payload env (0600, via SSH stdin — never argv) → `setsid nohup buzz-acp` → verify → set autostop policy → return `{ok: true, agent_id: "<sandbox-id>"}`. Reuse is keyed by the provider's own state file (`~/.local/state/buzz-lakebox/agents.json`, npub→sandbox-id per profile): the desktop does not echo `backend_agent_id` back, and Lakebox does not persist caller-set sandbox names, so without this file every redeploy would orphan the previous sandbox |
| `info` | Provider name/version/description |
| start/status/logs/stop/undeploy | Not in Buzz's provider protocol yet ("v2"), so the desktop can't invoke them — all five are implemented as operator CLI subcommands instead (see [Operating a deployed agent](#operating-a-deployed-agent) and the [runbook](docs/RUNBOOK.md)) |

Auth: the operator's existing `~/.databrickscfg` profile is used to provision the sandbox itself. An explicit `provider_config.profile` always wins; when it is empty, provider mode discovers local profile names without authenticating them, prefers the baked default when present, or selects the only discovered profile. It refuses to guess between multiple profiles. The agent's own inference auth is a separate knob, `provider_config.inference_auth` — see [Inference auth](#inference-auth-zero-token-for-new-buzz-agents-or-bring-a-scoped-token) above. The Databricks Sandbox preview is region-gated (verified in us-west-2).

## Quick start for Buzz Desktop

Buzz has a native extension point for remote execution providers: any trusted
executable named `buzz-backend-<id>` is discovered and offered in the agent
form's **Run on** selector. This project uses that interface; there is no
separate plugin screen or manual provider registration.

Lakebox and **Databricks v2** serve different purposes and are selected in two
different parts of the same form:

- **LLM provider → Databricks v2** chooses the workspace AI Gateway for model
  inference.
- **Run on → databricks-lakebox** moves the entire agent harness, MCP servers,
  skills, and session execution into a Databricks Sandbox.

### 1. Install the released provider

Prerequisites are the [`databricks` CLI](https://docs.databricks.com/dev-tools/cli/)
**1.8.0 or newer** and at least one configured `~/.databrickscfg` workspace
profile with Sandbox access. Deployment discovers local profile names and
registers the Sandbox SSH key automatically.

Download the archive for your OS and architecture plus `checksums.txt` from
the [latest release](https://github.com/bkvarda/buzz-lakebox/releases/latest).
Before extracting it, verify the archive from the download directory (set
`archive` to its exact filename), then install the executable:

```sh
archive=buzz-backend-databricks-lakebox_0.2.0_darwin_arm64.tar.gz

# Choose the verifier for your OS:
grep "  $archive$" checksums.txt | shasum -a 256 -c -       # macOS
# grep "  $archive$" checksums.txt | sha256sum -c -          # Linux

mkdir -p ~/.local/bin
tar -xzf "$archive" buzz-backend-databricks-lakebox
install -m 0755 buzz-backend-databricks-lakebox ~/.local/bin/
~/.local/bin/buzz-backend-databricks-lakebox version
~/.local/bin/buzz-backend-databricks-lakebox doctor
```

Build-from-source installation is also available below.

### 2. Authenticate a workspace profile

```sh
databricks auth login -p <profile> --host https://<workspace-url>
~/.local/bin/buzz-backend-databricks-lakebox --profile <profile> doctor
```

The provider calls `databricks sandbox register` during every deployment, so no
separate registration command is required. `doctor` is optional diagnostics,
not a prerequisite for creating an agent in Buzz.

Do not put a profile, workspace URL, token, or other environment-specific value
inside `mcp_config` or `skills_config`. The profile belongs in the dedicated
**Databricks CLI profile** field; inference credentials belong in the agent's
masked environment-variable editor.

### 3. Restart Buzz and create the agent

Buzz Desktop snapshots provider discovery at launch. Fully quit and reopen it
after installation, then follow [Setting up an agent in Buzz Desktop](#setting-up-an-agent-in-buzz-desktop).
If `databricks-lakebox` is absent from **Run on**, confirm the binary is
executable at `~/.local/bin/buzz-backend-databricks-lakebox`, rerun `doctor`,
and restart Buzz again.

## Install from source

### Prerequisites

- [Go](https://go.dev/dl/) **1.26.8** for reproducible embedded-helper generation (ordinary provider builds may use a compatible newer Go toolchain)
- `git` and `make`
- The [`databricks` CLI](https://docs.databricks.com/dev-tools/cli/) **1.8.0 or newer** (the version that ships the `sandbox` command group). The provider shells out to it for everything, and resolves it from `PATH` plus the usual install dirs (`/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin`, `~/bin`) so a Dock-launched Buzz Desktop finds it too.
- A configured `~/.databrickscfg` profile with access to the Databricks Sandbox preview (needed at runtime, not build time)

### Steps

1. **Clone the repository**

   ```sh
   git clone https://github.com/bkvarda/buzz-lakebox.git
   cd buzz-lakebox
   ```

2. **Build and install the binary**

   ```sh
   make install
   ```

   This runs `go install` with the version stamped in, placing `buzz-backend-databricks-lakebox` into `$GOBIN` (or `$(go env GOPATH)/bin` if `GOBIN` is unset). To stamp a specific version string, pass `VERSION`:

   ```sh
   make install VERSION=v0.1.0
   ```

   To bake in a default Databricks CLI profile other than `DEFAULT`, pass `PROFILE` (see [Choosing a Databricks profile](#choosing-a-databricks-profile)):

   ```sh
   make install PROFILE=EXAMPLE_PROFILE
   ```

3. **Ensure the install directory is on your `PATH`**

   Buzz Desktop discovers providers by scanning `PATH` for `buzz-backend-<id>` executables, so this step is required — not just convenient:

   ```sh
   export PATH="$(go env GOPATH)/bin:$PATH"
   ```

   Add that line to your shell profile (`~/.zshrc`, `~/.bashrc`, …) to make it permanent.

   Note that a GUI-launched Buzz Desktop (Dock/Finder) does not see your shell's `PATH` — it inherits launchd's minimal `PATH` and augments its provider search with only its own app bundle directory and `~/.local/bin` (it never scans `/usr/local/bin`). To cover that case, symlink the binary into `~/.local/bin`:

   ```sh
   make symlink               # links into ~/.local/bin; no-op if the link already exists
   ```

   Pass `SYMLINK_DIR=<dir>` to link somewhere else. No sudo is needed for the default location.

4. **Verify the install**

   ```sh
   buzz-backend-databricks-lakebox version   # prints the stamped version
   buzz-backend-databricks-lakebox doctor    # checks the runtime environment
   ```

To build into the repo root instead of installing (e.g. for local iteration), use `make build`. `make check` runs the complete hermetic public gate: formatting, vet, lint, race tests, and deterministic offline checks for both embedded helpers. It never resolves a Databricks profile or contacts a workspace. See `make help` for all targets.

Tagged releases publish checksummed archives for macOS/Linux on amd64/arm64. Release verification rebuilds both embedded Linux helpers byte-for-byte with pinned Go 1.26.8 before publishing; tagged inputs are never repaired or regenerated inside GoReleaser.

### Choosing a Databricks profile

The provider authenticates with a profile from `~/.databrickscfg` (create one with `databricks auth login -p <name> --host https://<workspace-url>`). Provider mode first runs `databricks auth profiles --skip-validate --output json`, which reads local profile metadata without validating credentials or contacting a workspace. It advertises the sorted profile names as a schema `enum`; current Buzz still renders the field as text, so an actual clickable dropdown requires Buzz renderer support.

Selection is deterministic, from most to least specific:

1. **Per-deploy, in the payload** — `provider_config.profile` in the JSON Buzz Desktop sends on stdin (or in the file given to `deploy --payload-file`). This always wins when set:

   ```json
   {"agent": {...}, "provider_config": {"profile": "EXAMPLE_PROFILE"}}
   ```

2. **Per-invocation, on the CLI** — the `--profile` flag, honored by all subcommands (`doctor`, `deploy`, ...). Used when the payload leaves the profile empty. Note that Buzz Desktop invokes the provider without arguments, so this only applies to manual CLI use:

   ```sh
   buzz-backend-databricks-lakebox --profile EXAMPLE_PROFILE doctor
   ```

3. **Automatic provider-mode selection** — when Buzz sends an empty profile, the provider prefers the baked default (normally `DEFAULT`) if that name was discovered; otherwise it selects the only discovered profile. If multiple profiles remain, deployment stops and lists their names rather than guessing. If local discovery fails or finds no profiles, the provider preserves the historical baked-default fallback so the existing preflight diagnostic remains authoritative. `make install PROFILE=EXAMPLE_PROFILE` can still stamp a different baked fallback.

For optional diagnostics, verify the selected profile resolves before deploying:

```sh
buzz-backend-databricks-lakebox doctor            # uses the baked-in default
buzz-backend-databricks-lakebox --profile EXAMPLE_PROFILE doctor
```

### Sandbox SSH key registration

Deploys run every in-sandbox step over `databricks sandbox ssh`. The provider
automatically runs `databricks sandbox register -p <selected-profile>` before it
creates or reuses a Sandbox, so ordinary Buzz setup needs no manual registration
step.

⚠️ **One key, one workspace.** The sandbox gateway is shared per region and binds a key to a single workspace identity. If the key is already registered elsewhere, automatic registration (and deployment) fails with `this SSH key is already registered to another user`. Free it first on the workspace that owns it:

```sh
databricks sandbox ssh-key list -p <other-profile>
databricks sandbox ssh-key delete <key-hash> -p <other-profile>
databricks sandbox register -p <target-profile>
```

Moving the key breaks sandbox SSH on the previous workspace until you register back. Preflight verifies actual registration (not just the register command's exit code) and fails with this remedy in the message.

## Setting up an agent in Buzz Desktop

With the released binary installed at `~/.local/bin`, **restart Buzz Desktop**
(it snapshots its provider scan at launch), then create the agent:

1. Open **Agents** and choose **Create agent**. Fill in the name and day-to-day
   instructions.
2. Under **AI configuration**, choose **Customize for this agent**. For the
   simplest fully configurable path:
   - **Agent harness:** Buzz Agent
   - **LLM provider:** **Databricks v2** — *not* Custom provider. Databricks v2
     routes Claude/GPT models through the workspace AI Gateway; Lakebox is the
     separate run destination selected in step 4.
   - **Model:** select a discovered gateway model or enter its model ID.

   Claude Code and Codex harnesses can also run in Lakebox, but their current
   adapters do not consume this exact model selection: Claude uses its adapter
   default to avoid gateway-ID rewriting, and Codex uses the provider's fixed,
   live-verified Codex model. See [`docs/BUZZ_DESKTOP_UX.md`](docs/BUZZ_DESKTOP_UX.md)
   for the current compatibility matrix and the proposed unified UI. Goose is
   not accepted until its complete runtime path is separately live-proven.
3. For the new-agent default `inference_auth=sandbox`, leave
   `DATABRICKS_HOST` and `DATABRICKS_TOKEN` unset. The provider derives them
   inside the Sandbox from its creator credential. To use a narrower grant,
   change Inference auth to `env`, then open **Advanced → Environment variables**
   and add:
   - `DATABRICKS_HOST` — the workspace URL serving the model and managed MCP.
   - `DATABRICKS_TOKEN` — **required inside the headless sandbox.** Buzz's
     browser OAuth flow cannot run there. Prefer a scoped service-principal or
     other least-privilege token with only the permissions the agent needs.

   These instructions apply to Buzz Agent. Claude and Codex use the same
   `DATABRICKS_HOST`/`DATABRICKS_TOKEN` pair as provider inputs, which Lakebox
   translates into their adapter-specific endpoint configuration.
4. Still under **Advanced**, set **Run on** to **databricks-lakebox**. Buzz probes
   the executable and renders these provider-owned fields directly in the form:

   | Buzz field | What to enter |
   |---|---|
   | **Databricks CLI profile** | The local profile used to create and operate the Sandbox. Enter one of the discovered profile names shown below the field, or leave empty for deterministic automatic selection. Current Buzz renders text; the provider also emits schema options for a future dropdown. |
   | **Inference auth** | `sandbox` (new-agent zero-token default; creator-identity grant) or `env` (bring a scoped host/token pair). An omitted legacy value still means `env`. |
   | **Idle timeout** | Optional duration such as `30m` or `2h`; empty means no autostop. |
   | **Managed MCP servers (JSON)** | Optional compact `buzz-managed-mcp` v1 JSON. Use the discover/validate/probe flow below instead of writing it blind. |
   | **Synchronized Databricks skills (JSON)** | Optional compact `buzz-skills` v1 JSON for a validated `aitools` skill sync. |

5. Choose **Create agent**. The first deployment normally takes a couple of
   minutes while the provider creates the Sandbox and installs the pinned Buzz
   harness. Editing and saving the agent redeploys idempotently into the mapped
   Sandbox.
6. Add the agent to a channel, then **@mention it**. `respond_to` defaults to
   owner-only, so the owner mention is the simplest first test.

### Adding managed MCP from the Buzz form

The current Buzz form is native, but its third-party provider schema renderer
accepts scalar fields only. For now, use the provider CLI as the safe resource
picker/preflight and paste only its compact `config` value into **Managed MCP
servers (JSON)**:

```sh
PROVIDER="$HOME/.local/bin/buzz-backend-databricks-lakebox"

# Discover resources visible to this profile. Catalog-backed kinds are bounded
# to each explicitly named catalog.schema scope.
"$PROVIDER" --profile <profile> mcp discover \
  --kind sql,genie,ai-search,functions,mcp-service,skills \
  --scope <catalog>.<schema> --emit-config

# Save the returned `config` string as mcp.json, then validate and live-probe it.
# Both commands return JSON; `config validate` returns the canonical compact
# `config` string to paste into Buzz.
"$PROVIDER" config validate --mcp-file mcp.json
"$PROVIDER" --profile <profile> mcp probe \
  --mcp-file mcp.json --force-refresh --timeout 30s
```

Only paste the value of the JSON result's `config` property, beginning with
`{"schema":"buzz-managed-mcp",...}`—not the outer command result. A successful
probe performs real MCP `initialize` and `tools/list` calls and reports each
server's tool count without printing the profile, host, or token.

Example for SQL plus one governed Skills scope:

```json
{"schema":"buzz-managed-mcp","version":1,"servers":[{"name":"sql","kind":"sql","auth":"env"},{"name":"skills","kind":"skills","resource":["my_catalog","my_schema"],"auth":"env"}]}
```

Paste this separately into **Synchronized Databricks skills (JSON)** when local
`aitools` skills should also be synchronized into the Sandbox:

```json
{"schema":"buzz-skills","version":1,"aitools":{"skills":["bundles","sql"],"experimental":false,"collision_policy":"fail"}}
```

Validate it before saving the agent:

```sh
"$PROVIDER" config validate --skills-file skills.json
```

For `inference_auth=sandbox`, leave `DATABRICKS_HOST` and `DATABRICKS_TOKEN`
unset. The provider derives them from the Sandbox's baked creator-identity
credential. This lets the agent act as you across the workspace and is
the explicit higher-risk default for newly created Buzz agents; managed MCP and
managed skill sync are refused in this mode. Switching an **existing** env-auth Sandbox to sandbox
auth requires deleting it and deploying a fresh one because the earlier
env-auth deployment neutralized its baked credential. See
[`docs/RUNBOOK.md`](docs/RUNBOOK.md#zero-token-inference-auth-inference_auth-sandbox).

The desktop's status indicators for remote agents come from relay observer
frames, which the rendered environment enables (`BUZZ_ACP_RELAY_OBSERVER=true`).
Lifecycle recovery is currently performed with the provider CLI because Buzz's
provider protocol does not yet expose start/stop/status/logs operations; see
[Operating a deployed agent](#operating-a-deployed-agent).

## Operating a deployed agent

The provider binary doubles as the operator CLI. Every lifecycle subcommand takes an optional `[sandbox-id]` (the `agent_id` returned by deploy) and defaults to the profile's single sandbox when there's exactly one:

```sh
buzz-backend-databricks-lakebox status    # sandbox state + buzz-acp liveness + log tail (JSON); non-zero exit when the agent is down
buzz-backend-databricks-lakebox start     # THE recovery command: start the sandbox if stopped, rerun launch.sh, verify
buzz-backend-databricks-lakebox logs      # tail acp.log (--tail-bytes to size)
buzz-backend-databricks-lakebox stop      # stop compute (agent goes offline; $HOME persists — recover with start)
buzz-backend-databricks-lakebox undeploy  # DESTRUCTIVE: shred in-sandbox secrets, delete the sandbox, drop the reuse mapping (--yes to skip the prompt)
```

[`docs/RUNBOOK.md`](docs/RUNBOOK.md) is the full operator guide: triage table, `!shutdown`/`stop`/`undeploy` trade-offs, orphan cleanup, Beta-storage-loss playbook, and the error-code reference. Every failure this provider reports is shaped `[<code>] <what happened> — remedy: <what to do> (sandbox <id>, databricks cli <version>)`; match on the code.

**When do you need `start`?** Whenever the sandbox stopped (manual stop, lifetime cap, or an opted-in idle timeout): all in-sandbox processes die with it and nothing relaunches buzz-acp automatically — and the desktop won't notice or redeploy on its own (its mention flow only redeploys agents whose record isn't "deployed"). Deploys default the sandbox to `--no-autostop` precisely because relay traffic doesn't count as sandbox activity, so any idle timeout kills healthy agents; pass `provider_config.idle_timeout` to opt back in, accepting `start`-based recovery.

Healthy log lines: `agent_pool_ready agents=N`, `connected to relay`, `subscribed to channel …`, `presence set to online`. You can also stop a remote agent from chat with a `!shutdown` owner mention, and everything above has a raw `databricks sandbox list/ssh/stop/start` equivalent if the provider binary isn't at hand.

## Design inputs

Full research (buzz architecture, omnigent's Lakebox integration patterns, live probe evidence with commands and timings) lives in [`docs/`](docs/):

- [`BUZZ_AGENT_SESSION_ARCHITECTURE.md`](docs/BUZZ_AGENT_SESSION_ARCHITECTURE.md) — how buzz hosts agent sessions today and the `BackendKind::Provider` seam
- [`OMNIGENT_DATABRICKS_SANDBOX_PATTERNS.md`](docs/OMNIGENT_DATABRICKS_SANDBOX_PATTERNS.md) — prior art: omnigent's lakebox launcher contract, bootstrap, auth gotchas
- [`LAKEBOX_LIVE_PROBE_RESULTS.md`](docs/LAKEBOX_LIVE_PROBE_RESULTS.md) — live-verified API surface, lifecycle timings, egress, persistence semantics, end-to-end `buzz` CLI ↔ relay proof from inside a sandbox
- [`UPSTREAM_BUZZ_GAPS.md`](docs/UPSTREAM_BUZZ_GAPS.md) — live-operations findings at the provider seam (misleading status, no recovery affordance, lost mentions, missing `backend_agent_id` echo, protocol v2), drafted as future block/buzz contributions
- [`BUZZ_DESKTOP_UX.md`](docs/BUZZ_DESKTOP_UX.md) — why Lakebox remains a run destination, the current harness/model compatibility matrix, and the generic Buzz UI/protocol extension for a unified configurator
- [`INTERNAL_ACCEPTANCE.md`](docs/INTERNAL_ACCEPTANCE.md) — the hard boundary between public hermetic CI and manual internal-workspace acceptance, including the double opt-in and sanitized evidence rules

## Key facts the design leans on (live-verified 2026-07-24)

- Sandbox create → Running in ~1s; restart after stop ~20s; idle autostop default 10m (tunable 1m–24h or `--no-autostop`).
- `$HOME` persists across stop/start; everything else (incl. `/tmp`) is wiped and processes die → binaries live in `$HOME`, relaunch-on-start is the provider's job.
- Outbound egress is open (Buzz relay WSS, GitHub, npm, PyPI, Anthropic). `setsid nohup` processes survive SSH disconnect.
- The public Buzz `.deb` release contains Linux x86_64 builds of `buzz-acp`, `buzz`, `buzz-agent`, `buzz-dev-mcp` — no cross-compile needed.
- The sandbox image bakes a **creator-identity PAT** in `~/.databrickscfg`; the provider must reset it unless the agent is meant to act as the owner on the workspace.

## Status

**M1 — working end-to-end** (live happy path verified 2026-07-25; full §6 M1 acceptance pending — see docs/ACCEPTANCE.md): deploy from Buzz Desktop → Databricks Sandbox provisioned → harness installed → agent online on the relay → owner mention answered via the workspace AI Gateway.

**M2 — code complete, live acceptance pending**: `status`, `start`, `logs`, `stop`, and `undeploy` ship as operator CLI subcommands (the desktop still can't call them — Buzz's provider protocol has no v2 lifecycle ops). Live acceptance checks are tracked in docs/ACCEPTANCE.md.

**M3 — hardening in place, live acceptance pending**: a failure taxonomy (stable code + remedy on every error), marker-secret fuzz across every deploy path plus credential-shaped scrubbing of any rendered log tail, executable double-launch/zombie/relaunch proofs against the real `launch.sh`, the `keep_workspace_pat` opt-out matrix, and [`docs/RUNBOOK.md`](docs/RUNBOOK.md). Live acceptance checks are tracked in docs/ACCEPTANCE.md and listed in the runbook's §9.

### Compatibility with current Buzz Desktop

The default sandbox payload is pinned to Buzz Desktop `v0.5.23` (release tag
`desktop-v0.5.23`) and its published Linux `.deb` SHA-256. The provider accepts
the current resolved `agent.launch` contract and retains
legacy payload compatibility. When `launch` is present, its `policy_env` then
`env` layers are authoritative, while the provider still owns identity, relay,
spawn command, response gate, and MCP wiring. This carries thread-scoped
sessions, lazy pools, team instructions, display/session titles, model/effort
projection, and owner identity into the sandbox without re-merging stale
legacy environment values.

Deploys also refresh the embedded current Buzz CLI skill under
`.agents/skills/buzz-cli` and create non-destructive harness-specific symlinks.
Goose and Pi are intentionally not accepted yet: each needs a separately
verified installer, inference bridge, ACP handshake, and skill contract rather
than accidental command pass-through.

## Configuring Databricks managed MCP servers

Current Buzz Desktop accepts only scalar provider-config values, so this
provider exposes a scalar `mcp_config` field containing compact, versioned JSON.
The configuration is generic: it carries typed resource identifiers, never a
workspace host, profile name, URL, or credential. The provider derives the host
from `DATABRICKS_HOST` at runtime and passes the credential only in the bridge
process environment.

```json
{"schema":"buzz-managed-mcp","version":1,"servers":[
  {"name":"warehouse","kind":"sql","auth":"env"},
  {"name":"space","kind":"genie","resource":["${GENIE_SPACE_ID}"],"auth":"env"},
  {"name":"search","kind":"ai-search","resource":["${CATALOG}","${SCHEMA}","${INDEX}"],"auth":"env"},
  {"name":"functions","kind":"functions","resource":["${CATALOG}","${SCHEMA}"],"auth":"env"},
  {"name":"service","kind":"mcp-service","resource":["${CATALOG}","${SCHEMA}","${SERVICE}"],"auth":"env"},
  {"name":"skills","kind":"skills","resource":["${CATALOG}","${SCHEMA}"],"auth":"env"}
]}
```

The `${...}` strings above are documentation placeholders; substitute literal
Unity Catalog/resource identifiers before submitting. Environment expansion is
not performed. Supported kinds are `sql`, `genie`, `ai-search`, legacy
schema-level `vector-search`, `functions` (schema or one function),
`mcp-service`, `skills`, and `local`. Multiple skill scopes are represented as
additional catalog/schema pairs in `resource`.

### Guided configuration and preflight

The operator CLI can validate configuration without touching a workspace, discover only resources visible to an explicit Databricks profile, and run a real `initialize` + `tools/list` probe before deployment:

```sh
# Offline and credential-free. Exactly one JSON/file flag is accepted.
buzz-backend-databricks-lakebox config validate --mcp-file mcp.json
buzz-backend-databricks-lakebox config validate --skills-file skills.json

# Read-only discovery. Catalog-backed routes are bounded to explicit scopes.
buzz-backend-databricks-lakebox --profile <profile> mcp discover \
  --kind sql,genie,ai-search,functions,mcp-service,skills \
  --scope <catalog>.<schema> --emit-config

# Live read-only probe. Config is validated before auth is resolved; a short-lived
# local U2M token is refreshed and passed only in the helper process environment.
buzz-backend-databricks-lakebox --profile <profile> mcp probe \
  --mcp-file mcp.json --force-refresh --timeout 30s
```

Discovery results contain portable typed identifiers—not the profile, host, or credential. Function, Skill, and MCP Service discovery never crawls all of Unity Catalog: each inspected schema must be named with `--scope`. Probe output contains only server names, kinds, deadlines, tool counts/names, and redacted errors. These live operator commands are intentionally absent from public CI.

Remote entries run through the embedded, pinned `bzhttpmcp` bridge. It accepts
only typed same-workspace endpoints, requires HTTPS, forwards JSON and SSE
Streamable HTTP responses, tracks MCP session IDs, and reads host/token only
from `DATABRICKS_HOST`/`DATABRICKS_TOKEN`. A provider-owned launcher restores
the 0600 launch environment after the agent runtime's MCP `env_clear`; `bzmux`
then gives each remote child only those two variables, never the Buzz private
key or auth tag.
Custom URLs and literal secret/env maps are not part of schema v1.

The local `mcp probe` command may refresh a short-lived token from an explicitly selected U2M CLI profile. That credential is used only by the local probe helper and is not written to config, state, argv, or output. This does **not** change deployed-agent authentication: deployed managed MCP still inherits the explicitly supplied env credential described below.

Managed MCP is currently allowed only with `inference_auth: "env"` and
`auth:"env"`. Sandbox-profile auth is reserved for a future per-request,
least-privilege token refresher; the provider will not quietly expose the
sandbox creator's owner-level credential to configured tools.

## Synchronizing Databricks agent skills

`provider_config.skills_config` accepts compact `buzz-skills` v1 JSON. It runs
`databricks aitools install --path` noninteractively in raw-skill mode, validates
the staged tree, and publishes safe skill directories into the persistent
canonical `.agents/skills` path:

```json
{"schema":"buzz-skills","version":1,"aitools":{
  "skills":["bundles","sql"],
  "experimental":false,
  "collision_policy":"fail"
}}
```

An empty `skills` list requests the current default Databricks skill set.
`collision_policy` defaults to `fail`; `replace-managed` can replace only a
directory carrying the exact provenance marker written by this provider. The
installer never overwrites `buzz-cli` or an unmanaged directory, rejects
symlinks/special files and unsafe names, and caps output at 64 skills / 16 MiB.
No source URL, profile, host, or credential can appear in the schema.

`databricks aitools --path` is intentionally raw-skill synchronization for all runtimes. This provider does not silently install native Claude/Codex plugins or mutate an agent tool's own global plugin registry; that remains a separate explicit, reviewable lifecycle.

For governed Unity Catalog Skills, configure a `kind:"skills"` entry in
`mcp_config`. With no `resource`, it exposes only schema-less utility tools;
with catalog/schema pairs, it exposes the live schema-backed Skills MCP, so
updates do not require copying files into an existing sandbox. Point-in-time
UC skill download is intentionally not automatic in schema v1; use the live
Skills MCP or an owner-reviewed `ug configure skills --location ... --path ...`
operation to avoid silently replacing governed executable content.
