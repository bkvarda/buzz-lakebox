# Live Acceptance Record — `buzz-backend-databricks-lakebox`

`docs/RUNBOOK.md` §9 is the **procedure**; this file is the **record**.
Every row is `LIVE` / `CI-ONLY` / `NOT RUN` with an evidence pointer a
third party can follow. No row may be blank.

**No secrets.** Only redacted output goes in this file; sandbox ids and
timestamps are fine — never paste raw payloads or env dumps. The check is
value-shaped rather than word-shaped, so that prose *about* credentials
(here and in the runbook) does not trip it while an actual leaked one
does:

```sh
grep -nEi 'nsec1[023456789acdefghjklmnpqrstuvwxyz]{20,}|dapi[0-9a-f]{16,}|(token|secret|password|api_?key)[=:][^ ]{8,}' \
  docs/ACCEPTANCE.md docs/RUNBOOK.md    # must find nothing
```

---

## §6 M1 — Deploy

Cross-checked against `docs/RUNBOOK.md` §9's Deploy block (items 1–4).

### 1(a) — non-member throwaway key

RUNBOOK §9 item 1's sub-clauses, run live 2026-07-26 with the operator
`deploy --payload-file` path on profile `EXAMPLE_PROFILE` and a throwaway key
minted for the occasion (npub12 `<redacted-npub12>`). Fresh-create verify
failures run §4.3 teardown, so the in-sandbox rows were captured by a
probe SSH racing the provision window (§9 item 1 documents this); the
log rows come from the tail the deploy embeds in its error text.

The first attempt (23:21Z, sandbox `<sandbox-id-a>`) returned
`verify.process_dead` for a log that plainly carried the terminal
relay-denial line: the classifier checked process liveness first, and a
non-member key kills buzz-acp in ~1 s, so `relay_denied` was unreachable
in exactly its target scenario. The commit introducing this row fixes
the ordering in `deployflow.verifyLaunch` (falsifier:
`TestDeploy_LaunchVerifyFails_TerminalError_DeadProcess_ClassifiesRelayDenied`);
the rows below are from the rerun after that fix.

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Deploy fails with `verify.relay_denied` | LIVE | `{"ok":false}` `[verify.relay_denied]` in 31 s wall clock (23:26:10→23:26:42Z) | 2026-07-26 | `<sandbox-id-b>` |
| Sandbox created for the attempt, then torn down (absent from `sandbox list` once delete settles) | LIVE | id visible in `sandbox list` 2 s after deploy start and named in the error text; `stopping…` immediately after the deploy returned, gone thereafter | 2026-07-26 | `<sandbox-id-b>` |
| Binaries (all five, `buzz-agent` ACP-initialize-verified) installed in `$HOME` | LIVE | mid-deploy probe: `bin/` = `buzz, buzz-acp, buzz-agent, buzz-dev-mcp, git-credential-nostr`; the 23:21Z run's embedded log tail shows `agent initialized … "name":"buzz-agent"` | 2026-07-26 | `<sandbox-id-b>` |
| Env file is `0600` | LIVE | mid-deploy probe `stat -c %a $HOME/.buzz-backend/env` → `600` | 2026-07-26 | `<sandbox-id-b>` |
| In-sandbox `databricks current-user me` fails (PAT reset) | LIVE | mid-deploy probe: exit code 1 | 2026-07-26 | `<sandbox-id-b>` |
| acp.log vocabulary; buzz-acp dead at N=10 s | LIVE | 23:21Z error tail carries `buzz-acp starting: relay=wss://…` and `initial relay connect failed with terminal error: Auth failed: restricted: not a relay member`; post-fix, the `relay_denied` code itself only fires on (process dead at N=10 s ∧ terminal-error line), so the 23:26Z error code doubles as the liveness evidence | 2026-07-26 | `<sandbox-id-a>`, `<sandbox-id-b>` |

### 1(b) — member test key

Run live 2026-07-27 with the operator `deploy --payload-file` path and
the "Throwaway Buzz" member test agent (pubkey `<redacted-test-pubkey>`), minted in
Buzz Desktop the same morning.

