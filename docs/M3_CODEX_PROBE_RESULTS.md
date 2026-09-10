# M3 Codex Probe Session Results

> Live session on the `EXAMPLE_PROFILE` (`workspace.example.invalid`, `example-region-1`), 2026-07-28. Sandbox `<sandbox-id-a>`, **deleted at session end**. Adapter under test: `@agentclientprotocol/codex-acp@1.1.7` (latest; `beta` dist-tag is `0.0.40`).
>
> Auth note: gateway probes G1–G5 used a CLI **OAuth** bearer token from the operator laptop; in-sandbox probes S1–S6 used the sandbox's **baked creator-identity PAT**, derived from `~/.databrickscfg` the way `nest.SandboxAuthSnippet` does.
>
> Answers all three probe checkboxes in issue #3. **Four of the issue's stated assumptions were overturned by live evidence, and two previously-unknown image behaviours were discovered.**

---

## Summary of overturns

| Issue #3 said | Live evidence says |
|---|---|
| `/responses` may not exist; chat-completions is the likely **fallback** | `/responses` **works**; `/chat/completions` is **rejected** for the codex model. The fallback is impossible — the native path is the *only* path |
| Point codex at `{host}/ai-gateway/openai/v1` | A **dedicated `{host}/ai-gateway/codex/v1` surface exists** and is what the image itself uses |
| The sandbox has no codex | The image ships `/usr/local/bin/codex` — but it is a **`ucode` wrapper, not the real CLI**. This *confirms* the existing alias-exclusion design rather than breaking it (S1) |
| `426 Upgrade Required` ChatGPT-login quirk is expected | **Did not reproduce** on v1.1.7. Empty stderr across every run |

---

## G1. `/ai-gateway/openai/v1/responses` — EXISTS ✅

`POST` with `databricks-gpt-5-3-codex` → **HTTP 200 in 1.21 s**, correct completion. This clears the issue's primary go/no-go gate.

`GET /ai-gateway/openai/v1/models` → `400 BAD_REQUEST` ("doesn't match any known API type"). There is **no model-listing route** on the gateway surface; use the `databricks_v2` discovery route (`GET /api/ai-gateway/v2/endpoints`) instead, as M0.5 did.

## G2. `/chat/completions` — REJECTED for the codex model ❌ (inverts the issue's fallback plan)

```
HTTP 400 — API type 'openai/v1/chat/completions' is not supported by 'databricks-gpt-5-3-codex'.
Supported API types: [openai/v1/responses, cursor/v1/chat/completions]
```

Support is **per-model**, not per-surface:

| Model | `/responses` | `/chat/completions` |
|---|---|---|
| `databricks-gpt-5-3-codex` | **200** | **400** |
| `databricks-gpt-5-4` | 200 | 200 |
| `databricks-gpt-5-5` | 200 | 200 |
| `databricks-gpt-oss-120b` | **400** | 200 |

**The `wire_api = "chat"` fallback in issue #3 must be deleted from the plan.** It cannot work with any codex-class model. `wire_api = "responses"` is mandatory, not preferred — which also means the "degraded codex features / reduced reasoning surface" concern evaporates.

## G3. SSE streaming on `/responses` — VERIFIED ✅

`HTTP/2 200`, `content-type: text/event-stream`. Proper Responses-API event sequence: `response.created` → `response.in_progress` → … Verified on **both** `/ai-gateway/openai/v1` and `/ai-gateway/codex/v1`.

## G4. Model catalog — `databricks-gpt-5-3-codex` is available ✅

42 endpoints total; 16 GPT-family. Present: `databricks-gpt-5`, `-5-1`, `-5-2`, **`-5-3-codex`**, `-5-4`, `-5-4-mini`, `-5-4-nano`, `-5-5`, `-5-5-pro`, `-5-6-luna`, `-5-6-sol`, `-5-6-terra`, `-5-mini`, `-5-nano`, `-gpt-oss-120b`, `-gpt-oss-20b`.

A `gpt-5-codex`-class model **is** available, so the issue's "codex's tuning assumes gpt-5-codex-class models" caveat is satisfied.

## G5. Auth shape — Bearer only ✅ (matches the Claude finding P2b)

`Authorization: Bearer <token>` → 200. `api-key: <token>` → **401** "Credential was not sent or was of an unsupported type."

