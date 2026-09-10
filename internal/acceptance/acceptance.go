// Package acceptance defines the local-only plan for manually exercising
// internal workspace acceptance. It parses and validates portable inputs, but
// deliberately has no API for resolving credentials, contacting a workspace,
// or mutating one.
package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/skillconfig"
)

const (
	// CurrentSchema identifies the local internal-acceptance manifest family.
	CurrentSchema = "buzz-internal-acceptance"
	// CurrentVersion is the only manifest version understood by this package.
	CurrentVersion = 1

	// OptInEnvironment is the second, process-local acknowledgement required in
	// addition to Options.AcceptInternalWorkspaceRisk.
	OptInEnvironment = "BUZZ_LAKEBOX_INTERNAL_ACCEPTANCE"
)

var (
	// ErrInternalWorkspaceRiskNotAccepted means the caller did not supply its
	// explicit --accept-internal-workspace-risk-style acknowledgement.
	ErrInternalWorkspaceRiskNotAccepted = errors.New("internal workspace risk was not explicitly accepted")
	// ErrEnvironmentOptInRequired means the local environment acknowledgement
	// was absent or was not exactly "1".
	ErrEnvironmentOptInRequired = errors.New("internal acceptance environment opt-in is required")
	// ErrCIForbidden means a CI environment was detected. Internal acceptance is
	// manual-only even when both acknowledgements are otherwise present.
	ErrCIForbidden = errors.New("internal acceptance is forbidden in CI")
)

// Manifest is a versioned, portable description of a manual internal
// acceptance run. Profile is a Databricks CLI profile name, never a host or
// credential. Resources must carry at least one existing portable config as a
// JSON string.
type Manifest struct {
	Schema    string     `json:"schema"`
	Version   int        `json:"version"`
	Profile   string     `json:"profile"`
	Runtimes  []string   `json:"runtimes"`
	Resources *Resources `json:"resources"`
}

// Resources contains the same scalar config strings accepted by provider
// configuration. Keeping them as nested versioned documents avoids adding a
// second representation of MCP or skills resources here.
type Resources struct {
	MCPConfig    string `json:"mcp_config,omitempty"`
	SkillsConfig string `json:"skills_config,omitempty"`
}

// Options supplies the two local execution-gate inputs. The boolean must be
// set directly by the caller from an explicit flag. LookupEnv defaults to
// os.LookupEnv; injection exists so package tests and embedding callers can be
// hermetic without changing process-global environment.
type Options struct {
	AcceptInternalWorkspaceRisk bool
	LookupEnv                   func(string) (string, bool)
}

// Runtime is one validated requested runtime. AgentCommand preserves the
// explicit manifest spelling; Kind and SpawnCommand are the existing payload
// parser's canonical planning values.
type Runtime struct {
	AgentCommand string
	Kind         payload.Runtime
	SpawnCommand string
}

// Plan is validated local planning data. Config strings are compact renderings
// of the typed documents returned by the existing parsers. It contains no host,
// token, resolved authentication, command runner, or workspace client.
type Plan struct {
	Profile      string
	Runtimes     []Runtime
	MCPConfig    string
	SkillsConfig string
}

// Parse strictly decodes and structurally validates one manifest. Parse is
// intentionally side-effect free and does not authorize a live run; callers
// must pass the result to BuildPlan, which enforces both opt-ins and the CI
// prohibition.
func Parse(data []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode internal acceptance manifest: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("decode internal acceptance manifest: multiple JSON values")
		}
		return Manifest{}, fmt.Errorf("decode internal acceptance manifest: trailing data: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate checks the manifest and delegates nested config validation to the
// production mcpconfig and skillconfig parsers. It reads no files, environment,
// profiles, credentials, or network state.
func (m Manifest) Validate() error {
	_, err := validateManifest(m)
	return err
}

// ParsePlan strictly parses a manifest and builds an authorized, immutable-by-
// convention planning value. It performs no workspace operation.
func ParsePlan(data []byte, options Options) (Plan, error) {
	manifest, err := Parse(data)
	if err != nil {
		return Plan{}, err
	}
	return BuildPlan(manifest, options)
}

// BuildPlan enforces the manual-only gate, revalidates the supplied Manifest,
// and returns only portable planning values. Revalidation prevents callers
// that construct or alter a Manifest directly from bypassing Parse.
func BuildPlan(manifest Manifest, options Options) (Plan, error) {
	if err := authorize(options); err != nil {
		return Plan{}, err
	}
	validated, err := validateManifest(manifest)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Profile:      manifest.Profile,
		Runtimes:     append([]Runtime(nil), validated.runtimes...),
		MCPConfig:    validated.mcpConfig,
		SkillsConfig: validated.skillsConfig,
	}, nil
}

