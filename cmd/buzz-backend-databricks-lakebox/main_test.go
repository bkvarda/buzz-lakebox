package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// databricksShim puts an executable named `databricks` that always fails
// on PATH, so no test in this package can reach a real control plane.
//
// Setting PATH to "" is NOT sufficient: internal/lakebox's
// DefaultBinPath()/fallbackDirs() falls back to absolute well-known
// directories (/opt/homebrew/bin, /usr/local/bin, ~/.local/bin, ~/bin)
// after a PATH-based exec.LookPath("databricks") misses, and a real CLI
// is installed at one of those on most dev machines. exec.LookPath
// consults PATH first, so a shim there always wins.
func databricksShim(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "databricks"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write databricks shim: %v", err)
	}
	t.Setenv("PATH", dir)
}

// TestUndeploy_YesRequiresExplicitID verifies the guard added to
// newUndeployCmd's RunE: `undeploy --yes` with no positional sandbox id
// must be refused before any resolution or exec happens, since resolving
// with no id infers the target from `sandbox list` rather than consulting
// the state file, and --yes would otherwise skip the typed-id
// confirmation entirely.
//
// The shim is not redundant here even though a working guard returns
// before any exec. Its purpose is to bound the blast radius of the
// failure case: if the guard ever regresses, this test must fail
// hermetically rather than fall through to a live `sandbox list` (and,
// with a resolvable single sandbox, on to a real delete) against
// whatever profile the developer happens to have configured. Verified
// by construction — with the guard temporarily removed, this test
// reached a real workspace before the shim was added.
func TestUndeploy_YesRequiresExplicitID(t *testing.T) {
	databricksShim(t)

	cmd := newRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"undeploy", "--yes"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error from `undeploy --yes` with no explicit sandbox id, got nil")
	}
	if !strings.Contains(err.Error(), "explicit sandbox id") {
		t.Fatalf("expected error to mention %q, got: %v", "explicit sandbox id", err)
	}
}

// TestUndeploy_YesWithExplicitIDPassesTheGuard is the falsifier for the
// test above: without it, TestUndeploy_YesRequiresExplicitID would still
// pass even if the guard wrongly rejected every `--yes` invocation
// regardless of whether an id was supplied. Here an explicit sandbox id
// is passed, so the new guard must let execution proceed past it; the
// command still fails (there's no real sandbox), but the failure must
// come from downstream CLI execution, not from the new guard.
//
// This one reaches exec by design, so the shim is what keeps it off the
// network — see databricksShim.
// TestDeploy_SandboxInferenceAuth_WarnsOnStderr pins the operator-path
// warning (docs/PLAN.md zero-token design item 5): inference_auth:
// "sandbox" reverses least-privilege, so `deploy --payload-file` must
// warn loudly on stderr. The deploy itself still fails (the shim always
// fails), which is fine — the warning must print before any exec is
// attempted.
func TestDeploy_SandboxInferenceAuth_WarnsOnStderr(t *testing.T) {
	databricksShim(t)

	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.json")
	payloadJSON := `{
		"agent": {
			"name": "reviewer",
			"relay_url": "wss://relay.example.com",
			"private_key_nsec": "nsec1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqstywftw",
			"auth_tag": "tag",
			"agent_command": "buzz-agent"
		},
		"provider_config": {"inference_auth": "sandbox"}
	}`
	if err := os.WriteFile(payloadPath, []byte(payloadJSON), 0o600); err != nil {
		t.Fatalf("write payload file: %v", err)
	}

	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"deploy", "--payload-file", payloadPath})

	// deploy's RunE always returns nil (errors are rendered into the
	// {"ok":false,...} stdout JSON instead), so no error assertion here.
	if err := cmd.Execute(); err != nil {
		t.Fatalf("deploy command itself errored: %v", err)
	}

	if !strings.Contains(errOut.String(), "inference_auth=sandbox") {
		t.Fatalf("expected a sandbox-mode stderr warning, got stderr: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "creator-identity") {
		t.Fatalf("expected the warning to name the creator-identity tradeoff, got: %q", errOut.String())
	}
}

// TestDeploy_EnvModeDefault_NoStderrWarning is the falsifier for the test
// above: with no inference_auth set (today's default), the operator path
// must print nothing extra on stderr.
func TestDeploy_EnvModeDefault_NoStderrWarning(t *testing.T) {
	databricksShim(t)

	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.json")
	payloadJSON := `{
		"agent": {
			"name": "reviewer",
			"relay_url": "wss://relay.example.com",
			"private_key_nsec": "nsec1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqstywftw",
			"auth_tag": "tag",
			"agent_command": "buzz-agent"
		}
	}`
	if err := os.WriteFile(payloadPath, []byte(payloadJSON), 0o600); err != nil {
		t.Fatalf("write payload file: %v", err)
	}

	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"deploy", "--payload-file", payloadPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("deploy command itself errored: %v", err)
	}

	if strings.Contains(errOut.String(), "inference_auth=sandbox") {
		t.Fatalf("env-mode (default) deploy must not print the sandbox-mode warning, got stderr: %q", errOut.String())
	}
}

func TestUndeploy_YesWithExplicitIDPassesTheGuard(t *testing.T) {
	databricksShim(t)

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"undeploy", "--yes", "some-sandbox-id"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error (the shim always fails), got nil")
	}
	if strings.Contains(err.Error(), "explicit sandbox id") {
		t.Fatalf("guard wrongly rejected an invocation that DID supply an explicit sandbox id: %v", err)
	}
}
