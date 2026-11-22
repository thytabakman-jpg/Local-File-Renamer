package mcplaunch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	Directory   = "Local File Renamer MCP"
	Executable  = "LocalFileRenamer-MCP.exe"
	Application = "LocalFileRenamer.exe"
	pointerFile = "current-installation.txt"
)

func Current(directory string) (string, error) {
	data, err := os.ReadFile(filepath.Join(directory, pointerFile))
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(data))
	programs := filepath.Dir(directory)
	relative, err := filepath.Rel(programs, filepath.Dir(target))
	if err != nil || !filepath.IsAbs(target) || filepath.Base(target) != Application ||
		!filepath.IsLocal(relative) || relative != filepath.Base(relative) ||
		!strings.HasPrefix(relative, "Local File Renamer ") {
		return "", errors.New("the installed Renamer path is invalid")
	}
	return target, nil
}

func Install(programs, app, bundledLauncher string) error {
	directory := filepath.Join(programs, Directory)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	launcher := filepath.Join(directory, Executable)
	if _, err := os.Stat(launcher); errors.Is(err, fs.ErrNotExist) {
		data, err := os.ReadFile(bundledLauncher)
		if err != nil {
			return err
		}
		if err := replace(launcher, data); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Existing launcher processes may be open during setup. They keep their
	// bridge, and new processes read the newly published installation path.
	return replace(filepath.Join(directory, pointerFile), []byte(app))
}

func replace(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".renamer-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
