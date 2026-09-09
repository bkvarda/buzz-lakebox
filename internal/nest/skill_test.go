package nest

import (
	"strings"
	"testing"
)

func TestBuzzSkillCurrentContract(t *testing.T) {
	text := string(BuzzSkillMD)
	for _, want := range []string{
		"name: buzz-cli",
		"buzz agents draft-create",
		"repos protect list|set|remove",
		"--mention <hex-or-npub>",
		"buzz mem patch",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("embedded Buzz skill missing current contract %q", want)
		}
	}
	for _, forbidden := range []string{"DATABRICKS_TOKEN=", "/Users/example", "https://workspace.example"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("embedded generic skill contains an environment-specific value %q", forbidden)
		}
	}
}

func TestBuzzSkillLinkScriptDoesNotOverwriteExistingPaths(t *testing.T) {
	if !strings.Contains(BuzzSkillLinkScript, `[ ! -e "$link" ] && [ ! -L "$link" ]`) {
		t.Fatal("skill link script must preserve existing real and symlink paths")
	}
	if strings.Contains(BuzzSkillLinkScript, "ln -sf") {
		t.Fatal("skill link script must not force-overwrite unmanaged skills")
	}
}
