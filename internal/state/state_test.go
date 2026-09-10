package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Path: filepath.Join(t.TempDir(), "nested", "agents.json")}
}

func TestLookup_MissingFileIsNotAnError(t *testing.T) {
	s := tempStore(t)
	_, ok, err := s.Lookup(Key("DEFAULT", "npub1abc"))
	if err != nil {
		t.Fatalf("Lookup on missing file: %v", err)
	}
	if ok {
		t.Fatal("Lookup on missing file must report not-found")
	}
}

func TestRecordAndLookup_RoundTrip(t *testing.T) {
	s := tempStore(t)
	key := Key("EXAMPLE_PROFILE", "npub1abc")
	want := Entry{SandboxID: "sandbox-fixture-1", Profile: "EXAMPLE_PROFILE"}
	if err := s.Record(key, want); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, ok, err := s.Lookup(key)
	if err != nil || !ok {
		t.Fatalf("Lookup after Record: ok=%v err=%v", ok, err)
	}
	if got.SandboxID != want.SandboxID || got.Profile != want.Profile {
		t.Fatalf("round trip mismatch: got %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("Record must stamp UpdatedAt when zero")
	}

	// Same npub on a different profile is a distinct key.
	if _, ok, _ := s.Lookup(Key("other", "npub1abc")); ok {
		t.Fatal("different profile must not share the mapping")
	}
}

func TestRecord_UpsertsWithoutClobberingOthers(t *testing.T) {
	s := tempStore(t)
	if err := s.Record("a", Entry{SandboxID: "one"}); err != nil {
		t.Fatalf("Record a: %v", err)
	}
	if err := s.Record("b", Entry{SandboxID: "two"}); err != nil {
		t.Fatalf("Record b: %v", err)
	}
	if err := s.Record("a", Entry{SandboxID: "one-v2"}); err != nil {
		t.Fatalf("Record a again: %v", err)
	}

	ea, _, _ := s.Lookup("a")
	eb, _, _ := s.Lookup("b")
	if ea.SandboxID != "one-v2" || eb.SandboxID != "two" {
		t.Fatalf("upsert broke entries: a=%+v b=%+v", ea, eb)
	}
}

func TestSave_FileModeIsPrivate(t *testing.T) {
	s := tempStore(t)
	if err := s.Record("a", Entry{SandboxID: "one", UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %o, want 600", perm)
	}
}

func TestLookup_CorruptFileIsAnError(t *testing.T) {
	s := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Lookup("a"); err == nil {
		t.Fatal("corrupt state file must surface as an error, not silent not-found")
	}
}

func TestForgetSandbox_RemovesOnlyTheMatchingEntry(t *testing.T) {
	s := tempStore(t)
	keep := Key("EXAMPLE_PROFILE", "npub1keep")
	drop := Key("EXAMPLE_PROFILE", "npub1drop")
	if err := s.Record(keep, Entry{SandboxID: "sandbox-keep", Profile: "EXAMPLE_PROFILE"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Record(drop, Entry{SandboxID: "sandbox-drop", Profile: "EXAMPLE_PROFILE"}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	removed, err := s.ForgetSandbox("EXAMPLE_PROFILE", "sandbox-drop")
	if err != nil {
		t.Fatalf("ForgetSandbox: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, ok, _ := s.Lookup(drop); ok {
		t.Fatal("the deleted sandbox's mapping must be gone")
	}
	if _, ok, _ := s.Lookup(keep); !ok {
		t.Fatal("ForgetSandbox must not touch other agents' mappings")
	}
}

// The same agent identity may be deployed to two workspaces; undeploying
// one must not forget the other.
func TestForgetSandbox_ScopedByProfile(t *testing.T) {
	s := tempStore(t)
	primary := Key("PRIMARY", "npub1same")
	secondary := Key("SECONDARY", "npub1same")
	if err := s.Record(primary, Entry{SandboxID: "sandbox-shared-name", Profile: "PRIMARY"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Record(secondary, Entry{SandboxID: "sandbox-shared-name", Profile: "SECONDARY"}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	removed, err := s.ForgetSandbox("PRIMARY", "sandbox-shared-name")
	if err != nil {
		t.Fatalf("ForgetSandbox: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, ok, _ := s.Lookup(secondary); !ok {
		t.Fatal("the other profile's mapping must survive")
	}
}

// Undeploy of a sandbox that was never mapped (or after the file was
// deleted) must not fail — there is simply nothing to forget.
func TestForgetSandbox_NoMatchIsNotAnError(t *testing.T) {
	s := tempStore(t)
	removed, err := s.ForgetSandbox("EXAMPLE_PROFILE", "sandbox-never-seen")
	if err != nil {
		t.Fatalf("ForgetSandbox on empty store: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatal("a no-op ForgetSandbox must not create the state file")
	}
}
