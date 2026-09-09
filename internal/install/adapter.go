package install

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
)

// adapterMarkerRel is the install-skip marker, relative to a spec's Dir.
// adapterPackageJSON renders the manifest `npm ci` reads; its %s slots are
// the root name, the package, and the version.
const (
	adapterMarkerRel   = ".installed_adapter"
	adapterPackageJSON = `{"name":"%s","private":true,"version":"0.0.0","dependencies":{%s:%s}}`
)

//go:embed lockfiles/claude-agent-acp-0.73.0.package-lock.json
var adapterLockfileClaude0_73_0 []byte

//go:embed lockfiles/codex-acp-1.8.0.package-lock.json
var adapterLockfileCodex1_8_0 []byte

// AdapterSpec is everything that differs between one ACP adapter and
// another. It exists so that adding a runtime is a table row rather than an
// edit to five hardcoded constants — the shape this package had while
// exactly one adapter existed, which could not express a second.
type AdapterSpec struct {
	// Label names the runtime in the install script's own output. It is
	// operator-facing text only; nothing branches on it.
	Label string

	// Package is the npm package installed.
	Package string

	// BinName is the executable the package places in node_modules/.bin,
	// and MUST equal the runtime's payload.Runtime.SpawnCommand(): it is
	// the bare name buzz-acp spawns, and the name symlinked into BinDir.
	BinName string

	// Dir is the $HOME-scoped npm tree, kept beside dist/ and bin/ under
	// the same .buzz-backend root. Each adapter needs its OWN tree: one
	// npm tree cannot hold two package.json roots.
	Dir string

	// PackageJSONName is the root "name" of the rendered manifest. It must
	// match the committed lockfile's own root name, or `npm ci` refuses to
	// run — see TestAdapterLockfile_RootNameMatchesSpec.
	PackageJSONName string

	// DefaultVersion is the build-time pin used when the payload's
	// per-runtime adapter version override is empty.
	DefaultVersion string

	// VerifyStdinHoldSeconds is this adapter's VerifySpec.StdinHoldSeconds
	// — how long the verification handshake must keep stdin open after
	// writing the initialize frame. See VerifySpec for why it is a
	// correctness requirement rather than a tuning knob.
	VerifyStdinHoldSeconds int

	// Lockfiles maps a pinned version to its committed package-lock.json.
	// This is the integrity pin, and the reason the install uses `npm ci`
	// rather than `npm install`: the lockfile carries a sha512 for every
	// package in the tree — including the large platform binary that
	// actually contains the executed runtime — and `npm ci` aborts on any
	// mismatch.
	//
	// Note this is registry trust-on-first-use with a long memory, NOT the
	// same guarantee as the .deb's maintainer-chosen sha256 pin: it
	// verifies that the bytes match what the registry served when the
	// lockfile was generated, not that anyone read them. It is still
	// strictly stronger than pinning only the top-level package and
	// letting the transitive tree resolve at deploy time.
	Lockfiles map[string][]byte
}

// adapterSpecs is keyed by canonical spawn command, NOT by payload.Runtime:
// taking a string keeps internal/install free of a dependency on
// internal/payload, matching how the rest of this package is decoupled (see
// VerifySpecFor).
var adapterSpecs = map[string]AdapterSpec{
	"claude-agent-acp": {
		Label:           "claude",
		Package:         "@agentclientprotocol/claude-agent-acp",
		BinName:         "claude-agent-acp",
		Dir:             "$HOME/.buzz-backend/npm-claude",
		PackageJSONName: "buzz-claude-adapter",
		DefaultVersion:  "0.73.0",

		// Answers and exits 0 on stdin EOF in ~355ms (probe P5), so the
		// handshake closes stdin immediately as it always has.
		VerifyStdinHoldSeconds: 0,

		// One lockfile covers every platform: npm records all 8
		// @anthropic-ai/claude-agent-sdk-<os>-<arch> optional dependencies
		// with integrity hashes and selects the matching one at install
		// time, so no --os/--cpu generation flags are needed.
		Lockfiles: map[string][]byte{"0.73.0": adapterLockfileClaude0_73_0},
	},

	"codex-acp": {
		Label:           "codex",
		Package:         "@agentclientprotocol/codex-acp",
		BinName:         "codex-acp",
		Dir:             "$HOME/.buzz-backend/npm-codex",
		PackageJSONName: "buzz-codex-adapter",
		DefaultVersion:  "1.8.0",

		// MUST be non-zero. `printf FRAME | codex-acp` exits 0 having
		// written nothing at all; the same frame with stdin held open for
		// one second returns the full initialize reply. 2s is that
		// measured minimum doubled, and it is paid once per deploy.
		VerifyStdinHoldSeconds: 2,

		// 25 packages, 0 missing integrity, 362 MB, 4.4s cold `npm ci`
		// (docs/M3_CODEX_PROBE_RESULTS.md S3).
		//
		// CONSTRAINT: unlike the claude lockfile, this one pins SIX
		// platform variants and NO -musl entry — @openai/codex ships
		// darwin/linux/win32 x arm64/x64 only. The Lakebox sandbox image
		// is glibc so this is fine today, but a musl base image would
		// break `npm ci` here rather than degrade.
		Lockfiles: map[string][]byte{"1.8.0": adapterLockfileCodex1_8_0},
	},
}

