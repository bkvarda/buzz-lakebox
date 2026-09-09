// Package provider implements the provider-mode wire protocol described in
// docs/CONTRACT.md: read one JSON object from stdin, dispatch on "op", and
// always emit exactly one JSON object on stdout — never a non-zero exit for
// a handled case (CONTRACT.md §2).
package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/redact"
	"github.com/IceRhymers/buzz-lakebox/internal/version"
)

const (
	// Name is the display name echoed in info responses — cosmetic only,
	// never identity (docs/CONTRACT.md §4). Identity is the binary name's
	// <id> suffix: buzz-backend-databricks-lakebox → databricks-lakebox,
	// distinct from Buzz's own built-in databricks provider.
	Name = "Databricks Lakebox"

	// Description is the human-readable info-response description
	// (docs/CONTRACT.md §4).
	Description = "Deploys Buzz agents into Databricks Lakebox sandboxes"

	// ProtocolVersion is the wire-contract version of the provider protocol,
	// emitted as the INTEGER "protocol_version" field of the info response.
	// Buzz Desktop's validate_provider_info (block/buzz
	// desktop/src-tauri/src/managed_agents/backend.rs) rejects a provider
	// whose protocol_version it does not speak — a missing field is an error,
	// not a presumed 1 — so this must be present, integer-typed, and equal to
	// the version the desktop requires (currently 1). It replaced the earlier
	// cosmetic string "protocol":"v1", which Buzz now rejects as an unknown
	// field (the info response is validated against a strict key whitelist).
	ProtocolVersion = 1

	opInfo   = "info"
	opDeploy = "deploy"
)

// supportedOps is the frozen list used to build the unknown-op error message.
// Order matters for the error string (docs/CONTRACT.md §4 "Unknown op"). It is
// deliberately NOT advertised in the info response: Buzz validates that
// response against a strict field whitelist and rejects an "ops" field.
var supportedOps = []string{opInfo, opDeploy}

// envelope is the minimal shape needed to route any request; deploy's
// remaining fields are parsed separately by internal/payload once we know
// op == "deploy".
type envelope struct {
	Op string `json:"op"`
}

// DeployFunc performs an actual deploy given a validated request and
// returns the sandbox id to report as agent_id. A nil DeployFunc means
// deploy is not implemented yet: every deploy op returns the M0 stub error.
type DeployFunc func(req *payload.DeployRequest) (agentID string, err error)

