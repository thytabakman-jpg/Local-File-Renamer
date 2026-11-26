package main

import (
	"archive/zip"
	"bytes"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
		_ = os.RemoveAll(dir)
		return "", err
	}
	app := filepath.Join(dir, appExe)
	if err := validateApplication(app); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return app, nil
}

// validateApplication checks the executable format, not runtime compatibility.
func validateApplication(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("application is not a regular file: %s", path)
	}
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("invalid Windows executable: %w", err)
	}
	defer f.Close()
	h, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || f.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 || f.Characteristics&pe.IMAGE_FILE_DLL != 0 || len(f.Sections) == 0 || h.AddressOfEntryPoint == 0 {
		return fmt.Errorf("application is not a Windows x64 executable: %s", path)
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
