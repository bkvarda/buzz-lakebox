package install

import (
	"fmt"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
	"github.com/IceRhymers/buzz-lakebox/internal/skillconfig"
)

const (
	// SkillsDir is the canonical harness-independent skill destination.
	SkillsDir = "$HOME/.buzz/.agents/skills"
	// AIToolsSkillMarker is kept inside each managed skill. Its exact content
	// distinguishes provider-owned directories from unmanaged user content.
	AIToolsSkillMarker = ".buzz-aitools-managed"
	// AIToolsSkillMarkerVersion is bumped when ownership/install semantics
	// change. Replacement requires an exact match, not merely marker presence.
	AIToolsSkillMarkerVersion = "buzz-databricks-aitools:v1"
	// MaxAIToolsSkillBytes bounds all regular files emitted in one invocation.
	MaxAIToolsSkillBytes int64 = 16 * 1024 * 1024
)

// BuildSkillsInstallScript validates cfg and renders a POSIX sh script that
// synchronizes the optional Databricks aitools skills into SkillsDir. The
// command only performs a local raw-skill install into a private staging
// directory; workspace/profile/URL/auth inputs cannot be represented by cfg.
//
// Staged output is treated as hostile: every top-level entry must be a safe,
// non-symlink directory containing a regular non-symlink SKILL.md; special
// files and symlinks anywhere in a skill are rejected; count and bytes are
// bounded before destination changes. Existing destinations are preserved
// unless replace-managed is selected and their exact provider marker matches.
func BuildSkillsInstallScript(cfg skillconfig.Config) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", fmt.Errorf("build skills install script: %w", err)
	}
	if cfg.AITools == nil {
		return "#!/bin/sh\nset -eu\numask 077\n# Databricks aitools skill synchronization is disabled.\n", nil
	}

	ai := *cfg.AITools
	policy := ai.EffectiveCollisionPolicy()

	var command strings.Builder
	// --path is already the noninteractive raw-skill mode. Current aitools
	// rejects both --skills-only and --output json when --path is present, so
	// combining those flags makes every live skill sync fail before staging.
	command.WriteString("databricks aitools install --path \"$STAGING\"")
	if len(ai.Skills) > 0 {
		// Identifiers were validated to exclude comma and shell syntax. Quoting is
		// still mandatory because this is configuration-derived text.
		command.WriteString(" --skills ")
		command.WriteString(shellquote.Single(strings.Join(ai.Skills, ",")))
	}
	if ai.Experimental {
		command.WriteString(" --experimental")
	}
	command.WriteString(" > /dev/null\n")

	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\numask 077\nLC_ALL=C\nexport LC_ALL\n\n")
	fmt.Fprintf(&b, "POLICY=%s\n", shellquote.Single(string(policy)))
	fmt.Fprintf(&b, "PROVENANCE=%s\n", shellquote.Single(AIToolsSkillMarkerVersion))
	fmt.Fprintf(&b, "MAX_SKILLS=%d\n", skillconfig.MaxSkills)
	fmt.Fprintf(&b, "MAX_BYTES=%d\n", MaxAIToolsSkillBytes)
	b.WriteString(`DEST_ROOT="$HOME/.buzz/.agents/skills"
LOCK=""
TMP_ROOT=""
TXN=""
ACTIVE_NAME=""
cleanup() {
  if [ -n "${TXN:-}" ] && [ -n "${ACTIVE_NAME:-}" ]; then
    old="$TXN/.old-$ACTIVE_NAME"
    dest="$DEST_ROOT/$ACTIVE_NAME"
    if [ -d "$old" ] && [ ! -e "$dest" ] && [ ! -L "$dest" ]; then
      mv "$old" "$dest" 2>/dev/null || :
    fi
  fi
  if [ -n "${TXN:-}" ]; then
    rm -rf "$TXN"
  fi
  if [ -n "${TMP_ROOT:-}" ]; then
    rm -rf "$TMP_ROOT"
  fi
  if [ -n "${LOCK:-}" ]; then
    rmdir "$LOCK" 2>/dev/null || :
  fi
}
trap cleanup 0
trap 'exit 1' 1 2 15

# Build the destination one component at a time, refusing symlinks before
# mkdir so a hostile pre-existing parent cannot redirect even directory setup.
if [ ! -d "$HOME" ] || [ -L "$HOME" ]; then
  echo "HOME is not a real directory: $HOME" >&2
  exit 1
fi
for component in "$HOME/.buzz" "$HOME/.buzz/.agents" "$DEST_ROOT"; do
  if [ -L "$component" ]; then
    echo "skills destination component may not be a symlink: $component" >&2
    exit 1
  fi
  if [ -e "$component" ]; then
    if [ ! -d "$component" ]; then
      echo "skills destination component is not a directory: $component" >&2
      exit 1
    fi
  else
    mkdir "$component"
  fi
  if [ ! -d "$component" ] || [ -L "$component" ]; then
    echo "skills destination component changed during setup: $component" >&2
    exit 1
  fi
done
LOCK="$DEST_ROOT/.buzz-aitools-install.lock"
if ! mkdir "$LOCK" 2>/dev/null; then
  echo "another aitools skill installation is active (or left a lock): $LOCK" >&2
  exit 1
fi

TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/buzz-aitools-skills.XXXXXX")
STAGING="$TMP_ROOT/staging"
# mktemp creates the transaction directory under the destination root, making
# final renames same-filesystem without trusting a predictable pathname.
TXN=$(mktemp -d "$DEST_ROOT/.buzz-aitools-txn.XXXXXX")
ENTRIES="$TMP_ROOT/top-level"
NAMES="$TMP_ROOT/names"
FOLDED_NAMES="$TMP_ROOT/folded-names"
BYTE_COUNTS="$TMP_ROOT/byte-counts"
mkdir "$STAGING"
: >"$ENTRIES"
: >"$NAMES"
: >"$FOLDED_NAMES"
: >"$BYTE_COUNTS"

`)
	b.WriteString(command.String())
	b.WriteString(`
# -prune makes this a portable, top-level-only traversal without relying on
# GNU find's -mindepth/-maxdepth. Any newline in a hostile name creates an
# invalid/nonexistent entry below and therefore fails closed.
find "$STAGING" ! -path "$STAGING" -prune -print >"$ENTRIES"
count=0
while IFS= read -r src; do
  name=${src##*/}
  case "$name" in
    ""|*[!A-Za-z0-9._-]*|[!A-Za-z0-9]*)
      echo "aitools emitted an unsafe top-level skill name: $name" >&2
      exit 1
      ;;
  esac
  if [ "${#name}" -gt 64 ]; then
    echo "aitools emitted a skill name longer than 64 bytes: $name" >&2
    exit 1
  fi
  folded=$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')
  if [ "$folded" = "buzz-cli" ]; then
    echo "aitools may not install or replace the reserved buzz-cli skill" >&2
    exit 1
  fi
  if grep -F -x "$folded" "$FOLDED_NAMES" >/dev/null 2>&1; then
    echo "aitools emitted duplicate case-insensitive skill name: $name" >&2
    exit 1
  fi
  if [ ! -d "$src" ] || [ -L "$src" ]; then
    echo "aitools top-level entry is not a real skill directory: $name" >&2
    exit 1
  fi
  if [ ! -f "$src/SKILL.md" ] || [ -L "$src/SKILL.md" ]; then
    echo "aitools skill lacks a regular, non-symlink SKILL.md: $name" >&2
    exit 1
  fi
  if [ -e "$src/` + AIToolsSkillMarker + `" ] || [ -L "$src/` + AIToolsSkillMarker + `" ]; then
    echo "aitools output contains the reserved provenance marker: $name" >&2
    exit 1
  fi
  if [ -n "$(find "$src" ! -type f ! -type d -print)" ]; then
    echo "aitools skill contains a symlink or special file: $name" >&2
    exit 1
  fi
  find "$src" -type f -exec sh -c '
    for file do
      wc -c <"$file"
    done
  ' sh {} + >>"$BYTE_COUNTS"
  count=$((count + 1))
  if [ "$count" -gt "$MAX_SKILLS" ]; then
    echo "aitools emitted more than $MAX_SKILLS skills" >&2
    exit 1
  fi
  printf '%s\n' "$name" >>"$NAMES"
  printf '%s\n' "$folded" >>"$FOLDED_NAMES"
done <"$ENTRIES"

bytes=$(awk '{ total += $1 } END { printf "%.0f", total + 0 }' "$BYTE_COUNTS")
case "$bytes" in ""|*[!0-9]*) echo "could not measure staged skill bytes" >&2; exit 1;; esac
if [ "$bytes" -gt "$MAX_BYTES" ]; then
  echo "aitools emitted $bytes bytes, exceeding the $MAX_BYTES-byte limit" >&2
  exit 1
fi

# Validate every collision before changing any destination. Marker presence
# alone is insufficient: only a real directory with a regular, non-symlink
# marker containing this installer's exact provenance/version is managed.
while IFS= read -r name; do
  dest="$DEST_ROOT/$name"
  if [ -e "$dest" ] || [ -L "$dest" ]; then
    if [ "$POLICY" != "replace-managed" ]; then
      echo "refusing to overwrite existing skill: $name" >&2
      exit 1
    fi
    marker="$dest/` + AIToolsSkillMarker + `"
    if [ ! -d "$dest" ] || [ -L "$dest" ] || [ ! -f "$marker" ] || [ -L "$marker" ] || [ "$(cat "$marker")" != "$PROVENANCE" ]; then
      echo "refusing to overwrite unmanaged skill: $name" >&2
      exit 1
    fi
  fi
done <"$NAMES"

# First move validated trees into a transaction directory on the destination
# filesystem. Final renames are same-filesystem and therefore atomic when the
# destination is absent. Replacements use a same-filesystem backup and restore
# it if publishing the new directory fails.
while IFS= read -r name; do
  src="$STAGING/$name"
  printf '%s\n' "$PROVENANCE" >"$src/` + AIToolsSkillMarker + `"
  chmod 600 "$src/` + AIToolsSkillMarker + `"
  mv "$src" "$TXN/$name"
done <"$NAMES"

while IFS= read -r name; do
  dest="$DEST_ROOT/$name"
  next="$TXN/$name"
  if [ -e "$dest" ] || [ -L "$dest" ]; then
    old="$TXN/.old-$name"
    ACTIVE_NAME="$name"
    mv "$dest" "$old"
    if ! mv "$next" "$dest"; then
      mv "$old" "$dest" || :
      ACTIVE_NAME=""
      echo "failed to publish replacement skill: $name" >&2
      exit 1
    fi
    rm -rf "$old"
    ACTIVE_NAME=""
  else
    # Recheck under our installer lock so a concurrent unmanaged creation is
    # not deliberately consumed as a directory by mv.
    if [ -e "$dest" ] || [ -L "$dest" ]; then
      echo "skill destination appeared during installation: $name" >&2
      exit 1
    fi
    mv "$next" "$dest"
  fi
done <"$NAMES"

rmdir "$TXN"
TXN=""
echo "installed $count Databricks aitools skill(s) ($bytes bytes)"
`)
	return b.String(), nil
}

// RenderSkillsInstallScript is an explicit rendering alias retained for
// callers that describe generated shell as rendered rather than built.
func RenderSkillsInstallScript(cfg skillconfig.Config) (string, error) {
	return BuildSkillsInstallScript(cfg)
}
