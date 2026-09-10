package mcpprobe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpops"
)

const (
	offeredProtocolVersion = "2025-06-18"
	maxFrameBytes          = 32 * 1024 * 1024
)

var supportedProtocolVersions = map[string]struct{}{
	"2024-11-05": {},
	"2025-03-26": {},
	"2025-06-18": {},
}

func supportedProtocolVersion(version string) bool {
	_, ok := supportedProtocolVersions[version]
	return ok
}

type clientCapabilities struct{}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      clientInfo         `json:"clientInfo"`
}

type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type callResult struct {
	result json.RawMessage
	err    error
}

type session struct {
	proc            childProcess
	stdin           io.WriteCloser
	cleanup         func()
	token           string
	host            string
	closeTimeout    time.Duration
	protocolVersion string

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan callResult
	readErr error
	done    chan struct{}

	closeOnce sync.Once
	closeErr  error
}

var _ mcpops.InitializedServer = (*session)(nil)

func newSession(proc childProcess, cleanup func(), token, host string, closeTimeout time.Duration) *session {
	s := &session{
		proc:         proc,
		stdin:        proc.Stdin(),
		cleanup:      cleanup,
		token:        token,
		host:         host,
		closeTimeout: closeTimeout,
		nextID:       1,
		pending:      make(map[int64]chan callResult),
		done:         make(chan struct{}),
	}
	go s.readLoop(proc.Stdout())
	return s
}

func (s *session) ListTools(ctx context.Context) ([]mcpops.Tool, error) {
	if s == nil {
		return nil, errors.New("MCP session is not configured")
	}
	result, err := s.call(ctx, "tools/list", struct{}{})
	if err != nil {
		return nil, s.safeError(fmt.Errorf("tools/list: %w", err))
	}
	var response struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := decodeResult(result, &response); err != nil {
		return nil, s.safeError(fmt.Errorf("tools/list: %w", err))
	}
	tools := make([]mcpops.Tool, len(response.Tools))
	for i, tool := range response.Tools {
		tools[i] = mcpops.Tool{Name: tool.Name}
	}
	return tools, nil
}

func (s *session) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if s == nil {
		return nil, errors.New("MCP session is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.readErr != nil {
		err := s.readErr
		s.mu.Unlock()
		return nil, s.safeError(err)
	}
	id := s.nextID
	s.nextID++
	response := make(chan callResult, 1)
	s.pending[id] = response
	s.mu.Unlock()

	request := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := s.writeJSON(request); err != nil {
		s.removePending(id)
		return nil, s.safeError(fmt.Errorf("send %s: %w", method, err))
	}

	select {
	case received := <-response:
		return received.result, received.err
	case <-ctx.Done():
		s.removePending(id)
		return nil, ctx.Err()
	case <-s.done:
		s.removePending(id)
		s.mu.Lock()
		err := s.readErr
		s.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return nil, s.safeError(err)
	}
}

func (s *session) notify(method string, params interface{}) error {
	return s.safeError(s.writeJSON(rpcNotification{JSONRPC: "2.0", Method: method, Params: params}))
}

func (s *session) writeJSON(value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stdin == nil {
		return errors.New("MCP session is closed")
	}
	if _, err := s.stdin.Write(data); err != nil {
		return err
	}
	return nil
}

func (s *session) readLoop(stdout io.ReadCloser) {
	defer stdout.Close()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxFrameBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var message rpcMessage
		if err := json.Unmarshal(line, &message); err != nil {
			s.fail(fmt.Errorf("MCP bridge returned malformed JSON-RPC: %w", err))
			return
		}
		if message.JSONRPC != "2.0" {
			s.fail(errors.New("MCP bridge returned a non-2.0 JSON-RPC message"))
			return
		}
		if len(message.ID) == 0 || string(message.ID) == "null" {
			// Server notifications are unrelated to the synchronous probe.
			continue
		}
		if message.Method != "" {
			// A probe cannot service server-initiated requests. Replying would
			// expand the client's capabilities; ignore them instead.
			continue
		}
		var id int64
		if err := json.Unmarshal(message.ID, &id); err != nil {
			s.fail(errors.New("MCP bridge returned a response with a non-integer id"))
			return
		}
		if (message.Error == nil) == (len(message.Result) == 0) {
			s.fail(fmt.Errorf("MCP bridge returned an invalid JSON-RPC response for id %d", id))
			return
		}
		s.mu.Lock()
		response := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if response == nil {
			continue
		}
		if message.Error != nil {
			response <- callResult{err: fmt.Errorf("JSON-RPC error %d: %s", message.Error.Code, message.Error.Message)}
		} else {
			response <- callResult{result: append(json.RawMessage(nil), message.Result...)}
		}
	}
	if err := scanner.Err(); err != nil {
		s.fail(fmt.Errorf("read MCP bridge stdout: %w", err))
		return
	}
	s.fail(io.EOF)
}

func (s *session) fail(err error) {
	err = s.safeError(err)
	s.mu.Lock()
	if s.readErr != nil {
		s.mu.Unlock()
		return
	}
	s.readErr = err
	pending := s.pending
	s.pending = make(map[int64]chan callResult)
	close(s.done)
	s.mu.Unlock()
	for _, response := range pending {
		response <- callResult{err: err}
	}
}

func (s *session) removePending(id int64) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func decodeResult(result json.RawMessage, target interface{}) error {
	if len(result) == 0 {
		return errors.New("JSON-RPC response omitted result")
	}
	if err := json.Unmarshal(result, target); err != nil {
		return fmt.Errorf("decode JSON-RPC result: %w", err)
	}
	return nil
}

func (s *session) safeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New(mcpops.RedactError(err, s.token, s.host))
}

func (s *session) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.writeMu.Lock()
		stdin := s.stdin
		s.stdin = nil
		if stdin != nil {
			_ = stdin.Close()
		}
		s.writeMu.Unlock()
		wait := make(chan error, 1)
		go func() { wait <- s.proc.Wait() }()
		timeout := s.closeTimeout
		if timeout <= 0 {
			timeout = defaultCloseWait
		}
		select {
		case err := <-wait:
			if err != nil && !errors.Is(err, context.Canceled) {
				s.closeErr = fmt.Errorf("MCP bridge exit: %w", err)
			}
		case <-time.After(timeout):
			if err := s.proc.Kill(); err != nil {
				s.closeErr = fmt.Errorf("stop MCP bridge: %w", err)
			}
			select {
			case err := <-wait:
				if err != nil && s.closeErr == nil {
					// A forced kill is expected after the grace period.
					s.closeErr = nil
				}
			case <-time.After(timeout):
				if s.closeErr == nil {
					s.closeErr = errors.New("MCP bridge did not exit after cancellation")
				}
			}
		}
		if s.cleanup != nil {
			s.cleanup()
		}
		s.closeErr = s.safeError(s.closeErr)
	})
	return s.closeErr
}