Codex's `env_key` mechanism sends `Authorization: Bearer`, so it is compatible. Model ids are **rewritten on the way back** (`databricks-gpt-5-3-codex` → `gpt-5.3-codex`), same as the Claude gateway — nothing may assume request/response id equality.

---

## S1. The image pre-wires codex — and it is a `ucode` wrapper, not codex ⚠️

`/usr/local/bin/codex` exists, but:

```
Usage: ucode codex [OPTIONS]
  Launch Codex via Databricks.
Options: --help
```

It accepts **no arguments**, prints Databricks auth banners on stderr, and launches an interactive TUI. It does **not** speak ACP on stdio. `/usr/local/bin/claude` is the same kind of wrapper.

**This confirms an existing design decision rather than revealing a defect.** `internal/payload/runtime.go` already excludes bare `claude` from the accepted alias set, on the stated grounds that it "names the underlying CLI binary, not the ACP adapter, and accepting it here would spawn a program that does not speak ACP on stdio." That rationale was derived by reading upstream source; this probe is the first direct evidence that such a colliding binary genuinely exists in the image. Two mechanisms make the shipped Claude runtime safe today:

1. `internal/nest/nest.go:416` — `launch.sh` exports `PATH="$HOME/.buzz-backend/bin:$PATH"`, so the provider's bin dir **precedes** `/usr/local/bin`.
2. Every claude alias canonicalizes to spawn command `claude-agent-acp`, which does not collide with the wrapper's name at all.

**The open question this leaves for codex** is that upstream buzz-acp *does* list bare `codex` as a zero-arg agent (`crates/buzz-acp/src/config.rs:620`), unlike bare `claude`. Whether to accept `codex` as an alias (canonicalizing it to `codex-acp`) or reject it for symmetry with the claude table is a design decision, not a forced move. Related: the adapter ships **both** `codex` and `codex-acp` in `node_modules/.bin`, so which name gets symlinked into `BinDir` decides whether the image's `ucode` wrapper is shadowed for everything else in the sandbox.

Incidentally this contradicts PLAN §4.2's premise that the image has "no goose/claude/codex".

## S2. `~/.codex/config.toml` and `~/.databrickscfg` are symlinks into `/run/lakebox/` 🚨

```
~/.codex/config.toml -> /run/lakebox/codex-config.toml
~/.databrickscfg     -> /run/lakebox/databrickscfg
```

The image's baked `codex-config.toml` already points at the gateway:

```toml
model_provider = "Databricks"
[model_providers.Databricks]
base_url = "https://<host>/ai-gateway/codex/v1"
wire_api = "responses"
[model_providers.Databricks.auth]
command = "sh"
args = ["-c", "awk '/^\\[DEFAULT\\]/{f=1} f && /^token *=/{print $NF; exit}' ~/.databrickscfg"]
```

Two consequences the plan must handle:

1. **PLAN §4.4 step 6 says "write `~/.codex/config.toml`".** Doing so naively **follows the symlink** and clobbers image state in `/run/lakebox/`, which is a runtime dir regenerated on restart — so the write would silently disappear across a stop/start. The provider must set **`CODEX_HOME`** to its own directory (verified working in S4) rather than writing `~/.codex/config.toml`.
2. **This confirms the previously-unverified PAT restore-on-start behaviour** (PLAN §4.4 step 9): `~/.databrickscfg` is a symlink into `/run`, so the baked PAT *is* regenerated on every start. Re-asserting the stub on each launch is therefore load-bearing, not belt-and-braces.

Also present under `~/.codex`: `ucode.config.toml` (a second provider block, `User-Agent = "ucode/0.1.0.post4 codex/0.137.0"`) and several sqlite state files — the image's own agent tooling. Our `CODEX_HOME` isolation keeps us clear of all of it.

## S3. Adapter install — VERIFIED ✅, and cheaper than Claude's

