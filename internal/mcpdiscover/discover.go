// Package mcpdiscover implements read-only managed-MCP discovery through the
// Databricks CLI. Workspace selection is always explicit, while returned
// values use mcpops' portable identifiers and therefore carry no connection or
// authentication material.
package mcpdiscover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpops"
)

// Runner executes one Databricks CLI invocation. Implementations must keep
// stdout and stderr separate: stdout is machine-readable JSON, while stderr is
// used only for a redacted failure diagnostic.
type Runner interface {
	Run(context.Context, ...string) (stdout, stderr []byte, err error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(context.Context, ...string) ([]byte, []byte, error)

func (f RunnerFunc) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return f(ctx, args...)
}

// ExecRunner runs a configured executable without involving a shell.
type ExecRunner struct {
	Path string
}

// NewExecRunner constructs a real command runner. An empty path selects the
// conventional "databricks" executable.
func NewExecRunner(path string) *ExecRunner {
	if path == "" {
		path = "databricks"
	}
	return &ExecRunner{Path: path}
}

func (r *ExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	path := "databricks"
	if r != nil && r.Path != "" {
		path = r.Path
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Scope bounds catalog-backed discovery. Functions, Skills, and MCP Services
// are never discovered outside the scopes explicitly supplied by the caller.
type Scope struct {
	Catalog string
	Schema  string
}

// Option configures an Adapter.
type Option func(*adapterOptions) error

type adapterOptions struct {
	scopes []Scope
	kinds  map[mcpconfig.Kind]bool
}

// WithScopes supplies the only catalog/schema scopes the adapter may inspect.
func WithScopes(scopes ...Scope) Option {
	return func(o *adapterOptions) error {
		o.scopes = append([]Scope(nil), scopes...)
		return nil
	}
}

// WithKinds limits discovery to the requested kinds. WithKinds with no values
// requests no routes. Without this option all routes supported by this adapter
// are enabled; scoped routes still require WithScopes.
func WithKinds(kinds ...mcpconfig.Kind) Option {
	return func(o *adapterOptions) error {
		o.kinds = make(map[mcpconfig.Kind]bool, len(kinds))
		for _, kind := range kinds {
			if !supportedKind(kind) {
				return fmt.Errorf("unsupported discovery kind %q", kind)
			}
			o.kinds[kind] = true
		}
		return nil
	}
}

// Adapter is a read-only mcpops.Discoverer backed by the Databricks CLI.
type Adapter struct {
	profile string
	runner  Runner
	scopes  []Scope
	kinds   map[mcpconfig.Kind]bool
}

var _ mcpops.Discoverer = (*Adapter)(nil)

// New constructs an adapter using an injected command runner.
func New(profile string, runner Runner, options ...Option) (*Adapter, error) {
	if strings.TrimSpace(profile) == "" {
		return nil, errors.New("databricks profile is required")
	}
	if hasControl(profile) {
		return nil, errors.New("databricks profile contains control characters")
	}
	if runner == nil {
		return nil, errors.New("databricks CLI runner is required")
	}

	opts := adapterOptions{}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil discovery option")
		}
		if err := option(&opts); err != nil {
			return nil, err
		}
	}
	if opts.kinds == nil {
		opts.kinds = allKinds()
	}

	seen := make(map[string]struct{}, len(opts.scopes))
	scopes := make([]Scope, 0, len(opts.scopes))
	for _, scope := range opts.scopes {
		if err := mcpops.FunctionsIdentifier(scope.Catalog, scope.Schema).Validate(); err != nil {
			return nil, errors.New("invalid discovery scope: catalog and schema must be safe identifier segments")
		}
		key := scope.Catalog + "\x00" + scope.Schema
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Catalog != scopes[j].Catalog {
			return scopes[i].Catalog < scopes[j].Catalog
		}
		return scopes[i].Schema < scopes[j].Schema
	})

	return &Adapter{
		profile: profile,
		runner:  runner,
		scopes:  scopes,
		kinds:   opts.kinds,
	}, nil
}

