// Package payload defines the deploy-request wire shapes documented in
// docs/CONTRACT.md §3 and validates them. Unknown JSON fields are always
// tolerated (buzz's payload may grow fields we don't yet know about).
package payload

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/skillconfig"
)

// envVarKeyPattern is a valid POSIX shell/environment variable name. Keys
// that don't match this are rejected by Agent.Validate() because
// internal/nest's RenderEnv writes the KEY of every env_vars entry raw
// (unquoted) into a file that gets `.`-sourced by a shell — an attacker-
// controlled key containing shell metacharacters or a newline would
// achieve command execution in that shell, which has just exported the
// agent's nsec, auth tag, and DATABRICKS_TOKEN.
var envVarKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// DeployRequest is the top-level envelope for an {"op":"deploy",...} request
// (docs/CONTRACT.md §3).
type DeployRequest struct {
	Op             string         `json:"op"`
	RequestID      string         `json:"request_id"`
	Agent          Agent          `json:"agent"`
	ProviderConfig ProviderConfig `json:"provider_config"`
}

// Launch is the current Buzz desktop's resolved launch contract. PolicyEnv
// contains overridable defaults (tier 1); Env is the fully resolved user and
// harness environment (tier 2). Provider-owned identity and relay values are
// applied later by the sandbox provider (tier 3). A nil Launch means the
// request came from a legacy desktop and the top-level fields remain the source
// of truth.
type Launch struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	PolicyEnv   map[string]string `json:"policy_env"`
	OwnerPubkey string            `json:"owner_pubkey"`
}

// Agent is the exhaustive agent payload (docs/CONTRACT.md §3, "agent
// fields"). Unknown fields remain tolerated for forward compatibility.
type Agent struct {
	Name                string            `json:"name"`
	RelayURL            string            `json:"relay_url"`
	PrivateKeyNsec      string            `json:"private_key_nsec"`
	AuthTag             string            `json:"auth_tag"`
	AgentCommand        string            `json:"agent_command"`
	AgentArgs           []string          `json:"agent_args"`
	SystemPrompt        string            `json:"system_prompt"`
	Model               *string           `json:"model"`
	Provider            *string           `json:"provider"`
	TurnTimeoutSeconds  int               `json:"turn_timeout_seconds"`
	IdleTimeoutSeconds  int               `json:"idle_timeout_seconds"`
	MaxTurnDurationSecs int               `json:"max_turn_duration_seconds"`
	Parallelism         int               `json:"parallelism"`
	RespondTo           string            `json:"respond_to"`
	RespondToAllowlist  []string          `json:"respond_to_allowlist"`
	EnvVars             map[string]string `json:"env_vars"`
	Launch              *Launch           `json:"launch"`
	OwnerPubkey         string            `json:"-"`
}

// ProviderConfig is the provider_config object (docs/CONTRACT.md §3). All
// fields are optional; buzz's desktop already validates it secret-free
// before it reaches us.
type ProviderConfig struct {
	Profile          string `json:"profile"`
	IdleTimeout      string `json:"idle_timeout"`
	KeepWorkspacePAT bool   `json:"keep_workspace_pat"`
	BuzzVersion      string `json:"buzz_version"`
	// InferenceAuth selects how the sandboxed agent authenticates to the
	// workspace AI Gateway. Allowed values: "" and "env" (default: the
	// agent uses DATABRICKS_HOST/DATABRICKS_TOKEN supplied via
	// agent.env_vars, today's behavior) or "sandbox" (zero-token: the
	// agent instead derives its credential at launch time from the
	// sandbox's own baked ~/.databrickscfg, so no token ever needs to be
	// minted or transmitted). The key is named "inference_auth" rather
	// than something containing token/key/secret/password/credential
	// deliberately, so it passes the desktop's secret-word config filter.
	InferenceAuth string `json:"inference_auth"`

	// ClaudeAdapterVersion pins the @agentclientprotocol/claude-agent-acp
	// version installed for the Claude runtime; empty means that adapter's
	// AdapterSpec.DefaultVersion. Expert-only: deliberately NOT
	// advertised in the provider's config_schema (same posture as
	// BuzzVersion and KeepWorkspacePAT), so Buzz Desktop's create-agent
	// dialog stays unchanged. Every segment of the key name avoids the
	// desktop's secret-word filter (token/key/secret/password/credential).
	ClaudeAdapterVersion string `json:"claude_adapter_version"`

	// CodexAdapterVersion is the codex twin of ClaudeAdapterVersion, with
	// the same expert-only posture and the same secret-word-safe naming.
	//
	// Bumping it is a deliberate act with a consequence beyond the version
	// number: the codex adapter — not this provider's config.toml — is
	// what governs the agent's sandbox and approval policy
	// (docs/M3_CODEX_PROBE_RESULTS.md S7/S10), so a new version must be
	// re-verified against that finding rather than assumed equivalent.
	CodexAdapterVersion string `json:"codex_adapter_version"`

	// ExtraBinaries and McpServers are the #15/#16 capability channel: extra
	// pinned binaries to install into the launch PATH dir, and MCP servers to
	// wire up. Expert-only and deliberately NOT advertised in the provider's
	// config_schema (same posture as KeepWorkspacePAT), so Buzz Desktop's
	// create-agent dialog stays unchanged.
	//
	// Both are ARRAY-valued, which is why they can only ever arrive via a raw
	// operator-CLI payload and never through Buzz Desktop: the desktop's
	// validate_provider_config accepts scalar values only, so a non-scalar
	// provider_config key is rejected upstream before it reaches us. This
	// package defines only their VALIDATION (issue #18); the install/wire
	// behavior is issues #15/#16, and a payload that sets these keys must
	// validate/refuse correctly but otherwise does nothing here.
	ExtraBinaries []ExtraBinary      `json:"extra_binaries"`
	McpServers    []string           `json:"mcp_servers"`
	MCP           mcpconfig.Config   `json:"mcp"`
	MCPConfig     string             `json:"mcp_config"`
	SkillsConfig  string             `json:"skills_config"`
	Skills        skillconfig.Config `json:"-"`
}

