package httpmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxFrameBytes = 32 * 1024 * 1024

type Bridge struct {
	client   *http.Client
	endpoint *url.URL
	token    string
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer

	mu              sync.Mutex
	outputMu        sync.Mutex
	sessionID       string
	protocolVersion string
}

func NewBridge(client *http.Client, endpoint *url.URL, token string, stdin io.Reader, stdout, stderr io.Writer) *Bridge {
	return &Bridge{client: client, endpoint: endpoint, token: token, stdin: stdin, stdout: stdout, stderr: stderr}
}

func (b *Bridge) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	scanner := bufio.NewScanner(b.stdin)
	scanner.Buffer(make([]byte, 64*1024), maxFrameBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			b.diagnostic("ignoring malformed JSON input")
			continue
		}
		payload := append([]byte(nil), line...)
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(payload, &message)
		isNotification := message.Method != "" && len(message.ID) == 0
		if message.Method == "initialize" {
			if err := b.exchange(ctx, payload, false); err != nil {
				cancel()
				wg.Wait()
				return err
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.exchange(ctx, payload, isNotification); err != nil {
				select {
				case errs <- err:
					cancel()
				default:
				}
			}
		}()
	}
	if err := scanner.Err(); err != nil {
		cancel()
		wg.Wait()
		return fmt.Errorf("read stdin: %w", err)
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return b.closeSession()
	}
}

func (b *Bridge) exchange(ctx context.Context, payload []byte, notification bool) error {
	b.rememberProtocolVersion(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create MCP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session := b.getSession(); session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version := b.getProtocolVersion(); version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("send MCP request: %s", Redact(err.Error(), b.token))
	}
	defer func() { _ = resp.Body.Close() }()

	if session := resp.Header.Get("Mcp-Session-Id"); session != "" {
		if err := ValidateHeaderValue(session); err != nil {
			return fmt.Errorf("server returned an invalid Mcp-Session-Id")
		}
		b.setSession(session)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("MCP authentication failed: HTTP %s", resp.Status)
		}
		return fmt.Errorf("MCP server returned HTTP %s", resp.Status)
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusAccepted || resp.ContentLength == 0 {
		return nil
	}

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("invalid MCP response Content-Type")
	}
	// JSON-RPC notifications have no response. Some Databricks managed MCP
	// endpoints acknowledge them with a small text/plain status body instead
	// of 202/204. Ignore successful notification bodies so that this wire-level
	// acknowledgement cannot poison stdio with non-JSON or fail the session.
	if notification {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	switch strings.ToLower(mediaType) {
	case "application/json":
		return b.forwardJSON(resp.Body)
	case "text/event-stream":
		return b.forwardSSE(resp.Body)
	default:
		return fmt.Errorf("unsupported MCP response Content-Type %q", mediaType)
	}
}

func (b *Bridge) forwardJSON(r io.Reader) error {
	body, err := io.ReadAll(io.LimitReader(r, maxFrameBytes+1))
	if err != nil {
		return fmt.Errorf("read JSON response: %w", err)
	}
	if len(body) > maxFrameBytes {
		return fmt.Errorf("JSON response exceeds %d bytes", maxFrameBytes)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	if !json.Valid(body) {
		return fmt.Errorf("server returned malformed JSON")
	}
	return b.writeLine(body)
}

func (b *Bridge) forwardSSE(r io.Reader) error {
	reader := bufio.NewReader(r)
	var data []string
	var eventBytes int
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := []byte(strings.Join(data, "\n"))
		data = data[:0]
		eventBytes = 0
		if string(payload) == "[DONE]" {
			return nil
		}
		if !json.Valid(payload) {
			return fmt.Errorf("server returned malformed JSON in SSE data")
		}
		return b.writeLine(payload)
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) != 0 {
			if len(line) > maxFrameBytes {
				return fmt.Errorf("SSE line exceeds %d bytes", maxFrameBytes)
			}
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if dispatchErr := dispatch(); dispatchErr != nil {
					return dispatchErr
				}
			} else if strings.HasPrefix(line, "data:") {
				value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
				eventBytes += len(value)
				if eventBytes > maxFrameBytes {
					return fmt.Errorf("SSE event exceeds %d bytes", maxFrameBytes)
				}
				data = append(data, value)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return dispatch()
			}
			return fmt.Errorf("read SSE response: %w", err)
		}
	}
}

func (b *Bridge) closeSession() error {
	session := b.getSession()
	if session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.endpoint.String(), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Mcp-Session-Id", session)
	if version := b.getProtocolVersion(); version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.diagnostic("session cleanup failed")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b.diagnostic("session cleanup returned a non-success status")
	}
	return nil
}

func (b *Bridge) writeLine(payload []byte) error {
	b.outputMu.Lock()
	defer b.outputMu.Unlock()
	return writeLine(b.stdout, payload)
}

func writeLine(w io.Writer, payload []byte) error {
	// Even when the HTTP server pretty-prints JSON or an SSE event joins
	// multiple data lines, stdio MCP requires exactly one JSON value per line.
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return fmt.Errorf("compact JSON response: %w", err)
	}
	if err := compact.WriteByte('\n'); err != nil {
		return fmt.Errorf("prepare stdout: %w", err)
	}
	if _, err := w.Write(compact.Bytes()); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	return nil
}

func (b *Bridge) getSession() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

func (b *Bridge) setSession(session string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessionID = session
}

func (b *Bridge) getProtocolVersion() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.protocolVersion
}

func (b *Bridge) rememberProtocolVersion(payload []byte) {
	var message struct {
		Method string `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if json.Unmarshal(payload, &message) != nil || message.Method != "initialize" || message.Params.ProtocolVersion == "" {
		return
	}
	if ValidateHeaderValue(message.Params.ProtocolVersion) != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.protocolVersion = message.Params.ProtocolVersion
}

func (b *Bridge) diagnostic(message string) {
	_, _ = fmt.Fprintf(b.stderr, "bzhttpmcp: %s\n", Redact(message, b.token))
}

// NewHTTPClient returns the hardened client used for managed MCP requests.
// Redirects are not followed, preventing credentials from being forwarded.
func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.ExpectContinueTimeout = time.Second

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ValidateHeaderValue rejects values that could inject HTTP headers.
func ValidateHeaderValue(value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("invalid UTF-8")
	}
	for _, r := range value {
		if r == 0 || r == '\r' || r == '\n' || unicode.IsControl(r) {
			return fmt.Errorf("control character")
		}
	}
	return nil
}

// Redact removes the exact token and bearer credential values from diagnostics.
func Redact(message, token string) string {
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	fields := strings.Fields(message)
	for i := 0; i+1 < len(fields); i++ {
		if strings.EqualFold(strings.Trim(fields[i], ",;:"), "bearer") {
			message = strings.ReplaceAll(message, fields[i+1], "[REDACTED]")
		}
	}
	return message
}
