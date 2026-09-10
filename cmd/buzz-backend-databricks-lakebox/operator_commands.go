package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/IceRhymers/buzz-lakebox/internal/mcpconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpdiscover"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpops"
	"github.com/IceRhymers/buzz-lakebox/internal/mcpprobe"
	"github.com/IceRhymers/buzz-lakebox/internal/operatorconfig"
	"github.com/IceRhymers/buzz-lakebox/internal/profileauth"
)

type authResolver interface {
	Resolve(context.Context) (profileauth.Auth, error)
}

type operatorDeps struct {
	discoverer func(profile string, scopes []mcpdiscover.Scope, kinds []mcpconfig.Kind) (mcpops.Discoverer, error)
	auth       func(profile string, forceRefresh bool) (authResolver, error)
	prober     func(host, token string) (mcpops.Prober, error)
}

func realOperatorDeps() operatorDeps {
	return operatorDeps{
		discoverer: func(profile string, scopes []mcpdiscover.Scope, kinds []mcpconfig.Kind) (mcpops.Discoverer, error) {
			options := []mcpdiscover.Option{mcpdiscover.WithScopes(scopes...)}
			if kinds != nil {
				options = append(options, mcpdiscover.WithKinds(kinds...))
			}
			return mcpdiscover.NewExec(profile, options...)
		},
		auth: func(profile string, forceRefresh bool) (authResolver, error) {
			return profileauth.NewExec(profile, profileauth.WithForceRefresh(forceRefresh))
		},
		prober: func(host, token string) (mcpops.Prober, error) {
			return mcpprobe.New(host, token)
		},
	}
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Build and validate portable provider configuration"}
	cmd.AddCommand(newConfigValidateCmd())
	return cmd
}

func newConfigValidateCmd() *cobra.Command {
	var input operatorconfig.Input
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Strictly validate one managed-MCP or skills configuration without network access",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := operatorconfig.Validate(input)
			if err != nil {
				return err
			}
			response := struct {
				OK      bool                   `json:"ok"`
				Kind    operatorconfig.Kind    `json:"kind"`
				Config  string                 `json:"config"`
				Summary operatorconfig.Summary `json:"summary"`
			}{true, result.Kind, result.CompactJSON, result.Summary}
			return writeJSON(cmd, response)
		},
	}
	cmd.Flags().StringVar(&input.MCPJSON, "mcp-json", "", "inline buzz-managed-mcp JSON")
	cmd.Flags().StringVar(&input.MCPFile, "mcp-file", "", "path to buzz-managed-mcp JSON")
	cmd.Flags().StringVar(&input.SkillsJSON, "skills-json", "", "inline buzz-skills JSON")
	cmd.Flags().StringVar(&input.SkillsFile, "skills-file", "", "path to buzz-skills JSON")
	return cmd
}

func newMCPCommand(profile *string, deps operatorDeps) *cobra.Command {
	cmd := &cobra.Command{Use: "mcp", Short: "Discover and probe Databricks managed MCP servers"}
	cmd.AddCommand(newMCPDiscoverCmd(profile, deps))
	cmd.AddCommand(newMCPProbeCmd(profile, deps))
	return cmd
}

func newMCPDiscoverCmd(profile *string, deps operatorDeps) *cobra.Command {
	var scopesRaw, kindsRaw []string
	var emitConfig bool
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "List accessible managed MCP resources using read-only Databricks APIs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scopes, err := parseScopes(scopesRaw)
			if err != nil {
				return err
			}
			kinds, err := parseKinds(kindsRaw)
			if err != nil {
				return err
			}
			discoverer, err := deps.discoverer(*profile, scopes, kinds)
			if err != nil {
				return err
			}
			ids, err := mcpops.Discover(cmd.Context(), discoverer)
			if err != nil {
				return err
			}
			response := struct {
				OK        bool                `json:"ok"`
				Resources []mcpops.Identifier `json:"resources"`
				Config    string              `json:"config,omitempty"`
			}{OK: true, Resources: ids}
			if emitConfig {
				cfg := mcpconfig.Config{Schema: mcpconfig.CurrentSchema, Version: mcpconfig.CurrentVersion}
				counts := map[mcpconfig.Kind]int{}
				for _, id := range ids {
					counts[id.Kind]++
					name := string(id.Kind)
					if counts[id.Kind] > 1 {
						name = fmt.Sprintf("%s-%d", id.Kind, counts[id.Kind])
					}
					cfg.Servers = append(cfg.Servers, id.Server(name))
				}
				if err := cfg.Validate(); err != nil {
					return fmt.Errorf("build discovered configuration: %w", err)
				}
				data, err := json.Marshal(cfg)
				if err != nil {
					return fmt.Errorf("render discovered configuration: %w", err)
				}
				response.Config = string(data)
			}
			return writeJSON(cmd, response)
		},
	}
	cmd.Flags().StringSliceVar(&kindsRaw, "kind", nil, "managed kind to discover; repeat or comma-separate (sql, genie, ai-search, functions, mcp-service, skills)")
	cmd.Flags().StringSliceVar(&scopesRaw, "scope", nil, "catalog.schema scope for functions, skills, and MCP Services; repeat or comma-separate")
	cmd.Flags().BoolVar(&emitConfig, "emit-config", false, "include a ready-to-edit compact mcp_config in the JSON result")
	return cmd
}

