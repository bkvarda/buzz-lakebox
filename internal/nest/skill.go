package nest

import _ "embed"

const (
	// BuzzSkillPath is the canonical harness-agnostic location used by current
	// Buzz. Harness-specific skill directories point here with symlinks.
	BuzzSkillPath        = "$HOME/.buzz/.agents/skills/buzz-cli/SKILL.md"
	BuzzSkillVersionPath = "$HOME/.buzz/.agents/skills/buzz-cli/.skill-version"
	BuzzSkillVersion     = "5"
)

// BuzzSkillMD is copied verbatim from block/buzz's current
// desktop/src-tauri/src/managed_agents/nest_skill.md. It contains no runtime,
// user, workspace, or credential values.
//
//go:embed assets/buzz-cli/SKILL.md
var BuzzSkillMD []byte

// BuzzSkillLinkScript creates the current canonical skill layout. Existing
// non-symlink harness skill directories are never overwritten.
const BuzzSkillLinkScript = `set -eu
umask 077
canonical="$HOME/.buzz/.agents/skills/buzz-cli"
mkdir -p "$canonical"
for parent in "$HOME/.buzz/.claude/skills" "$HOME/.buzz/.codex/skills" "$HOME/.buzz/.goose/skills"; do
  mkdir -p "$parent"
  link="$parent/buzz-cli"
  if [ ! -e "$link" ] && [ ! -L "$link" ]; then
    ln -s ../../.agents/skills/buzz-cli "$link"
  fi
done
`