// AdapterBinNames returns the BinName of every adapterSpecs entry, sorted for
// determinism. It exists so that the reserved-name set a payload's extra
// binaries must not collide with (issue #18) can be built from install's own
// source of truth rather than duplicating the adapter bin names in
// internal/payload.
func AdapterBinNames() []string {
	out := make([]string, 0, len(adapterSpecs))
	for _, spec := range adapterSpecs {
		out = append(out, spec.BinName)
	}
	sort.Strings(out)
	return out
}

// AdapterSpecFor returns the adapter spec for a canonical spawn command.
// The second result is false for runtimes that need no npm adapter at all
// (buzz-agent ships in the .deb) and for unknown commands — callers must
// distinguish "no adapter needed" from "install this" rather than treating
// a zero spec as installable.
func AdapterSpecFor(spawnCommand string) (AdapterSpec, bool) {
	spec, ok := adapterSpecs[spawnCommand]
	return spec, ok
}

// UnknownAdapterVersionError is returned by BuildAdapterInstallScript when
// the requested adapter version has no committed lockfile — the structural
// twin of UnknownVersionError, and for the same reason: overriding the
// version without a pinned integrity source must fail loud rather than
// silently install whatever the registry currently serves.
type UnknownAdapterVersionError struct {
	// Package names the adapter whose version was rejected, so an operator
	// who overrode the wrong runtime's version sees which one they hit.
	Package string
	Version string
}

func (e UnknownAdapterVersionError) Error() string {
	return fmt.Sprintf(
		"no committed package-lock.json for %s version %q; refusing to install without integrity pinning (known versions: %s)",
		e.Package, e.Version, strings.Join(knownAdapterVersions(e.Package), ", "),
	)
}