// NewExec constructs an adapter that invokes the real databricks executable.
func NewExec(profile string, options ...Option) (*Adapter, error) {
	return New(profile, NewExecRunner("databricks"), options...)
}

func supportedKind(kind mcpconfig.Kind) bool {
	switch kind {
	case mcpconfig.KindSQL, mcpconfig.KindGenie, mcpconfig.KindAISearch,
		mcpconfig.KindFunctions, mcpconfig.KindSkills, mcpconfig.KindMCPService:
		return true
	default:
		return false
	}
}

func allKinds() map[mcpconfig.Kind]bool {
	return map[mcpconfig.Kind]bool{
		mcpconfig.KindSQL:        true,
		mcpconfig.KindGenie:      true,
		mcpconfig.KindAISearch:   true,
		mcpconfig.KindFunctions:  true,
		mcpconfig.KindSkills:     true,
		mcpconfig.KindMCPService: true,
	}
}

// Discover performs only list operations. Any failed list makes discovery fail
// closed rather than presenting an incomplete result as authoritative.
func (a *Adapter) Discover(ctx context.Context) ([]mcpops.Identifier, error) {
	if a == nil || a.runner == nil {
		return nil, errors.New("databricks CLI discovery adapter is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var ids []mcpops.Identifier
	if a.kinds[mcpconfig.KindSQL] {
		// SQL is the one built-in route: it has no resource-list API.
		ids = append(ids, mcpops.SQLIdentifier())
	}
	if a.kinds[mcpconfig.KindGenie] {
		found, err := a.discoverGenie(ctx)
		if err != nil {
			return nil, err
		}
		ids = append(ids, found...)
	}
	if a.kinds[mcpconfig.KindAISearch] {
		found, err := a.discoverAISearch(ctx)
		if err != nil {
			return nil, err
		}
		ids = append(ids, found...)
	}
	if a.kinds[mcpconfig.KindFunctions] || a.kinds[mcpconfig.KindSkills] {
		found, skillScopes, err := a.discoverFunctions(ctx)
		if err != nil {
			return nil, err
		}
		if a.kinds[mcpconfig.KindFunctions] {
			ids = append(ids, found...)
		}
		if a.kinds[mcpconfig.KindSkills] && len(skillScopes) > 0 {
			pairs := make([][2]string, 0, len(skillScopes))
			for _, scope := range skillScopes {
				pairs = append(pairs, [2]string{scope.Catalog, scope.Schema})
			}
			ids = append(ids, mcpops.SkillsIdentifier(pairs...))
		}
	}
	if a.kinds[mcpconfig.KindMCPService] {
		found, err := a.discoverMCPServices(ctx)
		if err != nil {
			return nil, err
		}
		ids = append(ids, found...)
	}

	return normalize(ids, a.profile)
}

func (a *Adapter) discoverGenie(ctx context.Context) ([]mcpops.Identifier, error) {
	rows, err := a.listPages(ctx, "list Genie spaces", []string{"genie", "list-spaces"}, []string{"spaces"}, true)
	if err != nil {
		return nil, err
	}
	ids := make([]mcpops.Identifier, 0, len(rows))
	for _, row := range rows {
		id, ok := firstString(row, "space_id", "spaceId", "id")
		if !ok {
			return nil, errors.New("list Genie spaces: unsupported JSON item shape (missing space identifier)")
		}
		ids = append(ids, mcpops.GenieIdentifier(id))
	}
	return ids, nil
}

func (a *Adapter) discoverAISearch(ctx context.Context) ([]mcpops.Identifier, error) {
	endpoints, err := a.listPages(ctx, "list AI Search endpoints",
		[]string{"vector-search-endpoints", "list-endpoints"},
		[]string{"endpoints", "vector_search_endpoints"}, false)
	if err != nil {
		return nil, err
	}

	var ids []mcpops.Identifier
	for _, endpoint := range endpoints {
		name, ok := firstString(endpoint, "name", "endpoint_name", "endpointName")
		if !ok {
			return nil, errors.New("list AI Search endpoints: unsupported JSON item shape (missing endpoint name)")
		}
		rows, err := a.listPages(ctx, "list AI Search indexes",
			[]string{"vector-search-indexes", "list-indexes", name},
			[]string{"vector_indexes", "indexes"}, false)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			fullName, ok := firstString(row, "name", "index_name", "full_name", "fullName")
			if !ok {
				return nil, errors.New("list AI Search indexes: unsupported JSON item shape (missing index name)")
			}
			parts := strings.Split(fullName, ".")
			if len(parts) != 3 {
				return nil, errors.New("list AI Search indexes: unsupported index name shape (expected catalog.schema.index)")
			}
			ids = append(ids, mcpops.AISearchIdentifier(parts[0], parts[1], parts[2]))
		}
	}
	return ids, nil
}

func (a *Adapter) discoverFunctions(ctx context.Context) ([]mcpops.Identifier, []Scope, error) {
	var ids []mcpops.Identifier
	listable := make([]Scope, 0, len(a.scopes))
	for _, scope := range a.scopes {
		rows, err := a.listPages(ctx, "list functions",
			[]string{"functions", "list", scope.Catalog, scope.Schema},
			[]string{"functions"}, false)
		if err != nil {
			return nil, nil, err
		}
		listable = append(listable, scope)
		ids = append(ids, mcpops.FunctionsIdentifier(scope.Catalog, scope.Schema))
		for _, row := range rows {
			name, ok := firstString(row, "full_name", "fullName", "name")
			if !ok {
				return nil, nil, errors.New("list functions: unsupported JSON item shape (missing function name)")
			}
			function, ok := scopedName(name, scope)
			if !ok {
				return nil, nil, errors.New("list functions: function is outside the requested catalog/schema scope")
			}
			ids = append(ids, mcpops.FunctionIdentifier(scope.Catalog, scope.Schema, function))
		}
	}
	return ids, listable, nil
}

func (a *Adapter) discoverMCPServices(ctx context.Context) ([]mcpops.Identifier, error) {
	var ids []mcpops.Identifier
	for _, scope := range a.scopes {
		parent := "schemas/" + scope.Catalog + "." + scope.Schema
		rows, err := a.listPages(ctx, "list MCP services",
			[]string{"ai-gateway", "list-mcp-services", "--parent", parent},
			[]string{"mcp_services", "services"}, false)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			name, ok := firstString(row, "full_name", "fullName", "name", "service_name", "serviceName")
			if !ok {
				return nil, errors.New("list MCP services: unsupported JSON item shape (missing service name)")
			}
			service, ok := scopedName(name, scope)
			if !ok {
				return nil, errors.New("list MCP services: service is outside the requested catalog/schema scope")
			}
			ids = append(ids, mcpops.MCPServiceIdentifier(scope.Catalog, scope.Schema, service))
		}
	}
	return ids, nil
}

