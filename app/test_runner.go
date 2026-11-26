package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Section: Experiment evidence and result contracts.

type testFileResult struct {
	File, Result, Expected, Verdict string
	Status, Reason, Note, Applied   string
	Proposed, Unresolved            bool
	Inspection                      inspection
}

type testDuplicateResult struct {
	Kind, Keeper, Expected, Reason, Verdict string
	Members                                 []string
}

type testFinding struct {
	Name                               string
	Operation                          scanOperation
	Model, Status, Error               string
	ElapsedMS, ApplyMS                 int64
	PeakAppBytes, PeakModelBytes       uint64
	Selected, Correct, Wrong, Deferred int
	Rows                               []testFileResult
	Groups                             []testDuplicateResult
}

// Section: Frozen input identity and copying.

func testPathKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func testRelative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || (!filepath.IsLocal(rel) && rel != ".") {
		return path
	}
	return filepath.ToSlash(rel)
}

// Freeze the approved input once. Read through os.Root so a replaced link
// cannot redirect a source read outside the test folder.
func copyTestInputs(ctx context.Context, source, target string, items []entry) (string, int, error) {
	if err := validateRoot(source); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(target, 0700); err != nil {
		return "", 0, err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return "", 0, err
	}
	defer root.Close()
	inputs := sha256.New()
	count := 0
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return "", count, err
		}
		if item.Skip {
			continue
		}
		rel := testRelative(source, item.Path)
		if !filepath.IsLocal(rel) || rel == "." {
			return "", count, errors.New("A test input is outside its folder")
		}
		dst := filepath.Join(target, filepath.FromSlash(rel))
		if item.Folder {
			if err := os.MkdirAll(dst, 0700); err != nil {
				return "", count, err
			}
			fmt.Fprintf(inputs, "dir\x00%s\x00", rel)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return "", count, err
		}
		info, err := root.Lstat(rel)
		if err != nil || linked(info) || !info.Mode().IsRegular() || info.ModTime().UnixNano() != item.Modified {
			return "", count, fmt.Errorf("Input changed before copying: %s", rel)
		}
		sum, err := copyTestFile(ctx, root, rel, dst, info)
		if err != nil {
			return "", count, fmt.Errorf("Copy %s: %w", rel, err)
		}
		fmt.Fprintf(inputs, "file\x00%s\x00%s\x00%d\x00", rel, sum, item.Modified)
		count++
	}
	return hex.EncodeToString(inputs.Sum(nil)), count, nil
}

func copyTestFile(ctx context.Context, root *os.Root, rel, dst string, original os.FileInfo) (string, error) {
	source, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer source.Close()
	current, err := source.Stat()
	if err != nil || !os.SameFile(original, current) {
		return "", errors.New("file was replaced")
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), contextReader{ctx, source})
	if err := errors.Join(err, out.Close()); err != nil {
		return "", err
	}
	after, err := root.Lstat(rel)
	if err != nil || !os.SameFile(original, after) || linked(after) || after.Size() != n ||
		after.Size() != original.Size() || !after.ModTime().Equal(original.ModTime()) {
		return "", errors.New("file changed while copying")
	}
	if err := os.Chtimes(dst, original.ModTime(), original.ModTime()); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Section: Experiment orchestration and variant execution.

func (a *application) runTest(ctx context.Context, job savedTest) {
	dir := filepath.Join(a.home, "tests", job.Job)
	work := filepath.Join(dir, "work")
	defer func() {
		_ = os.RemoveAll(work)
		a.testMu.Lock()
		defer a.testMu.Unlock()
		if a.testCancel != nil {
			a.testCancel()
		}
		a.testCancel, a.testActive = nil, testReceipt{}
		close(a.testDone)
		a.testDone = nil
	}()
	items, _, err := scanInventory(ctx, job.Plan.Root, *job.Plan.Scope)
	frozen := filepath.Join(work, "input", filepath.Base(job.Plan.Root))
	if err == nil {
		job.InputHash, job.InputFiles, err = copyTestInputs(ctx, job.Plan.Root, frozen, items)
	}
	if err == nil && job.InputFiles == 0 {
		err = errors.New("The approved scope contains no readable regular files")
	}
	if err != nil {
		job.Status, job.InputError = "failed", err.Error()
	} else {
		a.runTestVariants(ctx, &job, work, frozen)
	}
	if ctx.Err() != nil {
		job.Status = "cancelled"
	} else if job.Status == "running" {
		job.Status = "finished"
	}
	if job.Status == "finished" && slices.ContainsFunc(job.Findings, func(f testFinding) bool { return f.Status == "failed" || f.Status == "unavailable" }) {
		job.Status = "finished with issues"
	}
	job.Headline = testHeadline(job)
	job.Report = "/api/test-report?job=" + job.Job
	if err := a.writeTest(job); err != nil {
		a.testMu.Lock()
		a.testActive = testReceipt{}
		a.testMu.Unlock()
	}
}

