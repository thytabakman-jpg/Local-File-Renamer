package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"localrenamer/internal/mcplaunch"
	"localrenamer/internal/version"
)

const appExe = "LocalFileRenamer.exe"
const installVersion = version.Current

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
		return "", discardIncompleteInstall(dir, err)
	}
	if err := validateInstallation(dir); err != nil {
		return "", discardIncompleteInstall(dir, err)
	}
	return filepath.Join(dir, appExe), nil
}

// Only the directory created by this installation is removed.
func discardIncompleteInstall(dir string, cause error) error {
	if err := os.RemoveAll(dir); err != nil {
		return errors.Join(cause, fmt.Errorf("could not remove incomplete installation %s: %w", dir, err))
	}
	return cause
}

var requiredEngineDLLs = []string{
	"llama-server-impl.dll", "llama-common.dll", "llama.dll", "mtmd.dll",
	"ggml.dll", "ggml-base.dll", "libomp.dll",
}

func validateInstallation(dir string) error {
	for _, name := range []string{appExe, mcplaunch.Executable, "engine/llama-server.exe"} {
		if err := mcplaunch.ValidateExecutable(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			return fmt.Errorf("installation executable %s: %w", name, err)
		}
	}
	for _, name := range requiredEngineDLLs {
		if err := mcplaunch.ValidateDLL(filepath.Join(dir, "engine", name)); err != nil {
			return fmt.Errorf("installation dependency %s: %w", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "engine"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "ggml-cpu-") && strings.HasSuffix(entry.Name(), ".dll") {
			if err := mcplaunch.ValidateDLL(filepath.Join(dir, "engine", entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func unpackArchive(dir string, payload []byte) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	z, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return err
	}
	for _, f := range z.File {
		if strings.ContainsAny(f.Name, "\\:") || !filepath.IsLocal(filepath.FromSlash(f.Name)) || f.FileInfo().IsDir() || !f.Mode().IsRegular() {
			return fmt.Errorf("unexpected installer entry: %s", f.Name)
		}
		path := filepath.Join(dir, filepath.FromSlash(f.Name))
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err = extract(f, path); err != nil {
			return err
		}
	}
	return nil
}
func extract(f *zip.File, path string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dest, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dest, src)
	return errors.Join(copyErr, dest.Close())
}
