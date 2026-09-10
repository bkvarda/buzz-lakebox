# Omnigent × Databricks sandboxes (Lakebox) — integration patterns

> Lane B of the Databricks-sandbox deep-dive (2026-07-24). Source: code-search index of `omnigent-ai/omnigent@main` (commit a35773f7).

## Headline finding

Omnigent's Databricks sandbox integration is a provider named **`lakebox`** ("Databricks Lakebox"). The actual launcher module, `omnigent/onboarding/sandboxes/lakebox.py` (`LakeboxLauncher`), is **deliberately excluded from the public/indexed repo** — it "ships only internally" in the Databricks distribution (`omnigent/cli_sandbox.py` module docstring; `omnigent/onboarding/sandboxes/__init__.py`: "launcher modules may be absent from a given distribution (e.g. the Databricks Lakebox launcher)"). Verified via `get_file`: `omnigent/onboarding/sandboxes/lakebox.py` returns `found: false` on `main`, as does `omnigent/onboarding/internal_beta`.

**Consequence:** the raw Databricks sandbox HTTP API (create/get/delete endpoints, request/response JSON, CPU/memory sizing, TTL knobs) does **not appear anywhere in this codebase**. What the repo does contain: (1) the exact launcher *contract* Lakebox implements, (2) the full bootstrap/auth/I-O architecture around it, (3) many concrete Lakebox facts in docstrings/comments.

---

## (a) Sandbox API surface as used by omnigent

### Transport: CLI shell-outs, not direct HTTP

Lakebox is driven by shelling out to a **`databricks lakebox` subcommand of a special Databricks CLI build**:

- `omnigent/cli.py` (~line 12675–12684): internal-beta setup runs `install_demo_databricks_cli()` — "Install the demo `databricks` CLI (with the `lakebox` subcommand) BEFORE profile onboarding… Idempotent… persists `~/.local/bin` in the user's shell rc files."
- `omnigent/onboarding/sandboxes/__init__.py` `get_launcher()`: the lakebox launcher takes a `workspace_host` and "pins its local `databricks lakebox` calls to it so sandboxes are created in the server's workspace."
- `omnigent/cli_sandbox.py` line ~395: "the local **`lakebox ssh`** transport must resolve through the same workspace" — exec/file/port-forward transport is SSH (`databricks lakebox ssh`).

### The launcher contract Lakebox implements (`omnigent/onboarding/sandboxes/base.py`)

| Primitive | Semantics | Lakebox specifics found in repo |
|---|---|---|
| `prepare()` | local preflight: install/verify provider tooling + credentials | installs the demo `databricks` CLI with `lakebox` subcommand |
| `provision(name) -> sandbox_id` | create sandbox, return provider id (animal-word ids like `"<sandbox-id>"`) | created **in the server's workspace** (pinned via `workspace_host`); "slow provisioning" |
| `attach(sandbox_id)` | validate/refresh access to an existing sandbox | "re-ship code into a long-lived sandbox, dodging its slow provisioning + per-sandbox OAuth dance" (`--sandbox-id`) |
| `keep_alive(sandbox_id)` | disable idle autostop / maximize lifetime; soft-fail | called on every bootstrap (`bootstrap.py::bootstrap_sandbox_host`) |
| `run(sandbox_id, command, check=True)` | run shell command, capture `{returncode, stdout, stderr}` | over `lakebox ssh` |
| `run_background(sandbox_id, command, log_path)` | default wraps in `setsid nohup sh -c '…' > log 2>&1 & echo launched` | |
| `put(sandbox_id, local, remote)` | copy file into sandbox (wheel tarball → `/tmp/oa-wheels.tgz`) | scp-equivalent over ssh transport |
| `stream_exec(sandbox_id, command, pty=False)` | spawn + stream stdout/stderr line-by-line; PTY option | used for in-sandbox `omnigent login` (PTY; ANSI stripped) |
| `exec_foreground(sandbox_id, command)` | stdio-inherited foreground exec; Ctrl-C detaches + kills remote | holds `omnigent host` open; `TERM=xterm-256color` forced ("native harnesses spawn tmux, which refuses to start under a dumb/unset TERM") |
| `forward_local_port(sandbox_id, port)` | `ssh -L` semantics | Lakebox is the named reference impl: "Providers with an inbound forwarding path (**Lakebox over SSH**) override it AND set `supports_local_port_forward = True`" |
| `wheel_install_command(remote_tgz)` | provider-specific pip flags | "the Lakebox image bakes omnigent and its deps, requiring `--force-reinstall --no-deps`" |
| `terminate` / `resume` / `is_running` | optional lifecycle capabilities | no in-repo evidence of Lakebox overriding (internal module) |
| class attrs | `provider="lakebox"`, `wheel_build_index_url`, `supports_local_port_forward=True`, `supports_cli_bootstrap=True` | `wheel_build_index_url`: "Providers tied to networks where public PyPI is unreachable (**Lakebox on the Databricks corp network**) override this" → internal PyPI proxy index |