// knownAdapterVersions lists the pinned versions of one adapter, found by
// package name so the error text lists that adapter's versions and not
// another runtime's.
func knownAdapterVersions(pkg string) []string {
	var out []string
	for _, spec := range adapterSpecs {
		if spec.Package != pkg {
			continue
		}
		for v := range spec.Lockfiles {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// adapterStamp is the marker-file content gating the install skip. It is
// <version>+<sha256 of the lockfile>, NOT the bare version: regenerating the
// lockfile at the same adapter version (a transitive dependency bump, a
// re-pin after a registry incident) must force a reinstall, which a
// version-only marker would silently skip.
func adapterStamp(version string, lockfile []byte) string {
	return fmt.Sprintf("%s+%x", version, sha256.Sum256(lockfile))
}

// BuildAdapterInstallScript renders the `set -eu` (no `set -x`) script that
// installs one runtime's ACP adapter into its own npm tree and exposes it as
// BinDir/<BinName>. spawnCommand selects the adapter; an unknown one is an
// error rather than a default, so a caller that reaches here for a runtime
// needing no adapter fails loudly.
//
// The symlink into the EXISTING BinDir is what makes this work with no PATH
// change anywhere: launch.sh already prepends $HOME/.buzz-backend/bin
// (internal/nest), buzz-acp spawns the agent by bare name with no path
// resolution or allowlist (block/buzz crates/buzz-acp/src/acp.rs:416), and
// the .deb installer only ever wipes $DIST_DIR, never $BIN_DIR — so the
// symlink survives a buzz_version bump.
//
// Exactly ONE name per runtime is symlinked. That matters for codex: the
// adapter's node_modules/.bin ships both `codex-acp` and a real `codex`, and
// the sandbox image already has a `/usr/local/bin/codex` that is a
// Databricks `ucode` wrapper (docs/M3_CODEX_PROBE_RESULTS.md S1). Linking
// only `codex-acp` leaves the image's own tooling alone.
//
// Measured on live sandboxes: claude 6s cold / 570 MB
// (docs/M2_CLAUDE_PROBE_RESULTS.md P4), codex 4.4s cold / 362 MB
// (docs/M3_CODEX_PROBE_RESULTS.md S3).
func BuildAdapterInstallScript(spawnCommand, version string) (string, error) {
	spec, ok := AdapterSpecFor(spawnCommand)
	if !ok {
		return "", fmt.Errorf("no ACP adapter is defined for spawn command %q; BuildAdapterInstallScript must not be called for runtimes that ship no adapter", spawnCommand)
	}
	if version == "" {
		version = spec.DefaultVersion
	}
	lockfile, ok := spec.Lockfiles[version]
	if !ok {
		return "", UnknownAdapterVersionError{Package: spec.Package, Version: version}
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n\n")
	fmt.Fprintf(&b, "ADAPTER_VERSION=%s\n", shellquote.Single(version))
	fmt.Fprintf(&b, "ADAPTER_STAMP=%s\n", shellquote.Single(adapterStamp(version, lockfile)))
	b.WriteString(`ADAPTER_DIR="` + spec.Dir + `"
BIN_DIR="` + BinDir + `"
MARKER="$ADAPTER_DIR/` + adapterMarkerRel + `"

mkdir -p "$ADAPTER_DIR" "$BIN_DIR"
`)

	// The manifest and lockfile are rewritten on every run (they are tiny
	// and fully determined by the pinned version), so a partially-written
	// tree from an interrupted deploy self-heals on the next one.
	fmt.Fprintf(&b, "\ncat > \"$ADAPTER_DIR/package.json\" <<'BUZZ_ADAPTER_PKG_EOF'\n%s\nBUZZ_ADAPTER_PKG_EOF\n",
		fmt.Sprintf(adapterPackageJSON, spec.PackageJSONName, `"`+spec.Package+`"`, `"`+version+`"`))
	fmt.Fprintf(&b, "\ncat > \"$ADAPTER_DIR/package-lock.json\" <<'BUZZ_ADAPTER_LOCK_EOF'\n%s\nBUZZ_ADAPTER_LOCK_EOF\n",
		strings.TrimRight(string(lockfile), "\n"))

	// `cd` rather than `npm ci --prefix`: --prefix semantics for `ci` have
	// varied across npm majors, and the probe ran the `cd` form verbatim.
	//
	// --ignore-scripts is safe here (probe P4 for claude, S3 for codex: the
	// tree still works and node_modules/.bin/<BinName> is still executable)
	// and removes package postinstall execution — worth having in
	// inference_auth "sandbox" mode, where the baked workspace credential
	// is present.
	fmt.Fprintf(&b, `
if [ -f "$MARKER" ] && [ "$(cat "$MARKER")" = "$ADAPTER_STAMP" ]; then
  echo "%s adapter $ADAPTER_VERSION already installed; skipping npm ci"
else`, spec.Label)
	b.WriteString(`
  cd "$ADAPTER_DIR"
  if ! npm ci --omit=dev --ignore-scripts --no-audit --no-fund; then
    echo "npm ci failed: integrity mismatch (do NOT retry - the registry served different bytes than the committed lockfile pins), or no egress to registry.npmjs.org" >&2
    exit 1
  fi
  printf '%s' "$ADAPTER_STAMP" > "$MARKER"
fi

`)

	// Outside the skip branch: a damaged or missing symlink self-heals even
	// when the npm tree itself is already current.
	fmt.Fprintf(&b, `ADAPTER_BIN="$ADAPTER_DIR/node_modules/.bin/%s"
if [ ! -x "$ADAPTER_BIN" ]; then
  echo "%s did not provide an executable %s after install" >&2
  exit 1
fi
ln -sf "$ADAPTER_BIN" "$BIN_DIR/%s"
`, spec.BinName, spec.Package, spec.BinName, spec.BinName)

	return b.String(), nil
}
