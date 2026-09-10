# Internal acceptance boundary

Public GitHub workflows are deliberately hermetic. They run formatting, vet,
lint, unit/race tests, vulnerability audits, deterministic embedded-helper
checks, cross-builds, and GoReleaser validation. They receive no Databricks
credential, profile, workspace host, internal resource identifier, or relay
identity and must never invoke this procedure.

End-to-end workspace acceptance is an operator-run local activity. The code in
`internal/acceptance` provides a strict, versioned manifest parser and a double
opt-in planning gate; it deliberately has no workspace client and performs no
mutation itself.

A caller must provide both:

1. an explicit `accept-internal-workspace-risk` boolean, and
2. `BUZZ_LAKEBOX_INTERNAL_ACCEPTANCE=1` in the local process.

The gate refuses to produce a plan when `GITHUB_ACTIONS=true` or `CI=true`, even
when both acknowledgements are present. Manifests carry only a local CLI profile
name, supported runtime names, and the same portable `mcp_config` /
`skills_config` JSON accepted by the provider. Hosts and credentials are not
valid manifest fields.

## Operator rules

- Store the manifest outside the repository. Never commit it or its output.
- Resolve credentials just in time and keep them in memory or process
environment only. Never write token-bearing deploy payloads, private keys, or
auth tags to disk.
- Use disposable resources where mutation is necessary. Record their cleanup
before ending the run.
- Publish only capability-level evidence: kind/runtime tested, tool counts,
pass/fail, sanitized timing, and cleanup status. Do not publish profiles, hosts,
resource identifiers, sandbox identifiers, account/workspace IDs, user names,
or raw API responses.
- Run a tracked-file scan before pushing. The public repository must remain
generic and reproducible without access to an internal environment.

## Suggested matrix

At minimum, exercise:

- managed SQL, Genie, AI Search, UC Functions, a governed MCP Service, and UC
Skills;
- Buzz Agent, Claude, and Codex;
- valid and denied authorization, token refresh, malformed/slow MCP responses,
child exit, redeploy, stop/start recovery, and teardown;
- thread-scoped Buzz replies, owner response policy, presence, and managed skill
replacement.

`mcp discover` and `mcp probe` are read-only preflight tools. Deployment and
resource creation remain separate explicit operator actions.