**Not found:** any `/api/2.x/...` sandbox endpoint, JSON create/delete payload, or lakebox REST client anywhere in the repo.

---

## (b) Agent-in-sandbox bootstrap + I/O streaming architecture

### CLI bootstrap (`omnigent sandbox create --provider lakebox --server <apps-url>`, alias `omnigent lakebox create`)

`bootstrap.py::bootstrap_sandbox_host` — six steps:

1. **Workspace derivation** (`derive_workspace`): unauthenticated `GET <server_url>/v1/me` without following redirects ("the 302 IS the signal for Databricks Apps"); then `GET <workspace_host>/login.html` reads the numeric workspace id off the **`x-databricks-org-id`** header.
2. **`prepare()` → `provision(name)` or `attach(id)`** (default label `omnigent-host`), then **`keep_alive()`**.
3. **Wheel build**: `uv build --wheel` for sdks + repo → tarball `/tmp/oa-wheels.tgz`; `UV_INDEX_URL` from `wheel_build_index_url` (internal index for lakebox).
4. **Ship + install**: `put` tarball → `wheel_install_command` (`pip install --force-reinstall --no-deps` because the image bakes omnigent) → persist `~/.local/bin` on PATH.
5. **In-sandbox App OAuth** (`login_app_oauth_in_sandbox`): reset sandbox `~/.databrickscfg` to one `[DEFAULT]` block (host + `auth_type = databricks-cli`) to **kill the image's baked PAT** ("the Apps edge 302s PATs, so it shadows the OAuth grant"); run `omnigent login` inside the sandbox over PTY; parse the `oidc/v1/authorize` URL; read the loopback callback port from `redirect_uri` (first free ≥ 8020); open `forward_local_port` (SSH `-L`) **before** revealing the URL; open browser locally. Cached workspace grant skips the browser.
6. Ready banner; user runs `omnigent sandbox connect`.

### Registration + I/O streaming

- `connect_sandbox_host` runs `omnigent host --server <url>` via `exec_foreground` — "The remote command **holds a WebSocket open** until interrupted."
- Execution model (`deploy/README.md`): server is a FastAPI/WebSocket coordinator; the **host/runner dials back over `WS /v1/runner/tunnel`**, "executes the LLM loop + tools locally, streams events back." **No inbound connectivity to the sandbox is needed for sessions** — only the OAuth callback uses inbound SSH port forwarding.
- Host→runner env forwarding (`omnigent/host/connect.py`): allowlisted harness credential vars (`ANTHROPIC_API_KEY/AUTH_TOKEN/BASE_URL`, `CLAUDE_CODE_OAUTH_TOKEN`, `CODEX_ACCESS_TOKEN`, `OPENAI_API_KEY/BASE_URL`, `GEMINI_API_KEY`, `GIT_TOKEN`/`GIT_USERNAME`, `OMNIGENT_*` aliases, `OMNIGENT_RUNNER_ENV_PASSTHROUGH` extras, `MLFLOW_*`/`OTEL_*`).
- **Lakebox host-naming gotcha**: all sandboxes share hostname `databricks`; server `hosts` table PK is `(owner, name)` — collisions overwrite / FK-violate. Hence `--host-name` + `set_sandbox_host_name`.

### Server-managed launch path (`omnigent/server/managed_hosts.py`)

`POST /v1/sessions {"host_type": "managed"}` → server provisions sandbox, injects `sandbox.host_config` into `~/.omnigent/config.yaml`, clones the repo-URL workspace (branch-only), then `run_background` of `OMNIGENT_HOST_TOKEN/ID/NAME=… omnigent host --server <url>` (logs `/tmp/omnigent-host.log`). Host authenticates back with a **server-minted per-launch token** — "the user's own credentials never enter the sandbox."

**`lakebox` is in `SUPPORTED_SANDBOX_PROVIDERS` but NOT in `PROVIDERS_WITH_MANAGED_LAUNCH`** — staged; managed launch 400s. Internal deployments inject custom launchers via `create_app(sandbox_config=ManagedSandboxConfig(launcher_factory=...))`. UI: capability `internal_sandbox_cli` set when `importlib.util.find_spec("omnigent.onboarding.sandboxes.lakebox")` is non-None (`omnigent/server/app.py` ~2025); `web/src/lib/capabilities.ts` maps `lakebox: "Databricks"`.

---

## (c) Auth, lifecycle, limits, gotchas

### Auth (three distinct credentials)

