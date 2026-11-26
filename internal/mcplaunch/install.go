package mcplaunch

import (
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	return ValidateExecutable(target)
}

func Install(programs, app, bundledLauncher string) error {
	if err := validateTarget(programs, app); err != nil {
		return err
	}
	if err := ValidateExecutable(bundledLauncher); err != nil {
		return fmt.Errorf("invalid bundled launcher: %w", err)
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
	if err := replace(filepath.Join(directory, pointerFile), []byte(app)); err != nil {
		return errors.Join(warn, err)
	}
	active, err := Current(directory)
	if err != nil {
		return errors.Join(warn, err)
	}
	if InstallationID(active) != InstallationID(app) {
		return errors.Join(warn, errors.New("launcher target did not activate"))
	}
	return warn
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
		if validationErr := ValidateExecutable(path); validationErr != nil {
			return nil, errors.Join(err, validationErr)
		}
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

const InstallationHeader = "X-Renamer-Installation"

// InstallationID identifies a program path without exposing it in HTTP.
func InstallationID(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(path)))
}

// ValidateExecutable checks the structural format of a Windows x64 executable.
func ValidateExecutable(path string) error {
	return validatePE(path, false)
}

// ValidateDLL checks the structural format of a Windows x64 DLL.
func ValidateDLL(path string) error {
	return validatePE(path, true)
}

func validatePE(path string, dll bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("invalid Windows binary %s: %w", path, err)
	}
	defer f.Close()
	h, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	isDLL := f.Characteristics&pe.IMAGE_FILE_DLL != 0
	if !ok || f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || f.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 || isDLL != dll || len(f.Sections) == 0 || (!dll && h.AddressOfEntryPoint == 0) {
		return fmt.Errorf("unexpected Windows x64 binary format: %s", path)
	}
	// A valid header alone is insufficient: verify every raw section fits.
	for _, section := range f.Sections {
		if section.Size != 0 && (section.Offset == 0 || uint64(section.Offset)+uint64(section.Size) > uint64(info.Size())) {
			return fmt.Errorf("truncated Windows binary section: %s", path)
		}
	}
	return nil
}
