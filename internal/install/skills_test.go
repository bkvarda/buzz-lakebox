package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/skillconfig"
)

func skillsConfig(policy skillconfig.CollisionPolicy, skills ...string) skillconfig.Config {
	return skillconfig.Config{
		Schema:  skillconfig.CurrentSchema,
		Version: skillconfig.CurrentVersion,
		AITools: &skillconfig.AITools{
			Skills:          skills,
			CollisionPolicy: policy,
		},
	}
}

func TestBuildSkillsInstallScriptCommandAndStaticSecurity(t *testing.T) {
	cfg := skillsConfig(skillconfig.CollisionReplaceManaged, "sql-helper", "catalog.reader")
	cfg.AITools.Experimental = true
	script, err := BuildSkillsInstallScript(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#!/bin/sh\nset -eu\numask 077",
		`databricks aitools install --path "$STAGING" --skills 'sql-helper,catalog.reader' --experimental`,
		`DEST_ROOT="$HOME/.buzz/.agents/skills"`,
		`if [ "$folded" = "buzz-cli" ]`,
		`[ ! -f "$src/SKILL.md" ] || [ -L "$src/SKILL.md" ]`,
		`! -type f ! -type d`,
		`MAX_SKILLS=64`,
		fmt.Sprintf("MAX_BYTES=%d", MaxAIToolsSkillBytes),
		`[ "$(cat "$marker")" != "$PROVENANCE" ]`,
		`mv "$dest" "$old"`,
		`mv "$next" "$dest"`,
		`PROVENANCE='` + AIToolsSkillMarkerVersion + `'`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing security/contract fragment %q", want)
		}
	}
	for _, forbidden := range []string{
		"set -x", "--profile", "--host", "--token", "DATABRICKS_HOST=", "DATABRICKS_TOKEN=",
		"curl ", "wget ", "rm -rf \"$dest\"", "ln -s", "eval ", "sh -c \"$",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("script contains forbidden fragment %q", forbidden)
		}
	}
}

func TestBuildSkillsInstallScriptSelectionVariants(t *testing.T) {
	script, err := BuildSkillsInstallScript(skillsConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	command := lineContaining(script, "databricks aitools install")
	if command != `databricks aitools install --path "$STAGING" > /dev/null` {
		t.Fatalf("unexpected default command: %s", command)
	}
	if strings.Contains(command, "--skills ") || strings.Contains(command, "--experimental") {
		t.Fatalf("omitted settings gained flags: %s", command)
	}
	if !strings.Contains(script, "POLICY='fail'") {
		t.Fatal("omitted policy must render fail-closed")
	}

	disabled, err := BuildSkillsInstallScript(skillconfig.Config{Schema: skillconfig.CurrentSchema, Version: skillconfig.CurrentVersion})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(disabled, "databricks") || !strings.Contains(disabled, "disabled") {
		t.Fatalf("disabled config rendered active sync: %s", disabled)
	}
}

func TestBuildSkillsInstallScriptRejectsInvalidConfigWithoutRenderingInput(t *testing.T) {
	for _, name := range []string{"bad;touch-pwned", "$(id)", "../escape", "buzz-cli", strings.Repeat("x", 65)} {
		_, err := BuildSkillsInstallScript(skillsConfig(skillconfig.CollisionFail, name))
		if err == nil {
			t.Errorf("unsafe name %q accepted", name)
		}
	}
	_, err := BuildSkillsInstallScript(skillsConfig("overwrite", "safe"))
	if err == nil {
		t.Fatal("unsafe collision policy accepted")
	}
}

func lineContaining(s, part string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, part) {
			return line
		}
	}
	return ""
}

type scriptResult struct {
	home   string
	stdout string
	stderr string
	err    error
}

// runSkillsScript executes the generated POSIX script against a fake
// databricks binary. Each output entry is name\tkind\tpayload, where kind is
// dir (payload becomes SKILL.md), noskill, file, symlink, skill-link, fifo, or
// marker. It exercises the script itself rather than merely matching text.
func runSkillsScript(t *testing.T, cfg skillconfig.Config, entries []string, prepare func(home string)) scriptResult {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script test")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := `#!/bin/sh
set -eu
if [ "$1" != aitools ] || [ "$2" != install ]; then exit 70; fi
path=""
prev=""
for arg do
  if [ "$prev" = path ]; then path=$arg; prev=""; continue; fi
  if [ "$arg" = --path ]; then prev=path; fi
done
[ -n "$path" ] || exit 71
while IFS='` + "\t" + `' read -r name kind payload; do
  [ -n "$name" ] || continue
  case "$kind" in
    dir) mkdir -p "$path/$name"; printf '%s' "$payload" >"$path/$name/SKILL.md" ;;
    noskill) mkdir -p "$path/$name" ;;
    file) printf '%s' "$payload" >"$path/$name" ;;
    symlink) ln -s "$payload" "$path/$name" ;;
    skill-link) mkdir -p "$path/$name"; ln -s "$payload" "$path/$name/SKILL.md" ;;
    fifo) mkdir -p "$path/$name"; printf ok >"$path/$name/SKILL.md"; mkfifo "$path/$name/pipe" ;;
    marker) mkdir -p "$path/$name"; printf ok >"$path/$name/SKILL.md"; printf hostile >"$path/$name/.buzz-aitools-managed" ;;
    *) exit 72 ;;
  esac
