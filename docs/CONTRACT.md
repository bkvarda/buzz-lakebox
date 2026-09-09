# Wire contract freeze — provider protocol v1

> Frozen against `block/buzz` @ `3bd3a014c6ed3f8c8c6c6ea359fee9a7e98dd670` (main), read 2026-07-24 via code-search. This satisfies the PLAN §6 M0 precondition. If buzz moves, re-verify against these files before changing provider behavior — tolerate unknown request fields, never remove response fields.

## 1. Invocation model

Source: `desktop/src-tauri/src/managed_agents/backend.rs` — `invoke_provider()` (line 19).

- The desktop spawns the provider binary with **no argv**, writes **one JSON object followed by `\n`** to stdin, and **closes stdin immediately** (provider sees EOF).
- Working directory: the desktop sets cwd to `default_agent_workdir()` when available (backend.rs:30-32) — do not assume cwd is the repo or `$HOME`.
- stdout is read incrementally and capped at **1 MB** (`STDOUT_CAP`, backend.rs:9); stderr capped at **64 KB** (`STDERR_CAP`, backend.rs:6). Response parsing: first stdout line that parses as JSON wins, else the whole trimmed buffer is tried (backend.rs:224-229). A trailing newline is not required but emitting exactly one JSON object on one line is the safe shape.
- **Timeouts**: `info` 10 s (`commands/agent_providers.rs:42`), `deploy` 600 s (backend.rs:372). On timeout the child is killed and the desktop reports `provider timed out after {N}s`.

## 2. Exit-code semantics (critical)

Source: backend.rs:205-217.

- **Non-zero exit fails the invocation regardless of stdout content** — the desktop discards any JSON already emitted and surfaces `provider failed (exit code N). stderr: <first 4096 chars, redacted>`.
- Therefore the provider must **exit 0 in every handled case, including errors**, and communicate failure via `{"ok":false,"error":"..."}` on stdout. Reserve non-zero exit for unhandleable crashes.

## 3. Request shapes

### `info` (agent_providers.rs:37-44)

```json
{"op":"info","request_id":"<uuid-v4>"}
```

`request_id` is for provider-side logging only — never validated in the response (stdin→stdout is 1:1 per process).

### `deploy` (backend.rs:365-371 `provider_deploy`)

```json
{
  "op": "deploy",
  "request_id": "<uuid-v4>",
  "agent": { ... },
  "provider_config": { ... }
}
```

The agent payload is **nested under `"agent"`** — not flat at the top level.

### `agent` fields (exhaustive)

Source: `desktop/src-tauri/src/commands/agents_deploy.rs` — `deploy_payload_json()` (line 112; doc comment: "every field the provider harness receives is deliberately listed here"). This also confirms PLAN §4.4 step 6: **there is no persona/team file dependency** — persona material arrives only as `system_prompt` and merged `env_vars`.

| Field | Type | Notes |
|---|---|---|
| `name` | string | display name; cosmetic only — never identity |
| `relay_url` | string | already resolved to a concrete URL by the desktop. Validated as **parseable** with a host and a `ws`/`wss`/`http`/`https` scheme, not merely non-empty: buzz-acp's `codex_network_env` does `Url::parse` and silently skips the `CODEX_CONFIG` network injection on failure, which for codex means the MCP subprocess — its only path back to the relay — has no egress, i.e. an agent that deploys clean and never answers |
| `private_key_nsec` | string | SECRET; identity key (npub derivation, PLAN §4.1). Desktop fails closed on keyring outage before serializing an empty one, but validate non-empty anyway |
| `auth_tag` | string | SECRET |
| `agent_command` | string | one of `buzz-agent`, `claude-agent-acp` (aliases `claude-code-acp`, `claude-code`, `claudecode`), `codex-acp` (alias `codex`), else reject (PLAN §4.2). Bare `claude` is deliberately NOT accepted: it names the underlying CLI, not the ACP adapter. Bare `codex` IS accepted and canonicalized to `codex-acp` — upstream's zero-arg identity list contains it, and canonicalization means the image's `ucode` wrapper is never spawned |
| `agent_args` | array of strings | |
| `system_prompt` | string | |
| `model` | string \| null | resolved persona→record→global. **Ignored by the `claude` and `codex` runtimes**, for different reasons. claude: a gateway model id is rewritten by the Anthropic SDK into a canonical id the gateway does not serve, failing every turn, so no model variable is emitted and the adapter's own default is used. codex: the model is written into the provider's generated `config.toml` as a fixed value, never interpolated from the payload — that file is sourced by the launch shell, so a payload-derived value would be an injection surface for no user-visible gain (PLAN §4.2) |
| `provider` | string \| null | structured provider id (e.g. `databricks_v2`) |
| `turn_timeout_seconds` | number | |
| `idle_timeout_seconds` | number | |
| `max_turn_duration_seconds` | number | |
| `parallelism` | number | |
| `respond_to` | string | falls back to `owner-only` if unread |
| `respond_to_allowlist` | array of strings | |
| `env_vars` | object (string→string) | merged global < persona < agent. Keys must match `^[A-Za-z_][A-Za-z0-9_]*$` (they are written raw into a shell-sourced file), and additionally may not collide with the provider's own shell scratch namespace — `buzz_*`, `BUZZ_PROBE_*`, `ENVF`, `OUTF` — since generated scripts assign those and then source this file, so a colliding key silently replaces the provider's value. `BUZZ_ACP_*` stays reachable. Further per-mode restrictions under `provider_config.inference_auth` below |