Finding on the way in: the first attempt carried `auth_tag: ""` and the
relay denied it (`verify.relay_denied`) even though the key IS a member
— Buzz Desktop had deployed the same key successfully minutes earlier
(sandbox `<sandbox-id-c>`, `agent_pool_ready agents=24`). The
auth tag is part of relay AUTH, not optional metadata: on the wire, a
member key with an empty tag is indistinguishable from a non-member.
RUNBOOK §7's `verify.relay_denied` remedy now says so. This does not
weaken the 1(a) rows — those keys were genuinely non-members — but a
`relay_denied` alone does not prove non-membership.

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| `{ok:true, agent_id}` in < 180 s | LIVE | `{"ok":true,"agent_id":"<sandbox-id-d>"}` in **31.4 s** wall clock (start 16:34:40Z), fresh create — the prior sandbox was undeployed first, dropping its mapping | 2026-07-27 | `<sandbox-id-d>` |
| `pgrep -f '[b]uzz-acp'` alive at N=10 s | LIVE | `{ok:true}` is gated on the deploy's own N=10 s process check returning alive; post-hoc `pgrep` returned a live pid | 2026-07-27 | `<sandbox-id-d>` |
| `acp.log` contains `agent_pool_ready agents=N` | LIVE | `agent_pool_ready agents=1` present | 2026-07-27 | `<sandbox-id-d>` |
| `acp.log` contains no terminal-error line | LIVE | `grep -c "terminal error"` → 0 | 2026-07-27 | `<sandbox-id-d>` |
| `status <id>` → `acp_running: true` | LIVE | `{"sandbox_status":"Running","acp_running":true}` | 2026-07-27 | `<sandbox-id-d>` |

### Item 2 — idempotent redeploy

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Redeploy same agent → same `agent_id`, no second sandbox, one buzz-acp process afterward | LIVE | Redeploy of the identical payload returned the same `agent_id` in 24.8 s; `sandbox list` unchanged (no second sandbox); `.installed_version` mtime **identical** before/after (`1785170092` — the install skip path ran, no .deb re-download); exactly one buzz-acp process group; `agent_pool_ready` present for both launches. (History: commit 5abe10b records this mechanism failing live pre-state-file-fix — every redeploy orphaned the previous sandbox.) | 2026-07-27 | `<sandbox-id-d>` |

### Item 3 — induced-failure teardown

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Kill the deploy's `install-exec` SSH child on a fresh create → `{ok:false}` naming the sandbox id, sandbox absent from `sandbox list` afterward | LIVE | `pkill -f 'buzz-step:install-exec'` fired 6 s into the deploy (23:29:29Z); deploy returned `[install.exec] … signal: terminated` naming the sandbox in 8.4 s; by 23:29:56Z `sandbox list` showed only the pre-existing live sandbox — teardown deleted it, no manual cleanup | 2026-07-26 | `<sandbox-id-e>` |

---

## §6 M2 — Lifecycle

One row per RUNBOOK §9 Lifecycle line.

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| `stop <id>` → Stopped, agent dead | NOT RUN | none found | — | — |
| `start <id>` → Running + agent alive ≤ 60 s, no redeploy | NOT RUN | none found | — | — |
| In-sandbox `databricks current-user me` still fails after `start` (PAT stub survived restart) | NOT RUN | none found | — | — |
| `status <id>` emits stable JSON | NOT RUN | none found | — | — |
| `undeploy <id>` → gone from `sandbox list` | NOT RUN | none found | — | — |

---

## §6 M3 — PAT opt-out

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Deploy with `keep_workspace_pat: true` → in-sandbox `databricks current-user me` SUCCEEDS as creator | NOT RUN | none found | — | — |

Note: CI proves the two decisions behind this check — the provider
neither writes the PAT stub at deploy nor re-asserts it from
`launch.sh` on any later relaunch. It cannot prove the live outcome,
which depends on the sandbox image's baked PAT actually being present
and working as the workspace creator.

---

## §6 M3 — fresh-machine walkthrough

Per RUNBOOK §8.

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Fresh-machine walkthrough (CLI check → install → `doctor` → `info` → deploy → `status`) succeeds end-to-end | NOT RUN | none found | — | — |

---

## Zero-token inference auth (issue #10)