func (a *application) runTestVariants(ctx context.Context, job *savedTest, work, frozen string) {
	frozenItems, _, err := scanInventory(ctx, frozen, *job.Plan.Scope)
	if err != nil {
		job.Status, job.InputError = "failed", err.Error()
		return
	}
	references := make(map[string]testExpectation)
	for _, ref := range job.Plan.Expected {
		if !slices.ContainsFunc(frozenItems, func(e entry) bool {
			return !e.Folder && !e.Skip && testPathKey(testRelative(frozen, e.Path)) == testPathKey(ref.File)
		}) {
			job.Status, job.InputError = "failed", fmt.Sprintf("Reference file is outside the frozen input: %s", ref.File)
			return
		}
		references[testPathKey(ref.File)] = ref
	}

	for i, variant := range job.Plan.Variants {
		if ctx.Err() != nil {
			break
		}
		job.Headline = fmt.Sprintf("Running variant %d of %d", i+1, job.Variants)
		a.sendTestProgress(*job)
		var selected map[string]bool
		opts := job.Settings[i]
		var f testFinding
		switch {
		case variant.Select != "all":
			prior := slices.IndexFunc(job.Findings, func(f testFinding) bool { return f.Name == variant.From })
			if prior < 0 || job.Findings[prior].Status != "finished" {
				f = testFinding{Name: variant.Name, Operation: variant.Operation, Status: "skipped", Error: "The earlier variant did not finish"}
			} else {
				selected = make(map[string]bool)
				for _, row := range job.Findings[prior].Rows {
					if variant.Select == "unresolved" && row.Unresolved || variant.Select == "wrong" && row.Verdict == "wrong" ||
						variant.Select == "changed" && row.Proposed {
						selected[testPathKey(row.File)] = true
					}
				}
				if len(selected) == 0 {
					f = testFinding{Name: variant.Name, Operation: variant.Operation, Status: "skipped", Error: "No files met the selection"}
				}
			}
		}
		if variant.BaselineJob != "" {
			f = a.readBaselineVariant(ctx, variant, frozen, frozenItems, references)
		} else if f.Status == "" {
			f = a.runTestVariant(ctx, variant, opts, work, frozen, frozenItems, selected, references, job.Plan.KeeperRule, i)
		}
		job.Findings = append(job.Findings, f)
		job.Completed = len(job.Findings)
		// Preserve completed branches even if a later one is interrupted.
		if err := a.writeTest(*job); err != nil {
			job.Status, job.InputError = "failed", err.Error()
			return
		}
	}
	if job.Plan.Interpret && ctx.Err() == nil {
		job.Summary = a.interpretTest(ctx, *job)
	}
}

