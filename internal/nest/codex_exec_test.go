package nest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
)

// These tests source CodexEnvSnippet through a real POSIX shell, the same
// way claude_exec_test.go does and for the same reason: a shell guard can be
// syntactically present and still be wrong on a real input.
//
// They carry more weight than the Claude ones because the Codex snippet's
// fail-closed property is NOT free. ClaudeEnvSnippet gets it from the
// process model — env vars are per-process, so "we did not set it" and "it
// is not set" are the same statement. This snippet writes a FILE into a
// persistent $HOME that survives redeploys, so the two statements come apart
// and only an executed test can tell them apart.

type codexResult struct {
	codexHome string
	// exportedHome is CODEX_HOME as observed by a CHILD process, which is
	// the only thing that matters: codex is spawned as a child, so a plain
	// shell assignment would be invisible to it while still satisfying any
	// same-shell assertion. Verified by mutation — dropping the `export`
	// keyword leaves codexHome intact and only this field moves.
	exportedHome string
	config       string // contents of $CODEX_HOME/config.toml, "" if absent
	exists       bool
	mode         os.FileMode
	exitCode     int
	env          string
}

// sourceCodexSnippet sources CodexEnvSnippet in a FRESH shell against the
// given home, with preset variables exported first — the ordering RenderEnv
// produces, since the snippet is appended last precisely so it observes what
// the fixed block and env_vars set.
//
// Taking home as a parameter rather than allocating one is what makes the
// redeploy test possible: two sourcings must share a $HOME but NOT a shell.
// Reusing one shell would be self-defeating — sourcing #1 exports CODEX_HOME
// and DATABRICKS_TOKEN, so sourcing #2 would take the owner-supplied branch
// and skip the removal entirely, testing nothing.
func sourceCodexSnippet(t *testing.T, home string, preset map[string]string, prelude string, checkLeaks bool) codexResult {
	t.Helper()

	snippetPath := filepath.Join(t.TempDir(), "codex.sh")
	// Written as the ENTIRE file, so every case also proves the snippet is
	// safe as the final content of a sourced file (its trailing bare `:`).
	if err := os.WriteFile(snippetPath, []byte(CodexEnvSnippet), 0o600); err != nil {
		t.Fatalf("write snippet: %v", err)
	}

	var script strings.Builder
	script.WriteString(prelude)
	script.WriteString("\n")
	for k, v := range preset {
		script.WriteString("export " + k + "=" + shellquote.Single(v) + "\n")
	}
	script.WriteString(". " + shellquote.Single(snippetPath) + "\n")
	script.WriteString("echo BUZZ_TEST_SOURCED_OK\n")
	script.WriteString(`echo "BUZZ_TEST_CODEX_HOME=${CODEX_HOME:-}"` + "\n")
	// A CHILD shell: only an EXPORTED value crosses this boundary, and a
	// child is exactly what codex is.
	script.WriteString(`sh -c 'echo "BUZZ_TEST_EXPORTED_HOME=${CODEX_HOME:-}"'` + "\n")
	if checkLeaks {
		script.WriteString("echo BUZZ_TEST_ENV_BEGIN\n")
		script.WriteString("env\n")
		script.WriteString("echo BUZZ_TEST_ENV_END\n")
	}

	cmd := exec.Command("sh", "-c", script.String())
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	exitCode := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run shell: %v (stderr: %s)", err, stderr.String())
		}
		exitCode = exitErr.ExitCode()
	}

	out := stdout.String()
	if exitCode == 0 && !strings.Contains(out, "BUZZ_TEST_SOURCED_OK") {
		t.Fatalf("snippet aborted the sourcing shell before it could continue.\nstdout: %s\nstderr: %s", out, stderr.String())
	}

	res := codexResult{exitCode: exitCode}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "BUZZ_TEST_CODEX_HOME="); ok {
			res.codexHome = v
		}
		if v, ok := strings.CutPrefix(line, "BUZZ_TEST_EXPORTED_HOME="); ok {
			res.exportedHome = v
		}
	}
	if checkLeaks {
		if b, a, ok := strings.Cut(out, "BUZZ_TEST_ENV_BEGIN\n"); ok {
			_ = b
			if envBlock, _, ok := strings.Cut(a, "BUZZ_TEST_ENV_END"); ok {
				res.env = envBlock
			}
		}
	}
	if res.codexHome != "" {
		cfgPath := filepath.Join(res.codexHome, "config.toml")
		if fi, err := os.Stat(cfgPath); err == nil {
			res.exists = true
			res.mode = fi.Mode().Perm()
			raw, rerr := os.ReadFile(cfgPath)
			if rerr != nil {
				t.Fatalf("read generated config: %v", rerr)
			}
			res.config = string(raw)
		}
	}
	return res
}