func scopedName(name string, scope Scope) (string, bool) {
	// AI Gateway list responses use an AIP resource name such as
	// `mcp-services/catalog.schema.service`; UC function APIs use either the
	// bare object name or `catalog.schema.object`.
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	parts := strings.Split(name, ".")
	switch len(parts) {
	case 1:
		return parts[0], parts[0] != ""
	case 3:
		if parts[0] == scope.Catalog && parts[1] == scope.Schema && parts[2] != "" {
			return parts[2], true
		}
	}
	return "", false
}

const maxPages = 10000

func (a *Adapter) listPages(ctx context.Context, action string, baseArgs, collectionKeys []string, supportsPageToken bool) ([]map[string]any, error) {
	var all []map[string]any
	seenTokens := make(map[string]struct{})
	token := ""
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= maxPages {
			return nil, fmt.Errorf("%s: pagination exceeded safety limit", action)
		}
		args := append([]string(nil), baseArgs...)
		args = append(args, "--output", "json", "--profile", a.profile)
		if token != "" {
			args = append(args, "--page-token", token)
		}
		stdout, stderr, runErr := a.runner.Run(ctx, args...)
		if runErr != nil {
			return nil, a.commandError(action, stderr, runErr)
		}
		rows, next, err := decodePage(stdout, collectionKeys)
		if err != nil {
			return nil, fmt.Errorf("%s: unsupported JSON response shape", action)
		}
		all = append(all, rows...)
		if next == "" {
			return all, nil
		}
		if !supportsPageToken {
			return nil, fmt.Errorf("%s: response requires pagination but this CLI route exposes no page-token flag", action)
		}
		if _, exists := seenTokens[next]; exists {
			return nil, fmt.Errorf("%s: repeated pagination token", action)
		}
		seenTokens[next] = struct{}{}
		token = next
	}
}

