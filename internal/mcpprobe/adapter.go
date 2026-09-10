// Package mcpprobe implements local managed-MCP probing by running the
// embedded bzhttpmcp stdio bridge. Workspace connection inputs are always
// supplied explicitly by the caller; this package never resolves profiles.
package mcpprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/IceRhymers/buzz-lakebox/internal/httpmcpbin"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpops"
)

const (
	helperName       = "bzhttpmcp"
	maxStderrBytes   = 64 * 1024
	defaultCloseWait = 2 * time.Second
)

// Adapter is an mcpops.Prober backed by a fresh local bzhttpmcp process for
// each server. Host and token are deliberately constructor inputs rather than
// process-global configuration or profile names.
type Adapter struct {
	host         string
	token        string
	helper       []byte
	start        startChildFunc
	tempParent   string
	closeTimeout time.Duration
	lookupEnv    func(string) (string, bool)
}

var _ mcpops.Prober = (*Adapter)(nil)

// New constructs a prober for one explicitly selected workspace. The token is
// retained only so it can be placed in each helper's clean environment and can
// be removed from every error returned by the adapter.
func New(host, token string) (*Adapter, error) {
	if strings.TrimSpace(host) == "" {
		return nil, errors.New("Databricks host is required")
	}
	if !validEnvValue(host) {
		return nil, errors.New("Databricks host is invalid")
	}
	if token == "" {
		return nil, errors.New("Databricks token is required")
	}
	if !validEnvValue(token) {
		return nil, errors.New("Databricks token is invalid")
	}
	return &Adapter{
		host:         host,
		token:        token,
		helper:       httpmcpbin.Binary,
		start:        startExecChild,
		closeTimeout: defaultCloseWait,
		lookupEnv:    os.LookupEnv,
	}, nil
}

func validEnvValue(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Initialize materializes and launches the embedded bridge, then performs the
// MCP initialize exchange and initialized notification. Local command servers
// do not use the HTTP bridge and are therefore rejected by this adapter.
func (a *Adapter) Initialize(ctx context.Context, server mcpconfig.Server) (mcpops.InitializedServer, error) {
	if a == nil || a.start == nil || len(a.helper) == 0 || a.lookupEnv == nil {
		return nil, errors.New("managed MCP probe adapter is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRemoteServer(server); err != nil {
		return nil, a.safeError(err)
	}

	args, err := bridgeArgs(server)
	if err != nil {
		return nil, a.safeError(err)
	}
	path, cleanup, err := writePrivateHelper(a.tempParent, a.helper)
	if err != nil {
		return nil, a.safeError(fmt.Errorf("prepare MCP bridge: %w", err))
	}
	cleanupPending := true
	defer func() {
		if cleanupPending {
			cleanup()
		}
	}()

	stderr := &limitedBuffer{limit: maxStderrBytes}
	proc, err := a.start(ctx, path, args, a.childEnv(), stderr)
	if err != nil {
		return nil, a.safeError(fmt.Errorf("start MCP bridge: %w%s", err, stderr.suffix()))
	}

	s := newSession(proc, cleanup, stderr, a.token, a.host, a.closeTimeout)
	cleanupPending = false
	// POSIX permits unlinking a running executable. Do it immediately when
	// possible; session.Close removes the private directory and retries the
	// file removal on platforms that lock running executables.
	_ = os.Remove(path)

	result, err := s.call(ctx, "initialize", initializeParams{
		ProtocolVersion: offeredProtocolVersion,
		Capabilities:    clientCapabilities{},
		ClientInfo: clientInfo{
			Name:    "buzz-managed-mcp-prober",
			Version: "1",
		},
	})
	if err != nil {
		_ = s.Close()
		return nil, a.safeError(fmt.Errorf("initialize MCP bridge: %w", err))
	}
	var initialized initializeResult
	if err := decodeResult(result, &initialized); err != nil {
		_ = s.Close()
		return nil, a.safeError(fmt.Errorf("initialize MCP bridge: %w", err))
	}
	if !supportedProtocolVersion(initialized.ProtocolVersion) {
		_ = s.Close()
		return nil, a.safeError(fmt.Errorf("initialize MCP bridge: server selected unsupported MCP protocol version %q", initialized.ProtocolVersion))
	}
	s.protocolVersion = initialized.ProtocolVersion
	if err := s.notify("notifications/initialized", struct{}{}); err != nil {
		_ = s.Close()
		return nil, a.safeError(fmt.Errorf("initialize MCP bridge notification: %w", err))
	}
	return s, nil
}

func validateRemoteServer(server mcpconfig.Server) error {
	if server.Kind == mcpconfig.KindLocal {
		return errors.New("local MCP commands cannot be probed through the managed HTTP bridge")
	}
	cfg := mcpconfig.Config{
		Schema:  mcpconfig.CurrentSchema,
		Version: mcpconfig.CurrentVersion,
		Servers: []mcpconfig.Server{server},
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid managed MCP server: %w", err)
	}
	return nil
}

// bridgeArgs intentionally mirrors deployflow's typed bzhttpmcp mapping.
func bridgeArgs(server mcpconfig.Server) ([]string, error) {
	args := []string{"--kind", string(server.Kind)}
	if server.Kind == mcpconfig.KindSkills {
		if len(server.Resource)%2 != 0 {
			return nil, errors.New("skills resources must be catalog/schema pairs")
		}
		for i := 0; i < len(server.Resource); i += 2 {
			args = append(args, "--schema", server.Resource[i]+"."+server.Resource[i+1])
		}
		return args, nil
	}
	for _, component := range server.Resource {
		args = append(args, "--resource", component)
	}
	return args, nil
}

func (a *Adapter) childEnv() []string {
	env := map[string]string{
		"DATABRICKS_HOST":  a.host,
		"DATABRICKS_TOKEN": a.token,
	}
	// Keep the baseline identical to the existing child isolation model. These
	// are process-hygiene values, not credential/profile lookup inputs.
	for _, key := range []string{"HOME", "PATH", "TMPDIR"} {
		if value, ok := a.lookupEnv(key); ok {
			env[key] = value
		}
	}
	if runtime.GOOS == "windows" {
		if value, ok := a.lookupEnv("SystemRoot"); ok {
			env["SystemRoot"] = value
		}
	}
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

func (a *Adapter) safeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New(mcpops.RedactError(err, a.token, a.host))
}

func writePrivateHelper(parent string, binary []byte) (string, func(), error) {
	dir, err := os.MkdirTemp(parent, "buzz-mcpprobe-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := sync.OnceFunc(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		cleanup()
		return "", func() {}, err
	}
	path := filepath.Join(dir, helperName)
	if err := os.WriteFile(path, binary, 0700); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := os.Chmod(path, 0700); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

type childProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Wait() error
	Kill() error
}

type startChildFunc func(context.Context, string, []string, []string, io.Writer) (childProcess, error)

type execChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (c *execChild) Stdin() io.WriteCloser { return c.stdin }
func (c *execChild) Stdout() io.ReadCloser { return c.stdout }
func (c *execChild) Wait() error           { return c.cmd.Wait() }
func (c *execChild) Kill() error {
	if c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

func startExecChild(ctx context.Context, path string, args, env []string, stderr io.Writer) (childProcess, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append([]string(nil), env...)
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	return &execChild{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

type limitedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
		} else {
			_, _ = b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) suffix() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.TrimSpace(b.buf.String())
	if text == "" {
		return ""
	}
	return ": " + text
}
