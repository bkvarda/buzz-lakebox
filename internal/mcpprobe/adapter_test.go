package mcpprobe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
)

const (
	testHost  = "https://workspace.example.invalid"
	testToken = "placeholder-sensitive-token-value"
)

func testServer(kind mcpconfig.Kind, resources ...string) mcpconfig.Server {
	return mcpconfig.Server{
		Name:     "generic-server",
		Kind:     kind,
		Resource: append([]string(nil), resources...),
		Auth:     mcpconfig.AuthEnv,
	}
}

func TestNewRequiresExplicitConnectionInputs(t *testing.T) {
	for _, tc := range []struct {
		name, host, token string
	}{
		{name: "missing host", token: testToken},
		{name: "missing token", host: testHost},
		{name: "host control", host: testHost + "\n", token: testToken},
		{name: "token control", host: testHost, token: testToken + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.host, tc.token); err == nil {
				t.Fatal("New accepted invalid explicit inputs")
			}
		})
	}
	if _, err := New(testHost, testToken); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestBridgeArgsMatchDeployTypedMapping(t *testing.T) {
	tests := []struct {
		server mcpconfig.Server
		want   []string
	}{
		{testServer(mcpconfig.KindSQL), []string{"--kind", "sql"}},
		{testServer(mcpconfig.KindGenie, "space_example"), []string{"--kind", "genie", "--resource", "space_example"}},
		{testServer(mcpconfig.KindAISearch, "catalog", "schema", "index"), []string{"--kind", "ai-search", "--resource", "catalog", "--resource", "schema", "--resource", "index"}},
		{testServer(mcpconfig.KindVectorSearch, "catalog", "schema"), []string{"--kind", "vector-search", "--resource", "catalog", "--resource", "schema"}},
		{testServer(mcpconfig.KindFunctions, "catalog", "schema", "function"), []string{"--kind", "functions", "--resource", "catalog", "--resource", "schema", "--resource", "function"}},
		{testServer(mcpconfig.KindMCPService, "catalog", "schema", "service"), []string{"--kind", "mcp-service", "--resource", "catalog", "--resource", "schema", "--resource", "service"}},
		{testServer(mcpconfig.KindSkills, "catalog_a", "schema_a", "catalog_b", "schema_b"), []string{"--kind", "skills", "--schema", "catalog_a.schema_a", "--schema", "catalog_b.schema_b"}},
	}
	for _, tc := range tests {
		got, err := bridgeArgs(tc.server)
		if err != nil {
			t.Fatalf("bridgeArgs(%s): %v", tc.server.Kind, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("bridgeArgs(%s) = %v, want %v", tc.server.Kind, got, tc.want)
		}
	}
}

func TestAdapterHandshakeEnvironmentAndPrivateHelperLifecycle(t *testing.T) {
	t.Setenv("UNRELATED_PRIVATE_VALUE", "must-not-reach-child")
	adapter, err := New(testHost, testToken)
	if err != nil {
		t.Fatal(err)
	}
	adapter.helper = []byte("generic embedded helper bytes")
	adapter.tempParent = t.TempDir()
	adapter.closeTimeout = 50 * time.Millisecond
	adapter.lookupEnv = func(key string) (string, bool) {
		values := map[string]string{
			"HOME":   "/generic/home",
			"PATH":   "/generic/bin",
			"TMPDIR": "/generic/tmp",
		}
		value, ok := values[key]
		return value, ok
	}

	var capturedPath string
	var capturedArgs, capturedEnv []string
	var helperMode os.FileMode
	var parentMode os.FileMode
	fake := fakeHelperConfig{
		protocolVersion: "2025-03-26",
		tools:           []string{"generic_read", "generic_write"},
	}
	adapter.start = func(_ context.Context, path string, args, env []string, _ io.Writer) (childProcess, error) {
		capturedPath = path
		capturedArgs = append([]string(nil), args...)
		capturedEnv = append([]string(nil), env...)
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, statErr
		}
		helperMode = info.Mode().Perm()
		parent, statErr := os.Stat(filepath.Dir(path))
		if statErr != nil {
			return nil, statErr
		}
		parentMode = parent.Mode().Perm()
		bytes, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		if string(bytes) != "generic embedded helper bytes" {
			return nil, errors.New("unexpected helper bytes")
		}
		return newFakeProcess(fake), nil
	}

	session, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSkills, "catalog", "schema"))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if helperMode != 0700 || parentMode != 0700 {
		t.Fatalf("helper/parent modes = %04o/%04o, want 0700/0700", helperMode, parentMode)
	}
	if _, err := os.Stat(capturedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper still exists after launch: %v", err)
	}
	if got, want := capturedArgs, []string{"--kind", "skills", "--schema", "catalog.schema"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	if strings.Contains(strings.Join(capturedArgs, "\x00"), testToken) || strings.Contains(capturedPath, testToken) {
		t.Fatal("token appeared in argv or executable path")
	}

	wantEnv := []string{
		"DATABRICKS_HOST=" + testHost,
		"DATABRICKS_TOKEN=" + testToken,
		"HOME=/generic/home",
		"PATH=/generic/bin",
		"TMPDIR=/generic/tmp",
	}
	if runtime.GOOS == "windows" {
		// This lookup intentionally returns false for SystemRoot, so there is no
		// additional entry even on Windows.
	}
	sort.Strings(wantEnv)
	if !reflect.DeepEqual(capturedEnv, wantEnv) {
		t.Fatalf("child env = %v, want %v", capturedEnv, wantEnv)
	}

	tools, err := session.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := []string{tools[0].Name, tools[1].Name}; !reflect.DeepEqual(got, fake.tools) {
		t.Fatalf("tools = %v, want %v", got, fake.tools)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	entries, err := os.ReadDir(adapter.tempParent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary helper artifacts remain: %v", entries)
	}
}

func TestAdapterNegotiatesSupportedVersions(t *testing.T) {
	for version := range supportedProtocolVersions {
		t.Run(version, func(t *testing.T) {
			adapter := testAdapter(t, fakeHelperConfig{protocolVersion: version})
			initialized, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSQL))
			if err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if got := initialized.(*session).protocolVersion; got != version {
				t.Fatalf("negotiated version = %q, want %q", got, version)
			}
			if err := initialized.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAdapterRejectsUnsupportedNegotiatedVersion(t *testing.T) {
	adapter := testAdapter(t, fakeHelperConfig{protocolVersion: "2099-01-01"})
	_, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSQL))
	if err == nil || !strings.Contains(err.Error(), "unsupported MCP protocol version") {
		t.Fatalf("error = %v", err)
	}
}

func TestAdapterParsesJSONRPCAndRedactsErrors(t *testing.T) {
	adapter := testAdapter(t, fakeHelperConfig{
		protocolVersion: offeredProtocolVersion,
		listError:       "request rejected for " + testToken + " at " + testHost,
	})
	session, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSQL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), testHost) {
		t.Fatalf("sensitive connection input survived error redaction: %q", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") || !strings.Contains(err.Error(), "JSON-RPC error") {
		t.Fatalf("error lost safe context: %q", err)
	}
	_ = session.Close()
}