done <"$FAKE_ENTRIES"
printf '{"ok":true}\n'
`
	fakePath := filepath.Join(bin, "databricks")
	if err := os.WriteFile(fakePath, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	entriesPath := filepath.Join(root, "entries")
	if err := os.WriteFile(entriesPath, []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(home)
	}
	script, err := BuildSkillsInstallScript(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_ENTRIES="+entriesPath)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return scriptResult{home: home, stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func TestSkillsScriptInstallsValidatedSkillsAndProvenance(t *testing.T) {
	res := runSkillsScript(t, skillsConfig(skillconfig.CollisionFail, "alpha", "beta"), []string{
		"alpha\tdir\t# alpha",
		"beta\tdir\t# beta",
	}, nil)
	if res.err != nil {
		t.Fatalf("script failed: %v, stderr=%s", res.err, res.stderr)
	}
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(res.home, ".buzz", ".agents", "skills", name)
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
			t.Errorf("%s not installed: %v", name, err)
		}
		marker, err := os.ReadFile(filepath.Join(dir, AIToolsSkillMarker))
		if err != nil || strings.TrimSpace(string(marker)) != AIToolsSkillMarkerVersion {
			t.Errorf("%s marker = %q, %v", name, marker, err)
		}
	}
	if !strings.Contains(res.stdout, "installed 2") {
		t.Fatalf("stdout = %q", res.stdout)
	}
	assertNoInstallerDebris(t, res.home)
}

func TestSkillsScriptCollisionPolicies(t *testing.T) {
	prepare := func(marker string) func(string) {
		return func(home string) {
			dir := filepath.Join(home, ".buzz", ".agents", "skills", "alpha")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if marker != "" {
				if err := os.WriteFile(filepath.Join(dir, AIToolsSkillMarker), []byte(marker), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	tests := []struct {
		name    string
		policy  skillconfig.CollisionPolicy
		marker  string
		ok      bool
		wantOld bool
	}{
		{"fail preserves managed", skillconfig.CollisionFail, AIToolsSkillMarkerVersion, false, true},
		{"replace refuses unmanaged", skillconfig.CollisionReplaceManaged, "", false, true},
		{"replace refuses wrong marker", skillconfig.CollisionReplaceManaged, "other:v1", false, true},
		{"replace exact managed", skillconfig.CollisionReplaceManaged, AIToolsSkillMarkerVersion, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runSkillsScript(t, skillsConfig(tt.policy, "alpha"), []string{"alpha\tdir\tnew"}, prepare(tt.marker))
			if (res.err == nil) != tt.ok {
				t.Fatalf("success=%v want %v; stderr=%s", res.err == nil, tt.ok, res.stderr)
			}
			got, err := os.ReadFile(filepath.Join(res.home, ".buzz", ".agents", "skills", "alpha", "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			want := "new"
			if tt.wantOld {
				want = "old"
			}
			if string(got) != want {
				t.Fatalf("destination = %q, want %q", got, want)
			}
			assertNoInstallerDebris(t, res.home)
		})
	}
}

func TestSkillsScriptRejectsHostileStagingBeforeAnyInstall(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
	}{
		{"reserved buzz-cli", []string{"safe\tdir\tok", "buzz-cli\tdir\tpwn"}},
		{"case folded reserved", []string{"BUZZ-CLI\tdir\tpwn"}},
		{"traversal-like unsafe", []string{"..bad\tdir\tpwn"}},
		{"top-level file", []string{"not-a-dir\tfile\tpwn"}},
		{"top-level symlink", []string{"link\tsymlink\t/tmp"}},
		{"missing SKILL md", []string{"empty\tnoskill\t"}},
		{"symlink SKILL md", []string{"linked\tskill-link\t/etc/passwd"}},
		{"special nested file", []string{"pipe-skill\tfifo\t"}},
		{"spoofed provider marker", []string{"spoof\tmarker\t"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runSkillsScript(t, skillsConfig(skillconfig.CollisionReplaceManaged), tt.entries, nil)
			if res.err == nil {
				t.Fatalf("hostile staging accepted; stdout=%s", res.stdout)
			}
			skills := filepath.Join(res.home, ".buzz", ".agents", "skills")
			if _, err := os.Stat(filepath.Join(skills, "safe")); !os.IsNotExist(err) {
				t.Fatalf("partial install occurred: %v", err)
			}
			if _, err := os.Stat(filepath.Join(skills, "buzz-cli")); !os.IsNotExist(err) {
				t.Fatalf("reserved buzz-cli was created: %v", err)
			}
			assertNoInstallerDebris(t, res.home)
		})
	}
}

func TestSkillsScriptRejectsCaseInsensitiveDuplicateOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script test")
	}
	probe := t.TempDir()
	if err := os.Mkdir(filepath.Join(probe, "Alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(probe, "alpha"), 0o700); err != nil {
		t.Skip("filesystem is case-insensitive")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := `#!/bin/sh
