package install

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// lockfileRoot is the subset of a package-lock.json this package asserts on.
type lockfileRoot struct {
	Name            string `json:"name"`
	LockfileVersion int    `json:"lockfileVersion"`
	Packages        map[string]struct {
		Name      string `json:"name"`
		Resolved  string `json:"resolved"`
		Integrity string `json:"integrity"`
	} `json:"packages"`
}

// TestEmbeddedLockfile_PinsEveryPackage is what makes the integrity claim
// mechanical rather than aspirational: `npm ci` only verifies what the
// lockfile pins, so a lockfile entry without an integrity hash is a package
// that would be fetched on trust at deploy time. The trees are ~111 and ~25
// packages and nobody will audit them by eye.
func TestEmbeddedLockfile_PinsEveryPackage(t *testing.T) {
	for spawnCommand, spec := range adapterSpecs {
		for version, raw := range spec.Lockfiles {
			id := spawnCommand + "@" + version
			var lf lockfileRoot
			if err := json.Unmarshal(raw, &lf); err != nil {
				t.Fatalf("adapter %s: lockfile is not valid JSON: %v", id, err)
			}
			if lf.LockfileVersion < 3 {
				t.Errorf("adapter %s: lockfileVersion = %d, want >= 3 (v2 omits integrity for some entries)", id, lf.LockfileVersion)
			}

			resolved := 0
			for name, pkg := range lf.Packages {
				if pkg.Resolved == "" {
					continue // the root project entry has no tarball
				}
				resolved++
				if pkg.Integrity == "" {
					t.Errorf("adapter %s: package %q has a resolved URL but no integrity hash — npm ci would fetch it unverified", id, name)
				}
			}
			if resolved == 0 {
				t.Errorf("adapter %s: lockfile pins no packages at all", id)
			}

			// The platform package carries the actual runtime and is by far
			// the largest thing installed; it must be pinned for the host
			// the sandbox actually runs (linux/x64, glibc).
			var platform string
			switch spawnCommand {
			case "claude-agent-acp":
				platform = "node_modules/@anthropic-ai/claude-agent-sdk-linux-x64"
			case "codex-acp":
				platform = "node_modules/@openai/codex-linux-x64"
			default:
				t.Fatalf("adapter %s has no known linux-x64 platform package; add one so this test cannot silently pass for a new adapter", id)
			}
			if _, ok := lf.Packages[platform]; !ok {
				t.Errorf("adapter %s: lockfile has no %s entry; the sandbox could resolve it outside the lockfile", id, platform)
			}
		}
	}
}

// TestAdapterLockfile_RootNameMatchesSpec pins a mismatch that fails only at
// deploy time, inside the sandbox, as an opaque `npm ci` error: npm refuses
// to run when the manifest's root name disagrees with the lockfile's. The
// manifest is rendered from the spec, so the committed lockfile must be
// generated from that same name.
func TestAdapterLockfile_RootNameMatchesSpec(t *testing.T) {
	for spawnCommand, spec := range adapterSpecs {
		for version, raw := range spec.Lockfiles {
			var lf lockfileRoot
			if err := json.Unmarshal(raw, &lf); err != nil {
				t.Fatalf("adapter %s@%s: %v", spawnCommand, version, err)
			}
			if lf.Name != spec.PackageJSONName {
				t.Errorf("adapter %s@%s: lockfile root name = %q, spec renders %q", spawnCommand, version, lf.Name, spec.PackageJSONName)
			}
			if got := lf.Packages[""].Name; got != spec.PackageJSONName {
				t.Errorf("adapter %s@%s: lockfile packages[\"\"].name = %q, spec renders %q", spawnCommand, version, got, spec.PackageJSONName)
			}
		}
	}
}

// TestAdapterSpecs_Coherent pins the invariants that make the table safe to
// extend: distinct npm trees (one tree cannot hold two package.json roots),
// a key that matches the symlinked binary name, and a default version that
// is actually pinned.
func TestAdapterSpecs_Coherent(t *testing.T) {
	dirs := map[string]string{}
	for spawnCommand, spec := range adapterSpecs {
		if spec.BinName != spawnCommand {
			t.Errorf("adapter %q: BinName = %q; the map key IS the spawn command and the symlinked name", spawnCommand, spec.BinName)
		}
		if _, ok := spec.Lockfiles[spec.DefaultVersion]; !ok {
			t.Errorf("adapter %q: DefaultVersion %q has no committed lockfile", spawnCommand, spec.DefaultVersion)
		}
		if prev, ok := dirs[spec.Dir]; ok {
			t.Errorf("adapter %q shares its npm tree %q with %q; each adapter needs its own", spawnCommand, spec.Dir, prev)
		}
		dirs[spec.Dir] = spawnCommand
		for _, field := range []string{spec.Label, spec.Package, spec.Dir, spec.PackageJSONName, spec.DefaultVersion} {
			if field == "" {
				t.Errorf("adapter %q has an empty spec field", spawnCommand)
			}
		}
	}
}