// SandboxInferenceAuth reports whether provider_config opts the deploy into
// zero-token inference auth (InferenceAuth == "sandbox"); see the field's
// doc comment above.
func (c ProviderConfig) SandboxInferenceAuth() bool {
	return c.InferenceAuth == "sandbox"
}

// OwnerPATInSandbox reports whether the deploy leaves a workspace-owner
// credential reachable inside the sandbox — the actual condition the
// env_vars restrictions below care about.
//
// Two distinct settings produce it, and gating on only the first is a hole:
// inference_auth="sandbox" derives the baked creator-identity PAT into the
// agent's environment, and keep_workspace_pat=true skips the PAT stub so
// the baked credential simply stays in ~/.databrickscfg — where the image's
// own ~/.codex/config.toml symlink has an auth.command that reads it back
// out (docs/M3_CODEX_PROBE_RESULTS.md S2). keep_workspace_pat is a
// documented total grant, so this is defense in depth there rather than a
// new guarantee; the point is that the check should key on the condition
// rather than on one of its two causes.
func (c ProviderConfig) OwnerPATInSandbox() bool {
	return c.SandboxInferenceAuth() || c.KeepWorkspacePAT
}

// MaxMcpServers caps provider_config.mcp_servers, mirroring buzz-agent's
// MAX_MCP_SERVERS. Enforced by validateMcpServers.
const MaxMcpServers = 16

// MuxBinaryName is the on-disk name of this provider's MCP multiplexer, the
// program a ≥2-entry mcp_servers deploy points BUZZ_ACP_MCP_COMMAND at (issue
// #16, McpMux). It is the single source of truth for the reserved name across
// BOTH capability channels — validateMcpServers refuses an mcp_servers entry
// with this name and validateExtraBinaries refuses an extra_binaries bin with
// it — so an operator payload can never shadow the multiplexer.
//
// It is kept here as a local const rather than imported from internal/mux
// (which delivers the binary itself in issue #16 Increment 2) to avoid an
// import cycle; the two must stay in sync.
const MuxBinaryName = "bzmux"

// McpMode classifies how provider_config.mcp_servers wires the agent's single
// BUZZ_ACP_MCP_COMMAND slot, by entry count. Callers (nest.RenderEnv,
// deployflow) branch on this intent rather than re-deriving it from len() at
// each site, the same spirit as EnvShape.
type McpMode int

const (
	// McpNone (0 entries): BUZZ_ACP_MCP_COMMAND is left to the runtime's
	// EnvShape default (buzz-agent/codex emit buzz-dev-mcp; claude emits
	// nothing). Byte-identical to the pre-#16 behavior.
	McpNone McpMode = iota
	// McpDirect (1 entry): BUZZ_ACP_MCP_COMMAND points straight at that single
	// command, for EVERY runtime — including claude, which emits nothing by
	// default (issue #14).
	McpDirect
	// McpMux (≥2 entries): BUZZ_ACP_MCP_COMMAND points at the embedded
	// multiplexer (MuxBinaryName), which fans out to each child. Issue #16
	// Increment 1 wires the mode and the emission; the multiplexer binary and
	// its mcp-mux.json config are delivered by Increment 2.
	McpMux
)

// McpMode returns the mode implied by the number of mcp_servers entries.
func (c ProviderConfig) McpMode() McpMode {
	if len(c.MCP.Servers) > 0 {
		// Managed servers need typed bridge arguments, so even one server uses
		// bzmux rather than the legacy bare-command direct slot.
		return McpMux
	}
	count := len(c.McpServers)
	switch count {
	case 0:
		return McpNone
	case 1:
		return McpDirect
	default:
		return McpMux
	}
}

// McpDirectCommand returns the single mcp_servers entry when McpMode is
// McpDirect, and "" otherwise. In McpDirect mode this is the value the
// resolved BUZZ_ACP_MCP_COMMAND takes.
func (c ProviderConfig) McpDirectCommand() string {
	if c.McpMode() == McpDirect && len(c.MCP.Servers) == 0 {
		return c.McpServers[0]
	}
	return ""
}

func (c ProviderConfig) HasManagedMCP() bool { return len(c.MCP.Servers) > 0 }

func (c *ProviderConfig) parseSkillsConfig() error {
	if c.SkillsConfig == "" {
		return nil
	}
	cfg, err := skillconfig.Parse([]byte(c.SkillsConfig))
	if err != nil {
		return fmt.Errorf("provider_config.skills_config: %w", err)
	}
	c.Skills = cfg
	return nil
}

func (c ProviderConfig) HasSkills() bool { return c.Skills.AITools != nil }

func (c *ProviderConfig) parseMCPConfig() error {
	if c.MCPConfig == "" {
		return nil
	}
	if len(c.MCP.Servers) > 0 {
		return fmt.Errorf("provider_config.mcp and provider_config.mcp_config are mutually exclusive")
	}
	cfg, err := mcpconfig.Parse([]byte(c.MCPConfig))
	if err != nil {
		return fmt.Errorf("provider_config.mcp_config: %w", err)
	}
	c.MCP = cfg
	return nil
}

