package main

import (
	"os"
	"path/filepath"
)

const appExe = "LocalFileRenamer.exe"
const installVersion = "4.0"

// Each install gets its own program directory. Windows can keep an earlier
// executable or engine DLL open without blocking an upgrade.
func installFiles(parent string, unpack func(string) error) (string, error) {
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, "Local File Renamer "+installVersion+"-")
	if err != nil {
		return "", err
	}
	if err := unpack(dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	app := filepath.Join(dir, appExe)
	if _, err := os.Stat(app); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return app, nil
}
