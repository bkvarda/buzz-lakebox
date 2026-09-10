# Buzz Desktop UX for Lakebox

This document separates what the current provider protocol already supports
from the generic Buzz UI extension needed for a first-class Lakebox setup
experience.

## Product model

Lakebox should remain a **run destination**, not become an agent harness.

The three configuration dimensions answer different questions:

1. **Harness** — which agent implementation runs: Buzz Agent, Claude Code,
   Codex, or (after separate compatibility work) Goose.
2. **Inference provider and model** — which API/model the harness calls.
3. **Run destination** — where the harness and its tools execute: the local
   computer or a Databricks Sandbox.

Collapsing these dimensions into a `Lakebox` harness would create duplicate
pseudo-harnesses such as Lakebox Claude and Lakebox Codex, make skills and MCP
ownership ambiguous, and prevent Buzz from applying one harness's capability
metadata consistently across local and remote execution.

A cohesive **Databricks Lakebox** setup card or preset is still desirable in
the UI. It should guide and constrain the three underlying dimensions rather
than replace them.

## What works today

As of `block/buzz` commit `00209076c7a10d9e4a475466c313e8ebecf041f5`,
Buzz already provides a native third-party run-destination surface:

- `desktop/src/features/agents/ui/WhereToRunSection.tsx` lists discovered
  `buzz-backend-*` executables in **Advanced → Run on**.
- Selecting one probes its `info` operation and renders its `config_schema`
  through `ProviderConfigFields.tsx`.
- The harness, inference provider, model, and environment-variable controls
  remain the ordinary Buzz agent controls. The selected backend receives the
  resolved launch payload.

For this provider, the resulting flow is:

- choose a supported harness;
- choose **Databricks v2** and a model where that harness supports Buzz model
  projection;
- choose **Run on → databricks-lakebox**;
- enter the local CLI profile and optional Lakebox MCP/skills settings.

The current model behavior is not yet uniform across all supported harnesses:

| Harness | Lakebox support | Model behavior today |
|---|---|---|
| Buzz Agent | Supported | Uses the Buzz **Databricks v2** provider and selected gateway model. |
| Claude Code | Supported | Routes inference through the workspace AI Gateway, but intentionally does not project the Buzz gateway model ID; the adapter default is used because setting `ANTHROPIC_MODEL` rewrites some gateway IDs into unsupported canonical IDs. |
| Codex | Supported | Routes through the workspace AI Gateway using the provider's fixed, live-verified Codex model. |
| Goose | Not supported | Requires a separately verified installer, ACP/inference bridge, permissions, MCP, and skills contract before it should appear. |

Therefore the exact existing Databricks v2 model picker is fully truthful only
for Buzz Agent today. Making it equally truthful for Claude and Codex requires
provider/runtime compatibility work, not just a UI label.

## Current UI limit

`ProviderConfigFields.tsx` currently iterates `config_schema.properties` and
renders every property with the same one-line `Input`. It reads only
`title`, `description`, `default`, `type`, and `required`; arrays, nested
objects, enums, secret presentation, text areas, resource discovery, and
cross-field constraints have no first-class representation.

That is why `mcp_config` and `skills_config` are compact JSON strings today.
The Lakebox CLI provides safe `discover`, `validate`, and `probe` operations,
but the operator must paste the resulting scalar into the Buzz form.

The provider protocol's `info` response also has a strict field whitelist in
`desktop/src-tauri/src/managed_agents/backend.rs`, so this provider cannot add
capability metadata unilaterally without a coordinated Buzz protocol change.

## Recommended Buzz extension

Implement this generically for every remote backend rather than hard-coding a
Lakebox branch.

### 1. Run-destination compatibility metadata

A provider protocol revision should let a backend describe, per harness:

- whether the harness is supported;
- allowed or required inference providers;
- model behavior: selectable from the provider catalog, ACP-native,
  runtime-default, fixed, or unsupported;
- an optional fixed model ID/label;
- whether changing the run destination may safely auto-apply defaults.

Conceptual shape (not a protocol commitment):

```json
{
  "launch_capabilities": {
    "runtimes": {
      "buzz-agent": {
        "providers": ["databricks_v2"],
        "model_mode": "provider-catalog"
      },
      "claude": {
        "providers": ["databricks_v2"],
        "model_mode": "runtime-default"
      },
      "codex": {
        "providers": ["databricks_v2"],
        "model_mode": "fixed",
        "fixed_model": "databricks-gpt-5-3-codex"
      }
    }
  }
}
```

When **Run on → Databricks Lakebox** is selected, Buzz should:

1. filter or clearly disable unsupported harnesses with an explanation;
2. auto-select and lock **Databricks v2** when the chosen harness requires it;
3. reuse the existing Databricks v2 model discovery for
   `model_mode=provider-catalog`;
4. hide the model selector and explain runtime-default/fixed behavior for the
   other model modes;
5. preserve user choices when switching destinations unless they are
   incompatible, and ask before replacing an existing explicit choice.

The Rust runtime catalog should remain Buzz's source of harness capability
facts. Backend metadata supplies only destination compatibility constraints;
it must not duplicate the harness catalog.

### 2. Structured provider configuration

Extend the provider schema renderer in stages:

1. honor string `enum` as a select, boolean as a switch, secret/write-only
   fields as masked inputs, and a multiline format as a text area;
2. render arrays of objects as repeatable rows and nested objects as grouped
   fields;
3. allow a trusted provider to expose read-only option discovery and validation
   operations for schema fields.

For Lakebox this enables an **MCP servers** editor with repeatable rows:

- name;
- kind;
- a kind-aware resource picker populated from the selected local Databricks
  profile;
- authentication mode (currently `env` only);
- per-row **Probe** result and tool count.

The form should serialize the same portable `buzz-managed-mcp` v1 document the
provider accepts today. Hosts, profiles, URLs, and credentials must remain
outside that document.

### 3. Lakebox setup presentation

With those generic primitives, Buzz can present one progressive section:

- **Run on:** Databricks Lakebox
- **Harness:** supported Buzz runtime
- **Inference:** Databricks v2 (required)
- **Model:** existing Databricks v2 selector when truthful
- **Workspace access:** local profile for provisioning plus masked headless
  inference credentials
- **Managed MCP:** discoverable repeatable server rows
- **Skills:** governed live Skills MCP and optional local `aitools` sync

This feels like one Lakebox configurator to the user while preserving the
correct independent data model underneath.

## Delivery sequence

1. Ship the README quick start and current click-by-click flow in this repo.
2. Add the low-risk scalar renderer improvements upstream (enum, boolean,
   masked, multiline).
3. Propose the run-destination compatibility metadata as a provider protocol
   revision, including migration behavior and tests across local/Kubernetes/
   Lakebox-style destinations.
4. Add generic provider option discovery, structured array/object editing, and
   validation/probe presentation.
5. Separately make model selection truthful for Claude/Codex before advertising
   Databricks v2 catalog selection for those harnesses; add Goose only after its
   full runtime contract is live-proven.
