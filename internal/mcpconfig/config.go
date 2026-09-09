// Package mcpconfig defines the portable, versioned configuration used for
// managed MCP servers.  It intentionally contains resource names and inherited
// environment-variable names, but never hosts, bearer tokens, profile names,
// or literal environment values.
package mcpconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

const (
	// CurrentSchema identifies this configuration family.  It is deliberately
	// not a URL: a managed config must not provide an alternate fetch location.
	CurrentSchema = "buzz-managed-mcp"
	// CurrentVersion is the only version understood by this package.
	CurrentVersion = 1
	// MaxServers mirrors the agent and bzmux server-count limit.
	MaxServers = 16
)

// Kind selects how a server is launched. Local is a normal stdio command; all
// other kinds are same-workspace Databricks Streamable HTTP endpoints reached
// through a caller-supplied stdio bridge.
type Kind string

const (
	KindLocal        Kind = "local"
	KindSQL          Kind = "sql"
	KindGenie        Kind = "genie"
	KindAISearch     Kind = "ai-search"
	KindVectorSearch Kind = "vector-search" // accepted spelling of ai-search
	KindFunctions    Kind = "functions"
	KindMCPService   Kind = "mcp-service"
	KindSkills       Kind = "skills"
)

// Auth is the credential source used by a remote bridge.  It says where the
// bridge obtains credentials; it never carries a credential or a profile name.
type Auth string

const (
	AuthEnv            Auth = "env"
	AuthSandboxProfile Auth = "sandbox-profile"
)

// Config is the top-level managed MCP document.
type Config struct {
	Schema  string   `json:"schema"`
	Version int      `json:"version"`
	Servers []Server `json:"servers"`
}

// Server is one managed MCP server. Resource contains individual identifier
// components rather than a URL. Its required arity is kind-specific:
//
//   - sql: none
//   - genie: one space identifier
//   - ai-search: catalog, schema, index
//   - vector-search: catalog, schema
//   - functions: catalog, schema, and optionally one function
//   - mcp-service: catalog, schema, service
//   - skills: either none (utility tools) or one or more catalog/schema pairs
//
// Command and Args belong only to local servers. InheritEnv is the complete
// allowlist bzmux may copy from its environment; literal values are impossible
// to represent in this schema.
type Server struct {
	Name       string   `json:"name"`
	Kind       Kind     `json:"kind"`
	Command    string   `json:"command,omitempty"`
	Resource   []string `json:"resource,omitempty"`
	Args       []string `json:"args,omitempty"`
	InheritEnv []string `json:"inherit_env,omitempty"`
	Auth       Auth     `json:"auth,omitempty"`
}

// Remote describes the validated, same-workspace endpoint passed to a bridge
// argument builder. Components is a defensive copy.
type Remote struct {
	Kind Kind
	// Endpoint is a same-host relative endpoint. It never contains a scheme or
	// authority, so the builder cannot accidentally honor a managed custom host.
	Endpoint   string
	Components []string
	Auth       Auth
}

// RemoteArgsBuilder converts a typed remote endpoint into bridge arguments.
// Host selection remains outside the managed document: the caller should bind
// its current workspace host while constructing the returned arguments.
type RemoteArgsBuilder func(Remote) ([]string, error)

// Options supplies the deployment-specific half of remote conversion. Local
// commands need no options and retain their existing muxcfg representation.
type Options struct {
	RemoteCommand string
	RemoteArgs    RemoteArgsBuilder
}

var (
	serverNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	commandRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	resourceRE   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,254}$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretArgRE  = regexp.MustCompile(`(?i)(^|[-_])(token|secret|password|passwd|credential|api[-_]?key|private[-_]?key)([-_=:]|$)`)
	windowsEnvRE = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)
)

