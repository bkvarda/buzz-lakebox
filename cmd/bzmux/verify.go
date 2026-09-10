package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// knownServerTools maps a known MCP server (by the command/name the deploy
// resolves) to the tool names it is expected to export. It powers the "where
// available" tier of the #17 verification contract (plan Question C): a valid
// initialize + a non-empty tools/list catalog is asserted ALWAYS, and for a
// recognized server every expected tool name must additionally be present —
// catching the initialized-but-under-exports / wrong-server case the fail-
// closed spawn/init checks miss.
//
// Source of these sets: the issue #16 body's contract notes, which the author
// transcribed from upstream block/buzz `crates/buzz-agent` @ 82f7ed15. They are
// therefore PROVISIONAL for our purposes: they have not been confirmed against a
// live `tools/list` from these servers. Until they are (the plan's live-
// acceptance open question, and #17's own PR is `Refs` not `Closes` precisely
// because that run has not happened), the "known names" tier can false-fail a
// healthy deploy if any baked name is stale or was agent-injected rather than
// server-exported (e.g. confirm `_Stop`/`_PostCompact` are exported by
// buzz-dev-mcp itself, not added by the agent). Confirm both inventories against
// a real `tools/list` before trusting this tier in production; the always-on
// non-empty-catalog tier does not depend on these names and is safe regardless.
var knownServerTools = map[string][]string{
	"buzz-dev-mcp": {"shell", "read_file", "view_image", "str_replace", "todo", "_Stop", "_PostCompact"},
	"shellbox-mcp": {"shell_create", "shell_send", "shell_read", "shell_list", "shell_resize", "shell_kill"},
}

// measured: MCP cold-start + initialize+tools/list handshake budget is
// PROVISIONAL (15s deploy default; see internal/deployflow.mcpVerifyTimeoutSeconds
// and plan Driver 3). No live figure was recorded by #14 (shellbox-mcp proven
// GO, no latency captured); backfill the real cold-start/handshake latency here
// from the #16/#17 live two-real-servers acceptance run, per the adapter.go:93-95
// precedent.

// runVerifyMCP is the entry point for --verify-mcp mode (issue #17). It runs a
// real MCP handshake (initialize → notifications/initialized → tools/list)
// against whatever BUZZ_ACP_MCP_COMMAND resolves to and asserts a non-empty
// tool catalog (always) plus the expected tool names where the server is known.
//
//   - direct (command != ""): spawn that single command as a child with the
//     FULL inherited environment (os.Environ()) — matching production, where
//     buzz-agent spawns the direct MCP child on session/new inheriting its
//     launch env (the per-child allowlist is a mux-only least-privilege feature
//     that has no analog on the direct path).
//   - mux (command == ""): read mcp-mux.json and spawn its children directly,
//     exactly as --selftest does, keeping every selftest check (collision/`__`/
//     64-byte budget and JSON smoke check) PLUS the
//     expected-names union of the known children.
//
// It returns nil on success and a non-nil error with a clear diagnostic on any
// failure (missing tool(s) named, empty catalog, timeout, child-exited, version
// mismatch). main() turns a non-nil error into a non-zero exit.
func runVerifyMCP(ctx context.Context, command string) error {
	if command != "" {
		return verifyDirectChild(ctx, command, command, nil, os.Environ())
	}
	return verifyMux(ctx)
}

// verifyDirectChild spawns a single command as an MCP child (with the given
// args + env), runs the initialize+tools/list handshake bounded by ctx,
// asserts a non-empty catalog, and — when knownName is a recognized server —
// asserts every expected tool name is present.
//
// knownName is both the child's label and the key used for the knownServerTools
// lookup (in production it is the resolved command; the executable itself is
// `command`). Separating them keeps the check testable: a fake child can run
// the test binary while still presenting a known logical server name.
//
// In production args is nil: an mcp_servers entry is a bare command name
// (validateMcpServers / CONTRACT.md §3) that the runtime resolves on PATH, and
// BuildMcpVerifyCommand's charset forbids spaces, so the direct verify path
// intentionally only exercises argv-less commands — matching the frozen contract,
// not a gap.
func verifyDirectChild(ctx context.Context, knownName, command string, args, env []string) error {
	p := &proxy{
		catalogReady:  make(chan struct{}),
		inflightByID:  make(map[int64]*inflightEntry),
		inflightByKey: make(map[string]int64),
	}

	c, err := spawn(knownName, command, args, env)
	if err != nil {
		return fmt.Errorf("spawn %q: %w", knownName, err)
	}
	defer func() {
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}()
	go c.readLoop(p)

	if err := c.initialize(ctx, p, selftestProtocolVersion); err != nil {
		// A child that exits before/at initialize surfaces as "child died";
		// a silent child surfaces as a ctx-deadline timeout. Both are named
		// distinctly by childCall's own error text.
		return fmt.Errorf("MCP handshake with %q failed: %w", knownName, err)
	}

	if len(c.tools) == 0 {
		return fmt.Errorf("MCP server %q returned an empty tool catalog (initialize succeeded but tools/list exported nothing)", knownName)
	}

	if missing := missingExpectedTools(knownName, catalogNames(c.tools)); len(missing) > 0 {
		return fmt.Errorf("MCP server %q is missing expected tool(s): %s (exported: %s)",
			knownName, strings.Join(missing, ", "), strings.Join(catalogNames(c.tools), ", "))
	}
	return nil
}

// verifyMux implements --verify-mcp mux mode: read mcp-mux.json, spawn its
// children directly, run the full selftest check set, then assert the
// expected-names union of the known children.
func verifyMux(ctx context.Context) error {
	path, err := resolveConfigPath()
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}

	children, catalog, err := runCatalogCheck(ctx, cfg, selftestProtocolVersion)
	defer func() {
		for _, c := range children {
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
	}()
	if err != nil {
		return err
	}

	// Expected-names union: for every configured server that is known, every
	// expected tool name must appear in the merged catalog.
	names := catalogNames(catalog)
	var missing []string
	for _, srv := range cfg.Servers {
		missing = append(missing, missingExpectedTools(srv.Name, names)...)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("MCP multiplexer is missing expected tool(s): %s (merged catalog: %s)",
			strings.Join(missing, ", "), strings.Join(names, ", "))
	}
	return nil
}

// missingExpectedTools returns the expected tool names for server that are not
// present in have. An unknown server ("where available" not satisfied) yields
// no missing names — only a non-empty catalog is required for it.
func missingExpectedTools(server string, have []string) []string {
	expected, ok := knownServerTools[server]
	if !ok {
		return nil
	}
	present := make(map[string]bool, len(have))
	for _, n := range have {
		present[n] = true
	}
	var missing []string
	for _, want := range expected {
		if !present[want] {
			missing = append(missing, want)
		}
	}
	return missing
}

// catalogNames extracts the tool names from a slice of toolEntry.
func catalogNames(tools []toolEntry) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.name
	}
	return names
}
