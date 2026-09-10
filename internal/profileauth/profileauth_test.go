package profileauth_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/profileauth"
)

const (
	fixtureProfile = "fixture-profile"
	fixtureHost    = "https://workspace.example.invalid"
	fixtureToken   = "fixture-token-value"
	fixtureExpiry  = "2099-04-05T06:07:08.123456789Z"
)

type response struct {
	want   []string
	stdout string
	stderr string
	err    error
}

type fakeRunner struct {
	t         *testing.T
	responses []response
	calls     int
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	f.t.Helper()
	if f.calls >= len(f.responses) {
		f.t.Fatalf("unexpected command: %q", args)
	}
	response := f.responses[f.calls]
	f.calls++
	if !reflect.DeepEqual(args, response.want) {
		f.t.Fatalf("command %d = %q, want %q", f.calls, args, response.want)
	}
	return []byte(response.stdout), []byte(response.stderr), response.err
}

func (f *fakeRunner) done() {
	f.t.Helper()
	if f.calls != len(f.responses) {
		f.t.Fatalf("ran %d commands, want %d", f.calls, len(f.responses))
	}
}

func describeCommand(profile string) []string {
	return []string{"auth", "describe", "--sensitive", "--output", "json", "--profile", profile}
}

func tokenCommand(profile string, forceRefresh bool) []string {
	args := []string{"auth", "token", profile, "--output", "json"}
	if forceRefresh {
		args = append(args, "--force-refresh")
	}
	return args
}

func validDescribe(host string) string {
	return `{"status":"success","details":{"host":"` + host + `"}}`
}

func validToken(token, expiry string) string {
	return `{"access_token":"` + token + `","token_type":"Bearer","expiry":"` + expiry + `"}`
}

func TestResolveUsesExplicitProfileAndParsesSeparateStdout(t *testing.T) {
	fake := &fakeRunner{t: t, responses: []response{
		{
			want:   describeCommand(fixtureProfile),
			stdout: validDescribe(fixtureHost),
			stderr: `{"status":"error","details":{"host":"https://stderr.example.invalid"}}`,
		},
		{
			want:   tokenCommand(fixtureProfile, false),
			stdout: validToken(fixtureToken, fixtureExpiry),
			stderr: `{"access_token":"stderr-token","expiry":"2000-01-01T00:00:00Z"}`,
		},
	}}
	resolver, err := profileauth.New(fixtureProfile, fake)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wantExpiry, err := time.Parse(time.RFC3339Nano, fixtureExpiry)
	if err != nil {
		t.Fatal(err)
	}
	want := profileauth.Auth{Host: fixtureHost, Token: fixtureToken, Expiry: wantExpiry}
	if got != want {
		t.Fatalf("Resolve = %#v, want %#v", got, want)
	}
	fake.done()
}

func TestResolveSupportsForceRefresh(t *testing.T) {
	for _, test := range []struct {
		name  string
		opt   profileauth.Option
		force bool
	}{
		{name: "enabled", opt: profileauth.WithForceRefresh(), force: true},
		{name: "explicitly enabled", opt: profileauth.WithForceRefresh(true), force: true},
		{name: "explicitly disabled", opt: profileauth.WithForceRefresh(false), force: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeRunner{t: t, responses: []response{
				{want: describeCommand(fixtureProfile), stdout: validDescribe(fixtureHost)},
				{want: tokenCommand(fixtureProfile, test.force), stdout: validToken(fixtureToken, fixtureExpiry)},
			}}
			resolver, err := profileauth.New(fixtureProfile, fake, test.opt)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := resolver.Resolve(context.Background()); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			fake.done()
		})
	}
}

func TestNewRequiresExplicitWellFormedProfileAndRunner(t *testing.T) {
	fake := &fakeRunner{t: t}
	for _, profile := range []string{"", " ", " profile", "profile ", "profile\nname", "profile\x00name", string([]byte{0xff})} {
		if _, err := profileauth.New(profile, fake); err == nil {
			t.Errorf("New(%q) succeeded", profile)
		}
	}
	if _, err := profileauth.New(fixtureProfile, nil); err == nil {
		t.Fatal("New succeeded with nil runner")
	}
	if _, err := profileauth.New(fixtureProfile, fake, nil); err == nil {
		t.Fatal("New succeeded with nil option")
	}
	if _, err := profileauth.New(fixtureProfile, fake, profileauth.WithForceRefresh(true, false)); err == nil {
		t.Fatal("New succeeded with malformed force-refresh option")
	}
}

func TestNewExecConstructors(t *testing.T) {
	if runner := profileauth.NewExecRunner(""); runner.Path != "databricks" {
		t.Fatalf("default executable = %q", runner.Path)
	}
	if runner := profileauth.NewExecRunner("fixture-databricks"); runner.Path != "fixture-databricks" {
		t.Fatalf("custom executable = %q", runner.Path)
	}
	if _, err := profileauth.NewExec(fixtureProfile); err != nil {
		t.Fatalf("NewExec: %v", err)
	}
}

