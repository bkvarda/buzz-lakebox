package mcpops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/redact"
)

const DefaultProbeTimeout = 15 * time.Second

// Tool is the portable subset of an MCP tools/list entry needed by an
// operator-facing probe result.
type Tool struct {
	Name string `json:"name"`
}

// Prober starts an MCP initialize handshake for server. The server has already
// passed mcpconfig validation. A real adapter may resolve the typed endpoint and
// credential while tests return an in-memory fake session.
type Prober interface {
	Initialize(context.Context, mcpconfig.Server) (InitializedServer, error)
}

// InitializedServer is the initialized connection returned by a Prober. Probe
// calls ListTools exactly once and then Close. Both initialize and tools/list
// receive the same per-server deadline.
type InitializedServer interface {
	ListTools(context.Context) ([]Tool, error)
	Close() error
}

// ProbeOptions controls the deadline applied independently to each server. A
// zero Timeout selects DefaultProbeTimeout; negative values are rejected.
type ProbeOptions struct {
	Timeout time.Duration
}

// ProbeResult reports every configured server, including failures. Server
// order matches the validated managed config.
type ProbeResult struct {
	Servers []ServerProbe `json:"servers"`
}

// ServerProbe contains the public, portable outcome for one server. Deadline
// is the exact per-server deadline supplied to the injected prober. Error is
// already redacted and safe to render; successful entries leave it empty.
type ServerProbe struct {
	Name      string         `json:"name"`
	Kind      mcpconfig.Kind `json:"kind"`
	Deadline  time.Time      `json:"deadline"`
	ToolCount int            `json:"tool_count"`
	ToolNames []string       `json:"tool_names"`
	Error     string         `json:"error,omitempty"`
}

// Probe parses and validates one complete buzz-managed-mcp document, then asks
// the injected prober to perform initialize + tools/list for each server. An
// individual handshake failure is captured in that server's redacted Error and
// does not prevent later servers from being tested. Parse/setup failures are
// returned as function errors because no per-server probe could be performed.
func Probe(ctx context.Context, document []byte, prober Prober, opts ProbeOptions) (ProbeResult, error) {
	if prober == nil {
		return ProbeResult{}, errors.New("managed MCP prober is required")
	}
	if opts.Timeout < 0 {
		return ProbeResult{}, errors.New("managed MCP probe timeout must not be negative")
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultProbeTimeout
	}

	cfg, err := mcpconfig.Parse(document)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("validate managed MCP config: %s", redact.Log(err.Error()))
	}

	result := ProbeResult{Servers: make([]ServerProbe, 0, len(cfg.Servers))}
	for _, server := range cfg.Servers {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		deadline, _ := probeCtx.Deadline()
		session, probeErr := prober.Initialize(probeCtx, cloneServer(server))
		var tools []Tool
		if probeErr == nil {
			if session == nil {
				probeErr = errors.New("initialize returned no MCP session")
			} else {
				tools, probeErr = session.ListTools(probeCtx)
				if closeErr := session.Close(); probeErr == nil && closeErr != nil {
					probeErr = fmt.Errorf("close MCP session: %w", closeErr)
				}
			}
		}
		cancel()

		entry := ServerProbe{
			Name:      server.Name,
			Kind:      server.Kind,
			Deadline:  deadline,
			ToolNames: []string{},
		}
		if probeErr != nil {
			entry.Error = redact.Log(probeErr.Error())
		} else {
			entry.ToolNames = make([]string, 0, len(tools))
			seen := make(map[string]struct{}, len(tools))
			for _, tool := range tools {
				if tool.Name == "" {
					entry.Error = "tools/list returned a tool with an empty name"
					entry.ToolNames = []string{}
					break
				}
				if _, ok := seen[tool.Name]; ok {
					entry.Error = fmt.Sprintf("tools/list returned duplicate tool name %q", tool.Name)
					entry.ToolNames = []string{}
					break
				}
				seen[tool.Name] = struct{}{}
				entry.ToolNames = append(entry.ToolNames, tool.Name)
			}
			if entry.Error == "" {
				sort.Strings(entry.ToolNames)
				entry.ToolCount = len(entry.ToolNames)
			}
		}
		result.Servers = append(result.Servers, entry)
	}
	return result, nil
}

func cloneServer(server mcpconfig.Server) mcpconfig.Server {
	server.Resource = append([]string(nil), server.Resource...)
	server.Args = append([]string(nil), server.Args...)
	server.InheritEnv = append([]string(nil), server.InheritEnv...)
	return server
}
