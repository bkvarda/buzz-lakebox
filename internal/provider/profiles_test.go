package provider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseProfilesJSONSortsDeduplicatesAndSkipsAccountProfiles(t *testing.T) {
	got, err := parseProfilesJSON([]byte(`{
		"profiles": [
			{"name":"zeta","host":"https://z.example","token":"must-not-be-read"},
			{"name":"account-console","host":"https://accounts.example.com"},
			{"name":"alpha","host":"https://a.example","valid":false},
			{"name":"zeta","host":"https://z.example"}
		]
	}`))
	if err != nil {
		t.Fatalf("parseProfilesJSON: %v", err)
	}
	want := []Profile{{Name: "alpha", Host: "https://a.example"}, {Name: "zeta", Host: "https://z.example"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles = %#v, want %#v", got, want)
	}
}

func TestParseProfilesJSONRejectsMalformedDefensivelyWithoutEcho(t *testing.T) {
	secret := "dapi-super-secret-marker"
	tests := []struct {
		name string
		json string
	}{
		{"invalid JSON", `{"profiles":[` + secret},
		{"missing envelope", `{}`},
		{"null profiles", `{"profiles":null}`},
		{"wrong profiles type", `{"profiles":{}}`},
		{"missing name", `{"profiles":[{"host":"https://a.example"}]}`},
		{"trimmed name", `{"profiles":[{"name":" alpha ","host":"https://a.example"}]}`},
		{"control name", "{\"profiles\":[{\"name\":\"alpha\\n" + secret + "\",\"host\":\"https://a.example\"}]}"},
		{"missing host", `{"profiles":[{"name":"alpha"}]}`},
		{"credential host", `{"profiles":[{"name":"alpha","host":"https://user:pass@a.example/` + secret + `"}]}`},
		{"unsupported HTTP host", `{"profiles":[{"name":"alpha","host":"http://a.example"}]}`},
		{"unsupported host scheme", `{"profiles":[{"name":"alpha","host":"file:///` + secret + `"}]}`},
		{"host path", `{"profiles":[{"name":"alpha","host":"https://a.example/api/` + secret + `"}]}`},
		{"host query", `{"profiles":[{"name":"alpha","host":"https://a.example?marker=` + secret + `"}]}`},
		{"host fragment", `{"profiles":[{"name":"alpha","host":"https://a.example#` + secret + `"}]}`},
		{"conflicting duplicate", `{"profiles":[{"name":"alpha","host":"https://a.example"},{"name":"alpha","host":"https://b.example/` + secret + `"}]}`},
		{"trailing value", `{"profiles":[]} {"secret":"` + secret + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseProfilesJSON([]byte(test.json))
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked input secret: %q", err)
			}
		})
	}
}

type fakeProfileRunner struct {
	stdout  []byte
	stderr  []byte
	err     error
	bin     string
	args    []string
	waitCtx bool
}

func (r *fakeProfileRunner) Run(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	r.bin = bin
	r.args = append([]string(nil), args...)
	if r.waitCtx {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	return r.stdout, r.stderr, r.err
}

func TestCLIProfileDiscovererUsesDirectSafeArgvAndStdoutOnly(t *testing.T) {
	runner := &fakeProfileRunner{
		stdout: []byte(`{"profiles":[{"name":"alpha","host":"https://a.example"}]}`),
		stderr: []byte(`warning token=dapi-stderr-secret`),
	}
	discoverer := &CLIProfileDiscoverer{bin: "/safe/path/databricks", timeout: time.Second, runner: runner}
	got, err := discoverer.DiscoverProfiles(context.Background())
	if err != nil {
		t.Fatalf("DiscoverProfiles: %v", err)
	}
	if want := []Profile{{Name: "alpha", Host: "https://a.example"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles = %#v, want %#v", got, want)
	}
	if runner.bin != "/safe/path/databricks" {
		t.Fatalf("bin = %q", runner.bin)
	}
	wantArgs := []string{"auth", "profiles", "--skip-validate", "--output", "json"}
	if !reflect.DeepEqual(runner.args, wantArgs) {
		t.Fatalf("argv = %#v, want %#v", runner.args, wantArgs)
	}
}

func TestCLIProfileDiscovererNormalizesTrailingHostSlash(t *testing.T) {
	runner := &fakeProfileRunner{stdout: []byte(`{"profiles":[{"name":"alpha","host":"https://a.example/"}]}`)}
	discoverer := &CLIProfileDiscoverer{bin: "databricks", runner: runner}
	got, err := discoverer.DiscoverProfiles(context.Background())
	if err != nil {
		t.Fatalf("DiscoverProfiles: %v", err)
	}
	if got[0].Host != "https://a.example" {
		t.Fatalf("normalized host = %q", got[0].Host)
	}
}

func TestCLIProfileDiscovererAppliesDefaultTimeout(t *testing.T) {
	runner := &fakeProfileRunner{waitCtx: true}
	discoverer := &CLIProfileDiscoverer{bin: "databricks", timeout: time.Millisecond, runner: runner}
	started := time.Now()
	_, err := discoverer.DiscoverProfiles(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("timeout was not enforced promptly")
	}
}

func TestCLIProfileDiscovererFailureDoesNotLeakOutputOrWrappedError(t *testing.T) {
	secrets := []string{"dapi-stdout-secret", "dapi-stderr-secret", "dapi-error-secret"}
	runner := &fakeProfileRunner{
		stdout: []byte(secrets[0]),
		stderr: []byte(secrets[1]),
		err:    errors.New(secrets[2]),
	}
	discoverer := &CLIProfileDiscoverer{bin: "databricks", timeout: time.Second, runner: runner}
	_, err := discoverer.DiscoverProfiles(context.Background())
	if err == nil {
		t.Fatal("expected command failure")
	}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q leaked %q", err, secret)
		}
	}
}

func TestSelectProfile(t *testing.T) {
	profiles := []Profile{{Name: "alpha", Host: "https://a.example"}, {Name: "zeta", Host: "https://z.example"}}
	tests := []struct {
		name       string
		baked      string
		profiles   []Profile
		discovery  error
		want       string
		wantErrSub string
	}{
		{"matching baked default", "zeta", profiles, nil, "zeta", ""},
		{"single profile", "DEFAULT", profiles[:1], nil, "alpha", ""},
		{"discovery failure compatibility fallback", "DEFAULT", nil, errors.New("failed"), "DEFAULT", ""},
		{"zero profiles compatibility fallback", "DEFAULT", nil, nil, "DEFAULT", ""},
		{"ambiguous refuses guess", "DEFAULT", profiles, nil, "", "available profiles: alpha, zeta"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectProfile(test.baked, test.profiles, test.discovery)
			if got != test.want {
				t.Fatalf("selected = %q, want %q", got, test.want)
			}
			if test.wantErrSub == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.wantErrSub != "" && (err == nil || !strings.Contains(err.Error(), test.wantErrSub)) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErrSub)
			}
		})
	}
}
