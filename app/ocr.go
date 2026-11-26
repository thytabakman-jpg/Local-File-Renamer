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

func unresolvedPDF(s *session, opts scanSettings, path, name, override string, e evidence) bool {
	if s.manifest.Operation == scanMove {
		return !s.sortIndex.proposedLocation(path, e, assessment{}, false).Clear
	}
	d := decideWithRules(name, e, assessment{}, false, opts.Naming, override)
	return d.Unknown || d.Note != "" || d.Name == name && d.Outcome != "Name verified"
}

func inspectOCR(ctx context.Context, s *session, transcribe transcribePage, opts scanSettings, item entry, name, override string, pre *preparedEvidence, e evidence) (evidence, string, int64, error) {
	ocrStatus := pre.Cached.OCR
	var ocrMS int64
	var ocrErr error
	if !pre.Reused && opts.Evidence.UseOCR && strings.EqualFold(extension(item.Path), ".pdf") && transcribe != nil && ctx.Err() == nil &&
		(len(e.Text) < 35 || opts.Evidence.OCRUnresolved && unresolvedPDF(s, opts, item.Path, name, override, e)) {
		ocrStart := time.Now()
		extracted, note, err := scanOCRWithError(ctx, item.Path, transcribe, opts.Evidence.OCRPages)
		ocrMS, ocrStatus, ocrErr = elapsedMS(ocrStart), note, err
		if extracted != "" {
			if len(e.Text) < 35 {
				e.Text = chars(extracted, opts.Evidence.TextBytes)
			} else if !strings.Contains(norm(e.Text), norm(extracted)) {
				e.Text = chars(chars(e.Text, opts.Evidence.TextBytes/2)+"\n"+extracted, opts.Evidence.TextBytes)
			}
			e.Note, ocrStatus = "", "Read sampled pages"
		} else {
			e.Note = joinNote(e.Note, note)
		}
	}
	return e, ocrStatus, ocrMS, ocrErr
}