const codexStrictPrelude = "set -eu"

// TestCodexEnv_WritesConfigWhenHostDerivable is the happy path: a derivable
// host yields a config.toml pointed at the gateway's codex surface, with the
// token supplied by reference (env_key) rather than written into the file.
func TestCodexEnv_WritesConfigWhenHostDerivable(t *testing.T) {
	res := sourceCodexSnippet(t, t.TempDir(), map[string]string{
		"DATABRICKS_HOST":  "https://example.databricks.com",
		"DATABRICKS_TOKEN": "dapi-marker-secret",
	}, codexStrictPrelude, false)

	if !res.exists {
		t.Fatal("expected a generated config.toml")
	}
	for _, want := range []string{
		`base_url = "https://example.databricks.com/ai-gateway/codex/v1"`,
		`wire_api = "responses"`,
		`env_key = "DATABRICKS_TOKEN"`,
		`model = "` + CodexDefaultModel + `"`,
	} {
		if !strings.Contains(res.config, want) {
			t.Errorf("generated config missing %q:\n%s", want, res.config)
		}
	}
	// The token is referenced by NAME, never materialized into the file.
	if strings.Contains(res.config, "dapi-marker-secret") {
		t.Error("the generated config must never contain the token value itself")
	}
	if res.mode != 0o600 {
		t.Errorf("generated config mode = %v, want 0600", res.mode)
	}
	if res.exportedHome != res.codexHome {
		t.Errorf("CODEX_HOME is not visible to child processes (child saw %q, shell has %q)", res.exportedHome, res.codexHome)
	}
}

// TestCodexEnv_StaleConfigRemovedOnRedeploy is the decisive test for this
// snippet, and the one a stateless-gate mental model gets wrong.
//
// It models a real redeploy: TWO separate shells sharing ONE persistent
// $HOME. The first has a host and writes a config. The second has a token
// but NO host — the exact case a regenerated ~/.databrickscfg produces, and
// the case the gate exists for. Without the unconditional removal, deploy
// #1's file survives and codex sends the CURRENT token to the PREVIOUS host,
// on a deploy that reports healthy.
//
// A version of this test that started from a clean temp dir would pass
// vacuously. The shared $HOME is the whole assertion.
func TestCodexEnv_StaleConfigRemovedOnRedeploy(t *testing.T) {
	home := t.TempDir()

	first := sourceCodexSnippet(t, home, map[string]string{
		"DATABRICKS_HOST":  "https://old-host.databricks.com",
		"DATABRICKS_TOKEN": "dapi-old",
	}, codexStrictPrelude, false)
	if !first.exists {
		t.Fatal("setup: first deploy should have written a config")
	}
	if !strings.Contains(first.config, "old-host") {
		t.Fatalf("setup: unexpected first config:\n%s", first.config)
	}

	// Redeploy: token present (SandboxAuthSnippet exports it UNGATED on
	// host), host absent.
	second := sourceCodexSnippet(t, home, map[string]string{
		"DATABRICKS_TOKEN": "dapi-current",
	}, codexStrictPrelude, false)

	if second.exists {
		t.Errorf("stale config survived a redeploy with no host; codex would send the current token to the previous host:\n%s", second.config)
	}

	// And a redeploy that DOES have a host must rewrite in place at the
	// stable path rather than be redirected to a fallback directory.
	//
	// This half is what makes the removal itself load-bearing rather than
	// incidental. Without it the test passes even with `rm -f` deleted:
	// the leftover file trips the removal-failed branch, CODEX_HOME is
	// redirected to a fresh mkdtemp, and "no stale config at CODEX_HOME"
	// holds for the wrong reason — while every launch silently leaks a new
	// directory. Found by mutating the snippet, not by reading it.
	third := sourceCodexSnippet(t, home, map[string]string{
		"DATABRICKS_HOST":  "https://new-host.databricks.com",
		"DATABRICKS_TOKEN": "dapi-current",
	}, codexStrictPrelude, false)

	if want := filepath.Join(home, ".buzz-backend", "codex"); third.codexHome != want {
		t.Errorf("redeploy was redirected to %q instead of rewriting in place at %q — the stale config was not removed", third.codexHome, want)
	}
	if !third.exists {
		t.Fatal("redeploy with a host must write a config")
	}
	if strings.Contains(third.config, "old-host") {
		t.Errorf("redeploy served the PREVIOUS host:\n%s", third.config)
	}
	if !strings.Contains(third.config, "new-host") {
		t.Errorf("redeploy did not pick up the current host:\n%s", third.config)
	}
}