type validatedManifest struct {
	runtimes     []Runtime
	mcpConfig    string
	skillsConfig string
}

func validateManifest(m Manifest) (validatedManifest, error) {
	if m.Schema != CurrentSchema {
		return validatedManifest{}, fmt.Errorf("schema must be %q, got %q", CurrentSchema, m.Schema)
	}
	if m.Version != CurrentVersion {
		return validatedManifest{}, fmt.Errorf("version must be %d, got %d", CurrentVersion, m.Version)
	}
	if err := validateProfile(m.Profile); err != nil {
		return validatedManifest{}, err
	}
	if len(m.Runtimes) == 0 {
		return validatedManifest{}, errors.New("runtimes must contain at least one explicit runtime")
	}

	validated := validatedManifest{runtimes: make([]Runtime, 0, len(m.Runtimes))}
	seen := make(map[payload.Runtime]int, len(m.Runtimes))
	for i, command := range m.Runtimes {
		kind, ok := payload.RuntimeFor(command)
		if !ok {
			return validatedManifest{}, fmt.Errorf("runtimes[%d] %q is not a supported agent command", i, command)
		}
		if prior, ok := seen[kind]; ok {
			return validatedManifest{}, fmt.Errorf("runtimes[%d] %q duplicates the runtime selected by runtimes[%d]", i, command, prior)
		}
		seen[kind] = i
		validated.runtimes = append(validated.runtimes, Runtime{
			AgentCommand: command,
			Kind:         kind,
			SpawnCommand: kind.SpawnCommand(),
		})
	}

	if m.Resources == nil {
		return validatedManifest{}, errors.New("resources is required")
	}
	if m.Resources.MCPConfig == "" && m.Resources.SkillsConfig == "" {
		return validatedManifest{}, errors.New("resources must contain mcp_config, skills_config, or both")
	}
	if m.Resources.MCPConfig != "" {
		config, err := mcpconfig.Parse([]byte(m.Resources.MCPConfig))
		if err != nil {
			return validatedManifest{}, fmt.Errorf("resources.mcp_config: %w", err)
		}
		compact, err := json.Marshal(config)
		if err != nil {
			return validatedManifest{}, fmt.Errorf("render resources.mcp_config: %w", err)
		}
		validated.mcpConfig = string(compact)
	}
	if m.Resources.SkillsConfig != "" {
		config, err := skillconfig.Parse([]byte(m.Resources.SkillsConfig))
		if err != nil {
			return validatedManifest{}, fmt.Errorf("resources.skills_config: %w", err)
		}
		compact, err := json.Marshal(config)
		if err != nil {
			return validatedManifest{}, fmt.Errorf("render resources.skills_config: %w", err)
		}
		validated.skillsConfig = string(compact)
	}
	return validated, nil
}

func validateProfile(profile string) error {
	if profile == "" || strings.TrimSpace(profile) == "" {
		return errors.New("profile is required")
	}
	if !utf8.ValidString(profile) || strings.TrimSpace(profile) != profile {
		return errors.New("profile is malformed")
	}
	for _, r := range profile {
		if unicode.IsControl(r) {
			return errors.New("profile is malformed")
		}
	}
	// A profile is a local selector, not a place to smuggle the host that this
	// schema intentionally cannot represent.
	if strings.Contains(profile, "://") {
		return errors.New("profile must be a profile name, not a workspace host")
	}
	return nil
}

func authorize(options Options) error {
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if environmentTrue(lookup, "GITHUB_ACTIONS") || environmentTrue(lookup, "CI") {
		return ErrCIForbidden
	}
	if !options.AcceptInternalWorkspaceRisk {
		return ErrInternalWorkspaceRiskNotAccepted
	}
	if value, ok := lookup(OptInEnvironment); !ok || value != "1" {
		return fmt.Errorf("%w: set %s=1 locally", ErrEnvironmentOptInRequired, OptInEnvironment)
	}
	return nil
}

func environmentTrue(lookup func(string) (string, bool), name string) bool {
	value, ok := lookup(name)
	return ok && strings.EqualFold(strings.TrimSpace(value), "true")
}