// Parse decodes one complete JSON document and validates it. Unknown fields,
// duplicate JSON documents, and non-current schema versions are rejected.
func Parse(data []byte) (Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode managed MCP config: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("decode managed MCP config: multiple JSON values")
		}
		return Config{}, fmt.Errorf("decode managed MCP config: trailing data: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces the v1 managed-config contract without consulting process
// state, the filesystem, or the network.
func (c Config) Validate() error {
	if c.Schema != CurrentSchema {
		return fmt.Errorf("schema must be %q, got %q", CurrentSchema, c.Schema)
	}
	if c.Version != CurrentVersion {
		return fmt.Errorf("version must be %d, got %d", CurrentVersion, c.Version)
	}
	if len(c.Servers) > MaxServers {
		return fmt.Errorf("servers has %d entries, more than the maximum of %d", len(c.Servers), MaxServers)
	}

	seen := make(map[string]int, len(c.Servers))
	for i, server := range c.Servers {
		path := fmt.Sprintf("servers[%d]", i)
		if err := validateServer(path, server); err != nil {
			return err
		}
		folded := strings.ToLower(server.Name)
		if prior, ok := seen[folded]; ok {
			return fmt.Errorf("%s.name %q duplicates servers[%d].name; names are case-insensitively unique", path, server.Name, prior)
		}
		seen[folded] = i
	}
	return nil
}

func validateServer(path string, s Server) error {
	if !serverNameRE.MatchString(s.Name) {
		return fmt.Errorf("%s.name %q is not safe; use 1-64 characters matching ^[A-Za-z0-9][A-Za-z0-9._-]*$", path, s.Name)
	}
	if strings.Contains(s.Name, "__") {
		return fmt.Errorf("%s.name %q must not contain \"__\"; it is the server/tool separator", path, s.Name)
	}
	if strings.EqualFold(s.Name, "bzmux") {
		return fmt.Errorf("%s.name %q collides with the reserved bzmux multiplexer name", path, s.Name)
	}

	switch s.Kind {
	case KindLocal:
		if !commandRE.MatchString(s.Command) || s.Command == "." || s.Command == ".." {
			return fmt.Errorf("%s.command %q must be a safe bare command name", path, s.Command)
		}
		if strings.EqualFold(s.Command, "bzmux") {
			return fmt.Errorf("%s.command %q collides with the reserved bzmux multiplexer", path, s.Command)
		}
		if len(s.Resource) != 0 {
			return fmt.Errorf("%s.resource is only valid for a Databricks remote kind", path)
		}
		if s.Auth != "" {
			return fmt.Errorf("%s.auth is only valid for a Databricks remote kind", path)
		}
	case KindSQL, KindGenie, KindAISearch, KindVectorSearch, KindFunctions, KindMCPService, KindSkills:
		if s.Command != "" {
			return fmt.Errorf("%s.command must be omitted for remote kind %q", path, s.Kind)
		}
		if len(s.Args) != 0 {
			return fmt.Errorf("%s.args must be omitted for remote kind %q; bridge arguments are derived", path, s.Kind)
		}
		if s.Auth != AuthEnv {
			return fmt.Errorf("%s.auth must be %q for remote kind %q; sandbox-profile auth is reserved until a least-privilege refresh helper is available", path, AuthEnv, s.Kind)
		}
		if err := validateResource(path, s.Kind, s.Resource); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s.kind %q is unsupported", path, s.Kind)
	}

	for j, arg := range s.Args {
		if err := validateArg(arg); err != nil {
			return fmt.Errorf("%s.args[%d]: %w", path, j, err)
		}
	}
	seenEnv := make(map[string]int, len(s.InheritEnv))
	for j, name := range s.InheritEnv {
		if !envNameRE.MatchString(name) {
			return fmt.Errorf("%s.inherit_env[%d] %q is not an environment variable name", path, j, name)
		}
		if prior, ok := seenEnv[name]; ok {
			return fmt.Errorf("%s.inherit_env[%d] %q duplicates inherit_env[%d]", path, j, name, prior)
		}
		seenEnv[name] = j
	}
	return nil
}

func validateResource(path string, kind Kind, components []string) error {
	validArity := false
	expected := ""
	switch kind {
	case KindSQL:
		validArity, expected = len(components) == 0, "no components"
	case KindGenie:
		validArity, expected = len(components) == 1, "one space identifier"
	case KindAISearch:
		validArity, expected = len(components) == 3, "catalog, schema, and index"
	case KindVectorSearch:
		validArity, expected = len(components) == 2, "catalog and schema"
	case KindFunctions:
		validArity, expected = len(components) == 2 || len(components) == 3, "catalog and schema, optionally followed by a function"
	case KindMCPService:
		validArity, expected = len(components) == 3, "catalog, schema, and service"
	case KindSkills:
		validArity, expected = len(components)%2 == 0, "no components or catalog/schema pairs"
	}
	if !validArity {
		return fmt.Errorf("%s.resource for kind %q must contain %s; got %d components", path, kind, expected, len(components))
	}
	for i, component := range components {
		if !resourceRE.MatchString(component) || component == "_" {
			return fmt.Errorf("%s.resource[%d] %q is not a safe identifier segment", path, i, component)
		}
		if strings.Contains(component, "__") {
			return fmt.Errorf("%s.resource[%d] %q must not contain \"__\"", path, i, component)
		}
	}
	return nil
}

func remoteEndpoint(kind Kind, components []string) string {
	switch kind {
	case KindSQL:
		return "/api/2.0/mcp/sql"
	case KindGenie:
		return "/api/2.0/mcp/genie/" + components[0]
	case KindAISearch:
		return "/api/2.0/mcp/ai-search/" + strings.Join(components, "/")
	case KindVectorSearch:
		return "/api/2.0/mcp/vector-search/" + strings.Join(components, "/")
	case KindFunctions:
		return "/api/2.0/mcp/functions/" + strings.Join(components, "/")
	case KindMCPService:
		return "/ai-gateway/mcp-services/" + strings.Join(components, ".")
	case KindSkills:
		if len(components) == 0 {
			return "/ai-gateway/skills/"
		}
		pairs := make([]string, 0, len(components)/2)
		for i := 0; i < len(components); i += 2 {
			pairs = append(pairs, strings.Join(components[i:i+2], "."))
		}
		return "/ai-gateway/skills/?schema=" + strings.Join(pairs, "&schema=")
	default:
		panic("remoteEndpoint called with non-remote kind")
	}
}

func validateArg(arg string) error {
	if !utf8.ValidString(arg) {
		return fmt.Errorf("must be valid UTF-8")
	}
	for _, r := range arg {
		if r == 0 || unicode.IsControl(r) {
			return fmt.Errorf("must not contain NUL or control characters")
		}
	}
	lower := strings.ToLower(arg)
	if strings.Contains(lower, "://") {
		return fmt.Errorf("must not contain a URL; v1 remote endpoints are derived on the current workspace")
	}
	if strings.Contains(arg, "${") || strings.Contains(arg, "$(") || windowsEnvRE.MatchString(arg) {
		return fmt.Errorf("must not contain environment-specific expansion")
	}
	if secretArgRE.MatchString(arg) || strings.HasPrefix(lower, "bearer ") || strings.HasPrefix(lower, "dapi") {
		return fmt.Errorf("must not contain a literal secret; inherit an environment variable instead")
	}
	return nil
}

// ToMuxConfig validates cfg and converts it to the frozen bzmux format.
// Server order is preserved. Slices are copied and inherited variable names are
// sorted so equivalent allowlists render identically.
func ToMuxConfig(cfg Config, opts Options) (muxcfg.Config, error) {
	if err := cfg.Validate(); err != nil {
		return muxcfg.Config{}, err
	}

	needsRemote := false
	for _, server := range cfg.Servers {
		if server.Kind != KindLocal {
			needsRemote = true
			break
		}
	}
	if needsRemote {
		if !commandRE.MatchString(opts.RemoteCommand) || opts.RemoteCommand == "." || opts.RemoteCommand == ".." {
			return muxcfg.Config{}, fmt.Errorf("remote bridge command %q must be a safe bare command name", opts.RemoteCommand)
		}
		if strings.EqualFold(opts.RemoteCommand, "bzmux") {
			return muxcfg.Config{}, fmt.Errorf("remote bridge command %q collides with bzmux", opts.RemoteCommand)
		}
		if opts.RemoteArgs == nil {
			return muxcfg.Config{}, fmt.Errorf("remote bridge argument builder is required")
		}
	}

	out := muxcfg.Config{Servers: make([]muxcfg.Server, 0, len(cfg.Servers))}
	for i, server := range cfg.Servers {
		inherit := append([]string(nil), server.InheritEnv...)
		sort.Strings(inherit)
		converted := muxcfg.Server{
			Name: server.Name,
			Env:  muxcfg.ServerEnv{Inherit: inherit},
		}
		if server.Kind == KindLocal {
			converted.Command = server.Command
			converted.Args = append([]string(nil), server.Args...)
		} else {
			remote := Remote{
				Kind:       server.Kind,
				Endpoint:   remoteEndpoint(server.Kind, server.Resource),
				Components: append([]string(nil), server.Resource...),
				Auth:       server.Auth,
			}
			args, err := opts.RemoteArgs(remote)
			if err != nil {
				return muxcfg.Config{}, fmt.Errorf("servers[%d] %q: build remote bridge arguments: %w", i, server.Name, err)
			}
			for j, arg := range args {
				if !utf8.ValidString(arg) || strings.IndexByte(arg, 0) >= 0 {
					return muxcfg.Config{}, fmt.Errorf("servers[%d] %q: remote bridge args[%d] contains invalid UTF-8 or NUL", i, server.Name, j)
				}
			}
			converted.Command = opts.RemoteCommand
			converted.Args = append([]string(nil), args...)
		}
		out.Servers = append(out.Servers, converted)
	}
	return out, nil
}

// MuxConfig is the method form of ToMuxConfig.
func (c Config) MuxConfig(opts Options) (muxcfg.Config, error) {
	return ToMuxConfig(c, opts)
}
