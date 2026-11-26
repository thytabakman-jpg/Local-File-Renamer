package mcplaunch

import (
	"bytes"
	"errors"
	"fmt"
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
	if err := validateTarget(filepath.Dir(directory), target); err != nil {
		return "", err
	}
	return target, nil
}

func validateTarget(programs, target string) error {
	relative, err := filepath.Rel(programs, filepath.Dir(target))
	if err != nil || !filepath.IsAbs(target) || filepath.Base(target) != Application ||
		!filepath.IsLocal(relative) || relative != filepath.Base(relative) ||
		!strings.HasPrefix(relative, "Local File Renamer ") {
		return errors.New("the installed Renamer path is invalid")
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("the installed Renamer is not a regular file")
	}
	return nil
}

func Install(programs, app, bundledLauncher string) error {
	if err := validateTarget(programs, app); err != nil {
		return err
	}
	directory := filepath.Join(programs, Directory)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	launcher := filepath.Join(directory, Executable)
	warn, err := refreshLauncher(launcher, bundledLauncher)
	if err != nil {
		return err
	}
	// Keep the stable path. A running Windows launcher may prevent replacing
	// its executable; still publish the new target and report the warning.
	return errors.Join(warn, replace(filepath.Join(directory, pointerFile), []byte(app)))
}

func refreshLauncher(path, bundled string) (warning, fatal error) {
	data, err := os.ReadFile(bundled)
	if err != nil {
		return nil, err
	}
	old, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, replace(path, data)
	}
	if err != nil {
		return nil, err
	}
	if bytes.Equal(old, data) {
		return nil, nil
	}
	if err := replace(path, data); err != nil {
		return fmt.Errorf("launcher refresh could not finish; close MCP sessions and rerun Setup: %w", err), nil
	}
	return nil, nil
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
