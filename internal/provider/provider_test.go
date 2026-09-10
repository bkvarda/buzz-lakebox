package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/version"
)

// run is a test helper driving the same entrypoint main.go uses in
// provider mode, so these tests exercise real end-to-end behavior rather
// than internals.
func run(t *testing.T, input string, deploy DeployFunc, opts ...Option) (line string, err error) {
	t.Helper()
	var out bytes.Buffer
	err = Run(strings.NewReader(input), &out, deploy, opts...)
	return out.String(), err
}

func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("response %q does not end in a single newline", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("response %q is not exactly one line", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &m); err != nil {
		t.Fatalf("response line is not valid JSON: %v (%q)", err, line)
	}
	return m
}

func TestInfo_FrozenShape(t *testing.T) {
	line, err := run(t, `{"op":"info"}`, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)

	want := map[string]any{
		"ok":               true,
		"name":             "Databricks Lakebox",
		"version":          version.Version,
		"description":      "Deploys Buzz agents into Databricks Lakebox sandboxes",
		"protocol_version": 1,
		"config_schema": map[string]any{
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
		},
	}
	for k, wv := range want {
		gv, ok := m[k]
		if !ok {
			t.Fatalf("info response missing field %q; got %#v", k, m)
		}
		if !equalJSON(t, wv, gv) {
			t.Fatalf("info response field %q = %#v, want %#v", k, gv, wv)
		}
	}

	// Buzz Desktop's validate_provider_info rejects any field outside a strict
	// whitelist, so the response must carry EXACTLY these keys — no legacy
	// "protocol"/"ops" — or the desktop fails the probe with "unknown field".
	if len(m) != len(want) {
		t.Fatalf("info response has %d fields, want exactly %d (%v); got %#v", len(m), len(want), keysOf(want), m)
	}

	// protocol_version must decode as a JSON number (an integer), never the
	// old "v1"-style string: the desktop reads it with as_u64 and a string
	// yields "provider info response missing integer protocol_version".
	pv, ok := m["protocol_version"].(float64)
	if !ok {
		t.Fatalf("protocol_version is not a JSON number: got %#v", m["protocol_version"])
	}
	if pv != 1 {
		t.Fatalf("protocol_version = %v, want 1", pv)
	}
}

type fakeDiscoverer struct {
	profiles []Profile
	err      error
	calls    int
}

func (d *fakeDiscoverer) DiscoverProfiles(context.Context) ([]Profile, error) {
	d.calls++
	return d.profiles, d.err
}