Per the zero-token plan's Live verification checklist
(`.omc/plans/zero-token-inference.md`). Tracking issue:
https://github.com/IceRhymers/buzz-lakebox/issues/10 — do not close until
every row below is LIVE.

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| 1. Zero-token happy path: fresh sandbox deploy with `inference_auth: "sandbox"` and no `DATABRICKS_TOKEN`/`DATABRICKS_HOST` env vars → deploy succeeds, derived env shows host/token set in-sandbox, @mention answered via the workspace AI Gateway | NOT RUN | none found | — | — |
| 2. Rotation tolerance: `sandbox stop` then operator `start` on a sandbox-mode agent → agent relaunches and still answers; baked cfg observed across the cycle | NOT RUN | none found | — | — |
| 3. Env-mode regression: default-config deploy (no `inference_auth`) → in-sandbox cfg is the stub, `current-user me` fails, agent with `env_vars` token still answers | NOT RUN | none found | — | — |
| 4. Precedence: sandbox mode WITH `env_vars` DATABRICKS_HOST/TOKEN supplied → those values survive env-file sourcing unchanged (derivation is a no-op) | NOT RUN | none found | — | — |
| 5. Stub-clobbered reuse: redeploy an env-mode sandbox with `inference_auth: "sandbox"` → fails with `[provision.sandbox_auth]` (cause (a), stub marker) and the delete-and-redeploy remedy; old env-mode agent's env file and process survive the failed switch; remedy then succeeds | NOT RUN | none found | — | — |
| 6. Redaction spot-check: force a failure that could echo a derived `dapi…` token into acp.log or remote output → rendered output shows `[REDACTED]`, never a bare token | NOT RUN | none found | — | — |

---

## §17 — deploy-time MCP slot verification

Per `.omc/plans/17-mcp-slot-verify.md` §4 (the infra-gated / live split). All
CI-verifiable behaviour is green; the rows below are infrastructure-gated
(require real registered MCP commands on a sandbox) and also finalize the
provisional 15s `mcpVerifyTimeoutSeconds` budget.

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Deploy an `mcp_servers` entry that starts then exits → deploy fails with `install.mcp_verify` + remedy | NOT RUN | infra-gated (needs a real broken MCP command on the sandbox) | — | — |
| Deploy with a wrong-sha256 extra-binary MCP artifact → deploy fails (at #15's extra-bin exec, or at `install.mcp_verify` if present-but-broken) with an actionable code | NOT RUN | infra-gated | — | — |
| Deploy real `mcp_servers: [buzz-dev-mcp, shellbox-mcp]` → `mcp-verify` passes end-to-end AND records the measured MCP cold-start/handshake latency (backfills the `// measured:` comment + finalizes the budget) | NOT RUN | infra-gated (feeds the #16 live-pending row) | — | — |
| Deploy a single `mcp_servers: [buzz-dev-mcp]` (McpDirect) → `mcp-bin-write` + `mcp-verify` run, the direct child receives the sourced `BUZZ_*` env, and the agent answers a relay mention | NOT RUN | infra-gated | — | — |

---

## Pre-merge undeploy probe

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| `undeploy` exercises `SandboxStatus` → real SSH → secret-shred command → delete-while-Running → `ForgetSandbox` returning 0 mappings | NOT RUN | none found | — | — |

Scope note: when run, this probe covers only the path above. It does
**not** cover deleting a sandbox with a live buzz-acp inside it, and it
does **not** cover a non-zero mapping removal (i.e. `ForgetSandbox`
actually dropping a tracked entry) — both remain unexercised regardless
of this probe's outcome.

---

## Substitutions

Places where RUNBOOK §9's procedure narrows PLAN §6 M1, so a future
reader knows a `LIVE` row there means the proxy passed, not the literal
clause:

- PLAN 1(a) says buzz-acp "exits 1"; the operator cannot observe a
  detached process's exit code, so the proxy is the deploy's own N=10 s
  process check — post-classifier-fix, `verify.relay_denied` fires only
  when that check found the process dead AND the log carried the
  terminal-error line.
- The 1(a) in-sandbox rows (binaries, env perms, PAT reset) are observed
  mid-deploy by a probe racing the provision window, because §4.3
  failure teardown deletes a freshly created sandbox before the deploy
  returns. There is no post-hoc sandbox to inspect; the teardown itself
  is one of the row's pass criteria.

---

## Blocked

Nothing in §6 M1 is blocked any longer: the member test key exists
("Throwaway Buzz", minted 2026-07-27) and the 1(b)/item-2 rows are LIVE.
The M2 Lifecycle rows need a member-key sandbox to run against — if the
test agent's sandbox has been undeployed since, redeploy it first (its
auth_tag lives in Buzz Desktop's local store; deploy payloads must carry
it, see the 1(b) note).
