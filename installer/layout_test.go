package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallBesideExistingProgram(t *testing.T) {
	parent := t.TempDir()
	old := filepath.Join(parent, "Local File Renamer")
	if err := os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	prior := filepath.Join(old, appExe)
	if err := os.WriteFile(prior, []byte("old version"), 0600); err != nil {
		t.Fatal(err)
	}
	unpack := func(dir string) error { return os.WriteFile(filepath.Join(dir, appExe), []byte("new version"), 0600) }
	first, err := installFiles(parent, unpack)
	if err != nil {
		t.Fatal(err)
	}
	second, err := installFiles(parent, unpack)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == prior || second == prior {
		t.Fatal(first, second)
	}
	if content, err := os.ReadFile(prior); err != nil || string(content) != "old version" {
		t.Fatalf("%q %v", content, err)
	}
}

func TestIncompleteInstallRemovesOnlyNewDirectory(t *testing.T) {
	parent := t.TempDir()
	old := filepath.Join(parent, "Local File Renamer")
	if err := os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := installFiles(parent, func(string) error { return nil }); err == nil {
		t.Fatal("installer accepted missing executable")
	}
	items, err := os.ReadDir(parent)
	if err != nil || len(items) != 1 || items[0].Name() != filepath.Base(old) {
		t.Fatalf("%v %v", items, err)
	}
}
