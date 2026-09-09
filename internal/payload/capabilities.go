package payload

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/install"
)

// ExtraBinary is one entry of provider_config.extra_binaries: a pinned binary
// to fetch and expose in the launch PATH dir. Issue #18 defines its VALIDATION
// (the structural rules below and the owner-PAT gate in
// validateCapabilityKeysOwnerPAT); issue #15 defines the install behavior. A
// payload that sets these keys must validate/refuse here but otherwise does
// nothing in this package.
type ExtraBinary struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Bin    string `json:"bin"`
}

// sha256HexPattern is a published sha256 digest as LOWERCASE hex: exactly 64
// characters, [0-9a-f] only. Uppercase or mixed case is rejected rather than
// normalized — matching the deliberate no-normalization stance elsewhere in
// this package (validInferenceAuthValues), so a mis-cased digest fails loudly
// instead of being quietly accepted. Compiled once at package scope in the
// style of envVarKeyPattern.
var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// extraBinaryNamePattern is a bare filename: no path separators, no shell
// metacharacters. The charset already excludes "/" and "\\"; the "."/".."
// exclusion in validateExtraBinaries is the traversal guard the charset alone
// cannot express.
var extraBinaryNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validateCapabilityKeysOwnerPAT is the SOLE guard on the provider_config
// channel, and that framing is load-bearing rather than incidental.
//
// The two capability keys — extra_binaries and mcp_servers — each decide what
// CODE runs in the sandbox: extra_binaries installs executables into the PATH
// dir launch.sh prepends, and mcp_servers wires up subprocesses. When the
// sandbox holds a workspace-owner credential (inference_auth="sandbox" or
// keep_workspace_pat=true), a process that code can reach can read and forward
// that credential — the exact escalation validateOwnerPATEnvVars refuses on
// the env_vars channel.
//
// But these keys live on the provider_config channel, which has NO upstream
// guard. BUZZ_ACP_MCP_COMMAND on the env_vars channel is guarded twice —
// upstream Buzz's RESERVED_ENV_KEYS strips it before the payload reaches us,
// and validateOwnerPATEnvVars refuses it again here — so through the Desktop
// BUZZ_ACP_MCP_COMMAND is never deliverable in any mode. What inference_auth=
// "env" permits is provider-side emission of the MCP command (the #15/#16
// mechanism), not an env_vars override. provider_config has neither of those
// upstream layers: the desktop's validate_provider_config only filters scalar
// values and secret-like key names, and these array-valued keys can only
// arrive via a raw operator-CLI payload in the first place. So this gate is
// the ONLY thing standing between such a payload and code running beside the
// owner credential — NOT belt-and-suspenders.
//
// The remedy mirrors validateOwnerPATEnvVars: under inference_auth="env" the
// credential in the sandbox is the owner's own, so running their own code
// beside it is their business and this returns nil.
func (r DeployRequest) validateCapabilityKeysOwnerPAT() error {
	if !r.ProviderConfig.OwnerPATInSandbox() {
		return nil
	}
	// Reported deterministically: extra_binaries first, then mcp_servers, so a
	// payload setting both always names the same key.
	var key string
	switch {
	case len(r.ProviderConfig.ExtraBinaries) > 0:
		key = "extra_binaries"
	case len(r.ProviderConfig.McpServers) > 0:
		key = "mcp_servers"
	case r.ProviderConfig.HasManagedMCP():
		key = "mcp"
	default:
		return nil
	}
	return fmt.Errorf(
		"provider_config.%s must not be set when the sandbox holds a workspace-owner credential "+
			"(provider_config.inference_auth=\"sandbox\" or keep_workspace_pat=true): it decides what code runs "+
			"in a process that can reach that credential, and that credential is the sandbox's baked owner token "+
			"which this payload never supplied. "+
			"Use inference_auth=\"env\" with your own DATABRICKS_HOST/DATABRICKS_TOKEN if you need to control this",
		key,
	)
}

