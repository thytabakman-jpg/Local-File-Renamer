package mcplaunch

import (
	"bytes"
	"encoding/binary"
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
		if err := os.WriteFile(app, testExecutable(version), 0600); err != nil {
			t.Fatal(err)
		}
		bundled := filepath.Join(installDir, Executable)
		if err := os.WriteFile(bundled, testExecutable(version), 0600); err != nil {
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
	if err != nil || !bytes.Equal(actual, testExecutable("Local File Renamer 4.0-test")) {
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

func testExecutable(marker string) []byte {
	b := make([]byte, 512)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[60:], 64)
	copy(b[64:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[68:], 0x8664)
	binary.LittleEndian.PutUint16(b[70:], 1)
	binary.LittleEndian.PutUint16(b[84:], 240)
	binary.LittleEndian.PutUint16(b[86:], 2)
	binary.LittleEndian.PutUint16(b[88:], 0x20b)
	binary.LittleEndian.PutUint32(b[104:], 0x1000)
	binary.LittleEndian.PutUint32(b[196:], 16)
	copy(b[328:], ".text")
	copy(b[400:], marker)
	return b
}

func TestInvalidUpgradePreservesActiveTarget(t *testing.T) {
	programs := t.TempDir()
	dir := filepath.Join(programs, Directory)
	makeInstall := func(name string, valid bool) (string, string) {
		folder := filepath.Join(programs, "Local File Renamer "+name)
		if err := os.Mkdir(folder, 0700); err != nil { t.Fatal(err) }
		app, launcher := filepath.Join(folder, Application), filepath.Join(folder, Executable)
		if err := os.WriteFile(app, testExecutable(name), 0600); err != nil { t.Fatal(err) }
		data := testExecutable(name)
		if !valid { data = []byte("corrupt") }
		if err := os.WriteFile(launcher, data, 0600); err != nil { t.Fatal(err) }
		return app, launcher
	}
	old, launcher := makeInstall("old", true)
	if err := Install(programs, old, launcher); err != nil { t.Fatal(err) }
	next, invalid := makeInstall("new", false)
	if err := Install(programs, next, invalid); err == nil { t.Fatal("accepted corrupt launcher") }
	if current, err := Current(dir); err != nil || current != old { t.Fatalf("active target: %q (%v)", current, err) }
	if err := os.WriteFile(next, []byte("corrupt"), 0600); err != nil { t.Fatal(err) }
	if err := Install(programs, next, launcher); err == nil { t.Fatal("accepted corrupt target") }
	if current, err := Current(dir); err != nil || current != old { t.Fatalf("active target: %q (%v)", current, err) }
}

func TestInstallationIdentity(t *testing.T) {
	a := filepath.Join(t.TempDir(), "Local File Renamer 4.0-a", Application)
	b := filepath.Join(filepath.Dir(filepath.Dir(a)), "Local File Renamer 4.0-b", Application)
	if InstallationID(a) == InstallationID(b) || InstallationID(a) == "" || InstallationID("") != "" { t.Fatal("installation identities not distinct") }
}