// TestCodexEnv_ExportsCodexHomeEvenWithoutHost pins the invariant that keeps
// the fail-closed path from being a bypass. With no host, no config is
// written — but CODEX_HOME must STILL be exported, because otherwise codex
// falls back to ~/.codex/config.toml, which in a Lakebox sandbox is a
// symlink to the image's baked gateway config whose auth.command reads the
// workspace PAT straight out of ~/.databrickscfg. A declined gate would then
// produce a fully working agent running on an owner-level credential,
// outside every provider gate.
func TestCodexEnv_ExportsCodexHomeEvenWithoutHost(t *testing.T) {
	res := sourceCodexSnippet(t, t.TempDir(), map[string]string{
		"DATABRICKS_TOKEN": "dapi-marker-secret",
	}, codexStrictPrelude, false)

	if res.codexHome == "" {
		t.Fatal("CODEX_HOME must be set even when no config is written, or codex falls back to the image's ~/.codex symlink")
	}
	// Exported, not merely assigned: codex runs as a child process, so a
	// bare assignment would leave it reading ~/.codex/config.toml — the
	// image symlink whose auth.command reads the workspace PAT.
	if res.exportedHome != res.codexHome {
		t.Errorf("CODEX_HOME is not visible to child processes (child saw %q, shell has %q)", res.exportedHome, res.codexHome)
	}
	if res.exists {
		t.Errorf("no config may be written without a derived host, got:\n%s", res.config)
	}
}

// TestCodexEnv_FailClosedWhenRemovalFails is the C1 post-condition. `rm -f`
// is `|| :`-guarded like every other filesystem statement in the snippet, so
// a failed removal is swallowed. If the snippet then fell through to the
// gate, a stale config would survive behind a declining gate — the exact
// leak the removal exists to prevent.
//
// Asserting only "the shell still exits 0" would PASS while demonstrating
// the hole. What must hold is that the agent is not left pointed at a config
// this launch did not write.
func TestCodexEnv_FailClosedWhenRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so an unremovable file cannot be staged")
	}
	home := t.TempDir()

	// Stage a stale config in a directory that cannot be modified.
	codexDir := filepath.Join(home, ".buzz-backend", "codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(stale, []byte("base_url = \"https://attacker.example\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(codexDir, 0o500); err != nil { // r-x: unlink denied
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(codexDir, 0o700) })

	res := sourceCodexSnippet(t, home, map[string]string{
		"DATABRICKS_HOST":  "https://example.databricks.com",
		"DATABRICKS_TOKEN": "dapi-marker-secret",
	}, codexStrictPrelude, false)

	if res.exitCode != 0 {
		t.Fatalf("snippet must never abort the sourcing shell, got exit %d", res.exitCode)
	}
	if res.codexHome == codexDir {
		t.Fatal("CODEX_HOME must be redirected away from a directory whose stale config could not be removed")
	}
	if res.exists && strings.Contains(res.config, "attacker.example") {
		t.Fatal("the agent must never be left pointed at a config this launch did not write")
	}
	// And the stale file is untouched where it was — we redirected rather
	// than pretending to have cleaned it.
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("stale file should still exist (we could not remove it): %v", err)
	}
}