func TestRejectsInvalidDescribeResponsesWithoutFetchingToken(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
	}{
		{name: "malformed JSON", stdout: `{"status":`},
		{name: "trailing JSON", stdout: validDescribe(fixtureHost) + `{}`},
		{name: "unsuccessful status", stdout: `{"status":"error","details":{"host":"` + fixtureHost + `"}}`},
		{name: "missing status", stdout: `{"details":{"host":"` + fixtureHost + `"}}`},
		{name: "missing host", stdout: `{"status":"success","details":{}}`},
		{name: "HTTP host", stdout: validDescribe("http://workspace.example.invalid")},
		{name: "bare host", stdout: validDescribe("workspace.example.invalid")},
		{name: "non-host URL path", stdout: validDescribe(fixtureHost + "/api")},
		{name: "non-host URL query", stdout: validDescribe(fixtureHost + "?x=y")},
		{name: "non-host URL fragment", stdout: validDescribe(fixtureHost + "#fragment")},
		{name: "credentials in URL", stdout: validDescribe("https://user@workspace.example.invalid")},
		{name: "host whitespace", stdout: validDescribe(" " + fixtureHost)},
		{name: "host control", stdout: `{"status":"success","details":{"host":"https://workspace.example.invalid\n"}}`},
		{name: "invalid UTF-8", stdout: "{\"status\":\"success\",\"details\":{\"host\":\"https://workspace.example.invalid/" + string([]byte{0xff}) + "\"}}"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeRunner{t: t, responses: []response{{
				want: describeCommand(fixtureProfile), stdout: test.stdout,
			}}}
			resolver, err := profileauth.New(fixtureProfile, fake)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got, err := resolver.Resolve(context.Background()); err == nil {
				t.Fatalf("Resolve succeeded: %#v", got)
			}
			fake.done()
		})
	}
}

func TestRejectsInvalidTokenResponses(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
	}{
		{name: "malformed JSON", stdout: `{"access_token":`},
		{name: "trailing JSON", stdout: validToken(fixtureToken, fixtureExpiry) + `{}`},
		{name: "missing token", stdout: validToken("", fixtureExpiry)},
		{name: "token whitespace", stdout: validToken("fixture token", fixtureExpiry)},
		{name: "token control", stdout: `{"access_token":"fixture\ntoken","expiry":"` + fixtureExpiry + `"}`},
		{name: "missing expiry", stdout: validToken(fixtureToken, "")},
		{name: "malformed expiry", stdout: validToken(fixtureToken, "tomorrow")},
		{name: "expiry whitespace", stdout: validToken(fixtureToken, " "+fixtureExpiry)},
		{name: "expiry control", stdout: `{"access_token":"` + fixtureToken + `","expiry":"2099-04-05T06:07:08Z\n"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeRunner{t: t, responses: []response{
				{want: describeCommand(fixtureProfile), stdout: validDescribe(fixtureHost)},
				{want: tokenCommand(fixtureProfile, false), stdout: test.stdout},
			}}
			resolver, err := profileauth.New(fixtureProfile, fake)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got, err := resolver.Resolve(context.Background()); err == nil {
				t.Fatalf("Resolve succeeded: %#v", got)
			}
			fake.done()
		})
	}
}

func TestErrorsNeverIncludeProfileHostOrToken(t *testing.T) {
	const (
		secretProfile = "profile-sensitive-value"
		secretHost    = "https://host-sensitive.example.invalid"
		secretToken   = "token-sensitive-value"
	)
	tests := []struct {
		name      string
		responses []response
	}{
		{
			name: "describe command failure",
			responses: []response{{
				want:   describeCommand(secretProfile),
				stdout: validDescribe(secretHost),
				stderr: "failed for " + secretProfile + " " + secretHost + " " + secretToken,
				err:    errors.New("runner failed with " + secretToken),
			}},
		},
		{
			name: "describe semantic failure",
			responses: []response{{
				want: describeCommand(secretProfile), stdout: `{"status":"error","error":"` + secretToken + `","details":{"host":"` + secretHost + `"}}`,
			}},
		},
		{
			name: "token command failure",
			responses: []response{
				{want: describeCommand(secretProfile), stdout: validDescribe(secretHost)},
				{
					want:   tokenCommand(secretProfile, false),
					stdout: validToken(secretToken, fixtureExpiry),
					stderr: "failed for " + secretProfile + " " + secretHost + " " + secretToken,
					err:    errors.New("runner failed with " + secretToken),
				},
			},
		},
		{
			name: "token parse failure",
			responses: []response{
				{want: describeCommand(secretProfile), stdout: validDescribe(secretHost)},
				{want: tokenCommand(secretProfile, false), stdout: `{"access_token":"` + secretToken + `","expiry":"invalid-` + secretHost + `"}`},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeRunner{t: t, responses: test.responses}
			resolver, err := profileauth.New(secretProfile, fake)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := resolver.Resolve(context.Background())
			if err == nil {
				t.Fatalf("Resolve succeeded: %#v", got)
			}
			if got != (profileauth.Auth{}) {
				t.Fatalf("partial auth escaped: %#v", got)
			}
			for _, secret := range []string{secretProfile, secretHost, secretToken} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %q: %q", secret, err)
				}
			}
			fake.done()
		})
	}
}

func TestCommandFailurePreservesContextCancellationOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeRunner{t: t, responses: []response{{
		want: describeCommand(fixtureProfile), err: errors.New("sensitive runner failure"),
	}}}
	resolver, err := profileauth.New(fixtureProfile, fake)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = resolver.Resolve(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve error = %v, want context cancellation", err)
	}
	fake.done()
}
