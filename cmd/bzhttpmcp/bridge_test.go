package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func endpointForServer(t *testing.T, server *httptest.Server) *url.URL {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestBridgeJSONHeadersAndSession(t *testing.T) {
	const token = "dapi-test-token-never-log"
	var mu sync.Mutex
	var calls int
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != "session-123" {
				t.Errorf("cleanup session = %q", r.Header.Get("Mcp-Session-Id"))
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls++
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("MCP-Protocol-Version"); got != "2025-06-18" {
			t.Errorf("MCP-Protocol-Version = %q", got)
		}
		if calls == 1 {
			if got := r.Header.Get("Mcp-Session-Id"); got != "" {
				t.Errorf("first session = %q", got)
			}
			w.Header().Set("Mcp-Session-Id", "session-123")
		} else if got := r.Header.Get("Mcp-Session-Id"); got != "session-123" {
			t.Errorf("second session = %q", got)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	input := strings.NewReader("  {\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\"}}  \n\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
	b := newBridge(server.Client(), endpointForServer(t, server), token, input, &stdout, &stderr)
	if err := b.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(bodies) != 2 || strings.Contains(strings.Join(bodies, ""), token) {
		t.Fatalf("unexpected request bodies: %#v", bodies)
	}
}

func TestBridgeSSEForwardsJSONDataOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": keepalive\r\nevent: message\r\nid: 1\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\r\n\r\n")
		_, _ = io.WriteString(w, "data:{\"jsonrpc\":\"2.0\",\n")
		_, _ = io.WriteString(w, "data: \"method\":\"notifications/tools/list_changed\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	var stdout bytes.Buffer
	b := newBridge(server.Client(), endpointForServer(t, server), "token", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`+"\n"), &stdout, io.Discard)
	if err := b.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "{\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestBridgeMalformedInputIsDiagnosticAndContinues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	b := newBridge(server.Client(), endpointForServer(t, server), "secret", strings.NewReader("not-json\n{}\n"), &stdout, &stderr)
	if err := b.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "{}\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "malformed JSON") || strings.Contains(stderr.String(), "secret") {
		t.Fatalf("unsafe/unhelpful stderr = %q", stderr.String())
	}
}

func TestBridgeHTTPAndAuthErrorsDoNotLeakBodiesOrTokens(t *testing.T) {
	const token = "super-secret-token"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "response-body-secret "+token)
			}))
			defer server.Close()
			b := newBridge(server.Client(), endpointForServer(t, server), token, strings.NewReader("{}\n"), io.Discard, io.Discard)
			err := b.run(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "response-body-secret") {
				t.Fatalf("error leaked secret: %v", err)
			}
			if status == http.StatusUnauthorized && !strings.Contains(err.Error(), "authentication failed") {
				t.Fatalf("auth error = %v", err)
			}
		})
	}
}

func TestBridgeResponseErrors(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{"missing content type", "", `{}`},
		{"unsupported content type", "text/plain", `{}`},
		{"malformed JSON", "application/json", `{`},
		{"malformed SSE JSON", "text/event-stream", "data: {\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			b := newBridge(server.Client(), endpointForServer(t, server), "token", strings.NewReader("{}\n"), io.Discard, io.Discard)
			if err := b.run(context.Background()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestBridgeIgnoresSuccessfulNotificationAcknowledgementBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "200 OK")
	}))
	defer server.Close()
	var stdout bytes.Buffer
	input := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n")
	b := newBridge(server.Client(), endpointForServer(t, server), "token", input, &stdout, io.Discard)
	if err := b.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestBridgeNoContentProducesNoOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	var stdout bytes.Buffer
	b := newBridge(server.Client(), endpointForServer(t, server), "token", strings.NewReader("{}\n"), &stdout, io.Discard)
	if err := b.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestClientRejectsCrossHostRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("redirect target must not receive credentials")
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	endpoint := endpointForServer(t, origin)
	client := newHTTPClient(endpoint, time.Second)
	b := newBridge(client, endpoint, "must-not-leak", strings.NewReader("{}\n"), io.Discard, io.Discard)
	err := b.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "307 Temporary Redirect") {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestRunRequiresEnvironmentAuthAndRedaction(t *testing.T) {
	getenv := func(name string) string {
		switch name {
		case "DATABRICKS_HOST":
			return "workspace.example.test"
		case "DATABRICKS_TOKEN":
			return ""
		default:
			return ""
		}
	}
	err := run(context.Background(), []string{"--kind", "sql"}, strings.NewReader(""), io.Discard, io.Discard, getenv)
	if err == nil || !strings.Contains(err.Error(), "DATABRICKS_TOKEN") {
		t.Fatalf("missing-token error = %v", err)
	}

	secret := "token-for-redaction"
	message := redact("failed Authorization: Bearer "+secret+" exact="+secret, secret)
	if strings.Contains(message, secret) || strings.Count(message, "[REDACTED]") < 2 {
		t.Fatalf("redaction failed: %q", message)
	}
}

func TestRunRejectsControlCharacterToken(t *testing.T) {
	getenv := func(name string) string {
		if name == "DATABRICKS_TOKEN" {
			return "token\nInjected: yes"
		}
		return "workspace.example.test"
	}
	err := run(context.Background(), []string{"--kind", "sql"}, strings.NewReader(""), io.Discard, io.Discard, getenv)
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("control-token error = %v", err)
	}
}

func TestBridgeTransportErrorDoesNotLeakToken(t *testing.T) {
	const token = "transport-secret"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport accidentally echoed " + token)
	})}
	u, _ := url.Parse("https://workspace.example.test/mcp")
	b := newBridge(client, u, token, strings.NewReader("{}\n"), io.Discard, io.Discard)
	err := b.run(context.Background())
	if err == nil {
		t.Fatal("expected transport error")
	}
	// main applies the exact-token redactor before emitting any returned error.
	if got := redact(err.Error(), token); strings.Contains(got, token) {
		t.Fatalf("redacted error leaked token: %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBridgeAcceptedAndEmptySuccessProduceNoOutput(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			var stdout bytes.Buffer
			b := newBridge(server.Client(), endpointForServer(t, server), "token", strings.NewReader("{}\n"), &stdout, io.Discard)
			if err := b.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout=%q", stdout.String())
			}
		})
	}
}

func TestBridgeProcessesCancellationWhileRequestIsPending(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		mu.Lock()
		methods = append(methods, msg.Method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if msg.Method == "tools/call" {
			close(started)
			<-release
		}
		if msg.Method == "notifications/cancelled" {
			close(release)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer server.Close()
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\"}\n")
	b := newBridge(server.Client(), endpointForServer(t, server), "token", input, io.Discard, io.Discard)
	done := make(chan error, 1)
	go func() { done <- b.run(context.Background()) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation was blocked behind request")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 {
		t.Fatalf("methods=%v", methods)
	}
}
