package provider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/version"
)

// run is a test helper driving the same entrypoint main.go uses in
// provider mode, so these tests exercise real end-to-end behavior rather
// than internals.
func run(t *testing.T, input string, deploy DeployFunc) (line string, err error) {
	t.Helper()
	var out bytes.Buffer
	err = Run(strings.NewReader(input), &out, deploy)
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
					"description": "Databricks CLI profile selection; empty = the build's baked default.",
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
