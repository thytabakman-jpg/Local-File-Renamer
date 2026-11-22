package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitForOperation(t *testing.T, a *application) view {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		state := a.snapshot()
		if !state.Busy {
			if state.Message != "" {
				t.Fatal(state.Message)
			}
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("The scan operation did not finish")
	return view{}
}

func writeScanFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFileMoverReviewsAndAppliesInsideSelectedFolder(t *testing.T) {
	base, home := t.TempDir(), t.TempDir()
	root := filepath.Join(base, "Selected")
	inbox := filepath.Join(root, "Inbox")
	for _, folder := range []string{inbox, filepath.Join(base, "Outside"), filepath.Join(root, "Course Forms")} {
		if err := os.MkdirAll(folder, 0700); err != nil {
			t.Fatal(err)
		}
	}
	lesson := filepath.Join(inbox, "Training Notes Lesson.txt")
	agenda := filepath.Join(inbox, "Training Notes Agenda.txt")
	form := filepath.Join(inbox, "Course Forms Registration.txt")
	for _, path := range []string{lesson, agenda, form} {
		writeScanFile(t, path, baseName(filepath.Base(path))+"\n\n"+filepath.Base(path))
	}
	outside := filepath.Join(base, "Outside", "Training Notes Outside.txt")
	writeScanFile(t, outside, "Outside the selected folder")
	a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close(); a.chat.cancel() }()
	if err := a.startUIOperation(root, true, true, true, true, true, nil, nil, false, scanMove); err != nil {
		t.Fatal(err)
	}
	state := waitForOperation(t, a)
	newFolder := filepath.Join(root, "Training Notes")
	if _, err := os.Stat(newFolder); !os.IsNotExist(err) {
		t.Fatalf("The scan created a folder before approval: %v", err)
	}
	results, err := a.scanResults(state.Job)
	if err != nil {
		t.Fatal(err)
	}
	var newMoves, formMoves []proposalRow
	for _, folder := range results.Folders {
		for _, proposal := range folder.Proposals {
			if !within(root, proposal.From) || !within(root, proposal.To) || filepath.Base(proposal.From) != filepath.Base(proposal.To) {
				t.Fatalf("Unsafe or unintended proposal: %+v", proposal)
			}
		}
		switch folder.Path {
		case newFolder:
			if !folder.New {
				t.Fatal("The folder proposal was not marked as new")
			}
			newMoves = folder.Proposals
		case filepath.Join(root, "Course Forms"):
			formMoves = folder.Proposals
		}
	}
	if len(newMoves) != 2 || len(formMoves) != 1 {
		t.Fatalf("Unexpected destinations: %+v", results.Folders)
	}
	if err := a.changeLocation(state.Job, state.Revision, newMoves[0].Index, filepath.Dir(outside)); err == nil {
		t.Fatal("The move accepted a destination outside the selected folder")
	}
	if err := a.applySelected(state.Job, state.Revision, []int{newMoves[0].Index}, false); err != nil {
		t.Fatal(err)
	}
	state = waitForOperation(t, a)
	if state.Proposed != 2 || state.UndoLeft != 1 {
		t.Fatalf("Partial approval changed unselected proposals: %+v", state)
	}
	for _, path := range []string{newMoves[0].To, newMoves[1].From, form, outside} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.undo(); err != nil {
		t.Fatal(err)
	}
	waitForOperation(t, a)
	if _, err := os.Stat(newMoves[0].From); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateCheckStaysReadOnlyAndSavesResults(t *testing.T) {
	base, home := t.TempDir(), t.TempDir()
	root := filepath.Join(base, "Selected")
	nested := filepath.Join(root, "Nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, "Intake Summary.txt"):      "identical content",
		filepath.Join(nested, "Unrelated Name.txt"):    "identical content",
		filepath.Join(root, "Intake Summary Copy.txt"): "different content",
		filepath.Join(base, "Outside.txt"):             "identical content",
	}
	for path, contents := range files {
		writeScanFile(t, path, contents)
	}
	outsideLink := filepath.Join(root, "Outside Link.txt")
	if err := os.Symlink(filepath.Join(base, "Outside.txt"), outsideLink); err == nil {
		defer os.Remove(outsideLink)
	}
	a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close(); a.chat.cancel() }()
	if err := a.startUIOperation(root, false, true, true, true, true, nil, nil, false, scanDuplicates); err != nil {
		t.Fatal(err)
	}
	state := waitForOperation(t, a)
	if state.Operation != scanDuplicates || !state.PreviewOnly || state.Proposed != 0 {
		t.Fatalf("Duplicate scan exposed write proposals: %+v", state)
	}
	if err := a.selectJob(state.Job); err != nil {
		t.Fatal(err)
	}
	results, err := a.scanResults(state.Job)
	if err != nil {
		t.Fatal(err)
	}
	if len(results.Exact) != 1 || len(results.Possible) != 1 || len(results.Exact[0].Files) != 2 || len(results.Possible[0].Files) != 2 {
		t.Fatalf("Incorrect duplicate groups: %+v", results)
	}
	for _, group := range append(results.Exact, results.Possible...) {
		for _, path := range group.Files {
			if !within(root, path) || path == outsideLink {
				t.Fatalf("Duplicate check included an out-of-scope file: %s", path)
			}
		}
	}
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("Duplicate check changed %s: %s, %v", path, got, err)
		}
	}
}