1. **Sandbox image's baked workspace PAT** — valid for workspace APIs in the sandbox's own workspace; **rejected by the Databricks Apps OAuth edge**. Shared across all Lakebox sandboxes.
2. **In-sandbox workspace OAuth U2M grant** — minted by `databricks auth login` inside the sandbox (port-forwarded browser flow). Never shipped from the laptop (keyring, cache-format skew, **single-use refresh tokens**). Access tokens ~1h; refresh grant ~90 days, then `omnigent sandbox auth --provider lakebox` re-auths without re-shipping wheels.
3. **Ambient/local auth** — `omnigent/inner/databricks_executor.py` delegates to databricks-sdk (all `auth_type`s); `_DatabricksBearerAuth` re-mints on expiry.
- Only feature-flag reference: the managed "Omnigent on Databricks (Beta)" requires enabling the **Omnigent preview in workspace settings** (`docs/databricks.md`, `deploy/README.md`). No sandbox-specific entitlement named in-repo.

### Lifecycle, limits, error handling

- **Creation latency**: Lakebox provisioning is "slow" (reuse flow exists to dodge it). Managed launches: `MANAGED_HOST_ONLINE_TIMEOUT_S = 120`, `MANAGED_LAUNCH_RENDEZVOUS_TIMEOUT_S = 240` (message POSTs park on a tracker instead of failing).
- **Keepalive**: `keep_alive()` on every bootstrap ("disable idle autostop / maximize lifetime… soft-fail").
- **Durable identity vs disposable sandbox**: `hosts` row carries `sandbox_provider` + `sandbox_id` + launch-token digest/expiry; relaunch overwrites in place — "a new sandbox generation under the same `host_id`, so session bindings survive a sandbox dying at the provider's lifetime cap."
- **Resume**: `can_resume`/`resume()` exist for providers with stop/resume + persistent volume; Lakebox impl not visible.
- **Reconnects**: WS upgrade retries only on 408/429; Apps OAuth session tokens expire ~1h → per-request re-auth for long-lived remote sessions.
- **Gotchas checklist**: baked-PAT-shadows-OAuth (fix: full `~/.databrickscfg` reset); shared hostname `databricks` → PK collisions; corp network can't reach public PyPI (internal `UV_INDEX_URL`); tmux needs `TERM=xterm-256color`; Apps edge 302s (not 401s) unauthenticated requests.

---

## (d) File path index (`omnigent-ai/omnigent@main`, commit a35773f7)

| Area | Files |
|---|---|
| Launcher contract | `omnigent/onboarding/sandboxes/base.py` (SandboxLauncher ABC, `DEFAULT_HOST_IMAGE=ghcr.io/omnigent-ai/omnigent-host:latest`) |
| Bootstrap flow | `omnigent/onboarding/sandboxes/bootstrap.py` |
| Provider registry | `omnigent/onboarding/sandboxes/__init__.py` (`_LAUNCHERS["lakebox"] = "omnigent.onboarding.sandboxes.lakebox:LakeboxLauncher"`) |
| CLI | `omnigent/cli_sandbox.py`, `omnigent/cli.py` ~12630–12720 (`install_demo_databricks_cli`) |
| Server managed hosts | `omnigent/server/managed_hosts.py`, `omnigent/server/app.py` (~2025), `omnigent/db/db_models.py` (~1243) |
| Host dial-back | `omnigent/host/connect.py`, `omnigent/host/identity.py` |
| Databricks auth plumbing | `omnigent/inner/databricks_executor.py`, `omnigent/chat.py` (~600–860), `omnigent/cli_auth.py` |
| Web UI | `web/src/lib/capabilities.ts`, `web/src/shell/NewChatDialog.tsx` |
| Docs | `docs/databricks.md` (server-on-Apps, not sandboxes), `deploy/README.md` (sandbox framework + auth), `deploy/databricks/` (Apps bundle for the *server*) |
| Absent (internal-only) | `omnigent/onboarding/sandboxes/lakebox.py`, `omnigent/onboarding/internal_beta.py` — `found: false` |

## Looked for and did NOT find

- Any Databricks sandbox REST endpoints/paths or request/response JSON (create/get/list/delete/exec/upload/port-URL) — entire surface is behind the internal `databricks lakebox` CLI.
- Sandbox spec knobs for lakebox: image name/registry, CPU/memory, disk, TTL values, volume/snapshot semantics, egress rules (these exist in-repo for islo/modal/kubernetes, not lakebox).
- The internal PyPI index URL, demo-CLI download URL, `_INTERNAL_BETA_DEFAULT_SERVER` value.
- Any workspace entitlement/preview-flag name specific to Lakebox sandboxes.
- `semantic_search` returned 0 results for this repo (not semantically indexed); all findings via lexical `search_code` + `get_file`.
