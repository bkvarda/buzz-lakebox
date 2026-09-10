// Package mcpops provides transport-independent foundations for discovering
// and probing managed MCP servers. It deliberately does not know how to find a
// Databricks profile, workspace host, or credential; command wiring supplies
// those capabilities through the interfaces in this package.
package mcpops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/redact"
)

// Identifier is a portable managed-MCP resource identifier. Resource contains
// only kind-specific identifier components; the type intentionally has nowhere
// to store a workspace host, URL, profile, token, or literal environment value.
//
// The arities are the same as mcpconfig.Server.Resource. Local servers are not
// discovery resources because they are commands rather than governed
// same-workspace resources.
type Identifier struct {
	Kind     mcpconfig.Kind `json:"kind"`
	Resource []string       `json:"resource,omitempty"`
}

// Clone returns an identifier whose Resource slice does not alias id.
func (id Identifier) Clone() Identifier {
	id.Resource = append([]string(nil), id.Resource...)
	return id
}

// Server converts an identifier to the portable managed config shape. The
// caller provides only the display/routing name; remote authentication remains
// inherited from the eventual runtime environment.
func (id Identifier) Server(name string) mcpconfig.Server {
	return mcpconfig.Server{
		Name:     name,
		Kind:     id.Kind,
		Resource: append([]string(nil), id.Resource...),
		Auth:     mcpconfig.AuthEnv,
	}
}

// Validate checks that id represents one supported discoverable kind with the
// correct portable component arity and syntax.
func (id Identifier) Validate() error {
	if id.Kind == mcpconfig.KindLocal {
		return errors.New("local MCP commands are not discoverable managed resources")
	}
	cfg := mcpconfig.Config{
		Schema:  mcpconfig.CurrentSchema,
		Version: mcpconfig.CurrentVersion,
		Servers: []mcpconfig.Server{id.Server("discovered")},
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return nil
}

// Constructors make the component meaning explicit at call sites. Validation
// still happens in Discover, so an adapter cannot accidentally return malformed
// identifiers merely by constructing the struct directly.
func SQLIdentifier() Identifier { return Identifier{Kind: mcpconfig.KindSQL} }

func GenieIdentifier(space string) Identifier {
	return Identifier{Kind: mcpconfig.KindGenie, Resource: []string{space}}
}

func AISearchIdentifier(catalog, schema, index string) Identifier {
	return Identifier{Kind: mcpconfig.KindAISearch, Resource: []string{catalog, schema, index}}
}

func VectorSearchIdentifier(catalog, schema string) Identifier {
	return Identifier{Kind: mcpconfig.KindVectorSearch, Resource: []string{catalog, schema}}
}

func FunctionsIdentifier(catalog, schema string) Identifier {
	return Identifier{Kind: mcpconfig.KindFunctions, Resource: []string{catalog, schema}}
}

func FunctionIdentifier(catalog, schema, function string) Identifier {
	return Identifier{Kind: mcpconfig.KindFunctions, Resource: []string{catalog, schema, function}}
}

func MCPServiceIdentifier(catalog, schema, service string) Identifier {
	return Identifier{Kind: mcpconfig.KindMCPService, Resource: []string{catalog, schema, service}}
}

// SkillsIdentifier constructs a skills scope from catalog/schema pairs. An
// empty argument list identifies the unscoped utility-tools route.
func SkillsIdentifier(scopes ...[2]string) Identifier {
	resource := make([]string, 0, len(scopes)*2)
	for _, scope := range scopes {
		resource = append(resource, scope[0], scope[1])
	}
	return Identifier{Kind: mcpconfig.KindSkills, Resource: resource}
}

// Discoverer is implemented by future Databricks CLI/API adapters and by
// hermetic test fakes. Implementations return only portable identifiers; auth
// and workspace selection stay inside the adapter and never enter the result.
type Discoverer interface {
	Discover(context.Context) ([]Identifier, error)
}

// Discover obtains, validates, deduplicates, defensively copies, and
// deterministically sorts portable identifiers from an injected adapter.
func Discover(ctx context.Context, discoverer Discoverer) ([]Identifier, error) {
	if discoverer == nil {
		return nil, errors.New("managed MCP discoverer is required")
	}
	ids, err := discoverer.Discover(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover managed MCP resources: %s", RedactError(err))
	}

	out := make([]Identifier, 0, len(ids))
	seen := make(map[string]int, len(ids))
	for i, original := range ids {
		id := original.Clone()
		if err := id.Validate(); err != nil {
			return nil, fmt.Errorf("discover managed MCP resources: result[%d]: %s", i, RedactError(err))
		}
		key := identifierKey(id)
		if prior, ok := seen[key]; ok {
			return nil, fmt.Errorf("discover managed MCP resources: result[%d] duplicates result[%d]", i, prior)
		}
		seen[key] = i
		out = append(out, id)
	}

	sort.Slice(out, func(i, j int) bool {
		return identifierKey(out[i]) < identifierKey(out[j])
	})
	return out, nil
}

func identifierKey(id Identifier) string {
	return string(id.Kind) + "\x00" + strings.Join(id.Resource, "\x00")
}

// RedactError scrubs credential-shaped material from adapter/prober errors.
// Adapters that possess a credential should pass its exact value in secrets;
// Probe and Discover additionally call this with no known values as a final
// defense for standard token/key assignment and prefix shapes.
func RedactError(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	scrubbed := redact.Redact(err.Error(), secrets)
	return redactPortableIdentifiers(redact.Log(scrubbed))
}

// redactPortableIdentifiers removes common opaque Databricks resource IDs
// from diagnostics. Portable configured names remain useful, but workspace,
// warehouse, Genie-space, and UUID identifiers must not escape through an
// adapter's raw API error.
func redactPortableIdentifiers(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\r', '\n', '"', '\'', '(', ')', '[', ']', '{', '}', ',', ';':
			return true
		default:
			return false
		}
	})
	for _, field := range fields {
		candidate := strings.Trim(field, ".:!?<>")
		lower := strings.ToLower(candidate)
		if strings.HasPrefix(lower, "ws-") || strings.HasPrefix(lower, "wh-") ||
			strings.HasPrefix(lower, "space-") || looksLikeUUID(candidate) {
			s = strings.ReplaceAll(s, candidate, redact.Placeholder)
		}
	}
	return s
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