// ParseDeployRequest unmarshals a raw deploy request body (the "agent" and
// "provider_config" sub-objects of the envelope already routed by
// internal/provider). Unknown fields anywhere are tolerated by default
// (encoding/json ignores them unless DisallowUnknownFields is used, which we
// deliberately never call).
func ParseDeployRequest(data []byte) (*DeployRequest, error) {
	var req DeployRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("parse deploy request: %w", err)
	}
	if err := req.ProviderConfig.parseMCPConfig(); err != nil {
		return nil, fmt.Errorf("parse deploy request: %w", err)
	}
	if err := req.ProviderConfig.parseSkillsConfig(); err != nil {
		return nil, fmt.Errorf("parse deploy request: %w", err)
	}
	if err := req.ApplyLaunch(); err != nil {
		return nil, fmt.Errorf("parse deploy request: %w", err)
	}
	return &req, nil
}

func stringPtr(value string) *string { return &value }

func providerOwnedEnvKey(key string) bool {
	switch strings.ToUpper(key) {
	case "BUZZ_RELAY_URL", "BUZZ_PRIVATE_KEY", "NOSTR_PRIVATE_KEY", "BUZZ_AUTH_TAG",
		"BUZZ_ACP_AGENT_OWNER", "BUZZ_ACP_AGENT_COMMAND", "BUZZ_ACP_AGENT_ARGS",
		"BUZZ_ACP_RESPOND_TO", "BUZZ_ACP_RESPOND_TO_ALLOWLIST", "BUZZ_ACP_MCP_COMMAND",
		"BUZZ_ACP_RELAY_OBSERVER", "MCP_HOOK_SERVERS":
		return true
	default:
		return false
	}
}

// ApplyLaunch projects a current Buzz launch block onto the legacy fields the
// rest of this provider already consumes. This is intentionally a replacement,
// not a merge with agent.env_vars: the desktop already resolved all user
// layers into launch.env, and re-applying the legacy map would resurrect stale
// or reserved values. Tier 2 wins over tier 1; provider-owned identity fields
// continue to be emitted authoritatively by nest.RenderEnv.
func (r *DeployRequest) ApplyLaunch() error {
	launch := r.Agent.Launch
	if launch == nil {
		return nil
	}

	command := strings.TrimSpace(launch.Command)
	if command == "" {
		return fmt.Errorf("agent.launch.command must not be empty when agent.launch is present")
	}

	env := make(map[string]string, len(launch.PolicyEnv)+len(launch.Env))
	for key, value := range launch.PolicyEnv {
		if providerOwnedEnvKey(key) {
			continue
		}
		env[key] = value
	}
	for key, value := range launch.Env {
		if providerOwnedEnvKey(key) {
			continue
		}
		env[key] = value
	}

	// A present launch block replaces every launch-controlled legacy field.
	// Missing values mean absent/default, never "fall back to the stale legacy
	// projection".
	r.Agent.AgentCommand = command
	r.Agent.AgentArgs = append([]string(nil), launch.Args...)
	r.Agent.EnvVars = env
	r.Agent.OwnerPubkey = strings.TrimSpace(launch.OwnerPubkey)
	r.Agent.SystemPrompt = ""
	r.Agent.Model = nil
	r.Agent.Provider = nil
	r.Agent.Parallelism = 1
	r.Agent.IdleTimeoutSeconds = 0
	r.Agent.MaxTurnDurationSecs = 0
	r.Agent.RespondTo = "owner-only"
	r.Agent.RespondToAllowlist = nil
	// These keys are provider-owned in the rendered sandbox environment, so
	// they were intentionally removed from env above. Project their resolved
	// launch values onto the typed legacy fields rather than falling back to
	// stale pre-launch payload fields or emitting an empty CLI argument.
	resolvedLaunchValue := func(key string) (string, bool) {
		value, ok := launch.PolicyEnv[key]
		if override, exists := launch.Env[key]; exists {
			return override, true
		}
		return value, ok
	}
	if value, ok := resolvedLaunchValue("BUZZ_ACP_RESPOND_TO"); ok && value != "" {
		r.Agent.RespondTo = value
	}
	if r.Agent.RespondTo == "allowlist" {
		if value, ok := resolvedLaunchValue("BUZZ_ACP_RESPOND_TO_ALLOWLIST"); ok && value != "" {
			r.Agent.RespondToAllowlist = strings.Split(value, ",")
		}
	}
	if value, ok := env["BUZZ_ACP_SYSTEM_PROMPT"]; ok {
		r.Agent.SystemPrompt = value
	}
	if value, ok := env["BUZZ_ACP_MODEL"]; ok {
		r.Agent.Model = stringPtr(value)
	} else if value, ok := env["ANTHROPIC_MODEL"]; ok {
		r.Agent.Model = stringPtr(value)
	}
	if value, ok := env["BUZZ_AGENT_PROVIDER"]; ok {
		r.Agent.Provider = stringPtr(value)
	} else if value, ok := env["GOOSE_PROVIDER"]; ok {
		r.Agent.Provider = stringPtr(value)
	}
	if value, ok := env["BUZZ_ACP_AGENTS"]; ok {
		parallelism, err := strconv.Atoi(value)
		if err != nil || parallelism < 1 {
			return fmt.Errorf("agent.launch environment BUZZ_ACP_AGENTS must be a positive integer")
		}
		r.Agent.Parallelism = parallelism
	}
	if value, ok := env["BUZZ_ACP_IDLE_TIMEOUT"]; ok {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 0 {
			return fmt.Errorf("agent.launch environment BUZZ_ACP_IDLE_TIMEOUT must be a non-negative integer")
		}
		r.Agent.IdleTimeoutSeconds = seconds
	}
	if value, ok := env["BUZZ_ACP_MAX_TURN_DURATION"]; ok {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 0 {
			return fmt.Errorf("agent.launch environment BUZZ_ACP_MAX_TURN_DURATION must be a non-negative integer")
		}
		r.Agent.MaxTurnDurationSecs = seconds
	}

	// Current Buzz requires DATABRICKS_HOST on the Databricks inference
	// provider even when this run provider will use the Sandbox-native
	// credential. That host (and any inherited token paired with it) arrives in
	// launch.env after Desktop has used it for local model discovery. Sandbox
	// auth must not forward either value: it derives an all-or-nothing pair from
	// the selected Sandbox instead. Drop only this current-launch projection;
	// legacy/operator payloads without a launch block still fail loudly in
	// validateOwnerPATEnvVars, preserving the existing security contract.
	if r.ProviderConfig.SandboxInferenceAuth() {
		delete(r.Agent.EnvVars, "DATABRICKS_HOST")
		delete(r.Agent.EnvVars, "DATABRICKS_TOKEN")
	}
	return nil
}

