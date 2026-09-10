package sshx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeDatabricks writes a POSIX shell shim standing in for the real
// `databricks` binary. It records every invocation's argv and stdin to
// FAKE_LOG (one block per call, "ARGS:"/"STDIN:" lines) so tests can
// assert both call shape and that secrets only ever arrive via stdin
// (PLAN.md §7).
func writeFakeDatabricks(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake databricks shim is a POSIX shell script")
	}
	script := `#!/bin/sh
log="${FAKE_LOG:?FAKE_LOG must be set}"
{
  echo "ARGS:$*"
  printf 'STDIN:'
  cat
  echo
  echo "---"
} >> "$log"
exit "${FAKE_EXIT:-0}"
`
	path := filepath.Join(dir, "databricks")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake databricks: %v", err)
	}
	return path
}

// TestBinName_ExplicitOverrideSkipsFallback: a non-default Bin (the test
// shim pattern) is used verbatim, never fallback-resolved. The
// PATH-then-fallback resolution itself is covered hermetically in
// internal/lakebox (TestResolveDefaultBin_*); sshx shares that resolver
// via lakebox.DefaultBinPath.
func TestBinName_ExplicitOverrideSkipsFallback(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "databricks")
	c := &Client{Bin: shim}
	if got := c.binName(); got != shim {
		t.Fatalf("binName = %q, want explicit override %q", got, shim)
	}
}

func TestRun_NoStdin(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	logFile := filepath.Join(dir, "log.txt")
	t.Setenv("FAKE_LOG", logFile)

	c := &Client{Bin: filepath.Join(dir, "databricks")}
	if _, err := c.Run(context.Background(), "EXAMPLE_PROFILE", "sandbox-1", "echo hi"); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	data, _ := os.ReadFile(logFile)
	log := string(data)
	if !strings.Contains(log, "ARGS:sandbox ssh sandbox-1 -p EXAMPLE_PROFILE -- echo hi") {
		t.Fatalf("log missing expected argv shape: %q", log)
	}
	if !strings.Contains(log, "STDIN:\n") {
		t.Fatalf("expected empty stdin for Run(), got: %q", log)
	}
}

func TestRunWithStdin_TransportsBytesVerbatim(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	logFile := filepath.Join(dir, "log.txt")
	t.Setenv("FAKE_LOG", logFile)

	c := &Client{Bin: filepath.Join(dir, "databricks")}
	secret := "nsec1supersecretvalue0000000000000000000000000000000000000"
	if _, err := c.RunWithStdin(context.Background(), "EXAMPLE_PROFILE", "sandbox-1", "cat > /tmp/x", strings.NewReader(secret)); err != nil {
		t.Fatalf("RunWithStdin() error: %v", err)
	}

	data, _ := os.ReadFile(logFile)
	log := string(data)
	if !strings.Contains(log, "STDIN:"+secret) {
		t.Fatalf("expected secret to arrive verbatim via stdin, got: %q", log)
	}
	// The secret must never appear on the ARGS line (argv), only via
	// stdin — the property PLAN.md §5/§7 requires.
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "ARGS:") && strings.Contains(line, secret) {
			t.Fatalf("secret leaked into argv: %q", line)
		}
	}
}

func TestRun_FailureIncludesOutputAndSandboxContext(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	logFile := filepath.Join(dir, "log.txt")
	t.Setenv("FAKE_LOG", logFile)
	t.Setenv("FAKE_EXIT", "1")

	c := &Client{Bin: filepath.Join(dir, "databricks")}
	_, err := c.Run(context.Background(), "EXAMPLE_PROFILE", "sandbox-99", "false")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "sandbox-99") {
		t.Fatalf("error %q should embed the sandbox id", err.Error())
	}
}