// TestCodexEnv_OwnerSuppliedCodexHomeUntouched pins bring-your-own-config:
// when the owner supplies CODEX_HOME the snippet does nothing at all — no
// export, no removal, no write. Note the payload layer separately REJECTS
// this combination under inference_auth="sandbox", where the credential in
// play is a workspace-owner PAT the payload never handled.
func TestCodexEnv_OwnerSuppliedCodexHomeUntouched(t *testing.T) {
	home := t.TempDir()
	owner := filepath.Join(home, "mine")
	if err := os.MkdirAll(owner, 0o700); err != nil {
		t.Fatal(err)
	}
	ownCfg := filepath.Join(owner, "config.toml")
	const ownContent = "# owner's own\nmodel = \"gpt-5\"\n"
	if err := os.WriteFile(ownCfg, []byte(ownContent), 0o600); err != nil {
		t.Fatal(err)
	}

	res := sourceCodexSnippet(t, home, map[string]string{
		"CODEX_HOME":       owner,
		"DATABRICKS_HOST":  "https://example.databricks.com",
		"DATABRICKS_TOKEN": "dapi-marker-secret",
	}, codexStrictPrelude, false)

	if res.codexHome != owner {
		t.Errorf("owner-supplied CODEX_HOME = %q, was rewritten to %q", owner, res.codexHome)
	}
	got, err := os.ReadFile(ownCfg)
	if err != nil {
		t.Fatalf("owner's config was removed: %v", err)
	}
	if string(got) != ownContent {
		t.Errorf("owner's config was overwritten:\n%s", got)
	}
}

// TestCodexEnv_SurvivesUnwritableHome proves the `set -e` hardening. A bare
// mkdir/rm/heredoc failure under `set -e` aborts the SOURCING shell before
// the trailing `:` is ever reached — the trailing `:` alone does not deliver
// the never-return-non-zero invariant, which draft review of this change got
// wrong. Staging $HOME/.buzz-backend as a regular FILE makes `mkdir -p` fail.
func TestCodexEnv_SurvivesUnwritableHome(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".buzz-backend"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := sourceCodexSnippet(t, home, map[string]string{
		"DATABRICKS_HOST":  "https://example.databricks.com",
		"DATABRICKS_TOKEN": "dapi-marker-secret",
	}, codexStrictPrelude, false)

	if res.exitCode != 0 {
		t.Fatalf("snippet must not abort the launch when the filesystem refuses it, got exit %d", res.exitCode)
	}
	if res.exists {
		t.Error("no config should have been written when its directory could not be created")
	}
}

// TestCodexEnv_NoScratchVarLeakUnderSetA is not hypothetical: the verify
// handshake sources this file under `set -a`, so any surviving temporary
// would be exported into the agent's own environment.
func TestCodexEnv_NoScratchVarLeakUnderSetA(t *testing.T) {
	res := sourceCodexSnippet(t, t.TempDir(), map[string]string{
		"DATABRICKS_HOST":  "https://example.databricks.com",
		"DATABRICKS_TOKEN": "dapi-marker-secret",
	}, "set -eu\nset -a", true)

	for _, line := range strings.Split(res.env, "\n") {
		if strings.HasPrefix(line, "buzz_") {
			t.Errorf("scratch variable leaked into the agent environment: %s", line)
		}
	}
	// CODEX_HOME is the one variable this snippet is SUPPOSED to export.
	if !strings.Contains(res.env, "CODEX_HOME=") {
		t.Error("CODEX_HOME must be exported")
	}
}

// TestCodexEnv_HostNormalization covers the shapes the host can arrive in:
// SandboxAuthSnippet reads ~/.databrickscfg without scheme handling, and an
// owner may equally write a bare hostname or a trailing slash.
func TestCodexEnv_HostNormalization(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"https://x.databricks.com", "https://x.databricks.com/ai-gateway/codex/v1"},
		{"https://x.databricks.com/", "https://x.databricks.com/ai-gateway/codex/v1"},
		{"x.databricks.com", "https://x.databricks.com/ai-gateway/codex/v1"},
		{"http://x.databricks.com", "http://x.databricks.com/ai-gateway/codex/v1"},
	} {
		res := sourceCodexSnippet(t, t.TempDir(), map[string]string{
			"DATABRICKS_HOST":  tc.host,
			"DATABRICKS_TOKEN": "dapi-marker-secret",
		}, codexStrictPrelude, false)
		want := `base_url = "` + tc.want + `"`
		if !strings.Contains(res.config, want) {
			t.Errorf("host %q: config missing %q:\n%s", tc.host, want, res.config)
		}
	}
}

