package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Profile is the non-secret subset of a local Databricks CLI profile needed
// for provider selection. Discovery never requests, reads, or retains profile
// credentials.
type Profile struct {
	Name string
	Host string
}

// ProfileDiscoverer discovers configured local profiles without validating or
// authenticating them.
type ProfileDiscoverer interface {
	DiscoverProfiles(context.Context) ([]Profile, error)
}

// CLIProfileDiscoverer runs the Databricks CLI's local, skip-validation profile
// listing. Its command is executed directly (never through a shell), and only
// stdout is parsed. Stderr is deliberately excluded from all returned errors
// because a misbehaving CLI or wrapper could write credentials there.
type CLIProfileDiscoverer struct {
	bin     string
	timeout time.Duration
	runner  profileCommandRunner
}

// NewCLIProfileDiscoverer returns a production profile discoverer. bin should
// be the resolved Databricks CLI path; timeout must be positive in production.
func NewCLIProfileDiscoverer(bin string, timeout time.Duration) *CLIProfileDiscoverer {
	return &CLIProfileDiscoverer{bin: bin, timeout: timeout, runner: execProfileCommandRunner{}}
}

type profileCommandRunner interface {
	Run(context.Context, string, ...string) (stdout, stderr []byte, err error)
}

type execProfileCommandRunner struct{}

func (execProfileCommandRunner) Run(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// DiscoverProfiles lists local configuration only. --skip-validate is
// load-bearing: discovery must not authenticate profiles or contact a
// workspace merely to render provider info or choose an unambiguous profile.
func (d *CLIProfileDiscoverer) DiscoverProfiles(ctx context.Context) ([]Profile, error) {
	if d == nil || strings.TrimSpace(d.bin) == "" {
		return nil, fmt.Errorf("discover Databricks CLI profiles: CLI path is empty")
	}
	if d.runner == nil {
		return nil, fmt.Errorf("discover Databricks CLI profiles: command runner is unavailable")
	}

	timeout := d.timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, _, err := d.runner.Run(ctx, d.bin, "auth", "profiles", "--skip-validate", "--output", "json")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("discover Databricks CLI profiles: timed out")
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, fmt.Errorf("discover Databricks CLI profiles: canceled")
		}
		// Do not include stdout, stderr, or an arbitrary wrapped command error.
		// ExitError adds no useful operator action here, while custom wrappers
		// can put command output (and therefore secrets) in Error().
		return nil, fmt.Errorf("discover Databricks CLI profiles: command failed")
	}

	profiles, err := parseProfilesJSON(stdout)
	if err != nil {
		return nil, fmt.Errorf("discover Databricks CLI profiles: invalid JSON response: %w", err)
	}
	return profiles, nil
}

type profileJSON struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

type profilesEnvelope struct {
	Profiles *[]profileJSON `json:"profiles"`
}

func parseProfilesJSON(data []byte) ([]Profile, error) {
	var envelope profilesEnvelope
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("could not decode profile envelope")
	}
	// Reject concatenated JSON rather than silently accepting an ambiguous
	// prefix. The error intentionally excludes input bytes.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing invalid data")
	}

	if envelope.Profiles == nil {
		return nil, fmt.Errorf("missing profiles array")
	}

	byName := make(map[string]Profile, len(*envelope.Profiles))
	for i, raw := range *envelope.Profiles {
		name := strings.TrimSpace(raw.Name)
		if !validProfileName(raw.Name, name) {
			return nil, fmt.Errorf("profile entry %d has an invalid name", i)
		}
		host, ok := validProfileHost(raw.Host)
		if !ok {
			return nil, fmt.Errorf("profile %q has an invalid host", name)
		}
		if accountProfileHost(host) {
			continue
		}
		if prior, exists := byName[name]; exists {
			// Identical duplicate names are harmless. Conflicting hosts make the
			// local CLI output ambiguous, so fail instead of choosing by order.
			if prior.Host != host {
				return nil, fmt.Errorf("profile %q is repeated with conflicting hosts", name)
			}
			continue
		}
		byName[name] = Profile{Name: name, Host: host}
	}

	profiles := make([]Profile, 0, len(byName))
	for _, profile := range byName {
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

func validProfileName(raw, trimmed string) bool {
	if raw != trimmed || trimmed == "" || len(trimmed) > 256 || !utf8.ValidString(trimmed) {
		return false
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validProfileHost(raw string) (string, bool) {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > 2048 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil {
		return "", false
	}
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.TrimSuffix(raw, "/"), true
}

func accountProfileHost(host string) bool {
	u, err := url.Parse(host)
	if err != nil {
		return false
	}
	// Account-console profiles cannot create Lakebox sandboxes. The public
	// account hosts use an "accounts" DNS label across clouds; skip them from
	// this workspace-profile picker rather than presenting a choice that can
	// only fail during preflight.
	for _, label := range strings.Split(strings.ToLower(u.Hostname()), ".") {
		if label == "accounts" {
			return true
		}
	}
	return false
}

func profileNames(profiles []Profile) []string {
	names := make([]string, 0, len(profiles))
	seen := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if !validProfileName(profile.Name, strings.TrimSpace(profile.Name)) {
			continue
		}
		if _, ok := seen[profile.Name]; ok {
			continue
		}
		seen[profile.Name] = struct{}{}
		names = append(names, profile.Name)
	}
	sort.Strings(names)
	return names
}

// selectProfile applies provider-mode deploy selection. Discovery failures and
// an empty result retain the historical baked-default fallback. A successful,
// ambiguous discovery refuses to aim a deploy by guesswork.
func selectProfile(bakedDefault string, profiles []Profile, discoveryErr error) (string, error) {
	names := profileNames(profiles)
	if discoveryErr != nil || len(names) == 0 {
		return bakedDefault, nil
	}
	for _, name := range names {
		if name == bakedDefault {
			return bakedDefault, nil
		}
	}
	if len(names) == 1 {
		return names[0], nil
	}
	return "", fmt.Errorf(
		"provider_config.profile is required because multiple Databricks CLI profiles are available and baked default %q was not discovered; available profiles: %s",
		bakedDefault, strings.Join(names, ", "),
	)
}

func deterministicProfileDefault(bakedDefault string, profiles []Profile) (string, bool) {
	names := profileNames(profiles)
	if len(names) == 0 {
		return "", false
	}
	for _, name := range names {
		if name == bakedDefault {
			return bakedDefault, true
		}
	}
	if len(names) == 1 {
		return names[0], true
	}
	return "", false
}
