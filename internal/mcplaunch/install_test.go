package mcplaunch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallRefreshesTargetAtTheSameLauncherPath(t *testing.T) {
	programs := t.TempDir()
	dir := filepath.Join(programs, Directory)
	for _, version := range []string{"Local File Renamer 3.6-test", "Local File Renamer 4.0-test"} {
		installDir := filepath.Join(programs, version)
		if err := os.Mkdir(installDir, 0700); err != nil {
			t.Fatal(err)
		}
		app := filepath.Join(installDir, Application)
		if err := os.WriteFile(app, []byte(version), 0600); err != nil {
			t.Fatal(err)
		}
		bundled := filepath.Join(installDir, Executable)
		if err := os.WriteFile(bundled, []byte(version), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Install(programs, app, bundled); err != nil {
			t.Fatal(err)
		}
		if found, err := Current(dir); err != nil || found != app {
			t.Fatalf("target=%q, error=%v", found, err)
		}
	}
	actual, err := os.ReadFile(filepath.Join(dir, Executable))
	if err != nil || string(actual) != "Local File Renamer 4.0-test" {
		t.Fatalf("stable launcher was not refreshed: %q (%v)", actual, err)
	}
}

func TestInstallRejectsMissingTarget(t *testing.T) {
	programs := t.TempDir()
	app := filepath.Join(programs, "Local File Renamer 4.0-test", Application)
	if err := Install(programs, app, "unused"); err == nil {
		t.Fatal("accepted missing target")
	}
	if _, err := os.Stat(filepath.Join(programs, Directory)); !os.IsNotExist(err) {
		t.Fatalf("invalid target created launcher directory: %v", err)
	}
}

func TestCurrentRejectsInvalidTarget(t *testing.T) {
	programs := t.TempDir()
	dir := filepath.Join(programs, Directory)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"relative.exe", filepath.Join(programs, "unrelated", Application), filepath.Join(programs, "Local File Renamer missing", Application)} {
		if err := os.WriteFile(filepath.Join(dir, pointerFile), []byte(target), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Current(dir); err == nil {
			t.Fatalf("accepted invalid target: %s", target)
		}
	}
}