func TestAdapterMalformedJSONAndContextCancellation(t *testing.T) {
	t.Run("malformed JSON", func(t *testing.T) {
		adapter := testAdapter(t, fakeHelperConfig{malformedInitialize: true})
		_, err := adapter.Initialize(context.Background(), testServer(mcpconfig.KindSQL))
		if err == nil || !strings.Contains(err.Error(), "malformed JSON-RPC") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		adapter := testAdapter(t, fakeHelperConfig{neverReply: true})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := adapter.Initialize(ctx, testServer(mcpconfig.KindSQL))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context deadline", err)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("cancellation took too long: %v", elapsed)
		}
	})
}

func TestAdapterRejectsLocalServerWithoutStartingChild(t *testing.T) {
	adapter := testAdapter(t, fakeHelperConfig{})
	started := false
	adapter.start = func(context.Context, string, []string, []string, io.Writer) (childProcess, error) {
		started = true
		return nil, errors.New("unexpected")
	}
	server := mcpconfig.Server{Name: "generic-local", Kind: mcpconfig.KindLocal, Command: "generic-command"}
	if _, err := adapter.Initialize(context.Background(), server); err == nil {
		t.Fatal("local server accepted")
	}
	if started {
		t.Fatal("child started for local server")
	}
}

func testAdapter(t *testing.T, config fakeHelperConfig) *Adapter {
	t.Helper()
	adapter, err := New(testHost, testToken)
	if err != nil {
		t.Fatal(err)
	}
	adapter.helper = []byte("generic helper")
	adapter.tempParent = t.TempDir()
	adapter.closeTimeout = 20 * time.Millisecond
	adapter.lookupEnv = func(string) (string, bool) { return "", false }
	adapter.start = func(context.Context, string, []string, []string, io.Writer) (childProcess, error) {
		return newFakeProcess(config), nil
	}
	return adapter
}

