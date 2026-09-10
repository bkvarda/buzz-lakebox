package mcpops_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpops"
)

type fakeDiscoverer struct {
	ids []mcpops.Identifier
	err error
}

func (f *fakeDiscoverer) Discover(context.Context) ([]mcpops.Identifier, error) {
	return f.ids, f.err
}

func TestDiscoverReturnsValidatedPortableIdentifiers(t *testing.T) {
	fake := &fakeDiscoverer{ids: []mcpops.Identifier{
		mcpops.SkillsIdentifier([2]string{"example_catalog", "example_schema"}),
		mcpops.SQLIdentifier(),
		mcpops.MCPServiceIdentifier("example_catalog", "example_schema", "example_service"),
		mcpops.FunctionIdentifier("example_catalog", "example_schema", "example_function"),
		mcpops.FunctionsIdentifier("example_catalog", "example_schema"),
		mcpops.VectorSearchIdentifier("example_catalog", "example_schema"),
		mcpops.AISearchIdentifier("example_catalog", "example_schema", "example_index"),
		mcpops.GenieIdentifier("example_space"),
	}}

	got, err := mcpops.Discover(context.Background(), fake)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != len(fake.ids) {
		t.Fatalf("got %d identifiers, want %d", len(got), len(fake.ids))
	}

	wantKinds := []mcpconfig.Kind{
		mcpconfig.KindAISearch,
		mcpconfig.KindFunctions,
		mcpconfig.KindFunctions,
		mcpconfig.KindGenie,
		mcpconfig.KindMCPService,
		mcpconfig.KindSkills,
		mcpconfig.KindSQL,
		mcpconfig.KindVectorSearch,
	}
	kinds := make([]mcpconfig.Kind, len(got))
	for i, id := range got {
		kinds[i] = id.Kind
		if err := id.Validate(); err != nil {
			t.Errorf("result[%d] is invalid: %v", i, err)
		}
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("kind order = %v, want %v", kinds, wantKinds)
	}

	// Results cannot expose connection material because Identifier has only
	// Kind and Resource fields. Also pin the defensive copy contract.
	fake.ids[0].Resource[0] = "mutated_catalog"
	for _, id := range got {
		for _, component := range id.Resource {
			if strings.Contains(component, "mutated") {
				t.Fatalf("result aliases adapter storage: %#v", got)
			}
		}
	}
}

func TestIdentifierServerCreatesValidManagedEntry(t *testing.T) {
	server := mcpops.AISearchIdentifier("example_catalog", "example_schema", "example_index").Server("search")
	cfg := mcpconfig.Config{
		Schema:  mcpconfig.CurrentSchema,
		Version: mcpconfig.CurrentVersion,
		Servers: []mcpconfig.Server{server},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("converted server is invalid: %v", err)
	}
	if server.Auth != mcpconfig.AuthEnv {
		t.Fatalf("auth = %q, want %q", server.Auth, mcpconfig.AuthEnv)
	}
}

func TestDiscoverRejectsMalformedDuplicateAndLocalResults(t *testing.T) {
	tests := []struct {
		name string
		ids  []mcpops.Identifier
		want string
	}{
		{
			name: "malformed",
			ids:  []mcpops.Identifier{{Kind: mcpconfig.KindGenie}},
			want: "one space identifier",
		},
		{
			name: "duplicate",
			ids: []mcpops.Identifier{
				mcpops.GenieIdentifier("example_space"),
				mcpops.GenieIdentifier("example_space"),
			},
			want: "duplicates",
		},
		{
			name: "local",
			ids:  []mcpops.Identifier{{Kind: mcpconfig.KindLocal}},
			want: "not discoverable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mcpops.Discover(context.Background(), &fakeDiscoverer{ids: tc.ids})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestDiscoverRedactsAdapterError(t *testing.T) {
	const privateValue = "placeholder-sensitive-value"
	fake := &fakeDiscoverer{err: errors.New("DATABRICKS_TOKEN=" + privateValue)}
	_, err := mcpops.Discover(context.Background(), fake)
	if err == nil {
		t.Fatal("Discover succeeded")
	}
	if strings.Contains(err.Error(), privateValue) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error was not redacted: %q", err)
	}
}
