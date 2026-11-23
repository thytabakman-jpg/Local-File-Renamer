package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type transcribePage func(context.Context, []byte) (string, error)

func scanOCR(ctx context.Context, path string, transcribe transcribePage) (string, string) {
	returnText, note, _ := scanOCRWithError(ctx, path, transcribe, 2)
	return returnText, note
}

func scanOCRWithError(ctx context.Context, path string, transcribe transcribePage, pages int) (string, string, error) {
	for i := 0; i < pages; i++ {
		jpeg, err := renderSamplePage(ctx, path, i)
		if err != nil {
			return "", "Could not render the PDF for local OCR", nil
		}
		if len(jpeg) == 0 {
			break
		}
		text, err := transcribe(ctx, jpeg)
		if err != nil {
			return "", "Local OCR did not finish reading the PDF", err
		}
		if len(strings.TrimSpace(text)) >= 35 {
			return chars(text, excerpt), "", nil
		}
	}
	return "", "Local OCR found no readable text in the sampled pages", nil
}

// Tesseract is optional and uses only the rendered page bytes on stdin.
func tesseractPage(ctx context.Context, jpeg []byte) (string, error) {
	binary, err := exec.LookPath("tesseract")
	if err != nil {
		return "", errors.New("Tesseract OCR is not installed on the app's PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "stdin", "stdout")
	cmd.Stdin = bytes.NewReader(jpeg)
	configureChild(cmd)
	data, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("Tesseract OCR did not finish: %w", err)
	}
	return chars(string(data), excerpt), nil
}