func newMCPProbeCmd(profile *string, deps operatorDeps) *cobra.Command {
	var inline, file string
	var timeout time.Duration
	var forceRefresh bool
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Run initialize and tools/list against every server in a managed MCP configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			document, err := exactlyOneDocument(inline, file)
			if err != nil {
				return err
			}
			// Validate before resolving any credential or contacting the workspace.
			if _, err := mcpconfig.Parse(document); err != nil {
				return fmt.Errorf("validate managed MCP config: %w", err)
			}
			resolver, err := deps.auth(*profile, forceRefresh)
			if err != nil {
				return err
			}
			auth, err := resolver.Resolve(cmd.Context())
			if err != nil {
				return err
			}
			prober, err := deps.prober(auth.Host, auth.Token)
			if err != nil {
				return err
			}
			result, err := mcpops.Probe(cmd.Context(), document, prober, mcpops.ProbeOptions{Timeout: timeout})
			if err != nil {
				return err
			}
			failed := false
			for _, server := range result.Servers {
				failed = failed || server.Error != ""
			}
			response := struct {
				OK      bool                 `json:"ok"`
				Servers []mcpops.ServerProbe `json:"servers"`
			}{OK: !failed, Servers: result.Servers}
			if err := writeJSON(cmd, response); err != nil {
				return err
			}
			if failed {
				return errors.New("one or more managed MCP probes failed; see redacted JSON results")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&inline, "mcp-json", "", "inline buzz-managed-mcp JSON")
	cmd.Flags().StringVar(&file, "mcp-file", "", "path to buzz-managed-mcp JSON")
	cmd.Flags().DurationVar(&timeout, "timeout", mcpops.DefaultProbeTimeout, "deadline for each initialize + tools/list probe")
	cmd.Flags().BoolVar(&forceRefresh, "force-refresh", false, "force refresh of the local U2M workspace token before probing")
	return cmd
}

func parseScopes(values []string) ([]mcpdiscover.Scope, error) {
	out := make([]mcpdiscover.Scope, 0, len(values))
	for _, value := range values {
		parts := strings.Split(value, ".")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("scope %q must be catalog.schema", value)
		}
		out = append(out, mcpdiscover.Scope{Catalog: parts[0], Schema: parts[1]})
	}
	return out, nil
}

func parseKinds(values []string) ([]mcpconfig.Kind, error) {
	if values == nil {
		return nil, nil
	}
	out := make([]mcpconfig.Kind, 0, len(values))
	for _, raw := range values {
		kind := mcpconfig.Kind(raw)
		switch kind {
		case mcpconfig.KindSQL, mcpconfig.KindGenie, mcpconfig.KindAISearch, mcpconfig.KindFunctions, mcpconfig.KindMCPService, mcpconfig.KindSkills:
			out = append(out, kind)
		default:
			return nil, fmt.Errorf("unsupported discovery kind %q", raw)
		}
	}
	return out, nil
}

func exactlyOneDocument(inline, file string) ([]byte, error) {
	if (inline == "") == (file == "") {
		return nil, errors.New("provide exactly one of --mcp-json or --mcp-file")
	}
	if inline != "" {
		return []byte(inline), nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read managed MCP config %q: %w", file, err)
	}
	return data, nil
}

func writeJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON result: %w", err)
	}
	return nil
}
