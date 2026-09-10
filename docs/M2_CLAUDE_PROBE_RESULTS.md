# M2 Claude Code Probe Session Results

> Live session on the `EXAMPLE_PROFILE` (`workspace.example.invalid`, **`example-region-1`**), 2026-07-27/28. Databricks CLI v1.8.0. Sandbox `<sandbox-id>`, `--no-autostop`, **deleted at session end**. Adapter under test: `@agentclientprotocol/claude-agent-acp@0.63.0`.
>
> Auth note: gateway probes P1–P3 used a CLI **OAuth** bearer token from the operator laptop; the in-sandbox probes used the sandbox's **baked creator-identity PAT**, derived from `~/.databrickscfg` exactly the way `nest.SandboxAuthSnippet` does. M0.5 §2 already proved a PAT works on this endpoint with the same `Authorization: Bearer` header shape.
>
> Answers the probe questions in issue #2 and the OQ/PM items in `.omc/plans/issue-2-merged-plan-FINAL.md`. **Three planned findings were overturned by live evidence; one new security finding was confirmed.**

## Sandbox baseline

`sandbox-agent@/home/sandbox-agent`, Node **v22.22.3**, npm **10.9.8**, 4 vCPU, 8.1 GiB RAM.

**`/home/sandbox-agent` has 98 G total / 93 G available** — not the ~10 GB overlay the plan assumed. Every disk-exhaustion concern (plan PM-1, critic M1) is over-stated by an order of magnitude and can be demoted.

---

## P1. SSE streaming on `POST {host}/ai-gateway/anthropic/v1/messages` — VERIFIED ✅

`HTTP/2 200`, `content-type: text/event-stream`, `x-amzn-bedrock-content-type: application/json`. Full Anthropic event sequence delivered incrementally: `message_start` → `content_block_start` → 4× `content_block_delta` → `content_block_stop` → `message_delta` (`stop_reason: end_turn`) → `message_stop`.

**This clears the only hard go/no-go gate**, for both `inference_auth` modes — streaming is a property of the endpoint, not of the credential that reached it.

Incidental: the response's `message.model` is `claude-haiku-4-5-20251001`, **not** the requested `databricks-claude-haiku-4-5`. The gateway rewrites model ids on the way back; nothing may assume request/response id equality.

## P2. `POST .../v1/messages/count_tokens` — NOT SUPPORTED, and harmless ❌→✅

```
HTTP 400 BAD_REQUEST — "Request path '/ai-gateway/anthropic/v1/messages/count_tokens' doesn't match
any known API type and is classified as an unmanaged api request."
```

No workaround exists: `Databricks-Model-Provider-Service: anthropic` → `404 'anthropic' does not exist`, `bedrock` → `404`, `Anthropic` → `400 Invalid name`.

**But P6 proved this is non-fatal.** Full sessions complete with `stopReason: end_turn` and **empty stderr**. Claude Code degrades gracefully. **OQ-2 is closed with no action required** — no opt-out env var is needed.

## P3. Gateway model catalog — VERIFIED ✅

All `READY`: `databricks-claude-opus-5 / -opus-4-8 / -opus-4-7 / -opus-4-6 / -opus-4-5 / -opus-4-1`, `-sonnet-5 / -sonnet-4-6 / -sonnet-4-5 / -sonnet-4`, `-haiku-4-5`.

## P2b. `x-api-key` is rejected by the gateway — VERIFIED ❌ (new)

`x-api-key: <token>` + `anthropic-version: 2023-06-01` → `HTTP 401 "Credential was not sent or was of an unsupported type for this API."`

`ANTHROPIC_AUTH_TOKEN` (→ `Authorization: Bearer`) is not merely preferred over `ANTHROPIC_API_KEY` (→ `x-api-key`); it is **the only one that works**. An `ANTHROPIC_API_KEY` has no working configuration against this gateway.

---

## P4. `npm ci` install — VERIFIED ✅, and PM-1 is invalidated

