package httpmcpbin_test

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/httpmcpbin"
)

func TestEmbeddedBridgeMatchesSource(t *testing.T) {
	root := filepath.Clean(filepath.Join(mustGetwd(t), "../.."))
	var paths []string
	for _, dir := range []string{"cmd/bzhttpmcp", "internal/httpmcpbin"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
				paths = append(paths, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(h, "%s\n", p)
		_, _ = h.Write(data)
	}
	if got, want := strings.TrimSpace(httpmcpbin.SrcHash), hex.EncodeToString(h.Sum(nil)); got != want {
		t.Fatalf("embedded bridge is stale: got %s want %s; regenerate it", got, want)
	}
}

func TestEmbeddedBridgeIsLinuxAMD64(t *testing.T) {
	f, err := elf.NewFile(strings.NewReader(string(httpmcpbin.Binary)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if f.Class != elf.ELFCLASS64 || f.Machine != elf.EM_X86_64 {
		t.Fatalf("wrong ELF target: %v %v", f.Class, f.Machine)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	p, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