func (a *application) runTestVariant(ctx context.Context, variant testVariant, opts scanSettings, work, frozen string, items []entry,
	selected map[string]bool, refs map[string]testExpectation, keeper testKeeperRule, index int) (f testFinding) {
	f = testFinding{Name: variant.Name, Operation: variant.Operation, Status: "finished"}
	root := frozen
	partial := variant.Operation == scanDuplicates || opts.AI.Model == "off" || opts.AI.Invoke == "never"
	f.Model = "Off"
	if variant.Operation != scanDuplicates && opts.Evidence.UseOCR && opts.Evidence.OCREngine == "tesseract" {
		if _, err := exec.LookPath("tesseract"); err != nil {
			f.Status, f.Error = "unavailable", "Tesseract OCR is not installed on the app's PATH"
			return
		}
	}
	if !partial {
		f.Model, _, _ = modelProfileFor(opts.AI.Model)
		if !modelReadyFor(a.modelDir, opts.AI.Model) {
			f.Status, f.Error = "unavailable", f.Model+" is unavailable on this computer"
			return
		}
	}
	variantDir := filepath.Join(work, fmt.Sprintf("variant-%d", index+1))
	if variant.Apply {
		root = filepath.Join(variantDir, filepath.Base(frozen))
		if _, _, err := copyTestInputs(ctx, frozen, root, items); err != nil {
			f.Status, f.Error = "failed", err.Error()
			return
		}
	}
	started := time.Now()
	stopMemory := sampleTestMemory()
	defer func() {
		f.PeakAppBytes = stopMemory()
	}()
	s, err := createSession(ctx, filepath.Join(variantDir, "runs"), root, manifest{
		Preview: true, RenameFolders: opts.Execution.FolderRenames, RepairMetadata: opts.Execution.Metadata, SortFolders: opts.Execution.Moves,
		Partial: partial, Settings: &opts, Operation: variant.Operation,
	})
	if err != nil {
		f.Status, f.Error = "failed", err.Error()
		return
	}
	defer s.close()
	// The frozen folder inventory remains available to scoring and grouping
	// when an earlier result selects only a subset for another assessment.
	if selected != nil {
		for i := range s.manifest.Items {
			item := &s.manifest.Items[i]
			if !item.Folder && !selected[testPathKey(testRelative(root, item.Path))] {
				item.Skip, item.Note = true, "Outside the variant selection"
			}
		}
	}
	s.cacheDir = filepath.Join(work, "cache")
	if err := os.MkdirAll(s.cacheDir, 0700); err != nil {
		f.Status, f.Error = "failed", err.Error()
		return
	}
	s.online = a.online
	runner := &application{home: variantDir, backend: a.backend, modelDir: a.modelDir, s: s, modelOwner: &a.chat}
	runner.v.Busy = true
	runner.syncSession(true)
	if err := runner.startClock(s, started); err != nil {
		f.Status, f.Error = "failed", err.Error()
		return
	}
	runner.run(ctx, s, partial)
	f.ElapsedMS, f.PeakModelBytes = elapsedMS(started), runner.testModelPeak
	if runner.v.Message != "" {
		f.Status, f.Error = "failed", runner.v.Message
	}
	if ctx.Err() != nil {
		f.Status = "cancelled"
	}

	var groups operationResults
	if variant.Operation == scanDuplicates {
		groups = s.duplicateResultsWithCandidates(true)
	}
	if variant.Operation == scanDuplicates {
		f.Groups = testDuplicateGroups(s, groups, keeper)
	}
	f.Rows = testRows(s, variant.Operation, selected, refs, f.Groups)
	if variant.Apply && f.Status == "finished" {
		started := time.Now()
		if err := applyTestVariant(ctx, runner, groups); err != nil {
			f.Status, f.Error = "failed", err.Error()
		}
		f.ApplyMS = elapsedMS(started)
		byPath := make(map[string]int, s.fileCount)
		for _, row := range s.plans {
			byPath[testPathKey(testRelative(root, row.Path))] = row.Index
		}
		for i := range f.Rows {
			row := &f.Rows[i]
			pos := byPath[testPathKey(row.File)]
			if applied, exists := s.outcomes[pos]; exists {
				row.Applied = joinNote(applied.Status, applied.Note)
			}
			if s.plans[pos].Decision == removedFlaggedFile {
				row.Applied = "Removed disposable exact copy"
			}
		}
	}
	f.countVerdicts()
	return
}

// Section: Finding aggregation.

func (f *testFinding) countVerdicts() {
	f.Selected = len(f.Rows)
	for _, row := range f.Rows {
		switch row.Verdict {
		case "correct":
			f.Correct++
		case "wrong":
			f.Wrong++
		case "deferred":
			f.Deferred++
		}
	}
}

// Section: Baseline reconstruction.

func (a *application) readBaselineVariant(ctx context.Context, v testVariant, frozen string, items []entry, refs map[string]testExpectation) testFinding {
	f := testFinding{Name: v.Name, Operation: v.Operation, Status: "failed"}
	s, err := openSessionReadOnly(filepath.Join(a.home, "runs", v.BaselineJob))
	if err != nil {
		f.Error = "Could not open the saved baseline run"
		return f
	}
	defer s.close()
	if s.manifest.Operation != "" && s.manifest.Operation != scanRename || !s.settings().Scope.VerifyComparison ||
		len(s.plans) != len(s.manifest.Items) || s.clock.Stopped.IsZero() {
		f.Error = "The saved baseline needs a completed Rename scan with comparison hashes"
		return f
	}
	hashes := make(map[string]string, s.fileCount)
	for _, item := range s.manifest.Items[:s.fileCount] {
		if item.Skip {
			continue
		}
		hashes[testPathKey(testRelative(s.manifest.Root, item.Path))] = item.Hash
	}
	for _, item := range items {
		if item.Folder || item.Skip {
			continue
		}
		key := testPathKey(testRelative(frozen, item.Path))
		hash, err := comparisonHash(ctx, item.Path)
		if err != nil || hash != hashes[key] {
			f.Error = "The saved baseline and the frozen inputs differ in relative paths or bytes"
			return f
		}
		delete(hashes, key)
	}
	if len(hashes) != 0 {
		f.Error = "The saved baseline contains extra files"
		return f
	}
	f.Model = "Off"
	if !s.partial {
		f.Model, _, _ = modelProfileFor(s.settings().AI.Model)
	}
	f.ElapsedMS, f.Status = s.clock.ActiveMS, "finished"
	f.Rows = testRows(s, scanRename, nil, refs, nil)
	f.countVerdicts()
	return f
}