### `provider_config`

Validated by the desktop before it reaches us (`validate_provider_config`, backend.rs:390-421): flat object, ≤20 fields, ≤64 KB, scalar values only, no secret-like key names (`secret|password|token|key|credential` as word segments). Our keys (`profile`, `idle_timeout`, `keep_workspace_pat`, `buzz_version`, `inference_auth`, `claude_adapter_version`, `codex_adapter_version`, `extra_binaries`, `mcp_servers`) all pass the **name** filter — `inference_auth`'s own segments (`inference`, `auth`) don't match, which is exactly why the knob is named that and never `*token*`, and the `extra_binaries`/`mcp_servers`/`url`/`sha256`/`bin` names likewise carry no secret-word segment. But `extra_binaries` and `mcp_servers` are **array-valued**, so the desktop's scalar-only check rejects them before they can be stored on an agent record: they are **operator-CLI only** and never Desktop-deliverable in any mode. See the `extra_binaries`/`mcp_servers` bullet below.

- `inference_auth` — string enum `""` \| `"env"` \| `"sandbox"`, default `""` (treated as `"env"`). `"env"`: current behavior — the provider resets the sandbox's baked creator-identity PAT to a stub and the owner supplies `DATABRICKS_HOST`/`DATABRICKS_TOKEN` via `agent.env_vars`. `"sandbox"`: zero-token — the provider skips the PAT reset/re-assert, keeps the baked `~/.databrickscfg`, and derives `DATABRICKS_HOST`/`DATABRICKS_TOKEN` from it at every launch, **all-or-nothing** (deriving one half let a payload pair its own endpoint with the sandbox's owner-level token). **The two `env_vars` restrictions below are scoped differently, and the difference is load-bearing.** Under `"sandbox"` ONLY, the credential pair itself is rejected — `DATABRICKS_HOST`, `DATABRICKS_TOKEN`, `DATABRICKS_CONFIG_FILE`, `DATABRICKS_CLIENT_ID`/`_SECRET`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` — because that is the only mode in which the provider derives the owner PAT on the payload's behalf. Under `"sandbox"` **or** `keep_workspace_pat: true`, a second set is rejected: names that decide what code runs beside that credential or make the provider's own stack read it — `BUZZ_ACP_AGENT_COMMAND`/`_ARGS`/`BUZZ_ACP_MCP_COMMAND`, `BUZZ_ACP_{BASE,SYSTEM,HEARTBEAT}_PROMPT_FILE`, `BUZZ_ACP_RELAY_OBSERVER`, proxy and CA-bundle variables, `PATH`/`SHELL`/`BUZZ_SHELL`/`HOME`/`BASH_ENV`/`ENV`/`IFS`, the `NODE_*`/`LD_*`/`DYLD_*`/`npm_config_*`/`PYTHON*`/`GIT_*` prefixes, and the codex-runtime set. So `keep_workspace_pat: true` with `"env"` **does** still accept your own `DATABRICKS_HOST`/`DATABRICKS_TOKEN` — that mode derives nothing, so the token in the agent's env is yours. `"sandbox"` supersedes `keep_workspace_pat` when both are set: they are redundant, not conflicting.

- `extra_binaries` / `mcp_servers` — the #15/#16 capability channel (extra pinned binaries fetched over https, sha256-verified, and placed into a separate `$HOME/.buzz-backend/extra-bin` directory that is appended to PATH (after system directories), so an extra binary can never shadow a system binary or a provider binary — only resolve when no earlier PATH entry provides the name; MCP servers to wire up), validated here per #18. **Expert-only**, deliberately NOT advertised in `config_schema` (same posture as `keep_workspace_pat`), and **non-scalar** — so, unlike every other provider_config key, they are rejected by the desktop's scalar-only filter and can only ever arrive via a **raw operator-CLI payload**, never through Buzz Desktop. Both are **refused whenever the sandbox holds a workspace-owner credential** (`inference_auth: "sandbox"` **or** `keep_workspace_pat: true`, i.e. `OwnerPATInSandbox()`): each key decides what code runs in a process that can reach that baked owner token, so the rejection mirrors the `env_vars` owner-PAT gate and points at `inference_auth: "env"` (where the credential in the sandbox is your own, so running your own code beside it is your business). This gate is the **sole guard on the `provider_config` channel, not belt-and-suspenders**: `BUZZ_ACP_MCP_COMMAND` on the `env_vars` channel is guarded twice (upstream Buzz `RESERVED_ENV_KEYS` strips it, and our own owner-PAT gate refuses it again), so through the Desktop it is never deliverable — what `inference_auth: "env"` permits there is provider-side *emission* of the MCP command (the #15/#16 mechanism), not an `env_vars` override — but `provider_config` has no upstream layer at all, so nothing else stands between an operator-CLI payload and code beside the credential. `extra_binaries` additionally has **structural rules enforced unconditionally in every mode** (independent of the owner-PAT gate): each entry's `url` must be non-empty, parseable, **`https`** (no `http`/other scheme — no unpinned/insecure fetch), and have a **host** (a hostless `https://` is rejected at the payload boundary rather than failing opaquely at fetch time); `sha256` is **required** and must be **64 lowercase hex** chars (`^[0-9a-f]{64}$` — uppercase/mixed case rejected, not normalized); `bin` must be a **bare filename** (`^[A-Za-z0-9._-]+$`, excluding path separators, not `.`/`..`, and not starting with `-` so it cannot be parsed as an option flag when later invoked) that does **not collide** with a binary this provider installs or deliberately preserves (`install.BinNames` ∪ the adapter bin names ∪ the image's `codex` ucode wrapper), since such a name would shadow the real binary in the PATH dir `launch.sh` prepends. That collision set is *not* the whole PATH — system binaries in later dirs are #15's placement problem, not this name check's. `extra_binaries` **#15 install is now implemented**: deploy fetches each entry's url over https, verifies the required sha256 (a mismatch fails the deploy hard — present-but-broken is do-not-proceed, not a skip), places the result as a single-file executable (`chmod +x`) into `$HOME/.buzz-backend/extra-bin`, and records a per-binary marker whose content is the sha256 so a redeploy with the same pin is a no-op (no refetch); absent (`len==0`) is a pure no-op — no script round-trip at all, output byte-identical to a no-`extra_binaries` deploy. **Single-file executables only** — archive extraction is a deferred follow-up. A `bin` name that collides with a system binary is surfaced at install time by a non-fatal warning (the extra binary is placed successfully but is unreachable by that name, since system directories precede `extra-bin` in PATH; the operator must resolve). Because the install path is silent-on-success — a healthy deploy returns only stdout, which the caller discards — that warning is written to a durable in-sandbox file, `$HOME/.buzz-backend/extra-bin-warnings.log` (truncated each deploy, so it reflects the current one; empty means "checked, no collisions"), where the operator can recover it after the fact; it is also echoed to stderr so it appears in the failure diagnostics if some later step aborts the deploy. `mcp_servers` **also has structural rules enforced unconditionally in every mode** (independent of the owner-PAT gate): each entry is a **bare command name** (`^[A-Za-z0-9._-]+$`, excluding path separators, not `.`/`..`, and not starting with `-`), entries must be **unique**, there may be **at most 16** (mirroring buzz-agent's `MAX_MCP_SERVERS`), and none may be named **`bzmux`** (reserved for this provider's MCP multiplexer — the same name is reserved on the `extra_binaries` channel). `mcp_servers` **#16 install is now fully implemented**: the entry count maps to how the agent's single `BUZZ_ACP_MCP_COMMAND` slot is wired — **0 entries** leave the runtime's own default in place (buzz-agent/codex emit `buzz-dev-mcp`, claude emits nothing; byte-identical to no `mcp_servers`); **1 entry** (the single-server / #14 case, **now implemented**) points `BUZZ_ACP_MCP_COMMAND` straight at that one command for **every** runtime — including claude, which otherwise emits nothing, so this is how a claude agent gains an MCP server. **2 or more entries** deploy the embedded **`bzmux` MCP multiplexer** (**Increment 2**, now implemented): the provider writes the `bzmux` binary and a 0600 `mcp-mux.json` config into `$HOME/.buzz-backend/bin/`, sets `BUZZ_ACP_MCP_COMMAND=bzmux`, then runs a deploy-time self-test (`bzmux --selftest`) that spawns every child, merges their tool catalogs, and validates there are no bare-name collisions or `__`-containing tool names before the agent is declared healthy — no silent live-bitten deploy. The `mcp-mux.json` config (written 0600 beside the binary) lists each child server's command and a per-child environment allowlist; `bzmux` never performs a blanket `os.Environ()` forward so each child receives only its explicitly allowlisted variables. Owner `env_vars` (rendered last) still override `BUZZ_ACP_MCP_COMMAND` in every case. **#17:** the resolved `BUZZ_ACP_MCP_COMMAND` (the single command for 1 entry, or `bzmux` for 2+) is now verified at deploy time with a real MCP `initialize` + `tools/list` handshake (`bzmux --verify-mcp`) that asserts a non-empty tool catalog — a broken/tool-less MCP server fails the deploy with `install.mcp_verify` rather than shipping a live-bitten agent — with **no wire-shape change** to `mcp_servers`.