func TestInfo_DynamicProfileSchemaAndFrozenTopLevelShape(t *testing.T) {
	discoverer := &fakeDiscoverer{profiles: []Profile{
		{Name: "alpha", Host: "https://a.example"},
		{Name: "zeta", Host: "https://z.example"},
	}}
	line, err := run(t, `{"op":"info"}`, nil, WithProfileDiscovery(discoverer, "zeta"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := decodeLine(t, line)
	if len(m) != 6 {
		t.Fatalf("info top-level fields = %d, want exactly 6: %#v", len(m), m)
	}
	schema := m["config_schema"].(map[string]any)
	profile := schema["properties"].(map[string]any)["profile"].(map[string]any)
	if got, want := profile["enum"], []any{"", "alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("profile enum = %#v, want %#v", got, want)
	}
	if got := profile["default"]; got != "zeta" {
		t.Fatalf("profile default = %#v, want zeta", got)
	}
	if description, _ := profile["description"].(string); !strings.Contains(description, "text input") || !strings.Contains(description, "alpha, zeta") || !strings.Contains(description, "clickable dropdown") {
		t.Fatalf("profile description is not current-renderer compatible: %q", description)
	}
}

func TestInfo_DynamicProfileSchemaDefaultOnlyWhenDeterministic(t *testing.T) {
	discoverer := &fakeDiscoverer{profiles: []Profile{
		{Name: "alpha", Host: "https://a.example"},
		{Name: "zeta", Host: "https://z.example"},
	}}
	line, err := run(t, `{"op":"info"}`, nil, WithProfileDiscovery(discoverer, "DEFAULT"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := decodeLine(t, line)
	profile := m["config_schema"].(map[string]any)["properties"].(map[string]any)["profile"].(map[string]any)
	if _, exists := profile["default"]; exists {
		t.Fatalf("ambiguous profile schema must not set default: %#v", profile)
	}
}

func TestInfo_UnavailableDiscoveryUsesStaticFallback(t *testing.T) {
	for _, test := range []struct {
		name       string
		discoverer *fakeDiscoverer
	}{
		{name: "failure", discoverer: &fakeDiscoverer{err: errors.New("dapi-secret-that-must-not-escape")}},
		{name: "no workspace profiles", discoverer: &fakeDiscoverer{profiles: []Profile{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			line, err := run(t, `{"op":"info"}`, nil, WithProfileDiscovery(test.discoverer, "DEFAULT"))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			m := decodeLine(t, line)
			if len(m) != 6 || m["ok"] != true {
				t.Fatalf("unavailable discovery changed frozen response: %#v", m)
			}
			profile := m["config_schema"].(map[string]any)["properties"].(map[string]any)["profile"].(map[string]any)
			if _, exists := profile["enum"]; exists {
				t.Fatalf("static fallback unexpectedly has enum: %#v", profile)
			}
			if strings.Contains(line, "dapi-secret") {
				t.Fatalf("info response leaked discovery error: %s", line)
			}
		})
	}
}

func TestDeploy_ExplicitProfileBypassesDiscovery(t *testing.T) {
	discoverer := &fakeDiscoverer{err: errors.New("should not be called")}
	body := `{"op":"deploy","agent":{"name":"a","relay_url":"wss://relay","private_key_nsec":"nsec1x","auth_tag":"tag","agent_command":"buzz-agent"},"provider_config":{"profile":"explicit"}}`
	line, err := run(t, body, func(req *payload.DeployRequest) (string, error) {
		if req.ProviderConfig.Profile != "explicit" {
			t.Fatalf("profile = %q", req.ProviderConfig.Profile)
		}
		return "sandbox-123", nil
	}, WithProfileDiscovery(discoverer, "DEFAULT"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if discoverer.calls != 0 {
		t.Fatalf("discovery calls = %d, want 0", discoverer.calls)
	}
	if m := decodeLine(t, line); m["ok"] != true {
		t.Fatalf("deploy failed: %#v", m)
	}
}

func TestDeploy_AutoSelectsAndRefusesAmbiguity(t *testing.T) {
	body := `{"op":"deploy","agent":{"name":"a","relay_url":"wss://relay","private_key_nsec":"nsec1x","auth_tag":"tag","agent_command":"buzz-agent"}}`
	t.Run("single", func(t *testing.T) {
		discoverer := &fakeDiscoverer{profiles: []Profile{{Name: "only", Host: "https://a.example"}}}
		line, err := run(t, body, func(req *payload.DeployRequest) (string, error) {
			if req.ProviderConfig.Profile != "only" {
				t.Fatalf("profile = %q, want only", req.ProviderConfig.Profile)
			}
			return "sandbox-123", nil
		}, WithProfileDiscovery(discoverer, "DEFAULT"))
		if err != nil || decodeLine(t, line)["ok"] != true {
			t.Fatalf("deploy: err=%v line=%s", err, line)
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		discoverer := &fakeDiscoverer{profiles: []Profile{{Name: "alpha", Host: "https://a.example"}, {Name: "zeta", Host: "https://z.example"}}}
		line, err := run(t, body, func(*payload.DeployRequest) (string, error) {
			t.Fatal("deploy must not run after ambiguous profile discovery")
			return "", nil
		}, WithProfileDiscovery(discoverer, "DEFAULT"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		m := decodeLine(t, line)
		if m["ok"] != false || !strings.Contains(m["error"].(string), "available profiles: alpha, zeta") {
			t.Fatalf("unexpected ambiguity response: %#v", m)
		}
	})
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestInfo_WithRequestIDIgnoresIt(t *testing.T) {
	line, err := run(t, `{"op":"info","request_id":"11111111-1111-1111-1111-111111111111"}`, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	if ok, _ := m["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %#v", m)
	}
}

func TestUnknownOp_FrozenShape(t *testing.T) {
	line, err := run(t, `{"op":"bogus"}`, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)

	wantErr := `unknown op "bogus"; supported: info, deploy`
	if ok, _ := m["ok"].(bool); ok {
		t.Fatalf("expected ok:false, got %#v", m)
	}
	if got, _ := m["error"].(string); got != wantErr {
		t.Fatalf("error = %q, want %q", got, wantErr)
	}
}

func TestDeploy_M0Stub(t *testing.T) {
	line, err := run(t, `{"op":"deploy","agent":{}}`, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	if ok, _ := m["ok"].(bool); ok {
		t.Fatalf("expected ok:false, got %#v", m)
	}
	wantErr := "deploy not implemented yet (M1)"
	if got, _ := m["error"].(string); got != wantErr {
		t.Fatalf("error = %q, want %q", got, wantErr)
	}
}

func TestMalformedJSON_HandledNotPanicked(t *testing.T) {
	cases := []string{
		``,
		`not json at all`,
		`{"op":`,
		`{"op": 5}`, // op as wrong type
		`null`,
		`[]`,
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			line, err := run(t, c, nil)
			if err != nil {
				t.Fatalf("Run returned unhandleable error for input %q: %v", c, err)
			}
			m := decodeLine(t, line)
			if ok, _ := m["ok"].(bool); ok {
				t.Fatalf("input %q: expected ok:false, got %#v", c, m)
			}
			if _, ok := m["error"].(string); !ok {
				t.Fatalf("input %q: expected string error field, got %#v", c, m)
			}
		})
	}
}

func TestDeploy_WithHandler_Success(t *testing.T) {
	deploy := func(req *payload.DeployRequest) (string, error) {
		return "sandbox-123", nil
	}
	body := `{"op":"deploy","agent":{"name":"a","relay_url":"wss://relay","private_key_nsec":"nsec1x","auth_tag":"tag","agent_command":"buzz-agent"}}`
	line, err := run(t, body, deploy)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	if ok, _ := m["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %#v", m)
	}
	if got, _ := m["agent_id"].(string); got != "sandbox-123" {
		t.Fatalf("agent_id = %q, want %q", got, "sandbox-123")
	}
}

func TestDeploy_WithHandler_ValidationRejectsUnsupportedRuntime(t *testing.T) {
	body := `{"op":"deploy","agent":{"name":"a","relay_url":"wss://relay","private_key_nsec":"nsec1secretvalue","auth_tag":"tag","agent_command":"goose"}}`
	line, err := run(t, body, func(req *payload.DeployRequest) (string, error) {
		t.Fatalf("deploy handler should not be called when validation fails")
		return "", nil
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	if ok, _ := m["ok"].(bool); ok {
		t.Fatalf("expected ok:false, got %#v", m)
	}
	errMsg, _ := m["error"].(string)
	if !strings.Contains(errMsg, "goose") {
		t.Fatalf("error %q should name the rejected runtime", errMsg)
	}
	if strings.Contains(errMsg, "nsec1secretvalue") {
		t.Fatalf("error %q leaked the nsec", errMsg)
	}
}

func TestDeploy_HandlerErrorIsRedacted(t *testing.T) {
	body := `{"op":"deploy","agent":{"name":"a","relay_url":"wss://relay","private_key_nsec":"nsec1verysecretvalue","auth_tag":"secret-auth-tag-value","agent_command":"buzz-agent","env_vars":{"DATABRICKS_TOKEN":"dapi-marker-secret-1234"}}}`
	deploy := func(req *payload.DeployRequest) (string, error) {
		return "", errFromAllSecrets(req)
	}
	line, err := run(t, body, deploy)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	errMsg, _ := m["error"].(string)
	for _, secret := range []string{"nsec1verysecretvalue", "secret-auth-tag-value", "dapi-marker-secret-1234"} {
		if strings.Contains(errMsg, secret) {
			t.Fatalf("error %q leaked secret %q", errMsg, secret)
		}
	}
}

func errFromAllSecrets(req *payload.DeployRequest) error {
	return errAllSecrets{
		nsec:    req.Agent.PrivateKeyNsec,
		auth:    req.Agent.AuthTag,
		envVars: req.Agent.EnvVars,
	}
}

type errAllSecrets struct {
	nsec    string
	auth    string
	envVars map[string]string
}

func (e errAllSecrets) Error() string {
	s := "deploy failed; nsec=" + e.nsec + " auth=" + e.auth
	for k, v := range e.envVars {
		s += " " + k + "=" + v
	}
	return s
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(ab) == string(bb)
}
