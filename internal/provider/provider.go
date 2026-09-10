// Package provider implements the provider-mode wire protocol described in
// docs/CONTRACT.md: read one JSON object from stdin, dispatch on "op", and
// always emit exactly one JSON object on stdout — never a non-zero exit for
// a handled case (CONTRACT.md §2).
package provider

import (
	"context"
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

// staticConfigSchema returns the additive JSON-Schema-ish object advertised
// for provider_config. A fresh value per request prevents dynamic profile
// augmentation from mutating package state. Current Buzz renders scalar
// properties as free-text inputs and ignores enum; enum is still emitted for
// future renderer support while title/description/default remain text-input
// compatible.
func staticConfigSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"profile": map[string]any{
				"type":        "string",
				"title":       "Databricks CLI profile",
				"description": "Enter a local Databricks CLI profile name, or leave empty for automatic selection. Current Buzz renders this as text; discovered choices require renderer support for a clickable dropdown.",
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
				"title":       "Managed MCP servers (JSON)",
				"description": "Optional compact buzz-managed-mcp v1 JSON. Generate it with `mcp discover --emit-config`, then run `config validate` and `mcp probe` before pasting. Hosts, credentials, and profile names must not appear here.",
			},
			"skills_config": map[string]any{
				"type":        "string",
				"title":       "Synchronized Databricks skills (JSON)",
				"description": "Optional compact buzz-skills v1 JSON for a validated databricks aitools raw-skill sync. Run `config validate --skills-file` before pasting; existing unmanaged skills are never overwritten.",
			},
		},
		"required": []string{},
	}
}

func dynamicConfigSchema(profiles []Profile, bakedDefault string) map[string]any {
	schema := staticConfigSchema()
	properties := schema["properties"].(map[string]any)
	profile := properties["profile"].(map[string]any)
	names := profileNames(profiles)
	profile["enum"] = append([]string{""}, names...)
	profile["description"] = "Enter a discovered local Databricks CLI profile name, or leave empty for automatic selection. Discovered profiles: " + strings.Join(names, ", ") + ". Current Buzz renders this as a text input; its renderer must support enum before these choices become a clickable dropdown."
	if selected, ok := deterministicProfileDefault(bakedDefault, profiles); ok {
		profile["default"] = selected
	}
	return schema
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

// Option configures provider-mode behavior. Run remains source-compatible with
// existing three-argument callers through variadic options.
type Option func(*runOptions)

type runOptions struct {
	discoverer   ProfileDiscoverer
	bakedDefault string
}

// WithProfileDiscovery enables provider-owned local profile discovery and
// selection. Production should pass a timeout-bound CLIProfileDiscoverer.
func WithProfileDiscovery(discoverer ProfileDiscoverer, bakedDefault string) Option {
	return func(options *runOptions) {
		options.discoverer = discoverer
		options.bakedDefault = bakedDefault
	}
}

// Run reads one JSON request from r, dispatches it, and writes exactly one
// JSON response line to w. It returns a non-nil error only for unhandleable I/O
// failures; handled cases always yield a response and nil error.
func Run(r io.Reader, w io.Writer, deploy DeployFunc, opts ...Option) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}

	options := runOptions{bakedDefault: version.DefaultProfile}
	for _, option := range opts {
		if option != nil {
			option(&options)
		}
	}

	resp := route(data, deploy, options)

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

func route(data []byte, deploy DeployFunc, options runOptions) any {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return newErrorResponse(fmt.Sprintf("malformed request: could not parse JSON: %v", err))
	}

	switch env.Op {
	case opInfo:
		schema := staticConfigSchema()
		if options.discoverer != nil {
			if profiles, err := options.discoverer.DiscoverProfiles(context.Background()); err == nil && len(profileNames(profiles)) > 0 {
				schema = dynamicConfigSchema(profiles, options.bakedDefault)
			}
		}
		return infoResponse{
			Ok:              true,
			Name:            Name,
			Version:         version.Version,
			ProtocolVersion: ProtocolVersion,
			Description:     Description,
			ConfigSchema:    schema,
		}
	case opDeploy:
		return handleDeploy(data, deploy, options)
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

func handleDeploy(data []byte, deploy DeployFunc, options runOptions) any {
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

	// An explicit profile is authoritative and bypasses discovery entirely.
	// This matters for both latency and security: selecting a named profile
	// must not enumerate unrelated local configuration.
	if req.ProviderConfig.Profile == "" && options.discoverer != nil {
		profiles, discoveryErr := options.discoverer.DiscoverProfiles(context.Background())
		selected, selectionErr := selectProfile(options.bakedDefault, profiles, discoveryErr)
		if selectionErr != nil {
			return newErrorResponse(selectionErr.Error())
		}
		req.ProviderConfig.Profile = selected
	}

	agentID, err := deploy(req)
	if err != nil {
		secrets := redact.SecretsFromPayload(req.Agent)
		return newErrorResponse(redact.Redact(err.Error(), secrets))
	}

	return deploySuccessResponse{Ok: true, AgentID: agentID}
}