// Validate checks the fields deploy provisioning depends on. It never
// returns a value derived from secrets in the error text.
func (a Agent) Validate() error {
	if a.PrivateKeyNsec == "" {
		return fmt.Errorf("agent.private_key_nsec must not be empty")
	}
	if a.RelayURL == "" {
		return fmt.Errorf("agent.relay_url must not be empty")
	}
	// Parsed, not merely non-empty, and this is a silent-agent guard rather
	// than tidiness. buzz-acp's codex_network_env does Url::parse on this
	// value and returns None on failure, skipping the CODEX_CONFIG
	// injection that grants codex's MCP subprocess outbound network
	// (block/buzz crates/buzz-acp/src/config.rs:717-749). Since the MCP
	// subprocess is how a codex agent answers a mention, a
	// malformed-but-non-empty relay_url yields an agent that installs,
	// handshakes, passes the inference probe, and is then permanently
	// silent — with nothing on the provider side reporting a problem.
	//
	// Upstream accepts ws/wss/http/https here; we require a scheme and a
	// host and nothing more, so this rejects only what upstream would also
	// fail to parse.
	if u, err := url.Parse(a.RelayURL); err != nil || u.Host == "" {
		return fmt.Errorf("agent.relay_url %q is not a parseable URL with a host; buzz-acp silently skips the codex network injection for URLs it cannot parse, which produces an agent that deploys clean and never answers", a.RelayURL)
	} else if u.Scheme != "ws" && u.Scheme != "wss" && u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("agent.relay_url scheme %q is not one of ws, wss, http, https", u.Scheme)
	}
	if _, ok := RuntimeFor(a.AgentCommand); !ok {
		return unsupportedRuntimeError(a.AgentCommand)
	}
	// env_vars keys are written raw (unquoted) into a shell-sourced env
	// file by internal/nest's RenderEnv, so a key that isn't a valid
	// shell/environment variable name is an arbitrary-command-execution
	// vector in that sandbox shell. Reject anything else here, the single
	// choke point both deploy entry points (provider.handleDeploy and
	// deployflow.Deploy) route through via DeployRequest.Validate().
	for key := range a.EnvVars {
		// The provider owns the lowercase buzz_ namespace across three
		// shell snippets (SandboxAuthSnippet, ClaudeEnvSnippet,
		// CodexEnvSnippet), whose scratch variables all match the key
		// charset below and are therefore settable from a payload. Those
		// snippets render AFTER env_vars, so an inherited value is visible
		// to them — and one such variable gating a config write is exactly
		// how a charset gate got bypassed once already.
		//
		// Each snippet also initializes its own scratch variables, which is
		// the real fix; this closes the class so the next snippet author
		// does not have to remember the rule.
		if reservedScratchEnvKey(key) {
			return fmt.Errorf(
				"env_vars key %q collides with this provider's own shell scratch variables; rename it",
				key,
			)
		}
		if !envVarKeyPattern.MatchString(key) {
			return fmt.Errorf(
				"env_vars key %q is not a valid environment variable name; only names matching ^[A-Za-z_][A-Za-z0-9_]*$ are allowed",
				key,
			)
		}
	}
	return nil
}

// validInferenceAuthValues are the only accepted provider_config.inference_auth
// values. Matching is exact-case; "SANDBOX" or similar variants are rejected
// rather than silently normalized, so callers can't be surprised by a typo
// quietly falling back to a different auth mode.
var validInferenceAuthValues = map[string]bool{
	"":        true,
	"env":     true,
	"sandbox": true,
}

// Validate validates the full deploy request: the agent sub-object plus the
// provider_config fields this provider itself constrains. It never returns a
// value derived from secrets in the error text.
func (r DeployRequest) Validate() error {
	if err := r.Agent.Validate(); err != nil {
		return err
	}
	if !validInferenceAuthValues[r.ProviderConfig.InferenceAuth] {
		return fmt.Errorf(
			"provider_config.inference_auth %q is not supported; allowed values are \"\", \"env\", \"sandbox\"",
			r.ProviderConfig.InferenceAuth,
		)
	}
	if err := r.validateOwnerPATEnvVars(); err != nil {
		return err
	}
	if err := r.validateCapabilityKeysOwnerPAT(); err != nil {
		return err
	}
	if err := r.validateExtraBinaries(); err != nil {
		return err
	}
	if err := r.validateMcpServers(); err != nil {
		return err
	}
	if r.ProviderConfig.HasManagedMCP() {
		if len(r.ProviderConfig.McpServers) > 0 {
			return fmt.Errorf("provider_config.mcp and provider_config.mcp_servers are mutually exclusive")
		}
		if err := r.ProviderConfig.MCP.Validate(); err != nil {
			return fmt.Errorf("provider_config.mcp: %w", err)
		}
	}
	if r.ProviderConfig.HasSkills() {
		if r.ProviderConfig.OwnerPATInSandbox() {
			return fmt.Errorf("provider_config.skills_config must not be set when the sandbox holds a workspace-owner credential; use inference_auth=\"env\" with a least-privilege credential")
		}
		if err := r.ProviderConfig.Skills.Validate(); err != nil {
			return fmt.Errorf("provider_config.skills_config: %w", err)
		}
	}
	if err := r.validateClaudeInferenceSource(); err != nil {
		return err
	}
	if err := r.validateCodexInferenceSource(); err != nil {
		return err
	}
	return nil
}

