// Package skillconfig defines the portable, versioned configuration for
// optional Databricks aitools skill synchronization. The schema deliberately
// cannot represent a workspace URL, CLI profile, credential, environment
// variable, or shell expression.
package skillconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	// CurrentSchema identifies this configuration family. It is a scalar name,
	// not a URL or another externally resolved schema reference.
	CurrentSchema = "buzz-skills"
	// CurrentVersion is the only schema version understood by this package.
	CurrentVersion = 1
	// MaxSkills limits both configuration and the installer's staged output.
	MaxSkills = 64
	// MaxSkillNameLength keeps names portable and bounds rendered arguments.
	MaxSkillNameLength = 64
)

// CollisionPolicy controls what happens when aitools emits a skill whose
// destination already exists.
type CollisionPolicy string

const (
	// CollisionFail preserves every existing destination.
	CollisionFail CollisionPolicy = "fail"
	// CollisionReplaceManaged permits replacement only when the destination has
	// the exact provider provenance marker/version written by this installer.
	CollisionReplaceManaged CollisionPolicy = "replace-managed"
)

// Config is the complete buzz-skills v1 document. A nil AITools disables
// synchronization; an object enables it.
type Config struct {
	Schema  string   `json:"schema"`
	Version int      `json:"version"`
	AITools *AITools `json:"aitools,omitempty"`
}

// AITools contains only local installation choices. An empty Skills slice asks
// aitools for its default skill set. The zero CollisionPolicy means "fail".
type AITools struct {
	Skills          []string        `json:"skills,omitempty"`
	Experimental    bool            `json:"experimental,omitempty"`
	CollisionPolicy CollisionPolicy `json:"collision_policy,omitempty"`
}

var skillNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Parse decodes exactly one strict JSON document and validates it. Unknown
// fields are rejected, which is important here: URL/profile/auth fields must
// not be silently accepted and later acquire meaning.
func Parse(data []byte) (Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode skills config: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("decode skills config: multiple JSON values")
		}
		return Config{}, fmt.Errorf("decode skills config: trailing data: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces the v1 contract without consulting process state, the
// filesystem, environment variables, CLI profiles, or the network.
func (c Config) Validate() error {
	if c.Schema != CurrentSchema {
		return fmt.Errorf("schema must be %q, got %q", CurrentSchema, c.Schema)
	}
	if c.Version != CurrentVersion {
		return fmt.Errorf("version must be %d, got %d", CurrentVersion, c.Version)
	}
	if c.AITools == nil {
		return nil
	}
	return c.AITools.Validate()
}

// Validate checks aitools settings. Names are case-insensitively unique to
// avoid divergent behavior between filesystems.
func (a AITools) Validate() error {
	if len(a.Skills) > MaxSkills {
		return fmt.Errorf("aitools.skills has %d entries, more than the maximum of %d", len(a.Skills), MaxSkills)
	}
	seen := make(map[string]int, len(a.Skills))
	for i, name := range a.Skills {
		if !skillNameRE.MatchString(name) || len(name) > MaxSkillNameLength || name == "." || name == ".." {
			return fmt.Errorf("aitools.skills[%d] %q is not a safe identifier; use 1-%d ASCII characters matching ^[A-Za-z0-9][A-Za-z0-9._-]*$", i, name, MaxSkillNameLength)
		}
		if strings.EqualFold(name, "buzz-cli") {
			return fmt.Errorf("aitools.skills[%d] %q is reserved for Buzz and may not be synchronized", i, name)
		}
		folded := strings.ToLower(name)
		if prior, ok := seen[folded]; ok {
			return fmt.Errorf("aitools.skills[%d] %q duplicates aitools.skills[%d]; names are case-insensitively unique", i, name, prior)
		}
		seen[folded] = i
	}
	if a.CollisionPolicy != "" && a.CollisionPolicy != CollisionFail && a.CollisionPolicy != CollisionReplaceManaged {
		return fmt.Errorf("aitools.collision_policy must be %q or %q, got %q", CollisionFail, CollisionReplaceManaged, a.CollisionPolicy)
	}
	return nil
}

// EffectiveCollisionPolicy returns the fail-closed default for an omitted
// collision_policy. Callers should validate before using it.
func (a AITools) EffectiveCollisionPolicy() CollisionPolicy {
	if a.CollisionPolicy == "" {
		return CollisionFail
	}
	return a.CollisionPolicy
}
