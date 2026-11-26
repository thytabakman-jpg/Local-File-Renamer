package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
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
	unpack := writeTestInstallation
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

func testPE(dll bool) []byte {
	// Minimal structural PE fixture; it is never executed.
	b := make([]byte, 512)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[60:], 64)
	copy(b[64:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[68:], 0x8664)
	binary.LittleEndian.PutUint16(b[70:], 1)
	binary.LittleEndian.PutUint16(b[84:], 240)
	flags := uint16(2)
	if dll { flags |= 0x2000 }
	binary.LittleEndian.PutUint16(b[86:], flags)
	binary.LittleEndian.PutUint16(b[88:], 0x20b)
	binary.LittleEndian.PutUint32(b[104:], 0x1000)
	binary.LittleEndian.PutUint32(b[196:], 16)
	copy(b[328:], ".text")
	return b
}

func TestInstallRejectsInvalidExecutable(t *testing.T) {
	parent := t.TempDir()
	unpack := func(dir string) error {
		return os.WriteFile(filepath.Join(dir, appExe), []byte("not an executable"), 0600)
	}
	if _, err := installFiles(parent, unpack); err == nil {
		t.Fatal("accepted invalid executable")
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatalf("incomplete install remains: %v (%v)", entries, err)
	}
}

func TestArchiveExtraction(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantError bool
	}{
		{"engine/model.dll", false},
		{"../outside", true},
		{"..\\outside", true},
		{"C:/outside", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			z := zip.NewWriter(&b)
			w, err := z.Create(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "install")
			err = unpackArchive(dir, b.Bytes())
			if (err != nil) != tc.wantError {
				t.Fatalf("unpack error = %v", err)
			}
			if !tc.wantError {
				got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(tc.name)))
				if err != nil || string(got) != "payload" {
					t.Fatalf("payload = %q (%v)", got, err)
				}
			}
		})
	}
}

func writeTestInstallation(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "engine"), 0700); err != nil { return err }
	for _, name := range []string{appExe, "LocalFileRenamer-MCP.exe", "engine/llama-server.exe"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), testPE(false), 0600); err != nil { return err }
	}
	for _, name := range requiredEngineDLLs {
		if err := os.WriteFile(filepath.Join(dir, "engine", name), testPE(true), 0600); err != nil { return err }
	}
	return nil
}

func TestIncompletePayloadPreservesPreviousInstall(t *testing.T) {
	for _, missing := range []string{"LocalFileRenamer-MCP.exe", "engine/llama-server.exe", "engine/llama.dll"} {
		t.Run(missing, func(t *testing.T) {
			parent := t.TempDir()
			prior, err := installFiles(parent, writeTestInstallation)
			if err != nil { t.Fatal(err) }
			unpack := func(dir string) error {
				if err := writeTestInstallation(dir); err != nil { return err }
				return os.Remove(filepath.Join(dir, filepath.FromSlash(missing)))
			}
			if _, err := installFiles(parent, unpack); err == nil { t.Fatal("accepted incomplete package") }
			if err := validateInstallation(filepath.Dir(prior)); err != nil { t.Fatal(err) }
			if entries, err := os.ReadDir(parent); err != nil || len(entries) != 1 { t.Fatalf("entries: %v (%v)", entries, err) }
		})
	}
}

func TestCorruptEngineDLLRejected(t *testing.T) {
	parent := t.TempDir()
	unpack := func(dir string) error {
		if err := writeTestInstallation(dir); err != nil { return err }
		return os.WriteFile(filepath.Join(dir, "engine", "llama.dll"), []byte("corrupt"), 0600)
	}
	if _, err := installFiles(parent, unpack); err == nil { t.Fatal("accepted corrupt DLL") }
}
