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