// validateExtraBinaries enforces the STRUCTURAL rules on every
// provider_config.extra_binaries entry, UNCONDITIONALLY — in every mode,
// independent of OwnerPATInSandbox. A malformed url, a missing or mis-cased
// digest, or a bin name that would shadow a real binary is a defect regardless
// of the inference-auth mode, so these checks are not gated on the owner-PAT
// condition the way validateCapabilityKeysOwnerPAT is.
//
// Error text names the field and offending index/value and never leaks a
// secret.
func (r DeployRequest) validateExtraBinaries() error {
	bins := r.ProviderConfig.ExtraBinaries
	if len(bins) == 0 {
		return nil
	}

	// The reserved set the bin name must not shadow: install's own symlinked
	// binaries UNION the adapter bin names, kept as install's source of truth
	// rather than duplicated here.
	reserved := make(map[string]bool)
	for _, name := range install.BinNames {
		reserved[name] = true
	}
	for _, name := range install.AdapterBinNames() {
		reserved[name] = true
	}
	// The sandbox image ships a /usr/local/bin/codex ucode wrapper that
	// internal/install/adapter.go deliberately leaves in place — it symlinks
	// only codex-acp, "so the image's own tooling is left alone". A payload bin
	// named "codex" would shadow a binary this provider deliberately PRESERVES
	// (as opposed to installs), so guard it explicitly.
	//
	// This reserved set is NOT the whole PATH: system binaries the stack
	// resolves by bare name (node, npm, sh, …) also live in dirs after the
	// prepended $HOME/.buzz-backend/bin and are not listed here. Closing that
	// class belongs to #15's install step — it must place extra binaries so an
	// unlisted name cannot shadow a system binary — not to this name check.
	reserved["codex"] = true
	// Reserve the MCP multiplexer's on-disk name (issue #16). A ≥2-entry
	// mcp_servers deploy installs the multiplexer into the same prepended
	// $HOME/.buzz-backend/bin dir, so an extra_binaries bin named MuxBinaryName
	// would shadow it. Reserved on both capability channels — here and in
	// validateMcpServers — so neither can introduce the name.
	reserved[MuxBinaryName] = true

	for i, eb := range bins {
		// url: non-empty, parseable, https only. No unpinned/insecure fetch.
		if eb.URL == "" {
			return fmt.Errorf("provider_config.extra_binaries[%d].url must not be empty", i)
		}
		u, err := url.Parse(eb.URL)
		if err != nil {
			return fmt.Errorf("provider_config.extra_binaries[%d].url is not a parseable URL", i)
		}
		if u.Scheme != "https" {
			return fmt.Errorf(
				"provider_config.extra_binaries[%d].url scheme %q is not \"https\"; only https fetch is allowed so the download cannot be intercepted or served in cleartext",
				i, u.Scheme,
			)
		}
		// Reject a hostless URL (e.g. "https://", "https:///path") at the payload
		// boundary rather than letting it fail with an opaque error at fetch time
		// in #15 — mirroring Agent.Validate()'s u.Host guard for relay_url.
		if u.Host == "" {
			return fmt.Errorf("provider_config.extra_binaries[%d].url %q has no host", i, eb.URL)
		}
		// sha256: required, lowercase 64-hex only.
		if eb.SHA256 == "" {
			return fmt.Errorf("provider_config.extra_binaries[%d].sha256 must not be empty; a pinned checksum is required so the fetched bytes can be verified", i)
		}
		if !sha256HexPattern.MatchString(eb.SHA256) {
			return fmt.Errorf(
				"provider_config.extra_binaries[%d].sha256 must be 64 lowercase hex characters (^[0-9a-f]{64}$); uppercase or mixed case is rejected rather than normalized",
				i,
			)
		}
		// bin: bare filename, no traversal, no option-injection. The checks are
		// ordered from most-specific to most-general so each rejection carries a
		// message that names its actual cause — "." / ".." and a leading "-" all
		// match the charset pattern, so they must be caught before it.
		if eb.Bin == "" {
			return fmt.Errorf("provider_config.extra_binaries[%d].bin must not be empty", i)
		}
		if eb.Bin == "." || eb.Bin == ".." {
			return fmt.Errorf(
				"provider_config.extra_binaries[%d].bin %q is the current- or parent-directory entry, not a filename",
				i, eb.Bin,
			)
		}
		// A leading "-" is a valid filename character but would be parsed as an
		// option flag when #15 later invokes the binary by name; reject it here so
		// the install step never has to defend against argv injection.
		if strings.HasPrefix(eb.Bin, "-") {
			return fmt.Errorf(
				"provider_config.extra_binaries[%d].bin %q must not start with '-'; a leading dash is parsed as a command-line option when the binary is invoked",
				i, eb.Bin,
			)
		}
		if !extraBinaryNamePattern.MatchString(eb.Bin) {
			return fmt.Errorf(
				"provider_config.extra_binaries[%d].bin %q is not a bare filename; only names matching ^[A-Za-z0-9._-]+$ are allowed (which excludes path separators)",
				i, eb.Bin,
			)
		}
		// bin collision: must not shadow a binary this provider installs or
		// deliberately preserves (see the reserved-set construction above); such a
		// name would shadow the real one in the PATH dir launch.sh prepends.
		if reserved[eb.Bin] {
			return fmt.Errorf(
				"provider_config.extra_binaries[%d].bin %q collides with a binary this provider installs or preserves; it would shadow the real one in the PATH dir launch.sh prepends",
				i, eb.Bin,
			)
		}
	}
	return nil
}

