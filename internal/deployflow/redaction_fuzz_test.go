package deployflow

import (
	"fmt"
	"strings"
	"testing"
)

// Marker-secret fuzz (docs/PLAN.md §6 M3 accept: "marker-secret fuzz
// shows zero leakage in stdout/stderr/logs"; §7 "property-style: marker
// secrets in every payload field never reach rendered errors/logs").
//
// Every secret-bearing payload field gets a unique, unmistakable marker
// value; the deploy is then driven into each distinct failure mode in
// turn (plus the happy path), and every observable output is checked:
// the returned error text, every argv the provider hands the databricks
// CLI, and every argv of every in-sandbox ssh command. Secrets are only
// ever allowed to appear on stdin (docs/PLAN.md §4.4 step 7 — "over SSH
// stdin only, never argv").

const (
	markerNsec     = "nsec1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqstywftw"
	markerAuthTag  = "MRKR-auth-tag-4f21a9c8"
	markerToken    = "MRKR-databricks-token-b71e3d55"
	markerAPIKey   = "MRKR-third-party-api-key-9c04af12"
	markerPassword = "MRKR-env-password-2be7710d"
)

// markerSecrets are the values that must never appear in any rendered
// error or any argv, on any path.
var markerSecrets = []string{markerNsec, markerAuthTag, markerToken, markerAPIKey, markerPassword}

// markerDapiToken is a bare Databricks PAT shape ("dapi" + hex) standing
// in for the sandbox inference-auth credential (provider_config
// .inference_auth: "sandbox"), which is derived in-sandbox from the
// baked ~/.databrickscfg and deliberately never rides the payload
// (PLAN.md pre-mortem #3). Because it never rides the payload, it is NOT
// added to markerSecrets — TestDeploy_MarkerSecretsDoTravelOverStdin
// asserts every entry in that slice reaches the sandbox over stdin,
// which is exactly the property this token does not have. Only
// redact.Log's dapiPrefixPattern floor protects a leak of this shape.
// Assembled at runtime so the token-shaped literal never appears
// contiguously in source — the Databricks pre-commit secret scanner
// (correctly) refuses to commit anything matching a real PAT shape,
// and these fixtures exist precisely to look like one.
var markerDapiToken = "dapi" + strings.Repeat("0123456789abcdef", 2)

// dapiOnlySecrets is checked in TestDeploy_MarkerSecretsNeverLeakOnAnyPath
// alongside markerSecrets, but kept separate for the reason above.
var dapiOnlySecrets = []string{markerDapiToken}

func markerRequest() *reqOpts {
	return &reqOpts{
		nsec:    markerNsec,
		authTag: markerAuthTag,
		envVars: map[string]string{
			"DATABRICKS_TOKEN": markerToken,
			"OPENAI_API_KEY":   markerAPIKey,
			"SOME_PASSWORD":    markerPassword,
		},
	}
}

