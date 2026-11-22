package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	if state.Operation != scanDuplicates || state.Renamed != 0 || state.Proposed != 0 {
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

func TestDuplicateReviewCompareVersionsAndRecycleExactCopies(t *testing.T) {
	base, home := t.TempDir(), t.TempDir()
	root := filepath.Join(base, "files")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"Study v1.txt":   "The earlier study contains these introductory results.",
		"Study v2.txt":   "The updated study contains these introductory results and new evidence.",
		"Study v10.txt":  "The last study contains these introductory results and a complete analysis.",
		"Original.txt":   "A complete sentence with the same contents under an unrelated filename.",
		"Other.txt":      "A complete sentence with the same contents under an unrelated filename.",
		"Reformatted.md": "A complete sentence with the same contents under an unrelated filename.\n",
	}
	for name, body := range paths {
		writeScanFile(t, filepath.Join(root, name), body)
	}
	a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close(); a.chat.cancel() }()
	if err := a.startUIOperation(root, true, false, false, false, false, nil, nil, false, scanDuplicates); err != nil {
		t.Fatal(err)
	}
	state := waitForOperation(t, a)
	results, err := a.scanResults(state.Job)
	if err != nil || len(results.Exact) != 1 || len(results.Possible) != 2 {
		t.Fatalf("%+v %v", results, err)
	}
	exact := results.Exact[0].Members
	var versions []duplicateMember
	for _, g := range results.Possible {
		if g.Grouping == "Similar filenames" {
			versions = g.Members
		}
	}
	if len(versions) != 3 || versions[0].Version != "1" || versions[1].Version != "2" || versions[2].Version != "10" {
		t.Fatalf("version order: %+v", versions)
	}
	ctx := context.Background()
	if report, err := a.compareDuplicateFiles(ctx, state.Job, versions[0].Index, versions[1].Index); err != nil || report.Verdict != "Different complete text" || report.Recommendation != "Keep both as versions" {
		t.Fatalf("%+v: %v", report, err)
	}
	if report, err := a.compareDuplicateFiles(ctx, state.Job, exact[0].Index, exact[1].Index); err != nil || report.Verdict != "Exact duplicate" {
		t.Fatalf("%+v: %v", report, err)
	}
	if _, err := a.recycleMatchedCopies(ctx, state.Job, state.Revision, exact[0].Index, []int{versions[0].Index}, os.Remove); err == nil {
		t.Fatal("allowed recycling a non-identical file")
	}
	count, err := a.recycleMatchedCopies(ctx, state.Job, state.Revision, exact[0].Index, []int{exact[1].Index}, os.Remove)
	if count != 1 || err != nil {
		t.Fatalf("recycled %d: %v", count, err)
	}
	if _, err := os.Stat(exact[0].Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exact[1].Path); !os.IsNotExist(err) {
		t.Fatalf("extra remains: %v", err)
	}
	if err := a.selectJob(state.Job); err != nil {
		t.Fatal(err)
	}
	results, err = a.scanResults(state.Job)
	if err != nil || len(results.Exact) != 0 || results.Recycled != 1 {
		t.Fatalf("saved duplicate state %+v %v", results, err)
	}
}

