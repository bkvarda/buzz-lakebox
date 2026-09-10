package install

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

// MuxBinDir is the directory bzmux is installed into. It is the same BinDir
// that the Buzz .deb executables land in, so bzmux is on the sandbox PATH
// without any additional PATH manipulation.
const MuxBinDir = BinDir

// MuxBinPath is the full in-sandbox path of the bzmux executable. deployflow
// writes the embedded binary bytes here and then the install script chmod
// +x's it. bzmux locates its config file relative to this path (Decision C1).
const MuxBinPath = MuxBinDir + "/bzmux"

// MuxConfigPath is the full in-sandbox path of the mcp-mux.json configuration
// file. It lives beside the bzmux binary so bzmux can find it via
// filepath.Dir(os.Executable()) without depending on $HOME surviving any
// env_clear the agent runtime applies.
const MuxConfigPath = MuxBinDir + "/mcp-mux.json"

// MuxLaunchPath is a tiny provider-owned launcher for bzmux. Buzz-agent
// deliberately clears MCP child environments and forwards only its fixed
// passthrough set, which excludes Databricks credentials. The launcher sources
// the provider's 0600 env file inside the MCP child process, then execs bzmux;
// bzmux still applies each configured child's strict allowlist.
const (
	MuxLaunchName = "bzmux-launch"
	MuxLaunchPath = MuxBinDir + "/" + MuxLaunchName
)

// HTTPMCPBinPath is the embedded stdio-to-Streamable-HTTP bridge used by
// typed Databricks managed MCP entries.
const HTTPMCPBinPath = MuxBinDir + "/bzhttpmcp"

// MuxInstall is the install-local twin of any payload-level MCP-mux
// description. It carries everything internal/install needs to render the
// config JSON and chmod script without importing internal/payload — the same
// twin-struct decoupling used by ExtraBinaryInstall. Callers in
// internal/deployflow are responsible for mapping the payload struct across
// this boundary.
//
// The actual byte-writes (bzmux binary and mcp-mux.json) are performed by
// deployflow via SSH RunWithStdin calls. BuildMuxInstallScript renders the
// subsequent chmod commands that must run in the sandbox after both files are
// written.
type MuxInstall struct {
	// Config is the fully-populated multiplexer configuration that will be
	// marshalled to JSON and written to MuxConfigPath at deploy time.
	Config muxcfg.Config
}

// BuildMuxConfigJSON marshals cfg to a JSON representation of mcp-mux.json
// with deterministic field order (struct fields marshal in declaration order;
// map keys within ServerEnv.Set are sorted by json.Marshal). Two-space
// indentation is used for readability.
//
// Validation enforced here (belt-and-suspenders; the payload validator runs
// first at the call boundary):
//   - at least one server
//   - every server Name and Command are non-empty
//   - server Names are unique
//
// Returns an actionable error naming the offending server on any violation.
func BuildMuxConfigJSON(cfg muxcfg.Config) ([]byte, error) {
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("mux config must have at least one server")
	}
	seen := make(map[string]bool, len(cfg.Servers))
	for i, srv := range cfg.Servers {
		if srv.Name == "" {
			return nil, fmt.Errorf("server[%d]: name must not be empty", i)
		}
		if srv.Command == "" {
			return nil, fmt.Errorf("server %q: command must not be empty", srv.Name)
		}
		if seen[srv.Name] {
			return nil, fmt.Errorf("duplicate server name %q: each server name must be unique", srv.Name)
		}
		seen[srv.Name] = true
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// BuildMuxInstallScript renders a POSIX sh script that chmods the bzmux
// binary and config file after deployflow has written their bytes into the
// sandbox via RunWithStdin. The script does NOT write any bytes itself.
//
// deployflow is responsible for (in order):
//  1. RunWithStdin → write muxbin.Binary bytes to MuxBinPath
//  2. RunWithStdin → write BuildMuxConfigJSON bytes to MuxConfigPath
//  3. Run → execute the script returned here to apply permissions
//
// binPath and cfgPath must be trusted static "$HOME"-relative literals (they
// are validated against muxPathCharset for the same reason BuildVerifyCommand
// validates its paths). Both must be non-empty.
func BuildMuxInstallScript(binPath, cfgPath string) (string, error) {
	if !muxPathCharset.MatchString(binPath) {
		return "", fmt.Errorf("mux bin path %q contains characters outside the allowed set [A-Za-z0-9_$/.-]; BuildMuxInstallScript accepts trusted static literals only", binPath)
	}
	if !muxPathCharset.MatchString(cfgPath) {
		return "", fmt.Errorf("mux config path %q contains characters outside the allowed set [A-Za-z0-9_$/.-]; BuildMuxInstallScript accepts trusted static literals only", cfgPath)
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n")
	// The dir is guaranteed to exist because the Buzz .deb install (which
	// creates BinDir) runs before the mux install. mkdir -p is idempotent
	// and cheap insurance against a re-deploy on a fresh sandbox.
	fmt.Fprintf(&b, "mkdir -p \"%s\"\n", MuxBinDir)
	fmt.Fprintf(&b, "chmod 755 \"%s\"\n", binPath)
	fmt.Fprintf(&b, "chmod 600 \"%s\"\n", cfgPath)
	return b.String(), nil
}

// muxPathCharset is the allowlist for BuildMuxInstallScript path parameters.
// Mirrors verifyEnvFileCharset from install.go: path characters plus '$' for
// "$HOME" prefix, and nothing that carries shell syntax inside an unquoted or
// double-quoted context.
var muxPathCharset = regexp.MustCompile(`^[A-Za-z0-9_$/.\-]+$`)