## 4. Response shapes

### `info`

**Validated** by the desktop before it reaches the frontend. As of `block/buzz` @ `f956e6f` (pin this citation; re-verify if buzz moves), `validate_provider_info` (`desktop/src-tauri/src/managed_agents/backend.rs`) enforces:

- `protocol_version` — a mandatory **integer** equal to the version the desktop speaks (currently `1`). Read with `as_u64`; a **missing** field is a hard error (`provider info response missing integer protocol_version`), not a presumed `1`, and a value the desktop doesn't speak errors with `unsupported provider protocol version <n>; desktop requires 1`. This replaced the earlier cosmetic string `"protocol":"v1"`.
- `ok` must be `true`; `name`, `version`, `description` must be **non-empty strings**; `config_schema` must be a JSON **object**.
- A **strict field whitelist**: exactly `ok`, `name`, `version`, `protocol_version`, `description`, `config_schema`. Any other field — including the old `protocol` and `ops` — is rejected (`provider info response contains unknown field <field>`). Do **not** add diagnostic fields to the info response.

`config_schema` still drives the create-agent UI form (unchanged): `desktop/src/shared/api/types.ts` (`BackendProviderProbeResult`) declares the shape; `WhereToRunSection.tsx` → `ProviderConfigFields.tsx` walks `config_schema.properties` and renders one free-text input per property, seeded from `title`/`description`/`default`; `config_schema.required` gates the create-agent button; `coerceConfigValues` type-coerces the entered strings back (e.g. `"true"` → bool) before they're stored on the agent record. Those stored values are resent as `provider_config` on **every** redeploy.

