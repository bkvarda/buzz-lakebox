// Package profileauth resolves an explicitly named Databricks CLI profile to
// short-lived workspace authentication for local probes.
package profileauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Auth is the short-lived workspace authentication resolved from a profile.
type Auth struct {
	Host   string
	Token  string
	Expiry time.Time
}

// Runner executes one Databricks CLI invocation. Implementations must keep
// stdout and stderr separate because only stdout is parsed as JSON.
type Runner interface {
	Run(context.Context, ...string) (stdout, stderr []byte, err error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(context.Context, ...string) ([]byte, []byte, error)

func (f RunnerFunc) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return f(ctx, args...)
}

// ExecRunner invokes a configured executable directly, without a shell.
type ExecRunner struct {
	Path string
}

// NewExecRunner constructs a real command runner. An empty path selects the
// conventional "databricks" executable.
func NewExecRunner(path string) *ExecRunner {
	if path == "" {
		path = "databricks"
	}
	return &ExecRunner{Path: path}
}

// Run executes the Databricks CLI while capturing stdout and stderr
// independently.
func (r *ExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	path := "databricks"
	if r != nil && r.Path != "" {
		path = r.Path
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Option configures a Resolver.
type Option func(*resolverOptions) error

type resolverOptions struct {
	forceRefresh bool
}

// WithForceRefresh requests that the CLI bypass its cached access token. It
// enables force refresh when called without an argument; an optional boolean
// lets callers set it conditionally.
func WithForceRefresh(enabled ...bool) Option {
	return func(o *resolverOptions) error {
		if len(enabled) > 1 {
			return errors.New("force-refresh option accepts at most one value")
		}
		o.forceRefresh = len(enabled) == 0 || enabled[0]
		return nil
	}
}

// Resolver resolves one explicit profile. It never consults profile or host
// defaults itself; both CLI commands receive the configured profile.
type Resolver struct {
	profile      string
	runner       Runner
	forceRefresh bool
}

// New constructs a Resolver using an injected command runner.
func New(profile string, runner Runner, options ...Option) (*Resolver, error) {
	if err := validateProfile(profile); err != nil {
		return nil, err
	}
	if runner == nil {
		return nil, errors.New("databricks CLI runner is required")
	}

	opts := resolverOptions{}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil profile authentication option")
		}
		if err := option(&opts); err != nil {
			return nil, fmt.Errorf("invalid profile authentication option: %w", err)
		}
	}

	return &Resolver{
		profile:      profile,
		runner:       runner,
		forceRefresh: opts.forceRefresh,
	}, nil
}

// NewExec constructs a Resolver that invokes the real databricks executable.
func NewExec(profile string, options ...Option) (*Resolver, error) {
	return New(profile, NewExecRunner("databricks"), options...)
}

// Resolve obtains the profile's workspace host and a U2M access token. It
// returns no partial authentication if either command or response is invalid.
func (r *Resolver) Resolve(ctx context.Context) (Auth, error) {
	if r == nil || r.runner == nil {
		return Auth{}, errors.New("profile authentication resolver is not configured")
	}

	describeOut, _, err := r.runner.Run(ctx,
		"auth", "describe", "--sensitive", "--output", "json", "--profile", r.profile,
	)
	if err != nil {
		return Auth{}, commandError(ctx, "describe workspace authentication")
	}
	host, err := parseHost(describeOut)
	if err != nil {
		return Auth{}, err
	}

	tokenArgs := []string{"auth", "token", r.profile, "--output", "json"}
	if r.forceRefresh {
		tokenArgs = append(tokenArgs, "--force-refresh")
	}
	tokenOut, _, err := r.runner.Run(ctx, tokenArgs...)
	if err != nil {
		return Auth{}, commandError(ctx, "obtain workspace access token")
	}
	token, expiry, err := parseToken(tokenOut)
	if err != nil {
		return Auth{}, err
	}

	return Auth{Host: host, Token: token, Expiry: expiry}, nil
}

func commandError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	// Do not wrap the runner error or quote either output stream. CLI errors can
	// contain the selected profile, host, or access token.
	return fmt.Errorf("%s: Databricks CLI command failed", operation)
}

type describeResponse struct {
	Status  string `json:"status"`
	Details struct {
		Host string `json:"host"`
	} `json:"details"`
}

func parseHost(stdout []byte) (string, error) {
	var response describeResponse
	if err := decodeOne(stdout, &response); err != nil {
		return "", errors.New("describe workspace authentication: invalid JSON response")
	}
	if response.Status != "success" {
		return "", errors.New("describe workspace authentication: authentication was not successful")
	}
	if err := validateHost(response.Details.Host); err != nil {
		return "", errors.New("describe workspace authentication: invalid workspace host")
	}
	return response.Details.Host, nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	Expiry      string `json:"expiry"`
}

func parseToken(stdout []byte) (string, time.Time, error) {
	var response tokenResponse
	if err := decodeOne(stdout, &response); err != nil {
		return "", time.Time{}, errors.New("obtain workspace access token: invalid JSON response")
	}
	if !validToken(response.AccessToken) {
		return "", time.Time{}, errors.New("obtain workspace access token: invalid token response")
	}
	if !validText(response.Expiry) || strings.TrimSpace(response.Expiry) != response.Expiry {
		return "", time.Time{}, errors.New("obtain workspace access token: invalid expiry response")
	}
	expiry, err := time.Parse(time.RFC3339Nano, response.Expiry)
	if err != nil || expiry.IsZero() {
		return "", time.Time{}, errors.New("obtain workspace access token: invalid expiry response")
	}
	return response.AccessToken, expiry, nil
}

func decodeOne(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("response contains trailing data")
	}
	return nil
}

func validateProfile(profile string) error {
	if profile == "" || strings.TrimSpace(profile) == "" {
		return errors.New("databricks profile is required")
	}
	if !validText(profile) || strings.TrimSpace(profile) != profile {
		return errors.New("databricks profile is malformed")
	}
	return nil
}

func validateHost(host string) error {
	if !validText(host) || strings.TrimSpace(host) != host {
		return errors.New("malformed host")
	}
	u, err := url.Parse(host)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.Hostname() == "" {
		return errors.New("host must be an HTTPS URL")
	}
	if u.Opaque != "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("URL contains non-host components")
	}
	return nil
}

func validToken(token string) bool {
	if token == "" || !validText(token) || strings.TrimSpace(token) != token {
		return false
	}
	for _, r := range token {
		if unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validText(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
