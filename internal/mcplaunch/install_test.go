package mcplaunch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallRefreshesTargetAtTheSameLauncherPath(t *testing.T) {
	programs := t.TempDir()
	dir := filepath.Join(programs, Directory)
	for _, version := range []string{"Local File Renamer 3.5-test", "Local File Renamer 3.6-test"} {
		installDir := filepath.Join(programs, version)
		if err := os.Mkdir(installDir, 0700); err != nil {
			t.Fatal(err)
		}
		app := filepath.Join(installDir, Application)
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
	if err != nil || string(actual) != "Local File Renamer 3.5-test" {
		t.Fatalf("fixed launcher was not preserved: %q (%v)", actual, err)
	}
}
