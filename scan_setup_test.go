package main

import (
	"context"
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
