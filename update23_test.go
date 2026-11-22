package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadableFilename(t *testing.T) {
	cases := []struct{ from, want string }{
		{"About _EFT with a Traumatized Couple.pdf", "About EFT with a Traumatized Couple.pdf"},
		{"ABOUT___EFT_WITH_A_TRAUMATIZED_COUPLE.PDF", "About EFT with a Traumatized Couple.PDF"},
		{"INTRODUCTION_TO_ADHD.pdf", "Introduction to ADHD.pdf"},
		{"THEORY_OF_CBT_-_MCC_642_-_V2.DOCX", "Theory of CBT - MCC 642 - V2.DOCX"},
		{"Lee - 2026 - SLEEP_AND_ADHD_IN_ADULTS - Journal of Counselling.pdf", "Lee - 2026 - Sleep and ADHD in Adults - Journal of Counselling.pdf"},
		{"Research on iPhone use in adults.pdf", "Research on iPhone use in adults.pdf"},
		{"ÉCOUTER_UN_COURS.mp3", "Écouter Un Cours.mp3"},
		{"TF-CBT_AND_DSM5_WITH_FAMILIES.pdf", "TF-CBT and DSM5 with Families.pdf"},
		{"O'NEILL_AND_THE_LIBRARY.pdf", "O'Neill and the Library.pdf"},
		{"ROMANCE.tar.gz", "Romance.tar.gz"},
		{"README.md", "README.md"},
		{"IMG_0042.jpg", "IMG_0042.jpg"},
		{"_CON.pdf", "File - Con.pdf"},
	}
	for _, tc := range cases {
		got := readableName(tc.from, false)
		if got != tc.want || readableName(got, false) != got {
			t.Errorf("style %q: got %q, want stable %q", tc.from, got, tc.want)
		}
	}
	d := decide("THEORY_OF_CBT_-_MCC_642_-_V2.DOCX", evidence{}, assessment{}, false)
	if d.Name != "Theory of CBT - MCC 642 - 2.DOCX" || !d.Strict {
		t.Fatalf("course code and version were not preserved: %+v", d)
	}
}

func TestAITimeoutKeepsStyleProposal(t *testing.T) {
	root := t.TempDir()
	name := "About _EFT with a Traumatized Couple.pdf"
	writeTestPDF(t, filepath.Join(root, name))
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(250 * time.Millisecond):
		}
	}))
	defer server.Close()
	original := loopbackTransport
	loopbackTransport = original.Clone()
	loopbackTransport.ResponseHeaderTimeout = 15 * time.Millisecond
	defer func() { loopbackTransport.CloseIdleConnections(); loopbackTransport = original }()
	ctx := context.Background()
	if err := process(ctx, s, &localModel{url: server.URL}, 0); err != nil {
		t.Fatal(err)
	}
	p := s.plans[0]
	if p.Move == nil || filepath.Base(p.Move.To) != "About EFT with a Traumatized Couple.pdf" ||
		p.Inspection.AI != "Timed out" || !strings.Contains(p.Note, "timed out after 3 minutes") {
		t.Fatalf("timed-out PDF did not retain a style proposal: %+v", p)
	}
	if p.Auto {
		t.Fatal("a timed-out proposal requires review")
	}
}

func TestCaseOnlyRenameAndUndo(t *testing.T) {
	root := t.TempDir()
	old, updated := filepath.Join(root, "READING SKILLS.txt"), filepath.Join(root, "Reading Skills.txt")
	writeTestFile(t, old, "Reading Skills\nA simple fixture.")
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	scanAndApply(t, s)
	if s.renamed != 1 || !actualSpelling(updated) {
		t.Fatalf("preserved earlier casing: %+v", s.moves)
	}
	if err = s.undoNext(); err != nil {
		t.Fatal(err)
	}
	if !actualSpelling(old) {
		t.Fatal("old casing not restored")
	}
	got, err := os.ReadFile(old)
	if err != nil || !strings.HasPrefix(string(got), "Reading Skills") {
		t.Fatalf("old filename or contents lost: %q %v", got, err)
	}
}

func TestCloseWaitsForActiveWork(t *testing.T) {
	a := &application{}
	ctx := a.begin("files")
	work, picker := a.requestClose()
	if picker != nil || work == nil || ctx.Err() != context.Canceled {
		t.Fatal("close did not cancel the active operation")
	}
	select {
	case <-work:
		t.Fatal("close finished before the active operation saved its state")
	default:
	}
	a.finish(ctx.Err())
	<-work
	v := a.snapshot()
	if v.Busy || !v.Closing || v.Message != "" {
		t.Fatalf("unexpected closing state: %+v", v)
	}
}

func TestCancelledIndexingLeavesNoNewRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	home := t.TempDir()
	if _, err := createSession(ctx, home, t.TempDir(), manifest{Preview: true}); err != context.Canceled {
		t.Fatalf("creation did not stop: %v", err)
	}
	dirs, err := os.ReadDir(home)
	if err != nil || len(dirs) != 0 {
		t.Fatalf("found a new run after cancelling indexing: %+v, %v", dirs, err)
	}
}
