package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOneRenameAndPersistedDiagnostics(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(root, "DOCUMENT_ONE.txt"), "Document One\nPrivate input text")
	writeTestFile(t, filepath.Join(root, "DOCUMENT_TWO.txt"), "Document Two\nPrivate input text")
	app, err := newApp(home, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = app.start(root, true, false, false, false, false); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	done := app.jobDone
	app.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("scan never finished")
		}
	}
	old := app.snapshot()
	if old.Proposed != 2 || old.Clock.ActiveMS == 0 {
		t.Fatalf("missing scan results: %+v", old)
	}
	rows, err := app.previewPage(0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("%+v, %v", rows, err)
	}
	if err := app.applyOne(old.Job, rows[0].Index); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(rows[0].To); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(rows[1].From); err != nil {
		t.Fatal(err)
	}
	if app.snapshot().Proposed != 1 {
		t.Fatalf("remaining proposal lost: %+v", app.snapshot())
	}
	app.s.close()
	reopened, err := newApp(home, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.s.close()
	report, err := reopened.diagnostic(old.Job, false)
	if err != nil || report.RenameMS == 0 || report.ReadMS == 0 || report.Clock.ActiveMS == 0 || len(report.Slowest) != 2 {
		t.Fatalf("missing persisted timing: %+v, %v", report, err)
	}
	if reopened.snapshot().Proposed != 1 {
		t.Fatal("remaining proposal was lost on reopen")
	}
	b, err := json.Marshal(report)
	if err != nil || bytes.Contains(b, []byte(root)) || bytes.Contains(b, []byte("Private input")) || bytes.Contains(b, []byte("Document One")) {
		t.Fatalf("diagnostics included private inputs: %s, %v", b, err)
	}
}

func TestDesktopConnectionDefaultsOnAndCanBeDisabled(t *testing.T) {
	a, err := newApp(t.TempDir(), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.chat.cancel()
	if !a.snapshot().BridgeEnabled {
		t.Fatal("desktop connection is off at launch")
	}
	req := func(key, origin string, want int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, bridgeURL, strings.NewReader(`{"action":"status"}`))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		server(a).Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("HTTP %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
	req("", "", 403)
	key, err := os.ReadFile(bridgeKeyPath(a.home))
	if err != nil {
		t.Fatal(err)
	}
	req(string(key), "http://127.0.0.1:39173", 403)
	req(string(key), "", 200)
	if err := a.setBridge(false); err != nil {
		t.Fatal(err)
	}
	req(string(key), "", 403)
	if a.snapshot().BridgeEnabled {
		t.Fatal("desktop connection remained on after opting out")
	}
	reopened, err := newApp(a.home, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.chat.cancel()
	if !reopened.snapshot().BridgeEnabled || reopened.bridgeKey == string(key) {
		t.Fatal("a fresh launch did not enable the connection with a new key")
	}
}

func TestFlaggedFileNamesAndRecycleReceipt(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	path := filepath.Join(root, "Untitled.txt")
	writeTestFile(t, path, "Workshop Planning Notes\n\nRead these notes before the session.")
	a, err := newApp(home, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.chat.cancel()
	s, err := createSession(context.Background(), filepath.Join(home, "runs"), root, manifest{Preview: true, Partial: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	st, err := fingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.append(record{Type: "plan", Index: 0, Path: path, Stamp: st, Decision: "Needs review"}); err != nil {
		t.Fatal(err)
	}
	a.loadSession(s)
	v := a.snapshot()
	names, err := a.recommendedNames(context.Background(), v.Job, v.Revision, 0)
	if err != nil || !slices.Contains(names, "Workshop Planning Notes.txt") {
		t.Fatalf("missing file-based name: %v, %v", names, err)
	}
	if _, err := a.recommendedNames(context.Background(), v.Job, v.Revision+1, 0); err == nil {
		t.Fatal("stale review revision accepted")
	}
	if err := s.append(record{Type: "recycle-intent", Index: 0, Path: path, Stamp: st}); err != nil {
		t.Fatal(err)
	}
	if err := s.finishRecycle(); err != nil || s.pending != nil || !flaggedPlan(s.plans[0]) {
		t.Fatalf("a still-present file was removed from review: %v", err)
	}
	if err := s.append(record{Type: "recycle-intent", Index: 0, Path: path, Stamp: st}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(root, "Recovered file.txt")); err != nil {
		t.Fatal(err)
	}
	s.close()
	reopened, err := openSession(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	a.loadSession(reopened)
	page, err := a.reviewInspections(0, false, "flagged")
	if err != nil || page.Total != 0 || reopened.pending != nil || reopened.plans[0].Decision != removedFlaggedFile {
		t.Fatalf("the reopened review did not reconcile the missing file: %+v, %v", page, err)
	}
}
