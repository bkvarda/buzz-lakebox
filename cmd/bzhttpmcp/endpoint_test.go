package main

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildEndpointKinds(t *testing.T) {
	t.Parallel()
	const host = "https://workspace.example.test"
	tests := []struct {
		name      string
		kind      endpointKind
		resources []string
		schemas   []string
		want      string
	}{
		{"sql", kindSQL, nil, nil, host + "/api/2.0/mcp/sql"},
		{"genie", kindGenie, []string{"space_123"}, nil, host + "/api/2.0/mcp/genie/space_123"},
		{"ai search", kindAISearch, []string{"catalog_a", "schema_b", "index_c"}, nil, host + "/api/2.0/mcp/ai-search/catalog_a/schema_b/index_c"},
		{"vector search", kindVectorSearch, []string{"catalog_a", "schema_b"}, nil, host + "/api/2.0/mcp/vector-search/catalog_a/schema_b"},
		{"functions schema", kindFunctions, []string{"catalog_a", "schema_b"}, nil, host + "/api/2.0/mcp/functions/catalog_a/schema_b"},
		{"single function", kindFunctions, []string{"catalog_a", "schema_b", "function_c"}, nil, host + "/api/2.0/mcp/functions/catalog_a/schema_b/function_c"},
		{"MCP service", kindMCPService, []string{"catalog_a", "schema_b", "service_c"}, nil, host + "/ai-gateway/mcp-services/catalog_a.schema_b.service_c"},
		{"skills without schema", kindSkills, nil, nil, host + "/ai-gateway/skills/"},
		{"skills schemas", kindSkills, nil, []string{"catalog_a.schema_b", "catalog_c.schema_d"}, host + "/ai-gateway/skills/?schema=catalog_a.schema_b&schema=catalog_c.schema_d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildEndpoint(host, options{kind: tt.kind, resources: tt.resources, schemas: tt.schemas})
			if err != nil {
				t.Fatalf("buildEndpoint: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("endpoint = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildEndpointAddsHTTPS(t *testing.T) {
	t.Parallel()
	got, err := buildEndpoint("workspace.example.test", options{kind: kindSQL})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "https://workspace.example.test/api/2.0/mcp/sql" {
		t.Fatalf("unexpected endpoint %q", got)
	}
}

func TestBuildEndpointRejectsUnsafeHost(t *testing.T) {
	t.Parallel()
	tests := []string{
		"http://workspace.example.test",
		"https://user:password@workspace.example.test",
		"https://workspace.example.test/base",
		"https://workspace.example.test?token=secret",
		"https://workspace.example.test#fragment",
		"https://workspace.example.test@evil.example.test/path",
		"https://workspace.example.test\n.evil.example.test",
		"ftp://workspace.example.test",
		"https://",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if _, err := buildEndpoint(raw, options{kind: kindSQL}); err == nil {
				t.Fatalf("buildEndpoint(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestBuildEndpointRejectsWrongArityAndUnsafeResources(t *testing.T) {
	t.Parallel()
	tests := []options{
		{kind: kindSQL, resources: []string{"extra"}},
		{kind: kindGenie},
		{kind: kindAISearch, resources: []string{"only-one"}},
		{kind: kindFunctions, resources: []string{"cat"}},
		{kind: kindMCPService, resources: []string{"cat", "schema"}},
		{kind: kindSkills, resources: []string{"cat", "schema"}},
		{kind: "unknown"},
		{kind: kindGenie, resources: []string{"../escape"}},
		{kind: kindGenie, resources: []string{"a..b"}},
		{kind: kindGenie, resources: []string{"a/b"}},
		{kind: kindGenie, resources: []string{"a\\b"}},
		{kind: kindGenie, resources: []string{"%2e%2e"}},
		{kind: kindGenie, resources: []string{"hello\nworld"}},
		{kind: kindGenie, resources: []string{"https:"}},
		{kind: kindSQL, schemas: []string{"cat.schema"}},
		{kind: kindSkills, schemas: []string{"missing_dot"}},
		{kind: kindSkills, schemas: []string{"cat.schema.extra"}},
		{kind: kindSkills, schemas: []string{"cat..."}},
	}
	for i, opts := range tests {
		if _, err := buildEndpoint("workspace.example.test", opts); err == nil {
			t.Errorf("case %d (%+v) unexpectedly succeeded", i, opts)
		}
	}
}

func TestParseFlagsNoSecretFlagsOrPositionals(t *testing.T) {
	t.Parallel()
	var stderr strings.Builder
	opts, err := parseFlags([]string{"--kind", "skills", "--schema", "cat.one", "--schema=cat.two", "--timeout", "5s"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.schemas) != 2 || opts.timeout != 5*time.Second {
		t.Fatalf("unexpected options: %+v", opts)
	}
	for _, args := range [][]string{
		{"--kind", "sql", "positional"},
		{"--kind", "sql", "--token", "secret"},
		{"--kind", "sql", "--host", "https://example.test"},
		{"--kind", "sql", "--timeout", "0s"},
		{"--kind", "sql", "--timeout", "11m"},
	} {
		stderr.Reset()
		if _, err := parseFlags(args, &stderr); err == nil {
			t.Errorf("parseFlags(%q) unexpectedly succeeded", args)
		}
	}
}

func TestSameAuthority(t *testing.T) {
	t.Parallel()
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	base := parse("https://workspace.example.test/path")
	if !sameAuthority(base, parse("https://WORKSPACE.EXAMPLE.TEST/other")) {
		t.Fatal("same authority rejected")
	}
	for _, raw := range []string{"https://other.example.test/x", "http://workspace.example.test/x", "https://workspace.example.test:8443/x"} {
		if sameAuthority(base, parse(raw)) {
			t.Errorf("different authority %q accepted", raw)
		}
	}
}
