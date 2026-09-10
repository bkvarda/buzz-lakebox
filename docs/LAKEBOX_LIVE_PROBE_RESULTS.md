# Databricks Sandbox (Lakebox) live probe results — sanitized environment

> Lane C of the Databricks-sandbox deep-dive (2026-07-24). All probes run live against profile `EXAMPLE_PROFILE` (`https://workspace.example.invalid`, AWS `example-region-1`, workspace `<workspace-id>`). Companion docs: [BUZZ_AGENT_SESSION_ARCHITECTURE.md](BUZZ_AGENT_SESSION_ARCHITECTURE.md) (lane A), [OMNIGENT_DATABRICKS_SANDBOX_PATTERNS.md](OMNIGENT_DATABRICKS_SANDBOX_PATTERNS.md) (lane B).

## Naming resolution (supersedes lane B's "internal CLI only" caveat)

The product shipped publicly since omnigent's integration was written. It is now **Databricks Sandbox**: `databricks sandbox` command group in the **stock public CLI** (verified in Homebrew CLI v1.8.0 locally — no demo build needed). The REST namespace is still the old codename: `/api/2.0/lakebox/...`. Public docs: docs.databricks.com `compute/serverless/sandbox` and `dev-tools/cli/reference/sandbox-commands`. Rename PR: databricks/cli#5487. (Source: Glean; confirmed live below.)

Region caveat confirmed: the preview is region-scoped — it was not enabled for one tested account/region, while `example-region-1` worked. The returned regional gateway host is redacted.

## API / CLI surface (verified live)

- REST: `GET/POST /api/2.0/lakebox/sandboxes`, `GET/PATCH/DELETE /sandboxes/{id}`, `POST /sandboxes/{id}/start|stop`, `POST /ssh-keys`. `GET /api/2.0/lakebox/sandboxes` returned `{}` on `EXAMPLE_PROFILE` (endpoint live). Create payload: `{"sandbox": {"name": ...}}` — name is the only caller-settable field; **no image, CPU/RAM, or disk knobs**.
- CLI: `register` (SSH keypair → `~/.ssh/sandbox_ed25519`), `create`, `list`, `status`, `ssh [id] [-- cmd|ssh-flags]`, `config` (`--name`, `--idle-timeout 1m–24h`, `--no-autostop`), `default`, `start`, `stop`, `delete`, `ssh-key`.
- Status JSON: `{sandboxId, status, gatewayHost, name, idleTimeout: "600s", noAutostop}`.

## Measured lifecycle (sandbox `<sandbox-id>`, created + deleted this session)

