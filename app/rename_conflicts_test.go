package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSingleRenameCanRetryAfterDestinationConflict(t *testing.T) {
	for _, recoverIntent := range []bool{false, true} {
		label := "live request"
		if recoverIntent {
			label = "recovered intent"
		}
		t.Run(label, func(t *testing.T) {
			root, home, engine, models := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			source := filepath.Join(root, "READING_GUIDE.txt")
			destination := filepath.Join(root, "Reading Guide.txt")
			writeTestFile(t, source, "A descriptive reading guide.")
			a, err := newApp(home, engine, models)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.start(root, true, false, false, false, false); err != nil {
				t.Fatal(err)
			}
			waitForPhase(t, a, "preview")
			defer func() { a.s.close() }()

			job := a.snapshot().Job
			rows, err := a.previewPage(0)
			if err != nil || len(rows) != 1 || rows[0].To != destination {
				t.Fatalf("unexpected preview: %+v, %v", rows, err)
			}
			index := rows[0].Index
			writeTestFile(t, destination, "Independent destination.")
			if recoverIntent {
				plan := a.s.plans[index]
				if err := a.s.append(record{Type: "single-intent", Index: index, Path: source, Move: plan.Move, Auto: true}); err != nil {
					t.Fatal(err)
				}
			} else {
				result := requestSingleRename(t, a, job, index)
				if result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "Name conflict") {
					t.Fatalf("failed rename returned HTTP %d: %s", result.Code, result.Body.String())
				}
			}
			a.s.close()
			reopened, err := newApp(home, engine, models)
			if err != nil {
				t.Fatal(err)
			}
			a = reopened

			rows, err = a.previewPage(0)
			if err != nil || len(rows) != 1 || rows[0].To != destination || !strings.Contains(rows[0].Note, "Name conflict") {
				t.Fatalf("expected a retryable proposal with a conflict reason: %+v, %v", rows, err)
			}
			if got := a.snapshot(); got.Pending || got.Renamed != 0 || got.Proposed != 1 {
				t.Fatalf("failed rename changed proposal state: %+v", got)
			}
			if got, err := os.ReadFile(source); err != nil || string(got) != "A descriptive reading guide." {
				t.Fatalf("source changed after a failed attempt: %q, %v", got, err)
			}
			if got, err := os.ReadFile(destination); err != nil || string(got) != "Independent destination." {
				t.Fatalf("destination changed after a failed attempt: %q, %v", got, err)
			}
			if err := os.Remove(destination); err != nil {
				t.Fatal(err)
			}
			result := requestSingleRename(t, a, job, index)
			if result.Code != http.StatusOK || a.snapshot().Renamed != 1 || a.snapshot().Proposed != 0 {
				t.Fatalf("rename retry returned HTTP %d: %s", result.Code, result.Body.String())
			}
			if got, err := os.ReadFile(destination); err != nil || string(got) != "A descriptive reading guide." {
				t.Fatalf("unexpected retried file: %q, %v", got, err)
			}
			if err := a.undo(); err != nil {
				t.Fatal(err)
			}
			waitForPhase(t, a, "undone")
			if got, err := os.ReadFile(source); err != nil || string(got) != "A descriptive reading guide." {
				t.Fatalf("unexpected restored file: %q, %v", got, err)
			}
		})
	}
}

func requestSingleRename(t *testing.T, a *application, job string, index int) *httptest.ResponseRecorder {
	t.Helper()
	handler := server(a).Handler
	const url = "http://127.0.0.1:39173"
	initial := httptest.NewRecorder()
	handler.ServeHTTP(initial, httptest.NewRequest(http.MethodGet, url+"/", nil))
	cookies := initial.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected a session cookie, got %d", len(cookies))
	}
	request := httptest.NewRequest(http.MethodPost, url+"/api/action", strings.NewReader(
		fmt.Sprintf(`{"action":"apply-one","id":%q,"index":%d}`, job, index)))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(cookies[0])
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	return result
}