// Section: Disposable-copy mutation testing.

func applyTestVariant(ctx context.Context, runner *application, groups operationResults) error {
	s := runner.s
	if s.manifest.Operation == scanDuplicates {
		batch, _ := s.exactBatch(groups.Exact)
		if len(batch.Groups) == 0 {
			return nil
		}
		_, err := s.recycleCopies(ctx, batch.Groups, false, "", os.Remove)
		return err
	}
	runner.mu.Lock()
	indices := make([]int, 0, runner.v.Proposed)
	for _, row := range s.plans {
		if s.reviewable(row) {
			indices = append(indices, row.Index)
		}
	}
	runner.mu.Unlock()
	if len(indices) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, runner.pauseRun)
	defer stop()
	receipt, err := runner.bridgeApplySelection(runner.v.Job, s.planRevision, indices, false, nil)
	if err != nil {
		return err
	}
	runner.mu.Lock()
	done := runner.jobDone
	runner.mu.Unlock()
	if done != nil {
		<-done
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if move, ok := receipt.(moveApplyReceipt); ok && (move.Failed > 0 || move.Remaining > 0) {
		return fmt.Errorf("Apply on copies: %d failed, %d remaining", move.Failed, move.Remaining)
	}
	if runner.v.Message != "" {
		return errors.New(runner.v.Message)
	}
	return nil
}

// Section: Resource measurement.

func sampleTestMemory() func() uint64 {
	stop := make(chan struct{})
	result := make(chan uint64, 1)
	go func() {
		var peak uint64
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			resident, _ := testProcessMemory(os.Getpid())
			peak = max(peak, resident)
			select {
			case <-stop:
				resident, _ := testProcessMemory(os.Getpid())
				result <- max(peak, resident)
				return
			case <-ticker.C:
			}
		}
	}()
	return func() uint64 {
		close(stop)
		return <-result
	}
}

// Section: Outcome summary and criterion evaluation.

func testHeadline(job savedTest) string {
	if job.InputError != "" {
		return "The test could not finish. See the report."
	}
	var failed int
	for _, f := range job.Findings {
		if f.Status == "failed" || f.Status == "unavailable" {
			failed++
		}
	}
	return fmt.Sprintf("%d of %d variants recorded, %d with issues", len(job.Findings), job.Variants, failed)
}

func testRows(s *session, op scanOperation, selected map[string]bool, refs map[string]testExpectation, groups []testDuplicateResult) []testFileResult {
	var rows []testFileResult
	kept := make(map[string]*bool)
	for _, g := range groups {
		if g.Keeper == "" {
			continue
		}
		for _, member := range g.Members {
			isKeeper := member == g.Keeper
			kept[testPathKey(member)] = &isKeeper
		}
	}
	for _, row := range s.plans[:min(s.fileCount, len(s.plans))] {
		file := testRelative(s.manifest.Root, row.Path)
		if selected != nil && !selected[testPathKey(file)] {
			continue
		}
		ref := refs[testPathKey(file)]
		r := testFileResult{File: file, Status: planOutcome(row), Reason: row.Reason, Note: row.Note, Inspection: row.Inspection,
			Proposed: row.Move != nil, Verdict: "unlabelled"}
		r.Unresolved = row.Unknown || row.Note != "" || slices.Contains([]string{"Needs review", "Analysis failed", "Insufficient evidence", "Skipped", "Not assessed"}, r.Status)
		switch op {
		case scanRename:
			r.Result = filepath.Base(row.Path)
			if row.Move != nil {
				r.Result = filepath.Base(row.Move.To)
			}
			if ref.Name != nil {
				r.Expected = *ref.Name
			}
		case scanMove:
			r.Result = testRelative(s.manifest.Root, filepath.Dir(row.Path))
			if row.Move != nil {
				r.Result = testRelative(s.manifest.Root, filepath.Dir(row.Move.To))
			}
			if ref.Folder != nil {
				r.Expected = *ref.Folder
			}
		case scanDuplicates:
			if decision := kept[testPathKey(file)]; decision != nil {
				r.Result = fmt.Sprint(*decision)
				r.Proposed = true
			} else {
				r.Unresolved = true
			}
			if ref.Keeper != nil {
				r.Expected = fmt.Sprint(*ref.Keeper)
			}
		}
		if r.Expected != "" {
			switch {
			case r.Unresolved && !r.Proposed:
				r.Verdict = "deferred"
			case r.Result == r.Expected:
				r.Verdict = "correct"
			default:
				r.Verdict = "wrong"
			}
		}
		if loc := r.Inspection.Location; loc != nil {
			relative := locationEvidence{Current: loc.Current, Alternatives: slices.Clone(loc.Alternatives)}
			relative.Current.Path = testRelative(s.manifest.Root, relative.Current.Path)
			for i := range relative.Alternatives {
				relative.Alternatives[i].Path = testRelative(s.manifest.Root, relative.Alternatives[i].Path)
			}
			r.Inspection.Location = &relative
		}
		rows = append(rows, r)
	}
	return rows
}