// validateMcpServers enforces the STRUCTURAL rules on every
// provider_config.mcp_servers entry, UNCONDITIONALLY — in every mode,
// independent of OwnerPATInSandbox — mirroring validateExtraBinaries. An
// empty entry, a name with a path separator, or a count past the receiver's
// limit is a defect regardless of the inference-auth mode, so these checks are
// not gated on the owner-PAT condition the way validateCapabilityKeysOwnerPAT
// is.
//
// Each entry is a bare command name the agent resolves on PATH (issue #16):
// with one entry the name becomes BUZZ_ACP_MCP_COMMAND directly (mcpDirect,
// #14); with two or more it names a child of the embedded multiplexer
// (mcpMux). The charset, traversal, and leading-dash rules are exactly those
// validateExtraBinaries applies to a bin name, and for the same reason — the
// name is invoked by bare command.
//
// Error text names the field and offending index/value and never leaks a
// secret.
func (r DeployRequest) validateMcpServers() error {
	servers := r.ProviderConfig.McpServers
	if len(servers) == 0 {
		return nil
	}
	// Count cap first, so a wildly oversized list fails on the count rather
	// than on whichever entry happens to trip a per-entry rule.
	if len(servers) > MaxMcpServers {
		return fmt.Errorf(
			"provider_config.mcp_servers has %d entries, more than the maximum of %d (mirrors buzz-agent's MAX_MCP_SERVERS)",
			len(servers), MaxMcpServers,
		)
	}
	// Checks are ordered most-specific to most-general so each rejection
	// carries a message naming its actual cause — "." / ".." and a leading
	// "-" all match the charset pattern, so they must be caught before it.
	seen := make(map[string]bool, len(servers))
	for i, name := range servers {
		if name == "" {
			return fmt.Errorf("provider_config.mcp_servers[%d] must not be empty", i)
		}
		if name == "." || name == ".." {
			return fmt.Errorf(
				"provider_config.mcp_servers[%d] %q is the current- or parent-directory entry, not a server name",
				i, name,
			)
		}
		if strings.HasPrefix(name, "-") {
			return fmt.Errorf(
				"provider_config.mcp_servers[%d] %q must not start with '-'; a leading dash is parsed as a command-line option when the server is invoked",
				i, name,
			)
		}
		if !extraBinaryNamePattern.MatchString(name) {
			return fmt.Errorf(
				"provider_config.mcp_servers[%d] %q is not a bare name; only names matching ^[A-Za-z0-9._-]+$ are allowed (which excludes path separators)",
				i, name,
			)
		}
		// The charset above permits a single "_", but the doubled "__" sequence
		// is buzz-agent's server/tool qualified-name separator: it hard-rejects
		// any server name containing "__" and fails the whole session/new, which
		// deployed as a silently broken agent. Reject it here so the misconfig
		// fails loud at deploy instead. A server name becomes the file stem of
		// BUZZ_ACP_MCP_COMMAND (mcpDirect) or a multiplexer child name (mcpMux),
		// both of which the receiver namespaces with "__".
		if strings.Contains(name, "__") {
			return fmt.Errorf(
				"provider_config.mcp_servers[%d] %q must not contain \"__\"; buzz-agent uses it as the server/tool name separator and rejects the whole session for a name that contains it",
				i, name,
			)
		}
		if name == MuxBinaryName {
			return fmt.Errorf(
				"provider_config.mcp_servers[%d] %q is reserved for this provider's MCP multiplexer and must not be used as a server name",
				i, name,
			)
		}
		if seen[name] {
			return fmt.Errorf(
				"provider_config.mcp_servers[%d] %q is a duplicate; each mcp_servers entry must be unique",
				i, name,
			)
		}
		seen[name] = true
	}
	return nil
}
