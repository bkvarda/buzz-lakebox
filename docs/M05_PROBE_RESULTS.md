# M0.5 Probe Session Results

> Live session on `EXAMPLE_PROFILE` (`example-region-1`), 2026-07-24 22:17–22:35 UTC (run in two sittings: the first was interrupted mid-WSS-window; the second re-verified probes 1–3/5/6 against the live sandboxes and observed the WSS-idle outcome for real). Databricks CLI v1.8.0. Buzz release `v0.4.24` (`Buzz_0.4.24_amd64.deb`, sha256 `ee9e58cf92707993f24f2eed18721ece6029e0b869c71770ad4a5d6e05f820d2`). Two sandboxes: `m05-work` (`<sandbox-id-a>`, interactive probes) and `m05-wss-idle` (`<sandbox-id-b>`, untouched idle window). Both deleted at session end. Answers the four questions PLAN.md §6 M0.5 poses, plus the AI Gateway question raised by the owner. Throwaway nostr key only; no real identity left the keyring.

## 1. SSH-stdin secret transport — VERIFIED ✅

```
printf 'stdin-probe-payload-12345\n' | databricks sandbox ssh <id> -p EXAMPLE_PROFILE -- 'cat > /tmp/stdin-probe && sha256sum /tmp/stdin-probe'
```

Byte-identical: sha256 `e6a4d9f7…eca24` (26 bytes) matched on both sides, exit 0. **PLAN §4.4 step 7's primary transport works as designed; no fallback needed.** (The revised fallback — raw ssh over CLI-generated ProxyCommand — remains documented but unexercised.)

Environment facts: in-sandbox user is `sandbox-agent` (not `user`), `$HOME=/home/sandbox-agent`, Node v22.22.3.

## 2. AI Gateway inference from in-sandbox (owner question) — VERIFIED end-to-end ✅

The baked `~/.databrickscfg` PAT works against the workspace AI Gateway from inside the sandbox:

- `GET {host}/api/ai-gateway/v2/endpoints` → HTTP 200 (buzz-agent's `databricks_v2` discovery route, `crates/buzz-agent/src/catalog.rs`)
- `GET {host}/api/2.0/serving-endpoints` → HTTP 200, full FMAPI catalog (claude-opus-4-8, claude-opus-5, gpt-5-x, gemini, llama, …)
- Direct inference: `POST {host}/ai-gateway/anthropic/v1/messages` with `databricks-claude-opus-4-8` → HTTP 200 in **1.44 s**, correct completion
- **Full buzz-agent ACP round-trip**: `BUZZ_AGENT_PROVIDER=databricks_v2 DATABRICKS_HOST=… DATABRICKS_TOKEN=<PAT> DATABRICKS_MODEL=databricks-claude-opus-4-8`, then `initialize` → `session/new` (returns the whole gateway model catalog + `currentModelId`) → `session/prompt` → streamed `agent_message_chunk` → `{"stopReason":"end_turn"}` in **3.5 s**.

Consequences for the plan:
- Inference auth is consumed from **env** (`DATABRICKS_TOKEN` static-bearer path; default is browser OAuth, not viable headless — `crates/buzz-agent/README.md`), not from `~/.databrickscfg`. The PAT-file reset (PLAN §4.4 step 4) and AI-Gateway inference are **compatible**: reset the file, pass the token explicitly in the env file.
- v0 deploy must include `DATABRICKS_HOST`, `DATABRICKS_TOKEN`, `DATABRICKS_MODEL` (or map from payload `model`) in the agent env when `BUZZ_AGENT_PROVIDER=databricks|databricks_v2`.
- Which token is an owner policy question (workspace-wide PAT vs least-privilege SP token with CAN QUERY on gateway endpoints); the mechanism is proven with the baked PAT.

## 3. buzz-acp behavior with a non-member key (403) — fails fast, exit 1 ✅

With a throwaway nsec (generated in-sandbox via `nostr-tools`), valid agent env, real relay:

```
INFO buzz_acp: buzz-acp starting: relay=… pubkey=… agent_cmd=…
INFO buzz_acp: agent initialized: {"agentInfo":{"name":"buzz-agent","version":"0.1.0"},…}
INFO buzz_acp: agent_pool_ready agents=1
WARN buzz_acp::relay: initial relay connect failed with terminal error: Auth failed: restricted: not a relay member
Error: relay connect error: Auth failed: restricted: not a relay member
```

- **Exit 1 within ~1 s.** No crash-loop, no lingering process (`pgrep` → none).
- Separately: invalid agent env (missing `DATABRICKS_HOST`) also fails fast: `Error: all 1 agents failed to start — cannot continue`, exit 1 — agent subprocess config errors are loud and immediate.
- Actual log vocabulary for M1 signals: startup line `buzz-acp starting: relay=… pubkey=…`; healthy-pool line `agent_pool_ready agents=N`; terminal failure line `initial relay connect failed with terminal error`.
- Defaults observed: `idle_timeout=900s max_turn=7200s subscribe=Mentions dedup=Queue respond_to=owner-only presence=true typing=true memory=true permission_mode=bypassPermissions`.

**Consequence for M1 acceptance (revises PLAN §6 M1):** `pgrep`-alive-after-N only proves success with a **relay-member key**. With a throwaway key the deterministic expected outcome is exit 1 + the terminal-error line — so the M1 live test splits into (a) non-member key → deploy's launch-verification correctly reports the documented 403 failure, and (b) a member test key (a real minted test agent) → pgrep alive + `agent_pool_ready` + no terminal-error line. N can be small: terminal failure lands ~1 s after launch; N=10 s is ample.

## 4. WSS-vs-idle — WSS relay traffic does NOT prevent idle autostop ❌

Setup: fresh sandbox, Node script (`~/wss-holder.mjs`) connecting to the production relay via WSS, launched with `setsid nohup`, then **zero SSH activity** from 22:18:52Z. Default idle timeout: 10 min.

What the holder log (`~/wss-holder.log`, 176 lines, 22:18:51.504Z → 22:29:50.950Z) actually shows: the relay accepts the connection, sends its NIP-42 `AUTH` challenge, then closes (code 1005) after ~7 s because the throwaway key never answers the challenge; the script reconnects 5 s later. Net effect: **continuous WSS open/AUTH/close traffic every ~5–7 s for the full 11-minute window** — not one persistently-held socket.

Result, observed by external `databricks sandbox status` polling (45 s interval): `running` at 22:29:50Z, **`stopped` at 22:30:36Z** — i.e. autostop fired 11m00s–11m44s after the last SSH disconnect, while relay round-trips were still landing every few seconds (last log write 22:29:50.950Z, right at the stop boundary). Constant outbound network traffic from an in-sandbox process does not count as activity; the idle timer keys on SSH/gateway activity only.

Caveat: because the throwaway key can't pass relay AUTH, the *authenticated long-held* socket variant (what a real member buzz-acp holds) wasn't exercised. It would be surprising if an idle authenticated socket counted as activity when active reconnect traffic doesn't, but the member-key M1 test (§3 consequence, test (b)) will confirm incidentally.

**Consequence (feeds Open Decision 3):** a deployed agent on the default 10-minute idle timeout **will be autostopped while healthily connected to the relay**, and (per the boot-hook survey below) nothing relaunches it. Default idle-timeout is NOT "free reliability" — the compute-posture choice is real: `--no-autostop` (24/7 bill) vs idle-timeout + explicit `start` recovery (agent silently dead between mentions). Recommendation for v0: default `--no-autostop`, expose `provider_config.idle_timeout` for owners who accept manual recovery.

## 5. Boot-hook survey — no hooks exist ❌

- No `cron`/`crontab`, no `at`
- No systemd (system or user); **PID 1 is `sandbox-daemon`**
- No `/etc/rc.local`; only `~/.bashrc` (interactive shells only)

**Consequence:** nothing in-sandbox can relaunch buzz-acp on `sandbox start`. The provider's `start` subcommand running `launch.sh` is the **only** recovery path, exactly as PLAN §3.3 designs it. The optional M3 in-sandbox supervisor is dead — strike it.

## 6. buzz-agent verification invocation (PLAN §4.2)

`buzz-agent --version` does not exist (exit 2: config validation runs before arg parsing; it demands `BUZZ_AGENT_PROVIDER`). The working no-op verification is an **ACP `initialize` handshake**: with provider env set, send one `initialize` frame on stdin → expect the `agentInfo` result. This validates binary + config in <1 s without touching the LLM. (`session/new` is the cheapest probe that also exercises gateway auth — it performs model discovery.)

## Timings (this session)

| Operation | Time |
|---|---|
| `sandbox create` → Running | ~1 s (both) |
| .deb download + extract in-sandbox | ~30 s |
| Direct gateway inference (anthropic/v1/messages) | 1.44 s |
| buzz-agent full ACP prompt via gateway | 3.5 s |
| buzz-acp start → terminal 403 exit | ~1 s |
| Idle autostop with continuous WSS relay traffic, no SSH | 11m00s–11m44s after last SSH |
