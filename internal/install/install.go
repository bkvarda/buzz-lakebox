// Package install renders the in-sandbox shell script that fetches,
// verifies, and extracts the pinned Buzz .deb (docs/PLAN.md §4.4 step 5),
// plus the runtime-verification command that pipes an ACP `initialize`
// frame into buzz-agent (docs/M05_PROBE_RESULTS.md §6). Nothing here talks
// to a sandbox directly — internal/deployflow ships the rendered text over
// internal/sshx.
package install

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
)

// DefaultVersion is the build-time-pinned Buzz release installed when a
// deploy request's provider_config.buzz_version is empty
// (docs/PLAN.md §4.4 step 5).
const DefaultVersion = "v0.5.23"

// releaseRepo is the GitHub repository Buzz release .deb assets are
// fetched from. URL template verified live 2026-07-24:
// https://github.com/block/buzz/releases/download/desktop-v0.5.23/Buzz_0.5.23_amd64.deb
// resolves to the release asset CDN. Desktop release tags gained the
// `desktop-` prefix after the original provider pin, while asset names keep
// the plain semantic version.
const releaseRepo = "block/buzz"

// pinnedSHA256 maps a known Buzz release version to its published .deb
// sha256 (docs/CONTRACT.md / M1 task: "make version→sha a small map so
// overriding version without a known sha fails loud rather than skipping
// verification").
var pinnedSHA256 = map[string]string{
	DefaultVersion: "94f1e50021f88f8864f568c86a8ea2b39993da64e3fe8d86bf731cd00c1c9cce",
}

// BinNames are the executables symlinked into $HOME/.buzz-backend/bin
// after extraction (docs/PLAN.md §4.4 step 5).
var BinNames = []string{"buzz-acp", "buzz", "buzz-agent", "buzz-dev-mcp", "git-credential-nostr"}

// DistDir / BinDir / versionMarkerFile are the well-known in-sandbox paths
// the rendered install script manages, exported so internal/deployflow and
// tests can refer to them without re-deriving the layout.
const (
	DistDir          = "$HOME/.buzz-backend/dist"
	BinDir           = "$HOME/.buzz-backend/bin"
	versionMarkerRel = ".installed_version"
)

// UnknownVersionError is returned by BuildInstallScript when the
// requested version has no known pinned sha256 — overriding the version
// without a known checksum must fail loud rather than silently skip
// verification.
type UnknownVersionError struct {
	Version string
}

func (e UnknownVersionError) Error() string {
	return fmt.Sprintf("no known sha256 pin for buzz version %q; refusing to install without checksum verification (known versions: %s)", e.Version, strings.Join(knownVersions(), ", "))
}

func knownVersions() []string {
	out := make([]string, 0, len(pinnedSHA256))
	for v := range pinnedSHA256 {
		out = append(out, v)
	}
	return out
}