// claudeInstallScriptDigest0_73_0 is the sha256 of the exact script rendered
// for the pinned adapter and integrity lockfile. A dependency or renderer
// change must update this review artifact deliberately.
//
// Refreshed 2026-09-09 when the adapter advanced from 0.63.0 to 0.73.0 to
// clear the current fast-uri high-severity advisories.
const claudeInstallScriptDigest0_73_0 = "8ebd1def734ca8fea480efcca8ffbe4565e2b2a4a09e590867ad59e6e1055bb2"

func TestBuildAdapterInstallScript_ClaudeByteIdenticalToBaseline(t *testing.T) {
	script, err := BuildAdapterInstallScript("claude-agent-acp", "0.73.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(script))); got != claudeInstallScriptDigest0_73_0 {
		t.Errorf("claude adapter install script changed.\n got sha256 = %s\nwant sha256 = %s\n(%d bytes rendered)", got, claudeInstallScriptDigest0_73_0, len(script))
	}
}

func TestBuildAdapterInstallScript_DefaultVersion(t *testing.T) {
	for spawnCommand, spec := range adapterSpecs {
		script, err := BuildAdapterInstallScript(spawnCommand, "")
		if err != nil {
			t.Fatalf("adapter %s: unexpected error: %v", spawnCommand, err)
		}
		for _, want := range []string{
			"set -eu",
			"npm ci --omit=dev --ignore-scripts --no-audit --no-fund",
			`cd "$ADAPTER_DIR"`,
			spec.DefaultVersion,
			`ln -sf "$ADAPTER_BIN"`,
			`ADAPTER_DIR="` + spec.Dir + `"`,
			`"$BIN_DIR/` + spec.BinName + `"`,
		} {
			if !strings.Contains(script, want) {
				t.Errorf("adapter %s: script missing %q", spawnCommand, want)
			}
		}
		// set -x would echo every command, and this script writes a
		// lockfile and manifest inline.
		if strings.Contains(script, "set -x") {
			t.Errorf("adapter %s: install script must not enable tracing", spawnCommand)
		}
		// The symlink must land in the dir launch.sh already prepends to
		// PATH, which is what lets buzz-acp spawn the adapter by bare name.
		if !strings.Contains(script, `BIN_DIR="`+BinDir+`"`) {
			t.Errorf("adapter %s: must be exposed via the existing bin dir %q", spawnCommand, BinDir)
		}
	}
}

// TestBuildAdapterInstallScript_CodexLinksOnlyCodexACP is the install half of
// the ucode-wrapper defense. The codex adapter's node_modules/.bin ships BOTH
// `codex-acp` and a real `codex`, and the sandbox image already has its own
// `/usr/local/bin/codex` that does not speak ACP on stdio
// (docs/M3_CODEX_PROBE_RESULTS.md S1). Symlinking bare `codex` into BinDir
// would shadow the image's tooling for everything else in the sandbox — an
// unrequested side effect on software this provider does not own.
func TestBuildAdapterInstallScript_CodexLinksOnlyCodexACP(t *testing.T) {
	script, err := BuildAdapterInstallScript("codex-acp", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, `ln -sf "$ADAPTER_BIN" "$BIN_DIR/codex-acp"`) {
		t.Error("codex adapter must symlink codex-acp")
	}
	if strings.Contains(script, `"$BIN_DIR/codex"`) {
		t.Error(`codex adapter must NOT symlink bare "codex" into BinDir; it would shadow the image's ucode wrapper`)
	}
}

func TestBuildAdapterInstallScript_UnknownRuntimeFailsLoud(t *testing.T) {
	// buzz-agent ships in the .deb and has no npm adapter: reaching here
	// for it is a caller bug, not a request to install something.
	if _, err := BuildAdapterInstallScript("buzz-agent", ""); err == nil {
		t.Error("a runtime with no adapter must be rejected, not silently defaulted")
	}
	if _, err := BuildAdapterInstallScript("goose", ""); err == nil {
		t.Error("an unknown spawn command must be rejected")
	}
}