set -eu
path=""; prev=""
for arg do
  if [ "$prev" = path ]; then path=$arg; prev=""; continue; fi
  if [ "$arg" = --path ]; then prev=path; fi
done
mkdir -p "$path/Alpha" "$path/alpha"
printf A >"$path/Alpha/SKILL.md"
printf a >"$path/alpha/SKILL.md"
printf '{}\n'
`
	if err := os.WriteFile(filepath.Join(bin, "databricks"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := BuildSkillsInstallScript(skillsConfig(skillconfig.CollisionFail))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), "case-insensitive") {
		t.Fatalf("case-folded duplicate accepted: err=%v stderr=%s", err, stderr.String())
	}
	assertNoInstallerDebris(t, home)
}

func TestSkillsScriptEnforcesCountAndByteLimits(t *testing.T) {
	many := make([]string, skillconfig.MaxSkills+1)
	for i := range many {
		many[i] = fmt.Sprintf("s%02d\tdir\tx", i)
	}
	res := runSkillsScript(t, skillsConfig(skillconfig.CollisionFail), many, nil)
	if res.err == nil || !strings.Contains(res.stderr, "more than") {
		t.Fatalf("count cap not enforced: err=%v stderr=%s", res.err, res.stderr)
	}

	// Sparse input is avoided: the test caps the generated script's constant to
	// a tiny value while retaining the production validation/install logic.
	cfg := skillsConfig(skillconfig.CollisionFail)
	script, err := BuildSkillsInstallScript(cfg)
	if err != nil {
		t.Fatal(err)
	}
	script = strings.Replace(script, fmt.Sprintf("MAX_BYTES=%d", MaxAIToolsSkillBytes), "MAX_BYTES=3", 1)
	res = runRenderedSkillsScript(t, script, []string{"large\tdir\t1234"})
	if res.err == nil || !strings.Contains(res.stderr, "exceeding") {
		t.Fatalf("byte cap not enforced: err=%v stderr=%s", res.err, res.stderr)
	}
}

func runRenderedSkillsScript(t *testing.T, script string, entries []string) scriptResult {
	t.Helper()
	// Use the standard harness to construct a fake CLI, then replace only the
	// generated script by duplicating its small execution setup here.
	root := t.TempDir()
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := `#!/bin/sh
set -eu
path=""; prev=""
for arg do
  if [ "$prev" = path ]; then path=$arg; prev=""; continue; fi
  if [ "$arg" = --path ]; then prev=path; fi
done
while IFS='` + "\t" + `' read -r name kind payload; do
  [ -n "$name" ] || continue
  mkdir -p "$path/$name"
  printf '%s' "$payload" >"$path/$name/SKILL.md"
done <"$FAKE_ENTRIES"
printf '{}\n'
`
	if err := os.WriteFile(filepath.Join(bin, "databricks"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	entriesPath := filepath.Join(root, "entries")
	if err := os.WriteFile(entriesPath, []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_ENTRIES="+entriesPath)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return scriptResult{home: home, stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func TestSkillsScriptRejectsSymlinkDestinationRoot(t *testing.T) {
	res := runSkillsScript(t, skillsConfig(skillconfig.CollisionFail), []string{"alpha\tdir\tok"}, func(home string) {
		target := filepath.Join(filepath.Dir(home), "outside")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".buzz", ".agents"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(home, ".buzz", ".agents", "skills")); err != nil {
			t.Fatal(err)
		}
	})
	if res.err == nil || !strings.Contains(res.stderr, "may not be a symlink") {
		t.Fatalf("symlink root accepted: err=%v stderr=%s", res.err, res.stderr)
	}
}

func assertNoInstallerDebris(t *testing.T, home string) {
	t.Helper()
	skills := filepath.Join(home, ".buzz", ".agents", "skills")
	matches, err := filepath.Glob(filepath.Join(skills, ".buzz-aitools-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("installer debris remains: %v", matches)
	}
}