| Measurement | Result |
|---|---|
| Lockfile generation (`--package-lock-only`) | **1.3 s** |
| `npm ci --ignore-scripts` (cold) | **4.4 s** |
| `node_modules` | 362 MB |
| `~/.npm` cache | 132 MB |
| Packages / lockfile lines | **25 / 365** (vs Claude's 112 / ~800) |
| `missing_integrity` | **0** |

`node_modules/.bin/` yields `codex-acp` **and a real `codex`** (`@openai/codex/bin/codex.js`) — the adapter is self-contained and does not depend on the image's `ucode` wrapper.

**Platform variants pinned:** `darwin-{arm64,x64}`, `linux-{arm64,x64}`, `win32-{arm64,x64}` — 6 optional deps, all with sha512. **Note: there is no `-musl` variant** (Claude's lockfile had one). The sandbox is glibc so this is fine today, but a musl base image would break `npm ci` here.

## S4. ACP `initialize` — VERIFIED ✅

```
protocolVersion: 1
agentInfo: {name: "@agentclientprotocol/codex-acp", title: "Codex", version: "1.1.7"}
agentCapabilities: {loadSession: true, promptCapabilities: {embeddedContext, image},
                    sessionCapabilities: {resume, list, close, delete, additionalDirectories},
                    mcpCapabilities: {acp: false, http: true, sse: false}}
authMethods: [{id: "api-key", …}, {id: "chat-gpt", …}]
stderr: (empty)
```

> **CORRECTION (added after code review).** `mcpCapabilities: {acp:false, http:true, sse:false}` was read here as "codex supports no stdio MCP, so `buzz-dev-mcp` is unusable". **That is wrong**, and it drove a decision that would have shipped silent codex agents.
>
> **stdio is the baseline ACP transport and appears in no capability flag at all** — the capability set covers only the optional transports (`http`, `sse`, `acp`). The adapter's own schema carries a `zMcpServerStdio` with `name`/`command`/`args`, so stdio MCP is fully supported. Upstream agrees and is explicit: desktop's discovery table sets `mcp_command: Some("buzz-dev-mcp")` for **codex** and `None` only for **claude**.
>
> The reason for that split is concrete, and it is the opposite of the intuition that codex "ships its own tools so needs no MCP": Claude Code's shell can run `buzz messages send` directly, while codex's tool calls execute under the adapter's `workspaceWrite` sandbox with `networkAccess:false` (S10), so its shell cannot reach the relay at all. The MCP subprocess is how a codex agent answers — which is also why buzz-acp injects `CODEX_CONFIG` `network_access` for exactly this runtime.

Note `authMethods` is **non-empty** here (Claude's was `[]`) — but S5 proves no `authenticate` call is needed when the provider supplies auth via config. The adapter **requires stdin to stay open**; closing it immediately yields `rc=0` with no response at all (an easy false-negative when probing).

## S5. Full ACP session against the gateway — VERIFIED ✅

`initialize` → `session/new` → `session/prompt` with `CODEX_HOME` pointed at a provider-written `config.toml`:

```toml
model = "databricks-gpt-5-3-codex"
model_provider = "databricks"
approval_policy = "never"
sandbox_mode = "danger-full-access"

[model_providers.databricks]
name = "Databricks AI Gateway"
base_url = "<host>/ai-gateway/codex/v1"
wire_api = "responses"
env_key = "DATABRICKS_TOKEN"
```

```
stopReason: end_turn    elapsed: 1.65 s    stderr: (empty)
agent output: "OK"      usage: 12104 total (10435 in, 1664 cached, 5 out)
```

**No `authenticate` round trip was needed** despite non-empty `authMethods`. **No `426 Upgrade Required` appeared** — the quirk documented in buzz-acp's README did not reproduce on v1.1.7 with config-supplied auth. The plan should not build mitigation for it, but should not assert it is gone either.

## S6. Model-metadata warning leaks into the transcript ⚠️ (new, unavoidable)

The **first agent message chunk of every session** is:

```
Warning: Model metadata for `databricks-gpt-5-3-codex` not found.
Defaulting to fallback metadata; this can degrade performance and cause issues.
```

It is delivered as `session/update` → `agent_message_chunk`, i.e. it is **user-visible transcript text**, not stderr.

Root cause is structural and has no clean fix:

- The gateway accepts **only** `databricks-`-prefixed ids (`gpt-5.3-codex` → `404 'system.ai.gpt-5-3-codex' does not exist`).
- Codex's metadata table contains **only** unprefixed ids (`gpt-5.6-sol`, …).
- The two sets are disjoint by construction.

Setting `model_context_window` / `model_max_output_tokens` explicitly **does not suppress it** (retested — warning identical). Sessions still complete correctly with `end_turn`, so this is cosmetic. Options: accept and document, or strip the chunk in the provider — the latter means pattern-matching agent output, which is fragile. **Recommend: accept and document.**

---

## S7. `sandbox_mode` and `approval_policy` are INERT under the ACP adapter 🚨 (second sandbox, `<sandbox-id-b>`, deleted)

Run to settle whether the provider should ship `sandbox_mode = "danger-full-access"` (what S5 used) or the narrower `workspace-write` that upstream buzz-acp's `codex_network_env` implies. The same three-part shell task — write inside cwd, write to `/tmp`, `curl` an external host — was run under three `sandbox_mode` values:

| `sandbox_mode` | write cwd | write `/tmp` | external `curl` |
|---|---|---|---|
| `workspace-write` (+ `network_access = true`) | EXISTS | EXISTS | **FAILED** — `curl: (6) Could not resolve host` |
| `danger-full-access` | EXISTS | EXISTS | **FAILED** — same |
| `read-only` | **EXISTS** | **EXISTS** | **FAILED** — same |

**`read-only` permitted both writes.** That is decisive: `sandbox_mode` in `config.toml` has **no effect whatsoever** when codex is driven through `@agentclientprotocol/codex-acp`. The adapter governs the policy; the config key is dead text.

`approval_policy = "never"` is likewise not honoured as written — the adapter still emitted `session/request_permission` (observed in the `danger-full-access` and `read-only` runs, when codex retried a failed command with escalated permissions).

**Design consequence: the provider must NOT ship either key.** Shipping `sandbox_mode`/`approval_policy` would place text in a config file that reads like a security control, is reviewed as a security control, and constrains nothing. That is worse than omitting it. Ship only `model`, `model_provider`, and the `[model_providers.databricks]` block. `docs/PLAN.md` §5's existing argument — that `BUZZ_ACP_PERMISSION_MODE` is not a lever because buzz-acp auto-approves before the mode is consulted — extends to these two keys verbatim.

This also **closes the open question** of whether to probe `workspace-write` before implementing: the answer is that the choice does not exist.

## S8. The shell tool could not resolve DNS, in any mode ⚠️ (probe artifact — see S9)

**Scope of this claim, stated precisely:** what was measured is that name resolution failed. **Egress to a bare IP was not tested.** Do not read this section as evidence of a network-egress control — it is not one, and §5 must not cite it as a compensating control. (Same reasoning as S7: something that reads like a security boundary but was never measured as one is worse than silence.)

In all three runs above, the agent's `curl` failed with `Could not resolve host: registry.npmjs.org` — a DNS failure, not a TLS or routing one. This is **not** the sandbox's own restriction:

```
# from an ordinary SSH shell in the same sandbox, same moment
curl https://registry.npmjs.org/  ->  HTTP 200
getent hosts registry.npmjs.org   ->  resolves (12 addresses)
/etc/resolv.conf                  ->  nameserver 192.168.200.6
no HTTP(S)_PROXY set anywhere; npm proxy config null
```

`npm ci` had already pulled 25 packages from that exact host minutes earlier. So the sandbox has open egress and working DNS; only **commands launched by codex's shell tool** cannot resolve. Setting `network_access = true` did not change it, consistent with S7 — the adapter's policy is what applies.

Note this does **not** affect inference: codex's own HTTP client reached the gateway successfully in every run (`stopReason: end_turn`). The restriction is specific to the tool-execution path.

**RESOLVED by a source read — this is a probe artifact, not a runtime defect.** See S9.

## S9. Source read of buzz-acp — explains S8, and confirms `config.toml` is authoritative for inference ✅

Read against a local `block/buzz` checkout at `de139605` (2026-07-27), `crates/buzz-acp/src/config.rs`.

**`codex_network_env` (`:717-749`) injects exactly one thing and nothing else:**

```
Some(("CODEX_CONFIG", "{\"sandbox_workspace_write\":{\"network_access\":true}}"))
```

It is called on the spawn path (`:1019-1027`), matches `normalize_agent_command_identity(cmd)` against `"codex" | "codex-acp"` — so our canonical spawn command `codex-acp` **does** trigger it — and returns `None` when the relay URL cannot be parsed.

Two consequences, both load-bearing:

1. **S8 is a probe artifact, not a runtime defect.** My probe drove `codex-acp` directly with `mcpServers: []` and no relay, so buzz-acp was never in the loop and `CODEX_CONFIG` was never injected — hence no network. In a real deploy buzz-acp injects it before spawning. **The earlier concern that a deployed agent might not reach the relay is withdrawn.** Residual uncertainty worth one RUNBOOK sentence: the upstream doc comment describes the mechanism in terms of **macOS Seatbelt**, and a Lakebox sandbox is Linux (landlock/seccomp). Whether `network_access=true` opens the Linux path is unverified here and is upstream's mechanism to own — confirm at live acceptance.
2. **`CODEX_CONFIG` carries nothing about `model`, `model_provider`, or `model_providers`.** So the provider's `config.toml` is authoritative for the entire inference wiring, and fork B's premise holds. This also **resolves the evidence inconsistency** in rejecting `CODEX_CONFIG` as an option while interacting with it: we now know precisely what it does, and it does not collide with what we write.

It also explains S7's mechanism: the adapter forwards `CODEX_CONFIG` as a **session-level** override (`:708`, "via `CODEX_CONFIG` → `thread/start config`"). Session-level config outranks `config.toml`, which is why the file's `sandbox_mode`/`approval_policy` never take effect — the adapter is setting session config at thread start regardless. Inference keys survive because nothing overrides them.

## S10. Adapter source read — the exact mechanism behind S7 and S8 ✅

Read of `@agentclientprotocol/codex-acp@1.1.7`'s published bundle (`npm pack`, `package/dist/index.js`, 1.1 MB).

> **CORRECTION (added after code review).** This section originally said the adapter reads "exactly six" environment variables. **That was wrong, and the error propagated into the provider's own reject list.** It reads **eleven**:
>
> `CODEX_PATH`, `CODEX_CONFIG`, `DEFAULT_AUTH_REQUEST`, `MODEL_PROVIDER`, `INITIAL_AGENT_MODE`, `DISABLE_MCP_CONFIG_FILTERING`, `APP_SERVER_LOGS`, `XDG_RUNTIME_DIR`, `NO_BROWSER`, `CODEX_API_KEY`, `OPENAI_API_KEY`.
>
> Two failures produced the undercount, and both are worth recording because they are repeatable mistakes. First, the original scan matched only literal `process.env.X` / `process.env["X"]`; the adapter also reads through an `env` parameter (`env["NO_BROWSER"]`) and through named constants (`env[CODEX_API_KEY_ENV_VAR]`), which that pattern cannot see. Second — and worse — the scan's own output listed **eight**, including `APP_SERVER_LOGS` and `XDG_RUNTIME_DIR`, and the write-up said six anyway. The data was in hand and mis-stated.
>
> Security-relevant among the misses: **`INITIAL_AGENT_MODE`** selects an `AgentMode` preset, and `agent-full-access` sets `approvalPolicy: "never"` with `dangerFullAccess` — suppressing every `session/request_permission`, i.e. the only trace an exfiltration would otherwise leave. `CODEX_API_KEY`/`OPENAI_API_KEY` are the `readApiKeyFromEnv()` fallback on the api-key auth path. All are now rejected at the payload boundary.
>
> **Do not treat any list derived from this section as exhaustive.** `NODE_OPTIONS` and `LD_PRELOAD` redirect the credential just as effectively and are not adapter settings at all, so no reading of this bundle could ever have contained them. That class is handled by `payload.validateOwnerPATEnvVars`.

**`AgentMode` (`src/AgentMode.ts`) is the real sandbox lever, and `config.toml` never gets a say.** Three presets, each carrying its own approval policy *and* sandbox policy, applied per session:

| Mode id | `approvalPolicy` | `sandboxPolicy` | `sandboxMode` |
|---|---|---|---|
| `read-only` | `on-request` | `readOnly`, `networkAccess:false` | `read-only` |
| **`agent` ← `DEFAULT_AGENT_MODE`** | `on-request` | `workspaceWrite`, `writableRoots:[]`, **`networkAccess:false`**, `excludeSlashTmp:false` | `workspace-write` |
| `agent-full-access` | `never` | `dangerFullAccess` | `danger-full-access` |

`getInitialAgentMode()` reads `INITIAL_AGENT_MODE` and otherwise returns `DEFAULT_AGENT_MODE` = **`agent`**. This explains every observation:

- **S7's `read-only` config permitting writes** — the adapter was in `agent` mode regardless of the file. And `/tmp` succeeded because `workspaceWrite` sets `excludeSlashTmp: false`, i.e. `/tmp` is writable *by design*, not by a leak.
- **S7's `approval_policy = "never"` not honoured** — the mode supplies `on-request`, so requests are emitted; buzz-acp then auto-approves them.
- **S8's DNS failure** — `networkAccess: false` on the default mode. Not a Lakebox restriction and not a mystery.
- **Why upstream injects `CODEX_CONFIG`** — `sandbox_workspace_write.network_access=true` targets precisely the `workspaceWrite` policy that is the default. Upstream's injection and the adapter's default mode fit together exactly.

**`CODEX_PATH` — the ucode wrapper cannot be reached by accident.** `startCodexConnection(codexPath)` spawns `codexPath` with `["app-server"]` when set; when unset it resolves the **bundled** binary via `createRequire(import.meta.url).resolve("@openai/codex/bin/codex.js")` and runs it under `process.execPath`. There is **no PATH lookup**, so leaving `CODEX_PATH` unset is safe and confirms fork D. The converse is a genuine footgun: a `CODEX_PATH` arriving through `agent.env_vars` would be spawned as `<path> app-server`, and `/usr/local/bin/codex` (the `ucode` wrapper, S1) does not accept arguments — an instant, silent break.

### Design consequences

1. **Do not set `INITIAL_AGENT_MODE`.** The default `agent` mode is exactly what upstream's `network_access` injection is built for; forcing `agent-full-access` would make that injection meaningless and widen confinement for no gain.
2. **Do not set `CODEX_PATH`**, and note in the RUNBOOK that supplying it via `env_vars` breaks the runtime.
3. **`docs/PLAN.md` §5 can now state codex's confinement precisely** instead of leaving it unknown — and it is **narrower than the Claude runtime's**: codex's tool calls run under `workspaceWrite` with no network, where Claude Code's shell has neither restriction. The honest caveat: codex escalates on sandbox denial by emitting `session/request_permission`, and buzz-acp auto-approves every one — so the confinement is real but **escalatable, and always escalated**. It is a speed bump, not a boundary, and §5 must say so in the same breath rather than claiming a control.
4. `MODEL_PROVIDER` and `DEFAULT_AUTH_REQUEST` exist as adapter-level overrides; we set neither, and our `config.toml` provider block remains authoritative (consistent with S5/S9).

## S11. `initialize` succeeds with no `config.toml` — the fail-closed diagnostic is reachable ✅

Run **offline on the operator laptop** (`npm ci` of the same pinned lockfile; no sandbox, no credentials). `codex-acp` was given a `CODEX_HOME` pointing at an empty directory and sent a single `initialize` frame:

```
rc=0    stderr: (empty)
{"protocolVersion":1,"agentInfo":{"name":"@agentclientprotocol/codex-acp","version":"1.1.7"}, …}
```

This matters because `internal/deployflow/deployflow.go:746-760` runs the ACP verify handshake **before** the inference probe at `:762-766`. On the primary fail-closed path — a redeploy where the regenerated `~/.databrickscfg` has a `token` line but no `host` line, so the gate declines to write — the handshake runs with no `config.toml` at all. Had `initialize` failed there, the deploy would have died as `CodeRuntimeVerify` ("ACP initialize handshake failed", whose remedy text names claude/buzz-agent variables) instead of reaching the precise `"unset"` diagnosis, and the runtime's headline fail-closed error would have been dead code on the one path it exists for.

It does not fail. **The ordering is safe and the `"unset"` cause is reachable.** No reordering of verify vs. probe is required.

Incidental, and relevant to teardown: codex populates `$CODEX_HOME` with its own state on first run — `installation_id`, `goals_*.sqlite`, `logs_*.sqlite`, `.tmp/`. So the provider-owned `CODEX_HOME` is never empty after one launch, which is why C1's remedy must test for the **`config.toml` file specifically**, not for an empty directory.

## S12. `DEFAULT_AUTH_REQUEST` is a full endpoint-redirect primitive 🚨 (offline source read)

Follow-up read of the same bundle, prompted by a reviewer asking whether this variable merely pre-selects among the `authMethods` S4 reported. **It does considerably more.**

`isCodexAuthRequest` accepts `methodId` ∈ {`api-key`, `chat-gpt`, **`gateway`**}. In `authenticate()`:

```js
case "gateway":
  const gatewaySettings = authRequest._meta["gateway"];
  this.applyGatewayConfig({
    baseUrl: gatewaySettings.baseUrl,
    apiType: GatewayAuthMethod._meta.gateway.protocol,
    headers: gatewaySettings.headers,
    providerName: gatewaySettings.providerName,
  });
```

and `applyGatewayConfig` builds `gatewayConfig = { modelProvider: CUSTOM_GATEWAY_PROVIDER_ID, config: { base_url: params.baseUrl, http_headers: {...params.headers}, wire_api } }`.

So a `DEFAULT_AUTH_REQUEST` supplied through `agent.env_vars` can set **an arbitrary `base_url`, arbitrary HTTP headers, and a provider id that supersedes our `[model_providers.databricks]` block entirely** — bypassing `config.toml` rather than merging with it. The `api-key` branch is separately auth-bearing: it reads an inline key from `_meta["api-key"].apiKey`, falling back to `readApiKeyFromEnv()`.

**This is the single most dangerous item in the adapter's env surface**, and it is invisible to any gate that operates on `config.toml` — which is every gate the provider has. It must be rejected at the payload boundary under `inference_auth="sandbox"`, where the credential in play is the baked owner-level PAT.

Corrects the working assumption that `DEFAULT_AUTH_REQUEST` was the most benign of the unmodelled variables; it is the most severe.

---

## S13. The verify handshake's stdin shape is a codex correctness requirement 🚨 (offline, settles the S4/S11 tension)

S4 recorded that "the adapter **requires stdin to stay open**; closing it immediately yields `rc=0` with no response at all (an easy false-negative when probing)". S11 then held stdin open for 6 s and got a clean reply, which read as reassurance. **It was not** — S11 simply avoided the case S4 warned about, and the shipped `install.BuildVerifyCommand` is the case S4 warned about.

Measured offline against the real adapter, using the exact rendered pipeline:

| stdin shape | rc | stdout | `agentInfo` |
|---|---|---|---|
| `printf FRAME \| timeout 10 codex-acp` (as shipped) | 0 | **0 bytes** | **absent** |
| `{ printf FRAME; sleep 1; } \| …` | 0 | 690 bytes | present |
| `{ printf FRAME; sleep 2; } \| …` | 0 | 690 bytes | present |

`rc=0` with empty output is the worst possible shape: the pipeline succeeds, so the failure surfaces only as the `AgentInfoMarker` scan finding nothing — reported as `install.runtime_verify`, whose remedy text points at inference variables that have nothing to do with it. **Every codex deploy would have failed there**, after a successful 362 MB adapter install.

`claude-agent-acp` is genuinely different (P5: answers and exits 0 on EOF in ~355 ms), so the fix is per-runtime — `AdapterSpec.VerifyStdinHoldSeconds`, 0 for claude and buzz-agent, 2 for codex. A global sleep would have taxed every deploy to fix one runtime.

**Lesson worth carrying: a probe that establishes a behaviour must be run in the shape production uses.** S11's value was real (it proved `initialize` succeeds with no `config.toml`) but its stdin handling silently diverged from the shipped command, and that divergence hid a total runtime failure through the entire plan and implementation.


## What this means for the plan

1. **Delete the `wire_api = "chat"` fallback branch entirely** (G2). One code path, not two.
2. **Use `base_url = "{host}/ai-gateway/codex/v1"`** — the surface the image itself uses (S2). `openai/v1` also works and is a viable second option.
3. **Set `CODEX_HOME`; never write `~/.codex/config.toml`** (S2) — the symlink makes that write both destructive and non-persistent.
4. **Canonicalize the spawn command to `codex-acp`, never bare `codex`** (S1). The existing `PATH` prepend plus a non-colliding binary name is what already protects the Claude runtime; keep codex inside the same invariant instead of relying on either mechanism alone.
5. **Mirror the existing Claude install pattern**: commit `internal/install/lockfiles/codex-acp-1.1.7.package-lock.json` alongside the existing `claude-agent-acp-0.63.0` one.
6. **Default model: `databricks-gpt-5-3-codex`, as a hardcoded const with no payload input.** An override via `provider_config` was considered and *deferred*, not adopted: any payload-derived value would be interpolated into a heredoc inside a shell-sourced file, which is the arbitrary-command-execution vector `internal/payload/payload.go:113-118` exists to prevent. The gateway accepts only `databricks-`-prefixed ids (G5) and the S6 warning fires regardless, so an override buys nothing observable.
7. **Document the S6 warning** in the RUNBOOK so operators do not file it as a bug.
8. **Emit no `sandbox_mode` and no `approval_policy`** (S7) — both are inert under the adapter, and inert text that reads as a security control is worse than no text.
9. **Record the S8 shell-tool egress gap in `docs/UPSTREAM_BUZZ_GAPS.md`** and re-check it during live acceptance.
