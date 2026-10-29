package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "state.json")
	for _, content := range []string{"one", "two"} {
		if err := Write(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(name); string(b) != content {
			t.Fatalf("got %q, want %q", b, content)
		}
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(name); fi.Mode().Perm() != 0o644 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestWritePrivate(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "relay.key")
	// A file squatting the old fixed temporary name changes nothing.
	if err := os.WriteFile(name+".tmp", []byte("squatter"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(name, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(name); string(b) != "secret" {
		t.Fatalf("got %q", b)
	}
	if b, _ := os.ReadFile(name + ".tmp"); string(b) != "squatter" {
		t.Fatal("wrote through the squatted name")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(name); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
	}
}