// BuildInstallScript renders the `set -eu` (no `set -x`) install script
// for the given pinned version (DefaultVersion if empty): curl -fL the
// release .deb, verify its pinned sha256, dpkg-deb -x it into
// $HOME/.buzz-backend/dist, and symlink BinNames into
// $HOME/.buzz-backend/bin. Skips the download/extract entirely when the
// version marker file already matches (docs/PLAN.md §4.4 step 5).
func BuildInstallScript(version string) (string, error) {
	if version == "" {
		version = DefaultVersion
	}
	version = strings.TrimPrefix(version, "desktop-")
	sha, ok := pinnedSHA256[version]
	if !ok {
		return "", UnknownVersionError{Version: version}
	}

	releaseTag := version
	if strings.HasPrefix(version, "v0.5.") {
		releaseTag = "desktop-" + version
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/Buzz_%s_amd64.deb", releaseRepo, releaseTag, strings.TrimPrefix(version, "v"))

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n\n")
	fmt.Fprintf(&b, "BUZZ_VERSION=%s\n", shellquote.Single(version))
	fmt.Fprintf(&b, "BUZZ_SHA256=%s\n", shellquote.Single(sha))
	fmt.Fprintf(&b, "BUZZ_URL=%s\n", shellquote.Single(url))
	b.WriteString(`DIST_DIR="$HOME/.buzz-backend/dist"
BIN_DIR="$HOME/.buzz-backend/bin"
MARKER="$DIST_DIR/` + versionMarkerRel + `"

mkdir -p "$DIST_DIR" "$BIN_DIR"

if [ -f "$MARKER" ] && [ "$(cat "$MARKER")" = "$BUZZ_VERSION" ]; then
  echo "buzz $BUZZ_VERSION already installed; skipping download"
else
  TMP_DEB=$(mktemp "${TMPDIR:-/tmp}/buzz-XXXXXX.deb")
  trap 'rm -f "$TMP_DEB"' EXIT
  curl -q -fL --retry 2 -o "$TMP_DEB" "$BUZZ_URL"
  echo "$BUZZ_SHA256  $TMP_DEB" | sha256sum -c -
  rm -rf "$DIST_DIR"
  mkdir -p "$DIST_DIR"
  dpkg-deb -x "$TMP_DEB" "$DIST_DIR"
  rm -f "$TMP_DEB"
  trap - EXIT
  printf '%s' "$BUZZ_VERSION" > "$MARKER"
fi

`)
	for _, bin := range BinNames {
		fmt.Fprintf(&b, `SRC=$(find "$DIST_DIR" -type f -name %s 2>/dev/null | head -n1)
if [ -z "$SRC" ]; then
  echo "installed .deb did not contain expected binary: %s" >&2
  exit 1
fi
ln -sf "$SRC" "$BIN_DIR/%s"
`, shellquote.Single(bin), bin, bin)
	}

	return b.String(), nil
}

// InitializeFrame is the ACP `initialize` JSON-RPC request piped into
// buzz-agent for runtime verification (docs/M05_PROBE_RESULTS.md §6: no
// `--version`; config validation runs before arg parsing).
//
// protocolVersion is an INTEGER, not the "0.1.0"-style string this
// frame originally carried: buzz-agent deserializes it as u32 and
// rejects strings outright with `initialize: invalid type: string
// "0.1.0", expected u32` (live-bitten during a real deploy;
// block/buzz crates/buzz-agent README + wire.rs). 1 is the lowest
// version every shipped buzz-agent accepts — the agent negotiates
// upward/downward itself, and this frame only needs a liveness answer,
// so pin the most compatible request, not the newest.
const InitializeFrame = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`

// AgentInfoMarker is the substring expected in the agent's response to
// InitializeFrame on success (docs/M05_PROBE_RESULTS.md §6: "expect the
// agentInfo result").
//
// It is shared by EVERY runtime, which was not a given: the ACP spec makes
// agentInfo optional, and buzz-acp itself tolerates its absence (reads
// agentInfo or serverInfo, then defaults to "unknown"). Probed live against
// claude-agent-acp@0.73.0 (docs/M2_CLAUDE_PROBE_RESULTS.md P5), whose reply
// carries `"agentInfo":{"name":"@agentclientprotocol/claude-agent-acp",...}`
// — so no per-runtime marker is needed and VerifySpec carries only a path.
const AgentInfoMarker = "agentInfo"

// VerifySpec is the per-runtime part of the verification handshake. The
// frame (InitializeFrame) and the success marker (AgentInfoMarker) are
// shared across runtimes; what differs is the binary and how it treats
// stdin.
type VerifySpec struct {
	// Bin is a TRUSTED, static "$HOME"-relative literal naming the
	// executable to hand the initialize frame to. Never payload data —
	// it is validated against verifyEnvFileCharset for the same reason
	// envFile is.
	Bin string

	// StdinHoldSeconds keeps the handshake's stdin open for this long
	// AFTER the frame is written, for runtimes that will not answer a
	// stdin that closes immediately. 0 means close right away.
	//
	// This is not a tuning knob, it is a correctness requirement, and it
	// differs per runtime because the adapters genuinely differ.
	// claude-agent-acp answers and exits 0 on EOF in ~355ms
	// (docs/M2_CLAUDE_PROBE_RESULTS.md P5), so it holds nothing and its
	// rendered command is unchanged. codex-acp writes NOTHING AT ALL when
	// stdin closes with the frame — measured: `printf FRAME | codex-acp`
	// exits 0 with zero bytes of output, while the same frame with stdin
	// held open for even one second returns the full initialize reply.
	//
	// Without this the marker scan below would find nothing and EVERY
	// codex deploy would die at CodeRuntimeVerify — a total runtime
	// failure, presented as a handshake error. docs/M3_CODEX_PROBE_RESULTS.md
	// S4 recorded the behaviour ("an easy false-negative when probing");
	// S11 then masked it by holding stdin open, which is not what this
	// command does.
	StdinHoldSeconds int
}

// VerifySpecFor returns the verification spec for a runtime, keyed by the
// canonical spawn command (payload.Runtime.SpawnCommand()). Taking a string
// rather than payload.Runtime keeps internal/install free of a dependency on
// internal/payload, matching how the rest of this package is decoupled.
//
// An unknown spawn command yields an empty Bin, which BuildVerifyCommand
// rejects — an unroutable runtime must fail loudly rather than silently
// verify the wrong binary.
func VerifySpecFor(spawnCommand string) VerifySpec {
	// buzz-agent ships in the .deb rather than as an npm adapter, so it has
	// no adapterSpecs row; every other runtime is verified via the binary
	// its adapter symlinks into BinDir under the same name.
	if spawnCommand == "buzz-agent" {
		return VerifySpec{Bin: BinDir + "/buzz-agent"}
	}
	if spec, ok := AdapterSpecFor(spawnCommand); ok {
		return VerifySpec{Bin: BinDir + "/" + spec.BinName, StdinHoldSeconds: spec.VerifyStdinHoldSeconds}
	}
	return VerifySpec{}
}

// BuildVerifyCommand renders the COMBINED verify script run as a single
// sshx.RunWithStdin round trip (BUG 1 fix): it reads the agent's env
// content from its own stdin, writes it to envFile (0600), sources it,
// and pipes InitializeFrame into buzz-agent with a timeout — removing
// envFile afterward regardless of outcome via a trap. No secret is ever
// interpolated into this command string itself; the only sanctioned path
// for the env content is stdin.
//
// envFile is a TRUSTED, static "$HOME"-relative literal (e.g.
// "$HOME/.buzz-backend/.env.verify") — NEVER payload/untrusted data — so
// it is interpolated via a double-quoted shell assignment (which must
// expand "$HOME") rather than shellquote.Single (which would instead
// suppress that expansion and break sourcing; this was the root cause of
// the deploy-breaking bug this function now fixes: the file was written
// with $HOME expanded but previously sourced/removed with $HOME literal,
// so sourcing always failed and the trap never removed the real file).
//
// Because envFile is interpolated inside double quotes unescaped, it is
// defensively validated against verifyEnvFileCharset: any character
// outside [A-Za-z0-9_$/.-] (double quote, backtick, $( ), backslash,
// whitespace, ...) is rejected with an error, so a future caller
// mistake can never smuggle shell syntax through this trusted-literal
// path.
// spec selects the runtime binary to hand the frame to and is validated
// against the same charset allowlist as envFile, for the same reason.
//
// The pipeline's exit status IS the verdict, unchanged from the buzz-agent
// era: claude-agent-acp exits 0 on stdin EOF in ~355ms, so `timeout` does
// not fire and no exit-code special-casing is needed
// (docs/M2_CLAUDE_PROBE_RESULTS.md P5). stderr is folded into the pipeline
// so adapter startup diagnostics survive: sshx returns stdout only on the
// SUCCESS path, which is exactly the path installAndVerify scans the marker
// on — without 2>&1 a agent that answers wrongly tells us nothing about why.
// It is bounded server-side with `tail -c` so a chatty runtime cannot push
// an unbounded blob across the SSH boundary into an error string.
func BuildVerifyCommand(envFile string, timeoutSeconds int, spec VerifySpec) (string, error) {
	if !verifyEnvFileCharset.MatchString(envFile) {
		return "", fmt.Errorf("verify env file path %q contains characters outside the allowed set [A-Za-z0-9_$/.-]; BuildVerifyCommand accepts trusted static literals only", envFile)
	}
	if spec.Bin == "" {
		return "", fmt.Errorf("verify spec has no binary; BuildVerifyCommand cannot verify an unknown runtime")
	}
	if !verifyEnvFileCharset.MatchString(spec.Bin) {
		return "", fmt.Errorf("verify binary path %q contains characters outside the allowed set [A-Za-z0-9_$/.-]; BuildVerifyCommand accepts trusted static literals only", spec.Bin)
	}
	// A runtime that will not answer a stdin closing with the frame needs
	// the pipe held open; see VerifySpec.StdinHoldSeconds. The brace group
	// keeps this a single stdin source for the pipeline.
	stdinSource := fmt.Sprintf("printf '%%s\\n' %s", shellquote.Single(InitializeFrame))
	if spec.StdinHoldSeconds > 0 {
		stdinSource = fmt.Sprintf("{ %s; sleep %d; }", stdinSource, spec.StdinHoldSeconds)
	}

	return fmt.Sprintf(`set -eu
umask 077
ENVF="%s"
OUTF="$ENVF.out"
trap 'rm -f "$ENVF" "$OUTF"' EXIT
cat > "$ENVF"
chmod 600 "$ENVF"
set -a
# shellcheck disable=SC1090
. "$ENVF"
set +a
rc=0
%s | timeout %d "%s" > "$OUTF" 2>&1 || rc=$?
tail -c %d "$OUTF"
exit $rc
`, envFile, stdinSource, timeoutSeconds, spec.Bin, maxVerifyOutputBytes), nil
}

// maxVerifyOutputBytes bounds the merged stdout+stderr the verify handshake
// may return across the SSH boundary. The initialize reply is ~700 bytes;
// this leaves room for a genuine diagnostic while capping a runaway log.
const maxVerifyOutputBytes = 4096

// verifyEnvFileCharset is the allowlist for BuildVerifyCommand's envFile
// parameter: path characters plus '$' (for the required "$HOME" prefix),
// and nothing that carries meaning inside a double-quoted shell string
// beyond parameter expansion the caller explicitly wants.
var verifyEnvFileCharset = regexp.MustCompile(`^[A-Za-z0-9_$/.\-]+$`)
