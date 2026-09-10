package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpdiscover"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpops"
	"github.com/IceRhymers/buzz-lakebox/internal/profileauth"
)

type commandDiscoverer struct{ ids []mcpops.Identifier }

func (d commandDiscoverer) Discover(context.Context) ([]mcpops.Identifier, error) { return d.ids, nil }

type commandAuthResolver struct {
	auth profileauth.Auth
	err  error
}

func (r commandAuthResolver) Resolve(context.Context) (profileauth.Auth, error) { return r.auth, r.err }

type commandProber struct{}

func (commandProber) Initialize(_ context.Context, server mcpconfig.Server) (mcpops.InitializedServer, error) {
	if server.Name == "broken" {
		return nil, errors.New("DATABRICKS_TOKEN=must-not-escape")
	}
	return commandSession{}, nil
}

type commandSession struct{}

func (commandSession) ListTools(context.Context) ([]mcpops.Tool, error) {
	return []mcpops.Tool{{Name: "zeta"}, {Name: "alpha"}}, nil
}
func (commandSession) Close() error { return nil }

func TestConfigValidateCommandIsOfflineAndCanonical(t *testing.T) {
	cmd := newConfigCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"validate", "--mcp-json", `{"schema":"buzz-managed-mcp","version":1,"servers":[{"name":"sql","kind":"sql","auth":"env"}]}`})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["ok"] != true || got["kind"] != "mcp" {
		t.Fatalf("result = %#v", got)
	}
	if config, _ := got["config"].(string); !strings.Contains(config, `"kind":"sql"`) {
		t.Fatalf("config = %q", config)
	}
}

func TestMCPDiscoverCommandUsesExplicitPortableInputs(t *testing.T) {
	var gotProfile string
	var gotScopes []mcpdiscover.Scope
	var gotKinds []mcpconfig.Kind
	deps := operatorDeps{
		discoverer: func(profile string, scopes []mcpdiscover.Scope, kinds []mcpconfig.Kind) (mcpops.Discoverer, error) {
			gotProfile, gotScopes, gotKinds = profile, scopes, kinds
			return commandDiscoverer{ids: []mcpops.Identifier{mcpops.GenieIdentifier("example_space"), mcpops.SQLIdentifier()}}, nil
		},
	}
	profile := "example-profile"
	cmd := newMCPCommand(&profile, deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"discover", "--kind", "sql,genie", "--scope", "example_catalog.example_schema", "--emit-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotProfile != profile {
		t.Fatalf("profile = %q", gotProfile)
	}
	if !reflect.DeepEqual(gotScopes, []mcpdiscover.Scope{{Catalog: "example_catalog", Schema: "example_schema"}}) {
		t.Fatalf("scopes = %#v", gotScopes)
	}
	if !reflect.DeepEqual(gotKinds, []mcpconfig.Kind{mcpconfig.KindSQL, mcpconfig.KindGenie}) {
		t.Fatalf("kinds = %#v", gotKinds)
	}
	var got struct {
		OK        bool                `json:"ok"`
		Config    string              `json:"config"`
		Resources []mcpops.Identifier `json:"resources"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || len(got.Resources) != 2 || got.Config == "" {
		t.Fatalf("result = %#v", got)
	}
	if strings.Contains(out.String(), profile) {
		t.Fatal("profile leaked into result")
	}
}

func TestMCPProbeCommandResolvesAuthOnlyAfterValidation(t *testing.T) {
	const host = "https://workspace.example.invalid"
	const token = "placeholder-sensitive-token-value"
	authCalls, proberCalls := 0, 0
	deps := operatorDeps{
		auth: func(profile string, force bool) (authResolver, error) {
			authCalls++
			if profile != "example-profile" || !force {
				t.Fatalf("auth input = %q/%v", profile, force)
			}
			return commandAuthResolver{auth: profileauth.Auth{Host: host, Token: token, Expiry: time.Now().Add(time.Hour)}}, nil
		},
		prober: func(gotHost, gotToken string) (mcpops.Prober, error) {
			proberCalls++
			if gotHost != host || gotToken != token {
				t.Fatal("wrong auth passed to prober")
			}
			return commandProber{}, nil
		},
	}
	profile := "example-profile"
	cmd := newMCPCommand(&profile, deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"probe", "--force-refresh", "--mcp-json", `{"schema":"buzz-managed-mcp","version":1,"servers":[{"name":"sql","kind":"sql","auth":"env"}]}`})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if authCalls != 1 || proberCalls != 1 {
		t.Fatalf("calls = %d/%d", authCalls, proberCalls)
	}
	if strings.Contains(out.String(), host) || strings.Contains(out.String(), token) || strings.Contains(out.String(), profile) {
		t.Fatal("connection material leaked into result")
	}
	if !strings.Contains(out.String(), `"tool_count":2`) || !strings.Contains(out.String(), `"alpha"`) {
		t.Fatalf("result = %s", out.String())
	}

	authCalls = 0
	invalid := newMCPCommand(&profile, deps)
	invalid.SetOut(&bytes.Buffer{})
	invalid.SetErr(&bytes.Buffer{})
	invalid.SetArgs([]string{"probe", "--mcp-json", `{}`})
	if err := invalid.Execute(); err == nil {
		t.Fatal("invalid config accepted")
	}
	if authCalls != 0 {
		t.Fatal("credential resolved before offline validation")
	}
}

func TestMCPProbeCommandReturnsRedactedPartialResults(t *testing.T) {
	deps := operatorDeps{
		auth: func(string, bool) (authResolver, error) {
			return commandAuthResolver{auth: profileauth.Auth{Host: "https://workspace.example.invalid", Token: "placeholder-sensitive-token-value"}}, nil
		},
		prober: func(string, string) (mcpops.Prober, error) { return commandProber{}, nil },
	}
	profile := "example-profile"
	cmd := newMCPCommand(&profile, deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"probe", "--mcp-json", `{"schema":"buzz-managed-mcp","version":1,"servers":[{"name":"broken","kind":"sql","auth":"env"}]}`})
	if err := cmd.Execute(); err == nil {
		t.Fatal("failed probe returned success")
	}
	if strings.Contains(out.String(), "must-not-escape") {
		t.Fatal("secret-shaped error leaked")
	}
	if !strings.Contains(out.String(), `"ok":false`) || !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatalf("result = %s", out.String())
	}
}