Convention (must not set `ok:false`):

```json
{"ok":true,"name":"Databricks Lakebox","version":"<semver>","protocol_version":1,"description":"...","config_schema":{"type":"object","properties":{"profile":{"type":"string","title":"Databricks CLI profile","description":"Databricks CLI profile selection; empty = the build's baked default."},"inference_auth":{"type":"string","title":"Inference auth","default":"env","description":"env (default): you supply DATABRICKS_HOST/DATABRICKS_TOKEN in the agent's environment variables. sandbox: zero-token — the agent reuses the sandbox's built-in per-user credential and can act AS YOU across the whole workspace (opt-in security tradeoff)."},"idle_timeout":{"type":"string","title":"Idle timeout","description":"Duration like 30m or 2h; empty = no autostop (default)."}},"required":[]}}
```

We advertise only `profile`, `inference_auth`, and `idle_timeout` in `config_schema.properties`. `keep_workspace_pat`, `buzz_version`, `claude_adapter_version`, and `codex_adapter_version` stay expert-only — documented here in CONTRACT.md, not schema-advertised — since they're footguns better set deliberately in a hand-written payload than exposed as a free-text field in the desktop UI.

### `deploy` success (backend.rs:373-376)

```json
{"ok":true,"agent_id":"<sandbox-id>"}
```

