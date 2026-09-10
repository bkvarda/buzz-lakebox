package httpmcp

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Kind string

const (
	KindSQL          Kind = "sql"
	KindGenie        Kind = "genie"
	KindAISearch     Kind = "ai-search"
	KindVectorSearch Kind = "vector-search"
	KindFunctions    Kind = "functions"
	KindMCPService   Kind = "mcp-service"
	KindSkills       Kind = "skills"
)

// Options identifies one documented Databricks managed MCP endpoint.
type Options struct {
	Kind      Kind
	Resources []string
	Schemas   []string
}

var resourceRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,254}$`)

// ValidateWorkspaceHost verifies that host is a secure workspace authority.
func ValidateWorkspaceHost(host string) error {
	_, err := parseWorkspaceHost(host)
	return err
}

// BuildEndpoint constructs only documented Databricks MCP endpoints. The host
// is an authority, never a base path: a supplied path, query, fragment, userinfo,
// or non-HTTPS scheme is rejected rather than joined into the endpoint.
func BuildEndpoint(host string, opts Options) (*url.URL, error) {
	base, err := parseWorkspaceHost(host)
	if err != nil {
		return nil, err
	}
	if err := validateTypedEndpoint(opts); err != nil {
		return nil, err
	}

	components := make([]string, len(opts.Resources))
	for i, resource := range opts.Resources {
		components[i] = url.PathEscape(resource)
	}

	switch opts.Kind {
	case KindSQL:
		base.Path = "/api/2.0/mcp/sql"
	case KindGenie:
		base.Path = "/api/2.0/mcp/genie/" + components[0]
	case KindAISearch:
		base.Path = "/api/2.0/mcp/ai-search/" + strings.Join(components, "/")
	case KindVectorSearch:
		base.Path = "/api/2.0/mcp/vector-search/" + strings.Join(components, "/")
	case KindFunctions:
		base.Path = "/api/2.0/mcp/functions/" + strings.Join(components, "/")
	case KindMCPService:
		base.Path = "/ai-gateway/mcp-services/" + strings.Join(components, ".")
	case KindSkills:
		base.Path = "/ai-gateway/skills/"
		query := url.Values{}
		for _, schema := range opts.Schemas {
			query.Add("schema", schema)
		}
		base.RawQuery = query.Encode()
	}
	return base, nil
}

func parseWorkspaceHost(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("DATABRICKS_HOST is not set")
	}
	if err := validateText(raw); err != nil {
		return nil, fmt.Errorf("DATABRICKS_HOST is invalid")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("DATABRICKS_HOST is invalid")
	}
	if u.User != nil || u.Host == "" || u.Hostname() == "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("DATABRICKS_HOST must contain only a workspace scheme and host")
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("DATABRICKS_HOST must use HTTPS")
	}
	if strings.Contains(u.Hostname(), "%") || net.ParseIP(u.Hostname()) == nil && !validDNSName(u.Hostname()) {
		return nil, fmt.Errorf("DATABRICKS_HOST has an invalid hostname")
	}
	return u, nil
}

func validateTypedEndpoint(opts Options) error {
	var arity int
	switch opts.Kind {
	case KindSQL:
		arity = 0
	case KindGenie:
		arity = 1
	case KindAISearch:
		arity = 3
	case KindVectorSearch:
		arity = 2
	case KindFunctions:
		if len(opts.Resources) != 2 && len(opts.Resources) != 3 {
			return fmt.Errorf("kind %q requires 2 or 3 --resource value(s), got %d", opts.Kind, len(opts.Resources))
		}
		arity = len(opts.Resources)
	case KindMCPService:
		arity = 3
	case KindSkills:
		arity = 0
	default:
		return fmt.Errorf("unsupported endpoint kind %q", opts.Kind)
	}
	if len(opts.Resources) != arity {
		return fmt.Errorf("kind %q requires %d --resource value(s), got %d", opts.Kind, arity, len(opts.Resources))
	}
	if opts.Kind != KindSkills && len(opts.Schemas) != 0 {
		return fmt.Errorf("--schema is valid only for kind %q", KindSkills)
	}
	for i, component := range opts.Resources {
		if err := validateIdentifier(component); err != nil {
			return fmt.Errorf("resource %d is invalid: %w", i+1, err)
		}
	}
	for i, schema := range opts.Schemas {
		parts := strings.Split(schema, ".")
		if len(parts) != 2 {
			return fmt.Errorf("schema %d must be catalog.schema", i+1)
		}
		for _, part := range parts {
			if err := validateIdentifier(part); err != nil {
				return fmt.Errorf("schema %d is invalid: %w", i+1, err)
			}
		}
	}
	return nil
}

func validateIdentifier(value string) error {
	if err := validateText(value); err != nil {
		return err
	}
	if !resourceRE.MatchString(value) || value == "_" || value == "." || value == ".." {
		return fmt.Errorf("must be a safe identifier")
	}
	if strings.Contains(value, "..") || strings.ContainsAny(value, `/\\?#%`) || strings.Contains(strings.ToLower(value), "%2e") {
		return fmt.Errorf("must not contain a URL, separator, escape, or path traversal")
	}
	return nil
}

func validateText(value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("must be valid UTF-8")
	}
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) {
			return fmt.Errorf("must not contain control characters")
		}
	}
	return nil
}

func validDNSName(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