func TestDuplicateBatchPreflightsEveryGroupAndRecordsPartialResults(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{
		"A (1).txt": "alpha", "A.txt": "alpha",
		"B copy.txt": "bravo", "B.txt": "bravo",
	} {
		writeScanFile(t, filepath.Join(root, name), body)
	}
	a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close(); a.chat.cancel() }()
	if err := a.startUIOperation(root, true, false, false, false, false, nil, nil, false, scanDuplicates); err != nil {
		t.Fatal(err)
	}
	state := waitForOperation(t, a)
	results, err := a.scanResults(state.Job)
	if err != nil || len(results.Exact) != 2 {
		t.Fatalf("exact results: %+v, %v", results, err)
	}
	groups := make([]duplicateSelection, 0, 2)
	for i, group := range results.Exact {
		want := []string{"A.txt", "B.txt"}[i]
		selection := duplicateSelection{Keep: group.RecommendedKeeper}
		for _, member := range group.Members {
			if member.Index == selection.Keep {
				if filepath.Base(member.Path) != want {
					t.Fatalf("keeper %s, want %s", member.Path, want)
				}
			} else {
				selection.Indices = append(selection.Indices, member.Index)
			}
		}
		groups = append(groups, selection)
	}
	changed := filepath.Join(root, "B copy.txt")
	file, err := os.Stat(changed)
	if err != nil {
		t.Fatal(err)
	}
	rewrite := func(content string) {
		t.Helper()
		if err := os.WriteFile(changed, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(changed, file.ModTime(), file.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("other")
	ctx := context.Background()
	receipt, err := a.recycleDuplicateGroups(ctx, state.Job, state.Revision, groups, os.Remove)
	if err == nil || receipt.Recycled != 0 {
		t.Fatalf("preflight allowed stale bytes: %+v, %v", receipt, err)
	}
	if _, err := os.Stat(filepath.Join(root, "A (1).txt")); err != nil {
		t.Fatalf("preflight recycled an earlier group: %v", err)
	}
	rewrite("bravo")
	receipt, err = a.recycleDuplicateGroups(ctx, state.Job, state.Revision, groups, func(path string) error {
		if path == changed {
			return os.ErrPermission
		}
		return os.Remove(path)
	})
	if err == nil || len(receipt.Groups) != 2 || receipt.Recycled != 1 || receipt.Groups[0].Recycled != 1 || receipt.Groups[1].Recycled != 0 {
		t.Fatalf("partial receipt: %+v, %v", receipt, err)
	}
	if err := a.selectJob(state.Job); err != nil {
		t.Fatal(err)
	}
	state = waitForOperation(t, a)
	results, err = a.scanResults(state.Job)
	if err != nil || results.Recycled != 1 || len(results.Exact) != 1 {
		t.Fatalf("persisted receipt: %+v, %v", results, err)
	}
	receipt, err = a.recycleDuplicateGroups(ctx, state.Job, state.Revision, groups[1:], os.Remove)
	if err != nil || receipt.Recycled != 1 {
		t.Fatalf("retry: %+v, %v", receipt, err)
	}
	for _, keeper := range []string{"A.txt", "B.txt"} {
		if _, err := os.Stat(filepath.Join(root, keeper)); err != nil {
			t.Fatalf("missing keeper %s: %v", keeper, err)
		}
	}
}

func TestDuplicateRecommendationUsesCompleteTextAndSharedVersionLabels(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	common := strings.Repeat("The opening summary describes all results before the final decision.\n", 3000)
	for name, body := range map[string]string{
		"Study [v1].txt":  common + "The treatment was effective.\n",
		"Study [v2].txt":  common + "The treatment was ineffective.\n",
		"Study [v10].txt": common + "The treatment was withdrawn.\n",
		"Notes.txt":       "Some meaningful text\n", "Notes copy.txt": "Some meaningful text\r\n",
		"Report.txt":      "Our astronomy telescope measured orbital speeds and stellar distances.",
		"Report copy.txt": "This recipe combines potatoes, carrots, butter and herbs.",
		"Record.bin":      "\x00first", "Record copy.bin": "\x00second",
	} {
		writeScanFile(t, filepath.Join(root, name), body)
	}
	a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close(); a.chat.cancel() }()
	if err := a.startUIOperation(root, true, false, false, false, false, nil, nil, false, scanDuplicates); err != nil {
		t.Fatal(err)
	}
	state := waitForOperation(t, a)
	results, err := a.scanResults(state.Job)
	if err != nil {
		t.Fatal(err)
	}
	recommendations := map[string]string{}
	for _, group := range results.Possible {
		first, second := group.Members[0], group.Members[1]
		report, err := a.compareDuplicateFiles(context.Background(), state.Job, first.Index, second.Index)
		if err != nil {
			t.Fatal(err)
		}
		key, _, _ := duplicateFilenameParts(baseName(filepath.Base(first.Path)))
		recommendations[key] = report.Recommendation
		if key == "Study" && (report.Verdict != "Different complete text" || len(group.Members) != 3 ||
			first.Version != "1" || second.Version != "2" || group.Members[2].Version != "10") {
			t.Fatalf("version comparison: %+v, %+v", group, report)
		}
	}
	for key, want := range map[string]string{
		"Study": "Keep both as versions", "Notes": "Keep one",
		"Report": "Keep both as distinct files", "Record": "Needs your judgment",
	} {
		if got := recommendations[key]; got != want {
			t.Errorf("%s recommendation = %q, want %q; all: %v", key, got, want, recommendations)
		}
	}
}