// THE INVARIANT THESE LISTS ENFORCE, stated once because three separate
// mistakes came from not having it written down:
//
//	agent.env_vars render LAST and win over everything the provider emits.
//	So the question is never "does an adapter read this" — it is "does this
//	decide where the credential goes, or what code runs beside it".
//
// An earlier version reasoned only about variables the codex ADAPTER reads,
// which missed both the loader class (NODE_OPTIONS, LD_PRELOAD) and the
// provider's own emitted variables (BUZZ_ACP_AGENT_COMMAND) — the latter
// being the most direct lever of all.
//
// The two lists are gated on DIFFERENT conditions, and conflating them was
// itself a defect. See validateOwnerPATEnvVars.

// reservedScratchEnvKey reports whether an env_vars key collides with a
// shell variable this provider uses in its own generated scripts.
//
// This is a namespace reservation, not a security control in itself — but
// the collisions it prevents are, because every one of these scripts
// `.`-sources the rendered env file, and env_vars are emitted LAST. So a
// payload key with one of these names silently replaces the provider's own
// value *after* the script assigned it. Two demonstrated consequences:
//
//   - Lowercase buzz_*: an inherited buzz_codex_url reached CodexEnvSnippet's
//     heredoc without passing the host charset gate, because the write and
//     the gate had been decoupled into separate branches. Arbitrary TOML,
//     including an auth.command codex runs at startup.
//   - Uppercase BUZZ_PROBE_*: the inference probes assign these BEFORE
//     sourcing, so the source overwrites them. Reproduced: the probe wrote
//     the owner-PAT bearer into a payload-chosen path and curl -K read it
//     from there, while the real mktemp file holding the agent's full env —
//     nsec, auth tag, token — was orphaned 0600 on disk, un-reclaimed by the
//     EXIT trap (which deletes the decoy) and invisible to
//     secretShredCommand's fixed path list.
//
// Each script also initializes its own scratch variables, which is the real
// fix in both cases. This closes the class so the next script author does
// not have to rediscover the rule.
//
// Uppercase BUZZ_ACP_* must stay reachable — it is buzz-acp's real
// configuration surface — so this cannot be a blanket BUZZ_ prefix.
func reservedScratchEnvKey(key string) bool {
	// Lowercase provider scratch, used across SandboxAuthSnippet,
	// ClaudeEnvSnippet and CodexEnvSnippet.
	if strings.HasPrefix(key, "buzz_") {
		return true
	}
	// Inference-probe scratch (internal/deployflow/{claude,codex}.go).
	if strings.HasPrefix(key, "BUZZ_PROBE_") {
		return true
	}
	// install.BuildVerifyCommand's env-file and output paths.
	return key == "ENVF" || key == "OUTF"
}

// credentialPairEnvKeys decide WHERE the provider-derived credential is
// sent. They are refused only under inference_auth="sandbox", because that
// is the only mode in which the provider derives the sandbox's baked
// owner PAT into the agent's environment at all (nest.RenderEnv renders
// SandboxAuthSnippet for that mode and no other).
//
// Under keep_workspace_pat with inference_auth="env" the provider derives
// nothing: the token in the agent's environment is the owner's own,
// supplied in the same payload, going to their own host. Refusing these
// there blocks the ONLY working inference configuration for buzz-agent —
// which reads DATABRICKS_HOST/DATABRICKS_TOKEN directly and has no
// bring-your-own-endpoint alternative — while preventing nothing.
//
// Enumerated rather than a DATABRICKS_ prefix on purpose: DATABRICKS_MODEL
// is a legitimate env_vars entry that RenderEnv itself emits for
// buzz-agent, and a prefix rule would reject it.
var credentialPairEnvKeys = []string{
	"DATABRICKS_HOST", "DATABRICKS_TOKEN",
	// Alternate paths to a workspace credential, and the file the
	// derivation reads.
	"DATABRICKS_CONFIG_FILE", "DATABRICKS_CLIENT_ID", "DATABRICKS_CLIENT_SECRET",
	// The claude runtime's own pair. Its snippet leaves both untouched when
	// supplied together, so in sandbox mode these would point the agent —
	// and the inference probe's own request — at an endpoint of the
	// payload's choosing while the provider supplies the credential.
	"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
}

// ownerPATForbiddenEnvPrefixes are env_vars name PREFIXES refused under the
// same condition as ownerPATForbiddenEnvKeys. These are loader-class
// variables: they choose code that runs inside the process holding the
// credential, before that process's own code does.
//
// A prefix rule rather than a name list is deliberate. NODE_OPTIONS with an
// `--import=data:text/javascript,...` URL executes arbitrary attacker code
// in the adapter with DATABRICKS_TOKEN in process.env and no file on disk;
// LD_PRELOAD does the same for the spawned native binary. Neither is an
// adapter setting, so no adapter-derived list would ever have caught them,
// and enumerating the loader surface exactly is not a winnable game.
var ownerPATForbiddenEnvPrefixes = []string{
	"LD_",         // dynamic-linker injection into any native child
	"DYLD_",       // the macOS equivalent, for completeness
	"NODE_",       // NODE_OPTIONS and friends, for the npm ACP adapters
	"npm_config_", // registry/CA/prefix redirection
	"PYTHON",      // PYTHONPATH / PYTHONSTARTUP for python children
	"GIT_",        // git-credential-nostr and any repo tooling
}