| Measurement | Result |
|---|---|
| Lockfile generation (`--package-lock-only`) | **4 s** |
| `npm ci` (warm cache) | **5 s** |
| `npm ci` (**cache purged**, true cold) | **6 s** |
| `npm ci --ignore-scripts` | **4 s**, bin still executable ✅ |
| Idempotent re-run after restart | **5 s** |
| `node_modules` | 570 MB |
| `~/.npm` cache | 169–197 MB |
| Total `$HOME` footprint | 738 MB of 98 G (**1 %**) |

**PM-1 (deploy-budget blowout → teardown deletes the sandbox → operator retries forever) is comprehensively invalidated.** A cold adapter install costs ~6 s against a 550 s budget. It does **not** need its own SSH round trip, sub-budget, or distinct timeout code. Delete that entire mitigation chain from the plan.

**OQ-9 answered: use `--ignore-scripts`.** It works, is marginally faster, and removes postinstall script execution — free supply-chain hardening in the mode where the baked PAT is present.

**Lockfile integrity — Principle 2 is satisfiable, and the plan's estimates were wrong:**

- **112 packages** (111 with `resolved`), not the "~12" the plan assumed. Expect ~800+ lines of embedded JSON, not "~120".
- **`missing_integrity = 0`** — every resolved package carries a sha512.
- **All 8 platform variants are pinned in one lockfile** (`linux-x64`, `linux-x64-musl`, `linux-arm64`, `-musl`, `darwin-x64/arm64`, `win32-x64/arm64`). This resolves the architect's N2 concern differently and better than proposed: no `--os`/`--cpu` generation flags are needed, because a single lockfile pins every platform with integrity and `npm ci` selects the matching one. Installed on this host: `claude-agent-sdk-linux-x64` **and** `-linux-x64-musl`.

## P5. ACP `initialize` handshake — VERIFIED ✅, and it simplifies Step 4

```
exit_rc=0   elapsed=355 ms   stderr: (empty)
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{…},
 "agentInfo":{"name":"@agentclientprotocol/claude-agent-acp","title":"Claude Agent","version":"0.63.0"},
 "authMethods":[],"_meta":{"steering":{"supported":true}}}}
```

Two planned complications evaporate:

1. **`agentInfo` IS present** → `install.AgentInfoMarker` works unchanged. **OQ-4 closed**; no per-runtime marker is needed. `VerifySpec` reduces to *the binary path only*.
2. **The adapter exits cleanly on stdin EOF (`rc=0`)** → the premise behind the `set +e` / `BUZZ_VERIFY_RC=` rework ("a Node ACP server may not exit; `timeout` would return 124 and kill the deploy") is **false**. `BuildVerifyCommand`'s existing exit-status semantics are correct as-is. Keep the `2>&1` capture for diagnostics; drop the exit-code rework.

`protocolVersion: 1` is accepted and echoed back as `1`.

## P6. Full headless round trip in `sandbox` mode — VERIFIED ✅

Credentials derived from the baked `~/.databrickscfg` exactly as `SandboxAuthSnippet` does; `ANTHROPIC_BASE_URL={host}/ai-gateway/anthropic`, `ANTHROPIC_AUTH_TOKEN=<derived PAT>`, `mcpServers: []`.

```
[init] ok           0.3 s
[session/new] ok    sid=ef8e9704-…
[prompt] streamed → GATEWAY_OK
[prompt] {"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":7,"cachedWriteTokens":35655}}   2.4 s
STDERR: (empty)
```

**Zero-token `inference_auth: "sandbox"` works end to end through the AI Gateway.** Also settled here:

