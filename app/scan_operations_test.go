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
	if len(results.Exact) != 1 || results.Unresolved != 1 || len(results.Exact[0].Files) != 2 || len(results.Verified) != 0 {
		t.Fatalf("Incorrect duplicate groups: %+v", results)
	}
	for _, group := range results.Exact {
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
	if err != nil || len(results.Exact) != 1 || results.Unresolved != 2 || len(results.Verified) != 0 {
		t.Fatalf("%+v %v", results, err)
	}
	exact := results.Exact[0].Members
	versions := make(map[string]int)
	for _, plan := range a.s.plans {
		versions[filepath.Base(plan.Path)] = plan.Index
	}
	ctx := context.Background()
	if report, err := a.compareDuplicateFiles(ctx, state.Job, versions["Study v1.txt"], versions["Study v2.txt"]); err != nil || report.Verdict != "Different complete text" || report.Recommendation != "Keep both as versions" {
		t.Fatalf("%+v: %v", report, err)
	}
	if report, err := a.compareDuplicateFiles(ctx, state.Job, exact[0].Index, exact[1].Index); err != nil || report.Verdict != "Exact duplicate" {
		t.Fatalf("%+v: %v", report, err)
	}
	if _, err := a.recycleMatchedCopies(ctx, state.Job, state.Revision, exact[0].Index, []int{versions["Study v1.txt"]}, os.Remove); err == nil {
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

func TestVerifiedDuplicateRecommendationsAndApproval(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	ctx := context.Background()
	content := "BT /F1 12 Tf 72 700 Td (A complete page of identical journal article content.) Tj ET\n"
	info := func(date string) string {
		return "<< /Title (Trees and Rivers) /CreationDate (D:" + date + "000000Z) >>"
	}
	pdfKeeper := filepath.Join(root, "Smith - 2023 - Trees and Rivers - Botany Journal.pdf")
	pdfCopy := filepath.Join(root, "Trees and Rivers (1).pdf")
	pdfThird := filepath.Join(root, "Trees and Rivers (2).pdf")
	pages := []string{content, content, content}
	writeTestPDFPages(t, pdfKeeper, pages, info("20260101"), "")
	writeTestPDFPages(t, pdfCopy, pages, info("20260102"), "")
	writeTestPDFPages(t, pdfThird, pages, info("20260103"), "")
	distinct := filepath.Join(root, "Trees and Rivers (3).pdf")
	revised := []string{content, content, strings.Replace(content, "identical", "different", 1)}
	writeTestPDFPages(t, distinct, revised, info("20260103"), "")
	differentMetadata := filepath.Join(root, "Trees and Rivers (4).pdf")
	writeTestPDFPages(t, differentMetadata, pages, strings.Replace(info("20260103"), "Trees and Rivers", "Other metadata", 1), "")
	annotated := filepath.Join(root, "Trees and Rivers (5).pdf")
	writeTestPDFPages(t, annotated, pages, info("20260103"), "/Annots [ << /Type /Annot /Subtype /Text /Rect [72 720 92 740] /Contents (Keep my reading notes) >> ]")
	for name, body := range map[string]string{
		"Notes.txt":      "Some meaningful text\n",
		"Notes copy.txt": "Some meaningful text\r\n",
		"Study v1.md":    "An identical opening.\nThe earlier results differ.\n",
		"Study v2.md":    "An identical opening.\nThe revised results differ.\n",
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
	if err := a.selectJob(state.Job); err != nil {
		t.Fatal(err)
	}
	state = a.snapshot()
	results, err := a.scanResults(state.Job)
	if err != nil || len(results.Exact) != 0 || len(results.Verified) != 2 || results.Unresolved < 2 {
		t.Fatalf("%+v %v", results, err)
	}
	var selections []duplicateSelection
	var pdfSelection duplicateSelection
	for _, group := range results.Verified {
		var sel duplicateSelection
		sel.Keep = group.RecommendedKeeper
		for _, member := range group.Members {
			if member.Index != sel.Keep {
				sel.Indices = append(sel.Indices, member.Index)
			}
		}
		if group.Finding == pdfDateDifference {
			pdfSelection = sel
			if a.s.plans[sel.Keep].Path != pdfKeeper || len(sel.Indices) != 2 {
				t.Fatalf("incorrect PDF recommendation: %+v", group)
			}
		}
		selections = append(selections, sel)
	}
	if len(pdfSelection.Indices) != 2 {
		t.Fatal("no PDF cleanup recommendation")
	}
	for _, plan := range a.s.plans {
		if plan.Path == distinct {
			wrong := []duplicateSelection{{Keep: pdfSelection.Keep, Indices: []int{plan.Index}}}
			if _, err := a.recycleApprovedCopies(ctx, state.Job, state.Revision, wrong, true, os.Remove); err == nil {
				t.Fatal("accepted a changed PDF as equivalent")
			}
		}
	}
	makeStale := filepath.Join(root, "Notes copy.txt")
	writeScanFile(t, makeStale, "Some newer text\r\n")
	if receipt, err := a.recycleApprovedCopies(ctx, state.Job, state.Revision, selections, true, os.Remove); err == nil || receipt.Recycled != 0 {
		t.Fatalf("stale batch recycled files: %+v %v", receipt, err)
	}
	if receipt, err := a.recycleApprovedCopies(ctx, state.Job, state.Revision, []duplicateSelection{pdfSelection}, true, os.Remove); err != nil || receipt.Recycled != 2 {
		t.Fatalf("approved PDF cleanup: %+v %v", receipt, err)
	}
	for _, file := range []string{pdfKeeper, distinct, makeStale, differentMetadata, annotated} {
		if _, err := os.Stat(file); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{pdfCopy, pdfThird} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("extra remains: %v", err)
		}
	}
}