func TestBuildAdapterInstallScript_UnknownVersionFailsLoud(t *testing.T) {
	for spawnCommand, spec := range adapterSpecs {
		_, err := BuildAdapterInstallScript(spawnCommand, "9.9.9-nope")
		if err == nil {
			t.Fatalf("adapter %s: an unpinned adapter version must be rejected, not installed on trust", spawnCommand)
		}
		var unknown UnknownAdapterVersionError
		if !strings.Contains(err.Error(), "9.9.9-nope") {
			t.Errorf("adapter %s: error should name the rejected version: %v", spawnCommand, err)
		}
		// The error must name the adapter actually hit, so an operator who
		// overrode the wrong runtime's version can tell which one it was.
		if !strings.Contains(err.Error(), spec.Package) {
			t.Errorf("adapter %s: error should name %q: %v", spawnCommand, spec.Package, err)
		}
		if !strings.Contains(err.Error(), spec.DefaultVersion) {
			t.Errorf("adapter %s: error should list the known version %q: %v", spawnCommand, spec.DefaultVersion, err)
		}
		if _, ok := err.(UnknownAdapterVersionError); !ok {
			t.Errorf("adapter %s: error should be %T, got %T", spawnCommand, unknown, err)
		}
	}
}

// TestAdapterStamp_IncludesLockfileHash pins that regenerating the lockfile
// at the SAME adapter version still forces a reinstall. A version-only
// marker would silently skip it, leaving a stale tree that no longer
// matches the committed pin.
func TestAdapterStamp_IncludesLockfileHash(t *testing.T) {
	a := adapterStamp("0.73.0", []byte(`{"a":1}`))
	b := adapterStamp("0.73.0", []byte(`{"a":2}`))
	if a == b {
		t.Fatal("stamp must change when the lockfile changes at the same version")
	}
	if !strings.HasPrefix(a, "0.73.0+") {
		t.Fatalf("stamp should remain human-readable at the version prefix, got %q", a)
	}
}

func TestVerifySpecFor_PerRuntime(t *testing.T) {
	if got := VerifySpecFor("buzz-agent").Bin; got != BinDir+"/buzz-agent" {
		t.Errorf("buzz-agent spec = %q", got)
	}
	if got := VerifySpecFor("claude-agent-acp").Bin; got != BinDir+"/claude-agent-acp" {
		t.Errorf("claude spec = %q", got)
	}
	// An unroutable runtime must produce an empty spec, which
	// BuildVerifyCommand then rejects — better than silently verifying the
	// wrong binary.
	if got := VerifySpecFor("goose").Bin; got != "" {
		t.Errorf("unknown runtime should yield an empty spec, got %q", got)
	}
}

func TestBuildVerifyCommand_RejectsEmptyAndHostileBins(t *testing.T) {
	if _, err := BuildVerifyCommand(`$HOME/.env.verify`, 10, VerifySpec{}); err == nil {
		t.Error("an empty verify binary must be rejected")
	}
	for _, bin := range []string{
		`$HOME/bin/x"; rm -rf /; echo "`,
		"$HOME/bin/$(whoami)",
		"$HOME/bin/`id`",
		"$HOME/bin/x y",
		"$HOME/bin/x\\ny",
	} {
		if cmd, err := BuildVerifyCommand(`$HOME/.env.verify`, 10, VerifySpec{Bin: bin}); err == nil {
			t.Errorf("hostile bin %q should have been rejected, got script: %q", bin, cmd)
		}
	}
}

// TestBuildVerifyCommand_PreservesExitStatus pins the property a naive
// `| tail -c` would silently destroy: a pipeline reports its LAST command's
// status, so bounding the output inline would make every failed handshake
// look like a success. Capturing to a file keeps both properties.
func TestBuildVerifyCommand_PreservesExitStatus(t *testing.T) {
	cmd, err := BuildVerifyCommand(`$HOME/.buzz-backend/.env.verify`, 10, VerifySpecFor("claude-agent-acp"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(cmd, `| tail -c`) {
		t.Error("output must not be bounded inside the pipeline; that would mask the handshake's exit status")
	}
	if !strings.Contains(cmd, "exit $rc") {
		t.Error("script must propagate the handshake's own exit status")
	}
	if !strings.Contains(cmd, "2>&1") {
		t.Error("stderr must be captured; sshx discards it on the success path this output is scanned on")
	}
	if !strings.Contains(cmd, "claude-agent-acp") {
		t.Error("claude verify should invoke the adapter binary")
	}
}