The **only** field the desktop reads is `agent_id` (must be a JSON string; missing → error `deploy response missing agent_id`). Extra fields are ignored — safe to add diagnostics (e.g. `cli_version`, `sandbox_name`).

### Any-op failure (backend.rs:232-235)

```json
{"ok":false,"error":"<actionable message>"}
```

Exit 0. `error` must be a string; the desktop surfaces it (after its own redaction pass) as the deploy failure. Embed the sandbox id and recorded CLI version here per PLAN §4.3.

### Unknown op

Same failure shape, exit 0: `{"ok":false,"error":"unknown op \"<op>\"; supported: info, deploy"}` — forward-compatible with v2 ops.

## 5. Desktop-side redaction (defense in depth, not a substitute)

backend.rs:189-193, 253-297: the desktop redacts `agent.env_vars` values (≥4 chars, longest-first) plus `nsec1…`/`sprt_tok_…` prefixed tokens out of stderr and `error` strings. It does **not** know about `private_key_nsec`/`auth_tag` beyond the prefix rules, and never sees our in-sandbox logs — the provider's own `internal/redact` must scrub every payload secret from everything it emits (PLAN §5).

## 6. Discovery

backend.rs:427-543: binaries named `buzz-backend-<id>` with the executable bit, discovered on `PATH` + the desktop exe's own dir + `~/.local/bin`; id must match `[a-z0-9][a-z0-9_-]*`. Our id: `databricks-lakebox` → binary `buzz-backend-databricks-lakebox` (buzz ships its own built-in `databricks` provider, so the plain id would collide).

## 7. Engineering constants settled here

- Minimum `databricks` CLI version gate: **v1.8.0** — the version live-verified with the full `sandbox` command group (lane C/D probes). `doctor` and deploy preflight enforce ≥ this; every output records the actual version string (PLAN §3.1).
- buzz-agent inference env (verified live, `docs/M05_PROBE_RESULTS.md` §2): `BUZZ_AGENT_PROVIDER` (`databricks_v2` | `databricks`), `DATABRICKS_HOST`, `DATABRICKS_TOKEN`, `DATABRICKS_MODEL`.
- claude inference env (verified live, `docs/M2_CLAUDE_PROBE_RESULTS.md`): `ANTHROPIC_BASE_URL` = `{DATABRICKS_HOST}/ai-gateway/anthropic`, `ANTHROPIC_AUTH_TOKEN` = the Databricks token. Both are *derived in-shell* by `nest.ClaudeEnvSnippet`, appended after the `env_vars` block and after the zero-token snippet, so one identical text serves both `inference_auth` modes. Deliberately absent: any model variable (see the `model` row in §3), and `ANTHROPIC_API_KEY` — it produces an `x-api-key` header, which the gateway rejects with 401.
- Buzz payload pin: `v0.5.23` (release tag `desktop-v0.5.23`), `.deb` SHA-256 `94f1e50021f88f8864f568c86a8ea2b39993da64e3fe8d86bf731cd00c1c9cce`.
- Adapter pin: `@agentclientprotocol/claude-agent-acp@0.73.0`, installed with `npm ci --ignore-scripts` against a committed `package-lock.json` (112 packages, every one integrity-pinned). Override with the expert-only `provider_config.claude_adapter_version`; an unpinned version fails loud rather than installing on trust.
- Adapter pin: `@agentclientprotocol/codex-acp@1.8.0`, same mechanism (25 packages, every one integrity-pinned; `provider_config.codex_adapter_version` to override). Note the codex lockfile pins **six** platform variants and **no `-musl`** entry, unlike the claude one — the Lakebox sandbox image is glibc, but a musl base image would break `npm ci` rather than degrade.
- **Bring-your-own endpoint requires bring-your-own token.** Setting `env_vars.ANTHROPIC_BASE_URL` without `ANTHROPIC_AUTH_TOKEN` is rejected at validation: the provider never attaches the workspace credential to an endpoint it did not derive, because in `inference_auth: "sandbox"` that credential is the sandbox's owner-level baked PAT.

