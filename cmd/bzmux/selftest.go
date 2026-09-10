package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

// runSelftest implements --selftest mode (§5C, plan step 3 "mux-selftest"):
// load the config, spawn + initialize every child, perform one internal
// tools/list merge, run all §5B validations, print the merged tool count to
// stderr, and exit 0 on success or non-zero with a clear message on any
// violation.  No agent stdin/stdout loop is started.
func runSelftest() {
	path, err := resolveConfigPath()
	if err != nil {
		logf("bzmux --selftest: resolve config path: %v\n", err)
		os.Exit(1)
	}
	logf("bzmux --selftest: loading config from %s\n", path)

	cfg, err := loadConfig(path)
	if err != nil {
		logf("bzmux --selftest: %v\n", err)
		os.Exit(1)
	}

	// runSelftest has no ctx of its own, so it preserves the historical 30s
	// per-child isolation by bounding runCatalogCheck with childInitTimeout
	// (issue #17, plan step 1b).
	ctx, cancel := context.WithTimeout(context.Background(), childInitTimeout)
	defer cancel()

	children, catalog, err := runCatalogCheck(ctx, cfg, selftestProtocolVersion)
	// Kill all spawned children on exit (selftest is a one-shot check).
	defer func() {
		for _, c := range children {
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
	}()
	if err != nil {
		logf("bzmux --selftest: FAIL: %v\n", err)
		os.Exit(1)
	}

	// Success: print summary.
	names := make([]string, len(children))
	for i, c := range children {
		names[i] = fmt.Sprintf("%s (%d tools)", c.name, len(c.tools))
	}
	logf("bzmux --selftest: OK: %d tools merged from %d children: %s\n",
		len(catalog), len(children), strings.Join(names, ", "))
}

// selftestProtocolVersion is the MCP protocolVersion bzmux offers its children
// during --selftest and --verify-mcp handshakes (mirrors the live proxy's
// default in handleInitialize).
const selftestProtocolVersion = "2024-11-05"

// runCatalogCheck is the shared spawn+initialize+merge+validate core used by
// BOTH runSelftest and runVerifyMCP (mux mode), so their checks cannot drift
// apart and mux-mode verify stays a literal superset of the selftest (issue
// #17, plan Question A/E). It spawns and initializes every server in cfg
// concurrently (each bounded by ctx), then runs the §5B collision/`__`/64-byte
// budget validations (via initialize+mergeCatalogs) and the buildToolsListResult
// JSON smoke check. Children may negotiate different supported protocol versions;
// each child's selected version is tracked and sent back by its own bridge.
//
// It returns the spawned children (ALWAYS, even on error, so the caller can
// kill them) and the merged catalog. A non-nil error carries a clear,
// operator-facing message naming the violation.
func runCatalogCheck(ctx context.Context, cfg *muxcfg.Config, protocolVersion string) ([]*child, []toolEntry, error) {
	type childResult struct {
		c   *child
		err error
	}
	ch := make(chan childResult, len(cfg.Servers))

	// We need a minimal proxy for the inflight machinery used by childCall.
	// No agent is connected, so agentIn/Out are nil.
	p := &proxy{
		cfg:           cfg,
		catalogReady:  make(chan struct{}),
		inflightByID:  make(map[int64]*inflightEntry),
		inflightByKey: make(map[string]int64),
	}

	for _, srv := range cfg.Servers {
		srv := srv
		go func() {
			env := buildChildEnv(srv.Env)
			c, err := spawn(srv.Name, srv.Command, srv.Args, env)
			if err != nil {
				ch <- childResult{err: fmt.Errorf("spawn %q: %w", srv.Name, err)}
				return
			}
			go c.readLoop(p)
			if err := c.initialize(ctx, p, protocolVersion); err != nil {
				c.markDead(err)
				ch <- childResult{c: c, err: err}
				return
			}
			ch <- childResult{c: c}
		}()
	}

	var children []*child
	var errParts []string
	for range cfg.Servers {
		r := <-ch
		if r.err != nil {
			errParts = append(errParts, r.err.Error())
		}
		if r.c != nil {
			children = append(children, r.c)
		}
	}

	if len(errParts) > 0 {
		return children, nil, fmt.Errorf("child initialization errors: %s", strings.Join(errParts, "; "))
	}

	// §5B validations: collision, __, 64-byte budget.
	_, catalog, err := mergeCatalogs(children)
	if err != nil {
		return children, nil, fmt.Errorf("catalog validation: %w", err)
	}

	// Check that the merged catalog JSON is valid (smoke check).
	if _, err := buildToolsListResult(catalog); err != nil {
		return children, nil, fmt.Errorf("build tools/list: %w", err)
	}

	return children, catalog, nil
}