// infoResponse is the info-op response. Its fields are exactly the set Buzz
// Desktop's validate_provider_info whitelists — ok, name, version,
// protocol_version, description, config_schema — and no others: any extra
// field is rejected as an unknown field (docs/CONTRACT.md §4). config_schema
// is required (must be a JSON object), so it carries no omitempty.
type infoResponse struct {
	Ok              bool   `json:"ok"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
	Description     string `json:"description"`
	ConfigSchema    any    `json:"config_schema"`
}

// configSchema is the additive, static JSON-Schema-ish object advertised in
// the info response (docs/CONTRACT.md §4) for provider_config. Buzz Desktop
// (block/buzz@8bb43d51) renders each property as a free-text create-agent
// input (title/description/default; `required` gates the create button;
// coerceConfigValues booleanizes the string "true" — hence string enum
// types here, never boolean); older desktops ignore this field entirely.
//
// keep_workspace_pat and buzz_version are deliberately NOT advertised here
// — they stay expert-only, documented in docs/CONTRACT.md.
//
// extra_binaries and mcp_servers are likewise intentionally omitted: they are
// expert-only, non-scalar (array-valued) operator-CLI keys that Buzz Desktop's
// scalar-only validate_provider_config would reject anyway, so advertising them
// here would only surface an input the desktop cannot submit.
var configSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"profile": map[string]any{
			"type":        "string",
			"title":       "Databricks CLI profile",
			"description": "Databricks CLI profile selection; empty = the build's baked default.",
		},
		"inference_auth": map[string]any{
			"type":    "string",
			"title":   "Inference auth",
			"default": "env",
			"description": "env (default): you supply DATABRICKS_HOST/DATABRICKS_TOKEN in the agent's " +
				"environment variables. sandbox: zero-token — the agent reuses the sandbox's built-in " +
				"per-user credential and can act AS YOU across the whole workspace (opt-in security tradeoff).",
		},
		"idle_timeout": map[string]any{
			"type":        "string",
			"title":       "Idle timeout",
			"description": "Duration like 30m or 2h; empty = no autostop (default).",
		},
		"mcp_config": map[string]any{
			"type":        "string",
			"title":       "Managed MCP configuration",
			"description": "Optional compact versioned JSON. Server hosts are derived from the selected workspace; credentials and profile names must not appear in this value.",
		},
		"skills_config": map[string]any{
			"type":        "string",
			"title":       "Databricks skills configuration",
			"description": "Optional compact buzz-skills v1 JSON for a noninteractive databricks aitools --skills-only sync. Existing unmanaged skills are never overwritten.",
		},
	},
	"required": []string{},
}

type errorResponse struct {
	Ok    bool   `json:"ok"`
	Error string `json:"error"`
}

type deploySuccessResponse struct {
	Ok      bool   `json:"ok"`
	AgentID string `json:"agent_id"`
}

func newErrorResponse(msg string) errorResponse {
	return errorResponse{Ok: false, Error: msg}
}

// Run reads one JSON request from r, dispatches it, and writes exactly one
// JSON response line to w. It returns a non-nil error only for
// unhandleable I/O failures (reading stdin or writing stdout) — per
// CONTRACT.md §2, every request that can be parsed enough to route is a
// "handled case" and always yields a written response with a nil error.
func Run(r io.Reader, w io.Writer, deploy DeployFunc) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}

	resp := route(data, deploy)

	out, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	out = append(out, '\n')
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

func route(data []byte, deploy DeployFunc) any {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return newErrorResponse(fmt.Sprintf("malformed request: could not parse JSON: %v", err))
	}

	switch env.Op {
	case opInfo:
		return infoResponse{
			Ok:              true,
			Name:            Name,
			Version:         version.Version,
			ProtocolVersion: ProtocolVersion,
			Description:     Description,
			ConfigSchema:    configSchema,
		}
	case opDeploy:
		return handleDeploy(data, deploy)
	default:
		return newErrorResponse(fmt.Sprintf("unknown op %q; supported: %s", env.Op, strings.Join(supportedOps, ", ")))
	}
}

// MarshalDeployResult renders the frozen {"ok":true,"agent_id":...} /
// {"ok":false,"error":...} deploy response shape (docs/CONTRACT.md §4)
// using the same typed response structs the provider-mode stdin path
// emits — one rendering for the frozen wire shape, shared by both the
// provider-mode handleDeploy path and cmd/buzz-backend-databricks-lakebox's
// operator `deploy --payload-file` command (which used to hand-roll an
// equivalent map literal of its own).
func MarshalDeployResult(agentID string, deployErr error) []byte {
	var resp any
	if deployErr != nil {
		resp = newErrorResponse(redact.Redact(deployErr.Error(), nil))
	} else {
		resp = deploySuccessResponse{Ok: true, AgentID: agentID}
	}
	data, err := json.Marshal(resp)
	if err != nil {
		// These are fixed-shape structs with no exotic field types, so
		// Marshal cannot fail in practice; degrade to a minimal valid
		// JSON error object rather than panicking or returning nil.
		return []byte(`{"ok":false,"error":"internal: failed to marshal deploy result"}`)
	}
	return data
}

func handleDeploy(data []byte, deploy DeployFunc) any {
	if deploy == nil {
		return newErrorResponse("deploy not implemented yet (M1)")
	}

	req, err := payload.ParseDeployRequest(data)
	if err != nil {
		// A body that fails even to unmarshal cannot be walked for
		// per-field secrets, but scrub any bare nsec1 token that made it
		// into the error text (e.g. from a JSON syntax error snippet).
		return newErrorResponse(redact.Redact(fmt.Sprintf("malformed deploy request: %v", err), nil))
	}

	if err := req.Validate(); err != nil {
		secrets := redact.SecretsFromPayload(req.Agent)
		return newErrorResponse(redact.Redact(err.Error(), secrets))
	}

	agentID, err := deploy(req)
	if err != nil {
		secrets := redact.SecretsFromPayload(req.Agent)
		return newErrorResponse(redact.Redact(err.Error(), secrets))
	}

	return deploySuccessResponse{Ok: true, AgentID: agentID}
}