## 8. Current Buzz `agent.launch` compatibility

Current Buzz Desktop sends a resolved `agent.launch` block in addition to the
legacy top-level fields:

```json
{
  "command": "buzz-agent",
  "args": [],
  "policy_env": {"BUZZ_ACP_SESSION_POLICY": "thread"},
  "env": {"BUZZ_AGENT_PROVIDER": "databricks_v2"},
  "owner_pubkey": "<hex>"
}
```

When present, this block is authoritative. The provider applies `policy_env`
first and `env` second, and does **not** merge legacy `env_vars` back on top.
Provider-owned identity, relay, command/arguments, respond-to, and MCP values
remain the final authority and are stripped from the lower tiers before the
environment is rendered. `owner_pubkey` becomes `BUZZ_ACP_AGENT_OWNER`.
This preserves current Desktop thread/channel session policy, lazy pools, team
instructions, display/session titles, projected effort, and model/provider
environment while remaining compatible with older payloads that omit `launch`.

The supported runtime capability table remains deliberately narrower than the
Desktop catalog: `buzz-agent`, Claude ACP, and Codex ACP are supported. Goose
and Pi are rejected rather than accepted without their runtime installation,
inference, verification, and skill-discovery contracts.

## 9. Embedded Buzz CLI skill

Every deploy refreshes the current Buzz CLI skill at
`$HOME/.buzz/.agents/skills/buzz-cli/SKILL.md` and writes harness-specific
symlinks for Claude, Codex, and Goose only when those paths do not already
exist. Existing unmanaged skill directories are never overwritten. The skill
content is embedded at build time from the current `block/buzz`
`desktop/src-tauri/src/managed_agents/nest_skill.md`; it contains no workspace,
profile, user, or credential values.

## 10. Managed MCP schema v1

`provider_config.mcp_config` is a scalar string containing this versioned JSON
shape, suitable for current Desktop's scalar-only provider configuration:

```json
{"schema":"buzz-managed-mcp","version":1,"servers":[...]}
```

Each server has `name`, `kind`, optional typed `resource` components, and
`auth:"env"`. It cannot represent a host, custom URL, profile, literal token,
or literal environment value. Resource arity determines a documented
same-workspace route. A single managed server still uses `bzmux`, because its
typed bridge arguments cannot fit the legacy bare-command direct slot.

The embedded `bzhttpmcp` bridge receives `DATABRICKS_HOST` and
`DATABRICKS_TOKEN` through its per-child allowlist, constructs an HTTPS URL,
rejects cross-host redirects, and supports JSON/SSE Streamable HTTP plus MCP
session IDs. Because current `buzz-agent` clears MCP child environments, a
provider-owned launcher sources the provider's 0600 env file before execing
`bzmux`; the mux then enforces every child's allowlist. No token appears in
argv, mux config, provider state, or diagnostics. Managed MCP remains
incompatible with any owner-PAT-in-sandbox mode.

The legacy operator-only `mcp_servers` array remains supported for local bare
stdio commands. The object-form `mcp` is likewise operator-only and cannot be
combined with scalar `mcp_config`; either managed form is mutually exclusive
with `mcp_servers`.

## 11. Skills schema v1

`provider_config.skills_config` is a scalar compact JSON value with
`schema:"buzz-skills"`, `version:1`, and an optional `aitools` object. It can
select safe skill names, opt into experimental skills, and choose `fail` or
`replace-managed` collision handling. It cannot carry a host, URL, profile,
credential, environment expansion, or arbitrary destination path.

The installer invokes `databricks aitools install --path` against a private
staging directory. (`--path` is the noninteractive raw-skill mode; current
`aitools` rejects combining it with `--skills-only` or JSON output.) Before
changing the canonical `.agents/skills` tree it validates top-level names, required
`SKILL.md`, symlink/special-file absence, count/byte limits, reserved
`buzz-cli`, and every collision. Replacement requires the exact provider
provenance marker/version. Managed skill synchronization is refused when a
sandbox owner credential remains reachable.

Live governed Unity Catalog Skills use the managed MCP schema's `skills` kind.
That path scopes schemas in the same-workspace route and loads current skill
content at session time. Automated UC file download is not part of schema v1.
