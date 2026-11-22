package main

import (
	"context"
	"strings"
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
