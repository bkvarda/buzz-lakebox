// Command bzhttpmcp bridges newline-delimited stdio JSON-RPC to a typed,
// same-workspace Databricks Streamable HTTP MCP endpoint.
//
// The workspace host and bearer credential are read only from DATABRICKS_HOST
// and DATABRICKS_TOKEN. They cannot be supplied on the command line.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultRequestTimeout = 2 * time.Minute
	maxRequestTimeout     = 10 * time.Minute
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type options struct {
	kind      endpointKind
	resources []string
	schemas   []string
	timeout   time.Duration
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bzhttpmcp: %s\n", redact(err.Error(), os.Getenv("DATABRICKS_TOKEN")))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) error {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	token := getenv("DATABRICKS_TOKEN")
	if token == "" {
		return fmt.Errorf("DATABRICKS_TOKEN is not set")
	}
	if err := validateHeaderValue(token); err != nil {
		return fmt.Errorf("DATABRICKS_TOKEN is invalid")
	}

	endpoint, err := buildEndpoint(getenv("DATABRICKS_HOST"), opts)
	if err != nil {
		return err
	}
	client := newHTTPClient(endpoint, opts.timeout)
	b := newBridge(client, endpoint, token, stdin, stdout, stderr)
	return b.run(ctx)
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var kind string
	var resources, schemas stringList
	opts := options{timeout: defaultRequestTimeout}

	fs := flag.NewFlagSet("bzhttpmcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&kind, "kind", "", "typed endpoint kind: sql, genie, ai-search, vector-search, functions, mcp-service, or skills")
	fs.Var(&resources, "resource", "endpoint identifier component; repeat once per component")
	fs.Var(&schemas, "schema", "skills scope as catalog.schema; repeat for more than one scope")
	fs.DurationVar(&opts.timeout, "timeout", defaultRequestTimeout, "per-request deadline")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected positional arguments")
	}
	if opts.timeout <= 0 || opts.timeout > maxRequestTimeout {
		return options{}, fmt.Errorf("timeout must be greater than zero and no more than %s", maxRequestTimeout)
	}
	opts.kind = endpointKind(kind)
	opts.resources = append([]string(nil), resources...)
	opts.schemas = append([]string(nil), schemas...)
	return opts, nil
}

func newHTTPClient(endpoint *url.URL, timeout time.Duration) *http.Client {
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

func sameAuthority(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func validateHeaderValue(value string) error {
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

// redact is a final defense for diagnostics. Request and response bodies are
// never logged, and callers additionally pass the exact environment token.
func redact(message, token string) string {
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