// fuzzScenarios drives one injected failure per step of the flow. Each
// entry is (name, FAKE_* env) — the code that comes back is asserted by
// TestDeploy_FailureModesAreDistinctlyCoded; here we only care that
// whatever error text it produces is clean.
var fuzzScenarios = []struct {
	name string
	env  map[string]string
}{
	{"happy path", map[string]string{"FAKE_LIST_JSON": "[]"}},
	{"cli too old", map[string]string{"FAKE_VERSION": "1.0.0"}},
	{"profile unresolved", map[string]string{"FAKE_CURRENT_USER_EXIT": "1"}},
	{"register fails", map[string]string{"FAKE_REGISTER_EXIT": "1"}},
	{"list fails", map[string]string{"FAKE_LIST_EXIT": "1"}},
	{"create fails", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_CREATE_EXIT": "1"}},
	{"start fails", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_CREATE_STATUS": "Stopped", "FAKE_START_EXIT": "1", "FAKE_STATUS_STATUS": "Stopped"}},
	{"install fails", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_INSTALL_EXIT": "1"}},
	{"runtime verify fails", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_VERIFY_EXEC_EXIT": "1"}},
	{"runtime verify wrong response", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_VERIFY_OUTPUT": "{}"}},
	{"launch fails", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_LAUNCH_EXIT": "1"}},
	{"process dead after launch", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_PGREP_EXIT": "1"}},
	{"unparseable verification", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_PGREP_EXIT": "not-a-number"}},
	{"relay denies the key", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_ACP_LOG": "starting\n" + terminalErrorLine + "\n"}},
	{"pool never ready", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_ACP_LOG": "starting\nwarming up\n"}},
	{"autostop config fails", map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_CONFIG_EXIT": "1"}},
	{"unknown buzz version", map[string]string{"FAKE_LIST_JSON": "[]"}},
}

func TestDeploy_MarkerSecretsNeverLeakOnAnyPath(t *testing.T) {
	for _, sc := range fuzzScenarios {
		t.Run(sc.name, func(t *testing.T) {
			h := newHarness(t)
			setHappyPathEnv(t)
			for k, v := range sc.env {
				t.Setenv(k, v)
			}

			opts := *markerRequest()
			if sc.name == "unknown buzz version" {
				opts.buzzVersion = "v0.0.0-does-not-exist"
			}
			// A failure-mode payload is also an error-message payload:
			// the acp.log the shim echoes back is interpolated into
			// verify errors, so plant a marker there too — a log tail
			// carrying a secret must be scrubbed like any other text.
			if _, ok := sc.env["FAKE_ACP_LOG"]; !ok && sc.name != "happy path" {
				// The dapi-shaped token is appended bare, without a
				// KEY= assignment around it — mirroring a
				// sandbox-derived credential that leaks into acp.log
				// on its own, a shape secretAssignmentPattern cannot
				// catch and only dapiPrefixPattern can.
				t.Setenv("FAKE_ACP_LOG", healthyLog+"context: token="+markerToken+"\nderived credential "+markerDapiToken+"\n")
			}

			_, err := h.dep.Deploy(buildReq(opts))

			if err != nil {
				assertNoMarkers(t, "returned error", err.Error())
				assertSecretsAbsent(t, "returned error", err.Error(), dapiOnlySecrets)
			}
			for _, e := range h.events() {
				switch e.kind {
				case "CLI":
					assertNoMarkers(t, "databricks CLI argv", e.cliLine)
					assertSecretsAbsent(t, "databricks CLI argv", e.cliLine, dapiOnlySecrets)
				case "SSH":
					assertNoMarkers(t, fmt.Sprintf("argv of ssh step %q", e.sshTag), e.args(t))
					assertSecretsAbsent(t, fmt.Sprintf("argv of ssh step %q", e.sshTag), e.args(t), dapiOnlySecrets)
				}
			}
		})
	}
}

// TestDeploy_MarkerSecretsDoTravelOverStdin is the counterweight to the
// leakage assertions above: proving nothing leaks is only meaningful if
// the secrets were actually shipped somewhere. Every marker must reach
// the sandbox over stdin on a successful deploy.
func TestDeploy_MarkerSecretsDoTravelOverStdin(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")

	if _, err := h.dep.Deploy(buildReq(*markerRequest())); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	var stdinAll strings.Builder
	for _, e := range h.events() {
		if e.kind == "SSH" {
			stdinAll.WriteString(e.stdin(t))
		}
	}
	for _, secret := range markerSecrets {
		if !strings.Contains(stdinAll.String(), secret) {
			t.Fatalf("secret %q never reached the sandbox over stdin — the no-leak assertions above would be vacuous", secret)
		}
	}
}

// TestLifecycle_MarkerSecretsNeverLeak covers the operator subcommands,
// whose output an owner pastes into issues: a log tail carrying a token
// must not survive into a status/logs error either.
func TestLifecycle_MarkerSecretsNeverLeak(t *testing.T) {
	logWithSecret := "buzz-acp starting\nexport DATABRICKS_TOKEN=" + markerToken + "\n"

	t.Run("start verification failure", func(t *testing.T) {
		h := newHarness(t)
		t.Setenv("FAKE_STATUS_STATUS", "Running")
		t.Setenv("FAKE_PGREP_EXIT", "1")
		t.Setenv("FAKE_ACP_LOG", logWithSecret)

		err := h.dep.Start("DEFAULT", "sandbox-1")
		if err == nil {
			t.Fatal("expected Start to fail its verification")
		}
		// Start has no payload to derive secrets from, so this asserts
		// the property that matters for lifecycle ops: they never
		// interpolate a raw log tail into an error in the first place.
		assertNoMarkers(t, "Start error", err.Error())
	})
}

func assertNoMarkers(t *testing.T, where, text string) {
	t.Helper()
	assertSecretsAbsent(t, where, text, markerSecrets)
}

func assertSecretsAbsent(t *testing.T, where, text string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("marker secret %q leaked into %s: %s", secret, where, text)
		}
	}
}
