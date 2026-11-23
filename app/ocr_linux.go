package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

func renderSamplePage(ctx context.Context, path string, page int) ([]byte, error) {
	bin, err := exec.LookPath("pdftoppm")
	if err != nil {
		return nil, errors.New("PDF previews require pdftoppm on Linux")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "renamer-pdf-preview-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	base := filepath.Join(dir, "page")
	number := strconv.Itoa(page + 1)
	cmd := exec.CommandContext(ctx, bin, "-f", number, "-l", number, "-scale-to", "1250", "-jpeg", "-singlefile", path, base)
	configureChild(cmd)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("could not render PDF page: %w", err)
	}
	return os.ReadFile(base + ".jpg")
}
