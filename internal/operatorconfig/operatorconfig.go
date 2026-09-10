// Package operatorconfig provides offline validation for operator-supplied MCP
// and skills configuration documents.
//
// Validate accepts exactly one inline JSON value or file path, delegates strict
// decoding and validation to mcpconfig or skillconfig, and returns the validated
// document as compact canonical JSON plus a typed summary. Apart from reading an
// explicitly selected file, this package does not inspect process state: it does
// not read environment variables or CLI profiles and does not access the network.
package operatorconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/skillconfig"
)

// Kind identifies the configuration schema selected by an Input.
type Kind string

const (
	KindMCP    Kind = "mcp"
	KindSkills Kind = "skills"
)

var (
	// ErrMissingInput means that no inline document or file was selected.
	ErrMissingInput = errors.New("operator config input is missing")
	// ErrAmbiguousInput means that more than one inline document or file was selected.
	ErrAmbiguousInput = errors.New("operator config input is ambiguous")
)

// Input describes the four mutually exclusive input modes supported by
// Validate. Exactly one field must be non-empty. Inline JSON is used verbatim;
// file fields name the only file that Validate will read.
type Input struct {
	MCPJSON    string
	MCPFile    string
	SkillsJSON string
	SkillsFile string
}

// Result is a successfully validated document. CompactJSON is encoding/json's
// compact rendering of the validated typed config, so unknown fields cannot be
// preserved and struct field order is stable. Exactly one member of Summary is
// non-nil and agrees with Kind.
type Result struct {
	Kind        Kind
	CompactJSON string
	Summary     Summary
}

// Summary is a typed union. A successful Result has exactly one non-nil field.
type Summary struct {
	MCP    *MCPSummary    `json:"mcp,omitempty"`
	Skills *SkillsSummary `json:"skills,omitempty"`
}

// MCPSummary describes validated servers without deriving endpoints or bridge
// arguments. Servers retain document order.
type MCPSummary struct {
	Schema            string             `json:"schema"`
	Version           int                `json:"version"`
	ServerCount       int                `json:"server_count"`
	LocalServerCount  int                `json:"local_server_count"`
	RemoteServerCount int                `json:"remote_server_count"`
	Servers           []MCPServerSummary `json:"servers"`
}

// MCPServerSummary is the operator-relevant identity of one validated server.
type MCPServerSummary struct {
	Name string         `json:"name"`
	Kind mcpconfig.Kind `json:"kind"`
}

// SkillsSummary describes validated aitools synchronization choices. When
// Enabled is true, UsesDefaultSkills reports whether the empty skills list asks
// aitools to select its defaults. CollisionPolicy is always the effective,
// fail-closed policy when enabled.
type SkillsSummary struct {
	Schema            string                      `json:"schema"`
	Version           int                         `json:"version"`
	Enabled           bool                        `json:"enabled"`
	Skills            []string                    `json:"skills,omitempty"`
	UsesDefaultSkills bool                        `json:"uses_default_skills"`
	Experimental      bool                        `json:"experimental"`
	CollisionPolicy   skillconfig.CollisionPolicy `json:"collision_policy,omitempty"`
}

type selectedInput struct {
	kind Kind
	json string
	file string
}

// Validate strictly parses exactly one selected MCP or skills input. It reads a
// file only after proving that the selection is unambiguous. Parsing is wholly
// delegated to the corresponding schema package; validation never resolves a
// host, credential, profile, environment variable, or other external state.
func Validate(input Input) (Result, error) {
	selected, err := selectInput(input)
	if err != nil {
		return Result{}, err
	}

	data := []byte(selected.json)
	if selected.file != "" {
		data, err = os.ReadFile(selected.file)
		if err != nil {
			return Result{}, fmt.Errorf("read %s config file %q: %w", selected.kind, selected.file, err)
		}
	}

	switch selected.kind {
	case KindMCP:
		return validateMCP(data)
	case KindSkills:
		return validateSkills(data)
	default:
		panic("operatorconfig: selected unsupported kind")
	}
}

func selectInput(input Input) (selectedInput, error) {
	candidates := []selectedInput{
		{kind: KindMCP, json: input.MCPJSON},
		{kind: KindMCP, file: input.MCPFile},
		{kind: KindSkills, json: input.SkillsJSON},
		{kind: KindSkills, file: input.SkillsFile},
	}

	var selected []selectedInput
	for _, candidate := range candidates {
		if candidate.json != "" || candidate.file != "" {
			selected = append(selected, candidate)
		}
	}
	if len(selected) == 0 {
		return selectedInput{}, fmt.Errorf("%w: provide exactly one of MCPJSON, MCPFile, SkillsJSON, or SkillsFile", ErrMissingInput)
	}
	if len(selected) != 1 {
		return selectedInput{}, fmt.Errorf("%w: provide exactly one of MCPJSON, MCPFile, SkillsJSON, or SkillsFile; got %d", ErrAmbiguousInput, len(selected))
	}
	return selected[0], nil
}

func validateMCP(data []byte) (Result, error) {
	cfg, err := mcpconfig.Parse(data)
	if err != nil {
		return Result{}, fmt.Errorf("validate MCP config: %w", err)
	}
	compact, err := json.Marshal(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("render MCP config: %w", err)
	}

	summary := MCPSummary{
		Schema:      cfg.Schema,
		Version:     cfg.Version,
		ServerCount: len(cfg.Servers),
		Servers:     make([]MCPServerSummary, 0, len(cfg.Servers)),
	}
	for _, server := range cfg.Servers {
		summary.Servers = append(summary.Servers, MCPServerSummary{Name: server.Name, Kind: server.Kind})
		if server.Kind == mcpconfig.KindLocal {
			summary.LocalServerCount++
		} else {
			summary.RemoteServerCount++
		}
	}

	return Result{
		Kind:        KindMCP,
		CompactJSON: string(compact),
		Summary:     Summary{MCP: &summary},
	}, nil
}

func validateSkills(data []byte) (Result, error) {
	cfg, err := skillconfig.Parse(data)
	if err != nil {
		return Result{}, fmt.Errorf("validate skills config: %w", err)
	}
	compact, err := json.Marshal(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("render skills config: %w", err)
	}

	summary := SkillsSummary{Schema: cfg.Schema, Version: cfg.Version}
	if cfg.AITools != nil {
		summary.Enabled = true
		summary.Skills = append([]string(nil), cfg.AITools.Skills...)
		summary.UsesDefaultSkills = len(cfg.AITools.Skills) == 0
		summary.Experimental = cfg.AITools.Experimental
		summary.CollisionPolicy = cfg.AITools.EffectiveCollisionPolicy()
	}

	return Result{
		Kind:        KindSkills,
		CompactJSON: string(compact),
		Summary:     Summary{Skills: &summary},
	}, nil
}
