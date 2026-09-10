// Package mcpprobe implements local managed-MCP probing with the native HTTP
// transport. Workspace connection inputs are always supplied explicitly by the
// caller; this package never resolves profiles or executes the deployed helper.
package mcpprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/IceRhymers/buzz-lakebox/internal/httpmcp"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpops"
)

const (
	defaultRequestTimeout = 2 * time.Minute
	defaultCloseWait      = 2 * time.Second
)

// Adapter is an mcpops.Prober backed by the same native HTTP MCP bridge used
// by bzhttpmcp. Host and token are constructor inputs rather than process-global
// configuration or profile names.
type Adapter struct {
	host         string
	token        string
	client       *http.Client
	start        bridgeFactory
	closeTimeout time.Duration
}

type bridgeFactory func(context.Context, *http.Client, *url.URL, string) childProcess

var _ mcpops.Prober = (*Adapter)(nil)

// New constructs a prober for one explicitly selected workspace. The token is
// retained only for request authorization and error redaction.
func New(host, token string) (*Adapter, error) {
	if strings.TrimSpace(host) == "" {
		return nil, errors.New("databricks host is required")
	}
	if !validConnectionValue(host) {
		return nil, errors.New("databricks host is invalid")
	}
	if err := httpmcp.ValidateWorkspaceHost(host); err != nil {
		return nil, errors.New("databricks host is invalid")
	}
	if token == "" {
		return nil, errors.New("databricks token is required")
	}
	if !validConnectionValue(token) || httpmcp.ValidateHeaderValue(token) != nil {
		return nil, errors.New("databricks token is invalid")
	}
	return &Adapter{
		host:         host,
		token:        token,
		client:       httpmcp.NewHTTPClient(defaultRequestTimeout),
		start:        newInProcessBridge,
		closeTimeout: defaultCloseWait,
	}, nil
}

func validConnectionValue(value string) bool {
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

// Initialize starts an in-process native bridge, then performs the MCP
// initialize exchange and initialized notification. No helper is written to
// disk or executed, so local probing is independent of the deployed binary's
// target operating system and architecture.
func (a *Adapter) Initialize(ctx context.Context, server mcpconfig.Server) (mcpops.InitializedServer, error) {
	if a == nil || a.client == nil || a.start == nil {
		return nil, errors.New("managed MCP probe adapter is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRemoteServer(server); err != nil {
		return nil, a.safeError(err)
	}

	opts, err := endpointOptions(server)
	if err != nil {
		return nil, a.safeError(err)
	}
	endpoint, err := httpmcp.BuildEndpoint(a.host, opts)
	if err != nil {
		return nil, a.safeError(fmt.Errorf("prepare managed MCP endpoint: %w", err))
	}
	proc := a.start(ctx, a.client, endpoint, a.token)
	if proc == nil {
		return nil, errors.New("managed MCP probe transport is not configured")
	}
	s := newSession(proc, nil, a.token, a.host, a.closeTimeout)

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
		return nil, a.safeError(fmt.Errorf("initialize managed MCP: %w", err))
	}
	var initialized initializeResult
	if err := decodeResult(result, &initialized); err != nil {
		_ = s.Close()
		return nil, a.safeError(fmt.Errorf("initialize managed MCP: %w", err))
	}
	if !supportedProtocolVersion(initialized.ProtocolVersion) {
		_ = s.Close()
		return nil, a.safeError(fmt.Errorf("initialize managed MCP: server selected unsupported MCP protocol version %q", initialized.ProtocolVersion))
	}
	s.protocolVersion = initialized.ProtocolVersion
	if err := s.notify("notifications/initialized", struct{}{}); err != nil {
		_ = s.Close()
		return nil, a.safeError(fmt.Errorf("initialize managed MCP notification: %w", err))
	}
	return s, nil
}

func validateRemoteServer(server mcpconfig.Server) error {
	if server.Kind == mcpconfig.KindLocal {
		return errors.New("local MCP commands cannot be probed through the managed HTTP transport")
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

// endpointOptions maps validated managed config into the single shared typed
// endpoint builder used by the deployed bridge.
func endpointOptions(server mcpconfig.Server) (httpmcp.Options, error) {
	opts := httpmcp.Options{Kind: httpmcp.Kind(server.Kind)}
	if server.Kind == mcpconfig.KindSkills {
		if len(server.Resource)%2 != 0 {
			return httpmcp.Options{}, errors.New("skills resources must be catalog/schema pairs")
		}
		for i := 0; i < len(server.Resource); i += 2 {
			opts.Schemas = append(opts.Schemas, server.Resource[i]+"."+server.Resource[i+1])
		}
		return opts, nil
	}
	opts.Resources = append([]string(nil), server.Resource...)
	return opts, nil
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

type childProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Wait() error
	Kill() error
}

type inProcessBridge struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	cancel context.CancelFunc
	done   chan error
	kill   sync.Once
}

func newInProcessBridge(parent context.Context, client *http.Client, endpoint *url.URL, token string) childProcess {
	bridgeCtx, cancel := context.WithCancel(parent)
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	proc := &inProcessBridge{
		stdin:  stdinW,
		stdout: stdoutR,
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() {
		err := httpmcp.NewBridge(client, endpoint, token, stdinR, stdoutW, io.Discard).Run(bridgeCtx)
		_ = stdinR.Close()
		_ = stdoutW.Close()
		proc.done <- err
	}()
	return proc
}

func (p *inProcessBridge) Stdin() io.WriteCloser { return p.stdin }
func (p *inProcessBridge) Stdout() io.ReadCloser { return p.stdout }
func (p *inProcessBridge) Wait() error           { return <-p.done }
func (p *inProcessBridge) Kill() error {
	p.kill.Do(func() {
		p.cancel()
		_ = p.stdin.CloseWithError(context.Canceled)
		_ = p.stdout.CloseWithError(context.Canceled)
	})
	return nil
}