// ownerPATForbiddenEnvKeys decide WHAT CODE runs in a process that can
// reach a workspace-owner credential. They are refused whenever such a
// credential is reachable at all (ProviderConfig.OwnerPATInSandbox), which
// includes keep_workspace_pat: there the baked PAT stays in
// ~/.databrickscfg, and the image's own ~/.codex/config.toml symlink has an
// auth.command that reads it straight back out (probe S2).
var ownerPATForbiddenEnvKeys = []string{
	// The programs buzz-acp spawns. RenderEnv emits all three BEFORE the
	// env_vars block, so an owner-supplied value wins — making these the
	// most direct lever in the whole surface, needing no loader trick and
	// nothing on disk: BUZZ_ACP_AGENT_COMMAND=sh with
	// BUZZ_ACP_AGENT_ARGS=-c,<exfiltrate> is complete on its own, using
	// only image-resident binaries. BUZZ_ACP_AGENT_COMMAND additionally
	// defeats the spawn-command canonicalization the entire ucode-wrapper
	// defense rests on: whatever agent_command said, this is what runs.
	"BUZZ_ACP_AGENT_COMMAND", "BUZZ_ACP_AGENT_ARGS", "BUZZ_ACP_MCP_COMMAND",
	// Interception: a proxy the agent's inference client honours sees
	// (or, with a supplied CA, terminates) requests carrying the token.
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	// Trust-store substitution, which is what turns a proxy into a
	// cleartext read of the bearer.
	"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	"NODE_EXTRA_CA_CERTS",
	// Command resolution and process layout. HOME relocates
	// ~/.databrickscfg, $CODEX_HOME, the launch lock, and the path
	// launch.sh execs buzz-acp from. BUZZ_SHELL is the one actually
	// honoured in this stack — buzz-dev-mcp's shell tool resolves it and
	// spawns it as its interpreter (buzz-dev-mcp/src/shell.rs:365-388) —
	// while plain SHELL is read by nothing here; both are listed because
	// the criterion is the variable's effect, not its popularity.
	"PATH", "SHELL", "BUZZ_SHELL", "BASH_ENV", "ENV", "IFS", "HOME",

	// FILE-READING LEVERS, and the reason this list's criterion had to be
	// widened. Everything above decides what CODE runs beside the
	// credential. These decide what the PROVIDER'S OWN STACK READS and
	// sends to a payload-chosen endpoint — no agent tool call, no
	// session/request_permission, no relay traffic, nothing on disk.
	//
	// buzz-acp reads each of these paths itself and prepends the contents
	// (up to 1 MB) to every prompt it sends to the configured inference
	// endpoint: base at config.rs:851, system at :823, heartbeat at :843,
	// all plain std::fs::read_to_string. Point one at ~/.databrickscfg
	// under keep_workspace_pat — where the baked PAT is still on disk and
	// the credential pair is legitimately payload-supplied — and the owner
	// token is exfiltrated in the prompt body on the first mention. The
	// heartbeat variant needs no mention at all: it fires on a timer.
	//
	// BUZZ_ACP_SYSTEM_PROMPT_FILE is the least reachable of the three
	// (clap conflicts with BUZZ_ACP_SYSTEM_PROMPT, which RenderEnv always
	// emits) and is listed anyway: a defense that depends on an upstream
	// conflict declaration staying put is not a defense.
	"BUZZ_ACP_BASE_PROMPT_FILE", "BUZZ_ACP_SYSTEM_PROMPT_FILE",
	"BUZZ_ACP_HEARTBEAT_PROMPT_FILE",

	// Observability integrity. RenderEnv emits BUZZ_ACP_RELAY_OBSERVER
	// ="true" and calls observer frames "the desktop's ONLY health signal
	// for a provider-deployed agent" — then emits env_vars after it, so a
	// payload wins. Not a credential lever on its own, which is why it sits
	// at the end of this list rather than among the ones above; it is here
	// because it composes with them, removing the last operator-visible
	// trace of an agent doing something else.
	"BUZZ_ACP_RELAY_OBSERVER",
}

// validateOwnerPATEnvVars refuses payload-supplied environment that could
// redirect, intercept, or read a workspace-owner credential the payload
// never had to carry.
//
// The asymmetry is the whole rule, and it is not a loophole. Under
// inference_auth="env" the token in the agent's environment is the OWNER'S
// OWN, supplied by them in this same payload: pointing it at another
// endpoint, through a proxy, or at instrumented code is their business, and
// this returns nil. Under inference_auth="sandbox" (or keep_workspace_pat)
// the credential is the sandbox's baked creator-identity PAT, which this
// provider materializes on the owner's behalf — so forwarding it somewhere
// of their choosing would be a privilege escalation performed BY the
// provider, and the deploy would report healthy while doing it.
//
// This applies to every runtime, not just the ACP adapters: buzz-agent
// reads DATABRICKS_HOST/DATABRICKS_TOKEN directly, so the endpoint half of
// the pair has always been reachable there too.
//
// Scope, stated honestly: this closes silent forwarding by the provider. It
// does not, and cannot, stop a determined agent OWNER from obtaining the
// credential — they can simply ask the agent to print it, and the answer
// returns over the relay as ordinary transcript text. See docs/PLAN.md §5.
func (r DeployRequest) validateOwnerPATEnvVars() error {
	// Sorted so the reported key is deterministic when a payload sets
	// several; map iteration order would otherwise vary between runs.
	keys := make([]string, 0, len(r.Agent.EnvVars))
	for k := range r.Agent.EnvVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		var why string
		if r.ProviderConfig.SandboxInferenceAuth() {
			for _, forbidden := range credentialPairEnvKeys {
				if key == forbidden {
					why = "it decides where this provider sends the credential it derived"
					break
				}
			}
		}
		if why == "" && r.ProviderConfig.OwnerPATInSandbox() {
			for _, forbidden := range ownerPATForbiddenEnvKeys {
				if key == forbidden {
					why = "it decides what code runs in a process that can reach that credential"
					break
				}
			}
			if why == "" {
				for _, prefix := range ownerPATForbiddenEnvPrefixes {
					if strings.HasPrefix(key, prefix) {
						why = "variables starting with " + prefix + " choose code that runs inside the process holding that credential"
						break
					}
				}
			}
		}
		if why == "" {
			continue
		}
		return fmt.Errorf(
			"env_vars.%s must not be set when the sandbox holds a workspace-owner credential "+
				"(provider_config.inference_auth=\"sandbox\" or keep_workspace_pat=true): %s, "+
				"and that credential is the sandbox's baked owner token which this payload never supplied. "+
				"Use inference_auth=\"env\" with your own DATABRICKS_HOST/DATABRICKS_TOKEN if you need to control this",
			key, why,
		)
	}
	return nil
}