// TestCodexEnv_HostileHostWritesNoConfig is the TOML-injection regression
// test. DATABRICKS_HOST arrives unvalidated from either source and is
// interpolated into a TOML file, so a value carrying a quote and a newline
// closes base_url and appends attacker-authored TOML — including a
// [model_providers.*.auth] block whose `command` codex runs at startup,
// with no shell tool, no permission request, and nothing in the transcript.
//
// Note this was never SHELL injection: the result of a parameter expansion
// is not re-scanned, so `$(...)` in the value stays literal. Writing a
// config this provider did not author is enough on its own.
func TestCodexEnv_HostileHostWritesNoConfig(t *testing.T) {
	hostile := []struct{ name, host string }{
		{"quote and newline", "https://evil.example\"\n[model_providers.pwn.auth]\ncommand = \"sh\"\n#"},
		{"bare newline", "https://evil.example\nkey = 1"},
		{"command substitution", "https://evil.example$(id)"},
		{"backtick", "https://evil.example`id`"},
		{"space", "https://evil.example and more"},
	}
	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			res := sourceCodexSnippet(t, t.TempDir(), map[string]string{
				"DATABRICKS_HOST":  tc.host,
				"DATABRICKS_TOKEN": "dapi-marker-secret",
			}, codexStrictPrelude, false)

			if res.exitCode != 0 {
				t.Fatalf("a hostile host must not abort the launch, got exit %d", res.exitCode)
			}
			if res.exists {
				t.Errorf("a host that is not URL-shaped must produce NO config, got:\n%s", res.config)
			}
			// And CODEX_HOME is still exported, so codex cannot fall back
			// to the image's ~/.codex symlink.
			if res.codexHome == "" {
				t.Error("CODEX_HOME must still be exported when the host is refused")
			}
		})
	}
}

// TestCodexEnv_WellFormedHostsStillAccepted keeps the charset gate from
// becoming a denial of service against legitimate hosts.
func TestCodexEnv_WellFormedHostsStillAccepted(t *testing.T) {
	for _, host := range []string{
		"https://workspace.example.invalid",
		"workspace.example.invalid",
		"http://localhost:8080",
		"https://example.databricks.com/",
		"https://my_workspace.example.com",
	} {
		res := sourceCodexSnippet(t, t.TempDir(), map[string]string{
			"DATABRICKS_HOST":  host,
			"DATABRICKS_TOKEN": "dapi-marker-secret",
		}, codexStrictPrelude, false)
		if !res.exists {
			t.Errorf("well-formed host %q must still produce a config", host)
		}
	}
}

// TestCodexEnv_FailClosedWhenRemovalAndMktempBothFail covers the
// correlated double failure the first fallback missed: `rm -f` fails
// because the tree is unwritable, and the first `mktemp -d` creates its
// directory inside that SAME tree, so it fails for exactly the same
// reason. A single attempt left CODEX_HOME pointed at the stale config
// while writing nothing new — the precise "current token, previous host"
// state this branch exists to prevent.
func TestCodexEnv_FailClosedWhenRemovalAndMktempBothFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	codexDir := filepath.Join(home, ".buzz-backend", "codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(stale, []byte("base_url = \"https://STALE-previous-host.example/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Both the config's own directory AND its parent are unwritable, so
	// the removal and the in-tree mktemp fail together.
	if err := os.Chmod(codexDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(home, ".buzz-backend"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(home, ".buzz-backend"), 0o700)
		_ = os.Chmod(codexDir, 0o700)
	})

	res := sourceCodexSnippet(t, home, map[string]string{
		"DATABRICKS_HOST":  "https://current.databricks.com",
		"DATABRICKS_TOKEN": "dapi-marker-secret",
	}, codexStrictPrelude, false)

	if res.exitCode != 0 {
		t.Fatalf("snippet must never abort the launch, got exit %d", res.exitCode)
	}
	if res.codexHome == codexDir {
		t.Fatal("CODEX_HOME must be redirected away from the directory holding the un-removable stale config")
	}
	if res.exists && strings.Contains(res.config, "STALE-previous-host") {
		t.Fatalf("the agent must never run against a config this launch did not write, got:\n%s", res.config)
	}
}