func testDuplicateGroups(s *session, result operationResults, rule testKeeperRule) []testDuplicateResult {
	var groups []testDuplicateResult
	for _, section := range []struct {
		kind string
		list []duplicateGroup
	}{{"exact", result.Exact}, {"verified equivalent", result.Verified}, {"possible", result.possible}} {
		for _, group := range section.list {
			row := testDuplicateResult{Kind: section.kind, Verdict: "unlabelled"}
			for _, m := range group.Members {
				rel := testRelative(s.manifest.Root, m.Path)
				row.Members = append(row.Members, rel)
				if section.kind != "possible" && m.Index == group.RecommendedKeeper {
					row.Keeper, row.Reason = rel, group.KeeperReason
				}
			}
			if rule.Mode != "" && row.Keeper != "" {
				row.Expected = testRuleKeeper(s, group.Members, rule)
				switch {
				case row.Expected == "":
					row.Verdict = "rule unresolved"
				case row.Expected == row.Keeper:
					row.Verdict = "agrees"
				default:
					row.Verdict = "differs"
				}
			}
			groups = append(groups, row)
		}
	}
	return groups
}

func testRuleKeeper(s *session, members []duplicateMember, rule testKeeperRule) string {
	best, ties, name := int64(-1<<62), 0, ""
	for _, m := range members {
		var score int64
		switch rule.Mode {
		case "newest":
			score = s.plans[m.Index].Stamp.Modified
		case "shortest_name":
			score = -int64(len([]rune(filepath.Base(m.Path))))
		case "prefer_folder":
			if testRelative(s.manifest.Root, filepath.Dir(m.Path)) != rule.Folder {
				continue
			}
		case "no_copy_suffix":
			if _, _, copyName := duplicateFilenameParts(baseName(filepath.Base(m.Path))); copyName {
				continue
			}
		}
		if score > best {
			best, ties, name = score, 1, testRelative(s.manifest.Root, m.Path)
		} else if score == best {
			ties++
		}
	}
	if ties == 1 {
		return name
	}
	return ""
}

// Section: Experiment status and execution initiation.

func (a *application) testStatus(id string) (testReceipt, error) {
	a.testMu.Lock()
	defer a.testMu.Unlock()
	if a.testActive.Job != "" && a.testActive.Job == id {
		return a.testActive, nil
	}
	job, err := a.readTest(id)
	if job.Status == "running" {
		job.Status, job.Headline = "interrupted", "The app closed before the test finished."
		if len(job.Findings) > 0 {
			job.Report = "/api/test-report?job=" + job.Job
		}
	}
	return job.testReceipt, err
}

func (a *application) startTest(id string) (testReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.testMu.Lock()
	defer a.testMu.Unlock()
	if a.v.Closing || a.testActive.Job != "" {
		return testReceipt{}, errors.New("The app is closing or a test is already running")
	}
	job, err := a.readTest(id)
	if err != nil {
		return testReceipt{}, err
	}
	if job.Status != "draft" {
		job, err = a.saveNewTest(job.Plan, job.Settings)
		if err != nil {
			return testReceipt{}, err
		}
	}
	job.Status, job.Headline = "running", "Preparing identical test inputs"
	if err := a.writeTest(job); err != nil {
		return testReceipt{}, err
	}
	a.testActive = job.testReceipt
	a.testDone = make(chan struct{})
	ctx, cancel := context.WithCancel(a.chat.lifetime)
	a.testCancel = cancel
	go a.runTest(ctx, job)
	return job.testReceipt, nil
}

func (a *application) sendTestProgress(job savedTest) {
	a.testMu.Lock()
	defer a.testMu.Unlock()
	a.testActive = job.testReceipt
}
