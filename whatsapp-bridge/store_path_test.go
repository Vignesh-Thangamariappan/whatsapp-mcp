package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsBlockedStorePath(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	mustWrite := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("store/whatsapp.db")
	mustWrite("store/chat123/photo.jpg")
	mustWrite("elsewhere/file.txt")

	if err := os.Symlink(filepath.Join(dir, "store"), "storelink"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"top-level store file", "store/whatsapp.db", true},
		{"absolute store file", filepath.Join(dir, "store", "whatsapp.db"), true},
		{"store dir itself", "store", true},
		{"dot-dot spelling", "elsewhere/../store/whatsapp.db", true},
		{"via symlinked dir", "storelink/whatsapp.db", true},
		{"per-chat media allowed", "store/chat123/photo.jpg", false},
		{"unrelated file allowed", "elsewhere/file.txt", false},
		{"missing file", "store/nope.db", true},
	}
	for _, c := range cases {
		got, err := isBlockedStorePath(c.path)
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: isBlockedStorePath(%q) = %v, want %v", c.name, c.path, got, c.want)
		}
	}
}