// TestCodexEnv_InheritedScratchVarsIgnored is the general guard for a
// regression that a fix introduced.
//
// Moving the config write out of the derivation branch (so an invalid host
// could decline it) decoupled the write gate from the charset check. Because
// agent.env_vars render BEFORE this snippet and every scratch name here
// matches payload's env_vars key charset (^[A-Za-z_][A-Za-z0-9_]*$ — lower
// case and underscores are legal), an owner could pre-export buzz_codex_url
// and reach the heredoc without ever passing the charset gate. That restored
// the exact TOML-injection hole the gate was added to close.
//
// ClaudeEnvSnippet opens with a bare `buzz_derived_url=` for precisely this
// reason; the refactor did not copy it. This test asserts the property for
// EVERY scratch variable rather than just the one that was wrong, since the
// next refactor may pick a different one.
func TestCodexEnv_InheritedScratchVarsIgnored(t *testing.T) {
	hostile := "https://attacker.example/v1\"\n[model_providers.pwn.auth]\ncommand = \"sh\"\n#"

	// Each case must actually EXERCISE the variable it names, or the subtest
	// is decoration: with no DATABRICKS_HOST, buzz_codex_h and
	// buzz_codex_alt are never read at all, so deleting their initialization
	// left those subtests green. Found by mutation; each now runs under the
	// conditions where its variable is live.
	for _, tc := range []struct {
		name   string
		preset map[string]string
	}{
		// Gates the write. Live with no host — that is the bypass.
		{"buzz_codex_url", map[string]string{"buzz_codex_url": hostile}},
		// Only read on the derivation path, so it needs a valid host.
		{"buzz_codex_h", map[string]string{
			"buzz_codex_h":     hostile,
			"DATABRICKS_HOST":  "https://real.databricks.com",
			"DATABRICKS_TOKEN": "dapi-marker-secret",
		}},
		// Names the file the gate removes and writes.
		{"buzz_codex_cfg", map[string]string{
			"buzz_codex_cfg":   hostile,
			"DATABRICKS_HOST":  "https://real.databricks.com",
			"DATABRICKS_TOKEN": "dapi-marker-secret",
		}},
		// Only read on the removal-failed path.
		{"buzz_codex_alt", map[string]string{"buzz_codex_alt": hostile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := sourceCodexSnippet(t, t.TempDir(), tc.preset, codexStrictPrelude, false)

			if res.exitCode != 0 {
				t.Fatalf("an inherited scratch var must not abort the launch, got exit %d", res.exitCode)
			}
			// Whatever else happens, the hostile value must never reach the
			// generated config, and CODEX_HOME must stay under the provider
			// root rather than being redirected by an inherited value.
			if strings.Contains(res.config, "attacker.example") || strings.Contains(res.config, "model_providers.pwn") {
				t.Errorf("inherited %s reached the generated config:\n%s", tc.name, res.config)
			}
			if !strings.Contains(res.codexHome, ".buzz-backend") && !strings.Contains(res.codexHome, "codex.") {
				t.Errorf("inherited %s redirected CODEX_HOME to %q", tc.name, res.codexHome)
			}
		})
	}

	// And with a legitimate host present, an inherited value must not
	// displace the derived one.
	res := sourceCodexSnippet(t, t.TempDir(), map[string]string{
		"buzz_codex_url":   hostile,
		"DATABRICKS_HOST":  "https://real.databricks.com",
		"DATABRICKS_TOKEN": "dapi-marker-secret",
	}, codexStrictPrelude, false)
	if !res.exists {
		t.Fatal("a derivable host must still produce a config")
	}
	if !strings.Contains(res.config, `base_url = "https://real.databricks.com/ai-gateway/codex/v1"`) {
		t.Errorf("the derived host must win over an inherited scratch var:\n%s", res.config)
	}
	if strings.Contains(res.config, "attacker.example") {
		t.Errorf("inherited value reached the generated config:\n%s", res.config)
	}
}