type fakeHelperConfig struct {
	protocolVersion     string
	tools               []string
	listError           string
	malformedInitialize bool
	neverReply          bool
}

type fakeProcess struct {
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	kill    func()
	done    chan error
	wait    sync.Once
	waitErr error
}

func newFakeProcess(config fakeHelperConfig) *fakeProcess {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	process := &fakeProcess{stdinW: stdinW, stdoutR: stdoutR, done: make(chan error, 1)}
	var killOnce sync.Once
	process.kill = func() {
		killOnce.Do(func() {
			_ = stdinR.CloseWithError(errors.New("killed"))
			_ = stdinW.CloseWithError(errors.New("killed"))
			_ = stdoutW.CloseWithError(errors.New("killed"))
			_ = stdoutR.CloseWithError(errors.New("killed"))
		})
	}
	go func() {
		err := runFakeHelper(stdinR, stdoutW, config)
		_ = stdinR.Close()
		_ = stdoutW.Close()
		process.done <- err
	}()
	return process
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdinW }
func (p *fakeProcess) Stdout() io.ReadCloser { return p.stdoutR }
func (p *fakeProcess) Wait() error {
	p.wait.Do(func() { p.waitErr = <-p.done })
	return p.waitErr
}
func (p *fakeProcess) Kill() error { p.kill(); return nil }

func runFakeHelper(stdin io.Reader, stdout io.Writer, config fakeHelperConfig) error {
	scanner := bufio.NewScanner(stdin)
	encoder := json.NewEncoder(stdout)
	initialized := false
	for scanner.Scan() {
		var message struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return err
		}
		if config.neverReply {
			continue
		}
		switch message.Method {
		case "initialize":
			var params initializeParams
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return err
			}
			if params.ProtocolVersion != offeredProtocolVersion || params.ClientInfo.Name == "" {
				return errors.New("invalid initialize params")
			}
			if config.malformedInitialize {
				_, _ = io.WriteString(stdout, "not-json\n")
				continue
			}
			version := config.protocolVersion
			if version == "" {
				version = offeredProtocolVersion
			}
			if err := encoder.Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": message.ID,
				"result": map[string]interface{}{"protocolVersion": version, "capabilities": map[string]interface{}{}},
			}); err != nil {
				return err
			}
		case "notifications/initialized":
			if len(message.ID) != 0 {
				return errors.New("initialized notification had an id")
			}
			initialized = true
		case "tools/list":
			if !initialized {
				return errors.New("tools/list before initialized notification")
			}
			if config.listError != "" {
				if err := encoder.Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": message.ID,
					"error": map[string]interface{}{"code": -32000, "message": config.listError},
				}); err != nil {
					return err
				}
				continue
			}
			tools := make([]map[string]string, len(config.tools))
			for i, name := range config.tools {
				tools[i] = map[string]string{"name": name}
			}
			if err := encoder.Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": message.ID,
				"result": map[string]interface{}{"tools": tools},
			}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