- **`mcpServers: []` produces a fully responsive agent** — it answered normally with no MCP servers at all. Desktop parity (`discovery.rs:107` `mcp_command: None`) is correct, and the buzz-agent silent-agent failure does not reproduce. **OQ-7 closed** without needing the expensive P7.
- **No `~/.claude.json` / `~/.claude/settings.json` seeding is required.** Both were auto-created on first run (`~/.claude` = 180 KB). **OQ-6 closed, no action.**
- **No standalone `claude` CLI install is needed.** The SDK vendors its own binary at `node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude`. (The Lakebox image also happens to ship `/usr/local/bin/claude`, but the design must not depend on that — point `CLAUDE_CODE_EXECUTABLE` at the vendored path if pinning is wanted.) **OQ-5 closed: Branch A.**
- `session/new` advertises a `mode` config option (`auto`/`default`/`acceptEdits`/`plan`) with `currentValue: "default"`.

## Model-selection matrix — `ANTHROPIC_MODEL` must NOT be emitted ⚠️

| # | Env | Result |
|---|---|---|
| A | no `ANTHROPIC_MODEL`, no `ANTHROPIC_API_KEY` | ✅ `GATEWAY_OK`, `end_turn` |
| B | `ANTHROPIC_MODEL=databricks-claude-sonnet-4-5` | ✅ `GATEWAY_OK`, `end_turn` |
| C | `ANTHROPIC_MODEL=databricks-claude-haiku-4-5` | ❌ `model_not_found` — *"issue with the selected model (**claude-haiku-4-5-20251001**)"* |
| D | `ANTHROPIC_API_KEY=""` (set, empty) | ✅ `GATEWAY_OK`, `end_turn` |
| E | `ANTHROPIC_API_KEY="sk-ant-bogus"` alongside `ANTHROPIC_AUTH_TOKEN` | ✅ `GATEWAY_OK`, `end_turn` |

**Case C is the finding.** The adapter/SDK rewrites `databricks-claude-haiku-4-5` into the canonical `claude-haiku-4-5-20251001`, which the gateway does not serve — so a *valid gateway model id* becomes a *hard per-turn failure*. The mangling is inconsistent (sonnet survives, haiku does not), so no gateway id can be trusted through this path.

**Decision: emit no model env var for the Claude runtime** — neither `ANTHROPIC_MODEL` nor `BUZZ_ACP_MODEL`. Case A proves the adapter's own default works. This matches desktop parity (`discovery.rs:118-119`: `model_env_var: None`, `supports_acp_model_switching: false`) and now has live evidence behind it rather than an inference. **OQ-3 closed: no small/fast-model pin is possible or needed.**

## PM-2 / critic C2 (double-header 401) — **OVERTURNED** ✅

Cases **D** and **E** both succeed. An `ANTHROPIC_API_KEY` that is set-but-empty, *and* one set to a bogus `sk-ant-…` value, **both complete normally** alongside `ANTHROPIC_AUTH_TOKEN`. The adapter does not exhibit the raw-SDK "sends both headers → API rejects" behavior on this path; `ANTHROPIC_AUTH_TOKEN` wins cleanly.

Consequences: the plan's PM-2 premortem, its payload-level both-keys-set rejection, the `${VAR+x}` set-ness rework, and the 5-row credential matrix test are all **unnecessary**. (An earlier apparent reproduction was misattributed — it was case C's model id, not the API key. The clean matrix isolates it.) Rejecting `ANTHROPIC_API_KEY` for this runtime remains *defensible* on the grounds that it cannot work against the gateway (P2b), but it is a usability choice, not a correctness guard.

## Critic C1 (credential egress) — **CONFIRMED, and it is the session's most important finding** 🚨

The sandbox has **open egress to `api.anthropic.com`**:

```
POST https://api.anthropic.com/v1/messages   HTTP 401   connect=0.008 s  total=0.147 s
{"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"},
 "request_id":"req_011CdTtBgiS62trTZyjDaxty"}
DNS: api.anthropic.com → 2607:6bc0::10
```

(Probed with a deliberately invalid dummy token — no real credential left the workspace.)

With `ANTHROPIC_BASE_URL` unset, Claude Code falls back to `https://api.anthropic.com`, and a session started that way hangs at `session/prompt` rather than failing fast. So the failure chain is real and reachable:

> `env` mode + `DATABRICKS_TOKEN` supplied + `DATABRICKS_HOST` omitted → base URL stays unset (a *silent* no-op) → the auth token is still derived → **a live `dapi…` workspace PAT is sent in an `Authorization: Bearer` header to Anthropic's production API.**

`install.runtime_verify` still passes (the handshake never touches the LLM), so the deploy reports healthy.

**Required mitigations, both:**
1. **Snippet-level, fail-closed, identical in both modes:** derive `ANTHROPIC_AUTH_TOKEN` **only inside the branch where `ANTHROPIC_BASE_URL` is known-set**. No base URL → no token. This is the only defense that works at runtime in `sandbox` mode, where no payload-time check can see the derived host.
2. **Payload-level, fail-loud:** for the Claude runtime, `DeployRequest.Validate()` requires exactly one coherent inference source (`inference_auth == "sandbox"`, or `DATABRICKS_HOST` in `env_vars`, or an explicit `ANTHROPIC_BASE_URL`).

The test must assert `ANTHROPIC_AUTH_TOKEN` is **absent** when the host is missing — the plan's originally-specified `TestClaudeEnv_MissingHostDoesNotFailSourcing` passes in the leaking state.

## P8. Persistence across stop/start — VERIFIED ✅

| Check | Before stop | After start |
|---|---|---|
| `package-lock.json` md5 | `25fdaa28…a879c6` | `25fdaa28…a879c6` (identical) |
| `npm-claude` size | 570 MB | 570 MB |
| `node_modules/.bin/claude-agent-acp` | executable | executable |
| `initialize` handshake | ok | ok |
| Baked `~/.databrickscfg` | present, `auth_type = pat` | **present** |

The adapter tree survives a stop/start byte-identical, so the marker-file skip is sound, and re-running `npm ci` is a 5 s no-op. **The baked creator-identity credential also survives a restart**, which `inference_auth: "sandbox"` depends on at every relaunch.

## P9. Adapter memory footprint — VERIFIED ✅

Three concurrent adapters, sampled while alive: **115.3 / 115.1 / 115.7 MB RSS**, ~115 MB each idle.

On 8.1 GiB (7.5 GiB available), `BUZZ_ACP_AGENTS=N` costs ~115 MB × N at idle before session working set. Comfortable for realistic parallelism; no clamp is needed for small N. **OQ-10: no ceiling required, but record the per-process cost in RUNBOOK.**

---

## Consequences for the plan

**Simplifications (delete work):**
- PM-1's adapter-install timeout mitigation chain — install is 6 s, not minutes.
- Per-runtime verify marker / `VerifySpec.Marker` — `agentInfo` is present; only the binary path differs.
- The `BUZZ_VERIFY_RC=` exit-code rework — the adapter exits 0 on EOF.
- PM-2's double-key guard, the `${VAR+x}` rework, and the 5-row credential matrix — not reproducible.
- P7 (zero-MCP responsiveness) — already answered by P6.
- `~/.claude.json` seeding sub-step — not needed.
- Standalone `claude` CLI install (Branch B) — the SDK vendors its own.
- Disk-exhaustion cause branch in `install.adapter_exec` — 738 MB of 98 G.
- `--os`/`--cpu` lockfile generation flags — one lockfile pins all 8 platforms.

**Additions / changes (new work):**
- **C1 fail-closed coupling + payload guard** — the one genuinely new requirement, and the highest-priority item in the change.
- **Emit no model env var** for the Claude runtime (omit both `ANTHROPIC_MODEL` and `BUZZ_ACP_MODEL`); document that model selection is the adapter's default.
- Use `--ignore-scripts` in the install script.
- Expect a ~800-line embedded lockfile, not ~120.
- RUNBOOK: ~115 MB per adapter process; `~/.claude` transcript growth.

**Still unprobed:** the runtime-switch-on-a-reused-sandbox path and the step-8 prelaunch-kill race (critic C3/C4) are provider-side logic, not adapter behavior — they need the implementation before they can be exercised, and belong in the E2E matrix.
