//go:build darwin && !no_vz

package vz

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalMacOSRestorePathResolvesStorageAncestor(t *testing.T) {
	root := t.TempDir()
	storage := filepath.Join(root, "external-storage")
	if err := os.MkdirAll(filepath.Join(storage, "mac"), 0700); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(storage, "mac", "image.ipsw")
	if err := os.WriteFile(media, []byte("restore fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	anchor := filepath.Join(root, "state")
	if err := os.Symlink(storage, anchor); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalMacOSRestorePath(filepath.Join(anchor, "mac", "image.ipsw"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(media)
	if got != want {
		t.Fatalf("Apple installation service would receive symlink path: got %q want %q", got, want)
	}
	if _, err := canonicalMacOSRestorePath(filepath.Join(anchor, "missing.ipsw")); err == nil {
		t.Fatal("missing restore image was accepted")
	}
}