func TestImageContentSkippedWithLocalAIOff(t *testing.T) {
	root := t.TempDir()
	body, err := assets.ReadFile("ui/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "READING_GUIDE.png"), body, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true, Partial: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	d, _, err := inspect(context.Background(), s, nil, nil, 0)
	if err != nil || d.Inspection.Image || d.Inspection.AI != "Off" || d.Outcome != "Not assessed" || d.Note != "" || !strings.Contains(d.Reason, "AI is off") {
		t.Fatalf("unexpected AI-off image inspection: %+v, %v", d, err)
	}
}

func TestReadySelectionDuringScanAndRecovery(t *testing.T) {
	root := t.TempDir()
	a, err := newApp(t.TempDir(), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ALPHA_GUIDE.txt", "BETA_GUIDE.txt", "GAMMA_GUIDE.txt"} {
		writeTestFile(t, filepath.Join(root, name), "A descriptive reading guide.")
	}
	s, err := createSession(context.Background(), filepath.Join(a.home, "runs"), root, manifest{Preview: true, Partial: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close() }()
	a.loadSession(s)
	a.scanActive, a.v.Busy, a.v.Phase = true, true, "files"
	if err := process(context.Background(), s, nil, 0); err != nil {
		t.Fatal(err)
	}
	firstRevision := s.planRevision
	if err := process(context.Background(), s, nil, 1); err != nil {
		t.Fatal(err)
	}
	a.syncSession(true)
	if err := a.applySelected(a.v.Job, firstRevision, []int{0}, false); err != nil {
		t.Fatal(err)
	}
	if a.v.Phase != "files" || a.v.Done != 2 || s.reviewCount() != 1 {
		t.Fatalf("applying ready file disturbed scan or unchecked selection: %+v", a.v)
	}
	if _, err := os.Stat(s.plans[0].Move.To); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.plans[1].Path); err != nil {
		t.Fatal(err)
	}
	// Simulate an interruption after approval and before the write.
	if err := s.append(record{Type: "apply-selection", Selection: []int{1}}); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.close()
	reopened, err := openSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.scanActive, a.v.Busy = false, false
	a.loadSession(reopened)
	if a.v.Phase != "apply-paused" {
		t.Fatalf("expected saved selection, got %s", a.v.Phase)
	}
	if err := a.applySelected(a.v.Job, a.v.Revision, nil, true); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "paused")
	if _, err := os.Stat(reopened.plans[1].Move.To); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "GAMMA_GUIDE.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestRoughImageRefinementCanUndoBothRenames(t *testing.T) {
	root := t.TempDir()
	name := "IMG_4821.jpg"
	path := filepath.Join(root, name)
	writeTestFile(t, path, "Disposable fake image bytes.")
	settings := defaultSettings()
	settings.Execution.Mode = "automatic"
	settings.Execution.AutoKinds = []string{"other"}
	a, err := newApp(t.TempDir(), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := createSession(context.Background(), filepath.Join(a.home, "runs"), root, manifest{Preview: true, Auto: true, Settings: &settings})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	st, err := fingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	proposal := decideImage(name, evidence{Photo: []byte{1}}, assessment{Label: "Potted Plant"}, true, settings.Naming)
	if err := recordSuggestion(s, 0, proposal, st); err != nil {
		t.Fatal(err)
	}
	if !s.plans[0].Rough {
		t.Fatalf("rough proposal was not eligible for configured automatic naming: %+v", s.plans[0])
	}
	if err := s.autoApply(s.plans[0]); err != nil {
		t.Fatal(err)
	}
	a.loadSession(s)
	page, err := a.reviewInspections(0, false, "flagged")
	if err != nil || len(page.Rows) != 1 || !page.Rows[0].Refinement || !page.Rows[0].ReviewActions {
		t.Fatalf("rough rename is missing from review: %+v, %v", page, err)
	}
	if err := a.useReviewName(a.v.Job, a.v.Revision, 0, "Cinnamon Basil on the Windowsill.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Cinnamon Basil on the Windowsill.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := a.undo(); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "undone")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if current := s.currentPath(path, 0); current != path {
		t.Fatalf("expected original after Undo, got %q", current)
	}
}