// codexAdapterEnvVars are the environment variables the codex RUNTIME (the
// ACP adapter plus the codex binary it spawns) reads that can redirect
// inference or widen the session's own policy. None is visible to
// nest.CodexEnvSnippet's file-based gate: agent.env_vars render BEFORE the
// snippet and are exported into the agent's process.
//
// This is a PURPOSE-SCOPED list, not an exhaustive one, and the distinction
// is load-bearing. An earlier version of this comment claimed to be the
// "COMPLETE set of environment variables the adapter reads", sourced from
// docs/M3_CODEX_PROBE_RESULTS.md S10 — but S10 undercounted (it said six;
// the bundle reads eleven, several through `env[VAR]` indirection that a
// literal `process.env.X` scan misses). Worse, completeness was never
// achievable: NODE_OPTIONS and LD_PRELOAD redirect the credential just as
// effectively and are not adapter settings at all, so no
// adapter-derived list could contain them. That whole class is handled by
// validateOwnerPATEnvVars instead, which is where the real defense lives.
//
// Deliberately EXCLUDED after review, because they cannot move a
// credential: DISABLE_MCP_CONFIG_FILTERING (MCP name dedup),
// XDG_RUNTIME_DIR, NO_BROWSER (suppresses the ChatGPT auth method, which
// buzz-acp never exercises).
//
// Ordered by severity rather than alphabetically, so a reader meets the
// worst one first.
var codexAdapterEnvVars = []string{
	// Adapter-level auth override. With methodId "gateway" it calls
	// applyGatewayConfig({baseUrl, headers, providerName}), installing a
	// provider that SUPERSEDES the generated [model_providers.databricks]
	// block entirely — an arbitrary base_url plus arbitrary HTTP headers.
	// Its "api-key" branch additionally carries an inline key. This is a
	// full endpoint-redirect primitive, not an auth-method selector.
	"DEFAULT_AUTH_REQUEST",
	// The owner's own config.toml, with their own base_url and an
	// env_key that names the token this provider exports.
	"CODEX_HOME",
	// Session-level config override, applied at thread start, outranking
	// anything in the generated file.
	"CODEX_CONFIG",
	// Selects a provider block; ours defines only "databricks". A silent
	// denial of service rather than a leak, but silent.
	"MODEL_PROVIDER",
	// Spawned as `<path> app-server`. The image's /usr/local/bin/codex is
	// a ucode wrapper that takes no arguments, so this also breaks the
	// runtime outright (probe S1/S10).
	"CODEX_PATH",
	// Selects an AgentMode preset. "agent-full-access" sets
	// approvalPolicy "never" and dangerFullAccess, so every tool call is
	// executed with no session/request_permission at all — removing the
	// only trace an exfiltration would otherwise leave, and granting
	// tool-path network without depending on buzz-acp's injection. S10's
	// own design consequence #1 says the provider must not set this; the
	// payload must not be able to either.
	"INITIAL_AGENT_MODE",
	// readApiKeyFromEnv()'s fallback on the api-key auth path. buzz-acp
	// never exercises that path today (probe S5: no authenticate round
	// trip), so this is latent rather than live — rejected because a
	// credential-bearing variable should not depend on that staying true.
	"CODEX_API_KEY",
	"OPENAI_API_KEY",
	// The adapter's Logger writes every JSON-RPC frame, plus a Startup
	// record containing the auth request, to this directory. In-sandbox
	// only, so nothing crosses a boundary — rejected because an
	// owner-chosen destination for auth-bearing frames is not something
	// this provider should be arranging on their behalf.
	"APP_SERVER_LOGS",
}