| Operation | Result |
|---|---|
| `create` | **1.1 s** to `status: Running` (omnigent's "slow provisioning" note is stale) |
| `stop` | ~0.5 s request, async; `Stopped` ≤5 s later; `start` during `stopping` → clean 4xx "retry once it settles" |
| `start` | **20.5 s** |
| `delete` | immediate, `--auto-approve` supported; final state transits `stopping…` |
| idle autostop | default 10 min; per-sandbox `--idle-timeout` up to 24 h or `--no-autostop` exemption |

## Environment (probed via `sandbox ssh -- ...`)

- Ubuntu 24.04.4 LTS, x86_64, 4 vCPU, 8.1 GiB RAM, ~10 GB overlay disk. User `sandbox-agent` (uid 10086), home `/home/sandbox-agent`.
- Preinstalled: Python 3.12.3, Node 22.22.3, cargo 1.75.0, git 2.54.0, Databricks CLI v1.7.0.
- PID 1 is `/usr/bin/sandbox-daemon --enable-sshd`; also `ttyd` (web terminal) and sshd. SSH rides the gateway on port 2222; the CLI handles ProxyCommand wiring itself (non-interactive `register` skips ssh-config edits but `sandbox ssh` still works).
- **Baked credential**: `~/.databrickscfg` has a `[DEFAULT]` PAT for the sandbox's workspace, authenticating as **the sandbox creator** (verified: `databricks current-user me` → the sandbox creator). Matches omnigent's "baked PAT" gotcha (theirs was a shared PAT; here it's per-creator). Any agent in the sandbox can act on the workspace as the creating user unless this file is reset.

## Network egress (probed from inside)

| Target | Result |
|---|---|
| `https://relay.example.invalid` (Buzz relay) | 200 in 157 ms |
| **WSS handshake to relay (Node 22 WebSocket)** | **OPEN; relay sent NIP-42 `["AUTH", <challenge>]`** |
| github.com / registry.npmjs.org / pypi.org | 200 (public internet open — unlike omnigent's corp-network lakebox note) |
| api.anthropic.com/v1/models | 401 (reachable; auth required, as expected) |

## Buzz-stack proof inside the sandbox

- `Buzz_0.4.24_amd64.deb` (public GitHub release) extracts with `dpkg-deb -x` to **Linux x86_64 builds of the whole toolchain**: `buzz-acp` (16M), `buzz` CLI (16M), `buzz-agent` (12M), `buzz-dev-mcp` (23M), `git-credential-nostr` (1.5M). No cargo build or cross-compile needed.
- Ran `/tmp/bz/usr/bin/buzz channels list` with a **throwaway key** generated in-sandbox: got structured `{"error":"auth_error","message":"relay error 403: relay_membership_required"}` — full CLI↔relay path (REST + NIP-42 signing) works; only membership was missing, which is exactly what the desktop deploy payload (`private_key_nsec`, `auth_tag`) provides.
- Did **not** run `buzz-acp` with my real identity: (a) two harnesses on one key double-respond to mentions; (b) the permission classifier blocked shipping the live nsec into the sandbox — a real trust-boundary consideration the provider design must own (key leaves the owner's keyring and lands in Databricks-managed infra).
- `setsid nohup … &` background process **survived SSH disconnect** across sessions → buzz-acp can be launched with omnigent's `run_background` pattern.

## Persistence semantics (verified via stop/start cycle)

- `/home/sandbox-agent` **persists** across stop/start.
- Everything outside home (`/tmp`, extracted binaries) is **wiped** on stop; all processes die.
- ⇒ install buzz binaries + workspace nest under `$HOME`; buzz-acp must be **relaunched on every start** (needs a supervisor: `sandbox-daemon` hook, cron `@reboot`-equivalent, or the provider binary re-exec'ing on start).
- Docs warn Beta storage may be deleted; sandbox death at lifetime cap must be assumed (buzz-acp is already stateless + replays mentions via `since` filter, so this is tolerable).

## Gaps / risks for a `buzz-backend-databricks` provider

1. **No stop/status/logs in buzz's provider protocol (v1 = deploy/info only)** — but Lakebox has clean REST for all of it; the provider binary can implement v2 ops trivially when buzz adds them. Interim: `!shutdown` over relay + `databricks sandbox` CLI by hand.
2. **Process supervision across autostop/restart** — nothing in the sandbox restarts buzz-acp after `start`. Options: `--no-autostop` (simplest; burns compute), long `--idle-timeout` + relaunch-on-start in the provider, or an in-sandbox supervisor script in `$HOME`.
3. **Secret delivery** — deploy payload contains the nsec; it must go over the SSH channel (stdin, not argv) into `$HOME` with 0600, and the sandbox's baked creator-PAT should be reset/scoped if the agent shouldn't act as the owner on the workspace.
4. **No resource knobs** — 4 vCPU/8 GiB/10 GB fixed; fine for buzz-acp + claude/codex CLI runtimes, tight for heavy builds.
5. **Preview status** — region-gated (us-west-2 confirmed), Beta storage guarantees, API surface may churn (already one rename).
6. **Auth for the provider binary** — it runs on the owner's machine and can use their `~/.databrickscfg` profile; per-sandbox SSH key (`sandbox register`) is machine-level, one key for all sandboxes.

## Verbatim reproduction

```bash
databricks sandbox register -p EXAMPLE_PROFILE
databricks sandbox create --name buzz-probe --json -p EXAMPLE_PROFILE         # 1.1s → Running
databricks sandbox ssh <id> -p EXAMPLE_PROFILE -- 'uname -a'                  # exec, non-interactive OK
databricks sandbox config <id> --no-autostop -p EXAMPLE_PROFILE
databricks sandbox delete <id> --auto-approve -p EXAMPLE_PROFILE
```
