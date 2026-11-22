package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChatFreshScanExplainsAndReplacesRestrictedScope(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "WhatsApp Documents")
	if err := os.MkdirAll(filepath.Join(root, "Subfolder"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 9 {
		writeTestFile(t, filepath.Join(root, fmt.Sprintf("Document %d.txt", i)), fmt.Sprint(i))
	}
	writeTestFile(t, filepath.Join(root, "Settings.custom"), "a matching file")
	writeTestFile(t, filepath.Join(root, "Subfolder", "README"), "a matching file")

	app, err := newApp(filepath.Join(base, "app"), filepath.Join(base, "backend"), filepath.Join(base, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		app.chat.cancel()
		app.s.close()
	}()

	info, err := app.settingsInfo("")
	if err != nil {
		t.Fatal(err)
	}
	settings := info.Effective
	settings.Scope.IncludeExtensions = []string{".txt"}
	settings.Scope.Subfolders = false
	if err := app.saveProfile(info.Revision, info.Active, &settings, true); err != nil {
		t.Fatal(err)
	}

	talk := func(message string) chatReply {
		t.Helper()
		reply, err := app.talk(context.Background(), chatRequest{Message: message, Folder: root})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	done := func(reply chatReply) view {
		t.Helper()
		if reply.ScanID == "" {
			t.Fatal(reply)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			state := app.snapshot()
			if state.StartID == reply.ScanID && !state.Busy {
				return state
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("scan did not finish")
		return view{}
	}

	first := done(talk("check for duplicates"))
	if first.Inventory.Included != 9 || first.Inventory.Filtered != 1 || first.Inventory.OmittedFolders != 1 {
		t.Fatal(first.Inventory)
	}

	reply := talk("Find out why it only found nine, then start a fresh preview of every file in this folder and its subfolders.")
	if !strings.Contains(reply.Message, "Included extensions: .txt") || !strings.Contains(reply.Message, "9 included") {
		t.Fatal(reply.Message)
	}
	second := done(reply)
	if second.Job == first.Job || second.Inventory.Included != 11 || second.FilesAssessed != 11 {
		t.Fatalf("unexpected fresh scan: %+v", second)
	}
	info, err = app.settingsInfo("")
	if err != nil {
		t.Fatal(err)
	}
	if info.Run.Cache.ReuseEvidence || info.Run.Cache.ReuseAssessment ||
		len(info.Effective.Scope.IncludeExtensions) != 1 || info.Effective.Scope.Subfolders {
		t.Fatal("fresh scan reused the cache or modified the profile")
	}

	reply = talk("Use my last WhatsApp folder, check every file and subfolder for duplicates, and show me the exact matches.")
	third := done(reply)
	results, err := app.chatScanUpdate(reply.ScanID, reply.ExactOnly, reply.ShowResults)
	if err != nil || third.Operation != scanDuplicates || len(results.Groups) != 1 || len(results.Groups[0].Files) != 2 {
		t.Fatalf("duplicate results: %+v, %v", results, err)
	}
	talk("Make that my default")
	info, err = app.settingsInfo("")
	if err != nil || len(info.Effective.Scope.IncludeExtensions) > 0 || !info.Effective.Scope.Subfolders ||
		!info.Effective.Cache.ReuseEvidence || !info.Effective.Cache.ReuseAssessment {
		t.Fatal("default changes did not persist or fresh-only settings leaked", err)
	}
}

func TestConnectorStartAndCompactSettings(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(root, "report.pdf"), "Synthetic PDF path for filter coverage.")
	writeTestFile(t, filepath.Join(root, "notes.txt"), "This file is excluded.")
	a, err := newApp(home, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close() }()
	original := a.activeSettings()
	response, err := a.bridgeAction(bridgeCommand{Action: "start", Root: root, Operation: scanRename,
		ScopeMode: "selected", Extensions: []string{".pdf"}, Changes: map[string]json.RawMessage{"execution.mode": json.RawMessage(`"review"`)}})
	if err != nil {
		t.Fatal(err)
	}
	job := response.(map[string]any)["job"].(string)
	if status, err := a.bridgeStatus(job); err != nil || status["Job"] != job {
		t.Fatalf("job not returned consistently: %+v, %v", status, err)
	}
	select {
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not complete")
	case <-func() <-chan struct{} {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.jobDone != nil {
			return a.jobDone
		}
		ready := make(chan struct{})
		close(ready)
		return ready
	}():
	}
	status, err := a.bridgeStatus(job)
	if err != nil || status["Matched"] != 1 || status["Mode"] != "review" || status["Partial"] != true {
		t.Fatalf("unexpected compact status: %+v, %v", status, err)
	}
	if len(a.activeSettings().Scope.IncludeExtensions) != len(original.Scope.IncludeExtensions) {
		t.Fatal("one-run settings unexpectedly persisted")
	}
	compact, err := a.bridgeAction(bridgeCommand{Action: "get_settings", Fields: []string{"execution.mode"}})
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := json.Marshal(compact)
	if err != nil || len(bytes) > 400 {
		t.Fatalf("individual settings response too large: %d bytes, %v", len(bytes), err)
	}
}