// validateCodexInferenceSource is the codex analogue of
// validateClaudeInferenceSource, and it has to work differently because the
// mechanism that makes the Claude rule safe is unavailable here.
//
// Claude's rule is "bring-your-own endpoint requires bring-your-own token",
// and it is enforceable because the provider can WITHHOLD its credential:
// nest.ClaudeEnvSnippet simply does not export ANTHROPIC_AUTH_TOKEN for an
// endpoint it did not derive. Codex has no such lever. Its token arrives via
// `env_key = "DATABRICKS_TOKEN"`, and DATABRICKS_TOKEN is already exported
// for other reasons — by SandboxAuthSnippet in sandbox mode, ungated on the
// host, or by the owner's own env_vars in env mode.
//
// So under inference_auth="sandbox" — where the credential is the sandbox's
// baked workspace-OWNER PAT, which the payload never had to carry — any of
// the adapter's env vars would let a deploy forward that credential to an
// endpoint of the owner's choosing, on every turn, with the deploy reporting
// healthy: the inference probe reads the same artifact the owner supplied,
// so it would validate the attacker's endpoint rather than catch it.
// Rejecting the whole set is the only enforceable rule.
//
// Under inference_auth="env" they are all permitted: there the token is the
// owner's own, supplied by them, and pointing it wherever they like is their
// business. That asymmetry — not a blanket ban — is the true mirror of the
// Claude posture.
func (r DeployRequest) validateCodexInferenceSource() error {
	rt, ok := RuntimeFor(r.Agent.AgentCommand)
	if !ok || rt != RuntimeCodex {
		return nil
	}
	// OwnerPATInSandbox, not SandboxInferenceAuth: keep_workspace_pat=true
	// leaves the baked PAT in ~/.databrickscfg, where the image's own
	// ~/.codex/config.toml symlink has an auth.command that reads it back
	// out (probe S2). Gating on one of the two causes rather than the
	// condition is the shape of the bug this whole round was about.
	if !r.ProviderConfig.OwnerPATInSandbox() {
		return nil
	}
	for _, key := range codexAdapterEnvVars {
		if _, present := r.Agent.EnvVars[key]; !present {
			continue
		}
		return fmt.Errorf(
			"agent_command %q with provider_config.inference_auth=\"sandbox\" must not set env_vars.%s: "+
				"the codex adapter reads that variable to decide where it sends inference requests, and in this mode the credential it would carry "+
				"is the sandbox's baked workspace-owner token that this payload never supplied. "+
				"Use inference_auth=\"env\" with your own DATABRICKS_HOST/DATABRICKS_TOKEN to point codex at another endpoint",
			r.Agent.AgentCommand, key,
		)
	}
	return nil
}

// validateClaudeInferenceSource requires the Claude runtime to have exactly
// one coherent place to get its inference endpoint from, and is the
// fail-loud half of the credential-egress defense (the fail-closed half
// lives in nest.ClaudeEnvSnippet).
//
// Why this exists, from a live probe (docs/M2_CLAUDE_PROBE_RESULTS.md):
// Claude Code falls back to https://api.anthropic.com when
// ANTHROPIC_BASE_URL is unset, and a Lakebox sandbox has OPEN egress to
// that host (HTTP 401 in 147ms with a genuine Anthropic error body). So an
// inference_auth="env" deploy that supplies DATABRICKS_TOKEN but forgets
// DATABRICKS_HOST would otherwise send a live workspace PAT to a third
// party in an Authorization header — while every provider-side gate
// (install verification, launch verification) still reported success,
// because none of them touch the LLM.
//
// This must live on DeployRequest rather than Agent: it is the only level
// that sees both agent.env_vars and provider_config.inference_auth.
//
// Note there is deliberately NO check here for ANTHROPIC_API_KEY coexisting
// with ANTHROPIC_AUTH_TOKEN. That hazard was hypothesized and then
// disproven live: the adapter completes turns normally with an
// ANTHROPIC_API_KEY that is empty, and with one set to a bogus value,
// alongside ANTHROPIC_AUTH_TOKEN. An API key simply cannot work against
// this gateway (it sends x-api-key, which the gateway rejects with 401), so
// it is inert rather than dangerous.
func (r DeployRequest) validateClaudeInferenceSource() error {
	rt, ok := RuntimeFor(r.Agent.AgentCommand)
	if !ok || rt != RuntimeClaude {
		return nil
	}
	// Bring-your-own endpoint requires bring-your-own token, checked here so
	// it fails at the payload boundary rather than at the deploy-time
	// inference probe (whose "unset" diagnosis would send the operator to
	// DATABRICKS_HOST, not to the token they actually omitted).
	//
	// The requirement is not arbitrary: an explicit ANTHROPIC_BASE_URL
	// suppresses the snippet's own derivation, and the snippet deliberately
	// refuses to attach the workspace credential to an endpoint it did not
	// choose — so this combination can only ever produce an agent with an
	// endpoint and no credential.
	if r.Agent.EnvVars["ANTHROPIC_BASE_URL"] != "" {
		if r.Agent.EnvVars["ANTHROPIC_AUTH_TOKEN"] == "" {
			return fmt.Errorf(
				"agent_command %q sets env_vars.ANTHROPIC_BASE_URL without env_vars.ANTHROPIC_AUTH_TOKEN: "+
					"an endpoint this provider did not derive is never given the workspace credential, so the agent would have no token. "+
					"Supply both, or drop ANTHROPIC_BASE_URL to use the workspace AI Gateway",
				r.Agent.AgentCommand,
			)
		}
		return nil
	}
	if r.ProviderConfig.SandboxInferenceAuth() {
		// Zero-token mode derives DATABRICKS_HOST in-sandbox from the
		// baked ~/.databrickscfg, so there is nothing more to require.
		// Checked AFTER the bring-your-own-endpoint rule above, not
		// before: an explicit ANTHROPIC_BASE_URL suppresses derivation in
		// this mode too, so short-circuiting here would accept exactly the
		// endpoint-without-credential deploy that rule exists to reject.
		return nil
	}
	if r.Agent.EnvVars["DATABRICKS_HOST"] != "" {
		return nil
	}
	return fmt.Errorf(
		"agent_command %q needs an inference endpoint: set env_vars.DATABRICKS_HOST (with DATABRICKS_TOKEN) to use the workspace AI Gateway, "+
			"or set provider_config.inference_auth=\"sandbox\" to derive both from the sandbox's own credential, "+
			"or set env_vars.ANTHROPIC_BASE_URL together with env_vars.ANTHROPIC_AUTH_TOKEN to target another endpoint "+
			"(an endpoint this provider did not derive is never given the workspace credential). "+
			"Without one of these the agent would fall back to the public Anthropic API and send its token there",
		r.Agent.AgentCommand,
	)
}