func decodePage(data []byte, collectionKeys []string) ([]map[string]any, string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, "", err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, "", errors.New("multiple JSON values")
		}
		return nil, "", err
	}

	switch value := root.(type) {
	case []any:
		rows, err := objectRows(value)
		return rows, "", err
	case map[string]any:
		var rawRows any
		found := false
		for _, key := range collectionKeys {
			if candidate, ok := value[key]; ok {
				rawRows, found = candidate, true
				break
			}
		}
		if !found {
			// Several Databricks list APIs render an empty result as `{}` rather
			// than `{<collection>:[]}`. With no unknown keys and no pagination
			// token this is an unambiguous empty page, not shape drift.
			if len(value) == 0 {
				return []map[string]any{}, "", nil
			}
			return nil, "", errors.New("collection key missing")
		}
		if rawRows == nil {
			rawRows = []any{}
		}
		items, ok := rawRows.([]any)
		if !ok {
			return nil, "", errors.New("collection is not an array")
		}
		rows, err := objectRows(items)
		if err != nil {
			return nil, "", err
		}
		next, _ := firstString(value, "next_page_token", "nextPageToken")
		return rows, next, nil
	default:
		return nil, "", errors.New("root is not an object or array")
	}
}

func objectRows(items []any) ([]map[string]any, error) {
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("collection item is not an object")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func firstString(row map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		value, ok := row[key]
		if !ok {
			continue
		}
		text, ok := value.(string)
		if ok && text != "" {
			return text, true
		}
	}
	return "", false
}

func normalize(ids []mcpops.Identifier, profile string) ([]mcpops.Identifier, error) {
	seen := make(map[string]struct{}, len(ids))
	out := make([]mcpops.Identifier, 0, len(ids))
	for _, id := range ids {
		if err := id.Validate(); err != nil {
			return nil, fmt.Errorf("invalid discovered resource: %s", mcpops.RedactError(err, profile))
		}
		key := string(id.Kind) + "\x00" + strings.Join(id.Resource, "\x00")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		left := string(out[i].Kind) + "\x00" + strings.Join(out[i].Resource, "\x00")
		right := string(out[j].Kind) + "\x00" + strings.Join(out[j].Resource, "\x00")
		return left < right
	})
	return out, nil
}

var urlPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

func (a *Adapter) commandError(action string, stderr []byte, runErr error) error {
	diagnostic := strings.TrimSpace(string(stderr))
	if diagnostic == "" && runErr != nil {
		diagnostic = runErr.Error()
	}
	diagnostic = urlPattern.ReplaceAllString(diagnostic, "[REDACTED]")
	diagnostic = mcpops.RedactError(errors.New(diagnostic), a.profile)
	diagnostic = strings.Join(strings.Fields(diagnostic), " ")
	if len(diagnostic) > 512 {
		diagnostic = diagnostic[:512] + "..."
	}
	if diagnostic == "" {
		diagnostic = "command failed"
	}
	return fmt.Errorf("%s: Databricks CLI failed: %s", action, diagnostic)
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
