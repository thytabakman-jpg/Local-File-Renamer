package main

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Section: Completion receipt and outcome summaries.

type completionCount struct {
	Completed, Pending, Skipped, Failed int
}
type completionReceipt struct {
	Assessments, Renames, Metadata, Undo completionCount
	NotAssessed, UndoAvailable           int
	LastError                            string
}

func (s *session) receipt() completionReceipt {
	r := completionReceipt{LastError: s.operationError}
	r.Assessments.Pending = max(0, s.fileCount-len(s.plans))
	r.Undo.Completed = s.undoCount + s.metadataUndone
	r.UndoAvailable = len(s.moves) + len(s.repaired) - r.Undo.Completed
	if r.Undo.Completed > 0 || s.pending != nil && strings.Contains(s.pending.Type, "undo") {
		r.Undo.Pending = r.UndoAvailable
	}
	r.Undo.Failed = s.undoFailures
	for _, p := range s.plans {
		if p.Index < s.fileCount {
			switch planOutcome(p) {
			case "Analysis failed":
				r.Assessments.Failed++
			case "Not assessed":
				r.NotAssessed++
			case "Skipped":
				r.Assessments.Skipped++
			default:
				r.Assessments.Completed++
			}
		}
		result := s.outcomes[p.Index]
		switch {
		case result.Move != nil:
			r.Renames.Completed++
		case result.Type == "single-failed" || result.Type == "single-error" || strings.Contains(result.Note, "Rename failed:") || p.Move != nil && !s.excluded[p.Index] && result.Type == "result":
			r.Renames.Failed++
			if s.reviewable(p) && p.Index >= s.applyThrough && !s.autoCompleted[p.Index] {
				r.Renames.Pending++
			}
		case p.Move != nil && s.excluded[p.Index]:
			r.Renames.Skipped++
		case p.Move != nil:
			r.Renames.Pending++
		}
		if p.Metadata != nil && (p.Index >= s.applyThrough || s.excluded[p.Index]) && !s.metadataApproved(p.Index) {
			if s.excluded[p.Index] {
				r.Metadata.Skipped++
			} else {
				r.Metadata.Pending++
			}
		}
	}
	r.Metadata.Completed = len(s.repaired)
	r.Metadata.Failed = s.metadataIndex - len(s.repaired)
	r.Metadata.Pending += len(s.metadataPlans) - s.metadataIndex
	return r
}

// Section: Receipt and run outcome helpers.

func (s *session) metadataApproved(index int) bool {
	for _, plan := range s.metadataPlans {
		if plan.Index == index {
			return true
		}
	}
	return false
}

func (s *session) availableTypes() []string {
	if s.manifest.Inventory != nil {
		return s.manifest.Inventory.FileTypes
	}
	return nil
}

func (s *session) zeroChangeReason(busy bool, pending int) string {
	count := s.fileCount
	if s.manifest.Inventory != nil {
		count = s.manifest.Inventory.Included
	}
	switch {
	case count == 0:
		if s.manifest.Inventory != nil && s.manifest.Inventory.Found == 0 {
			return "No files were found inside the selected scope"
		}
		return "No files matched the saved filter"
	case s.renamed > 0 || len(s.repaired) > 0:
		return ""
	}
	receipt := s.receipt()
	switch {
	case receipt.Renames.Failed > 0:
		return "One or more file writes failed"
	case len(s.plans) < s.fileCount && busy:
		return "File assessment is in progress"
	case len(s.plans) < s.fileCount:
		return "Assessment paused or failed before all files were checked"
	case s.manifest.Operation == scanDuplicates:
		return "Duplicate Scanner leaves files unchanged"
	case pending > 0 && s.manifest.Auto:
		return "Pending proposals did not qualify for automatic apply"
	case pending > 0:
		return "Proposals await approval"
	case receipt.Assessments.Failed > 0:
		return "Assessment failed and produced no proposals"
	case s.manifest.Operation == scanMove:
		return "No eligible destination was found for the assessed files"
	default:
		return "Assessment produced no proposals"
	}
}

// Section: Context-aware content hashing.

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(buf []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(buf)
}
func comparisonHash(ctx context.Context, path string) (string, error) {
	st, err := fingerprint(path)
	if err != nil || st.Dir {
		return "", errors.New("Cannot record identity for comparison")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, contextReader{ctx, f}); err != nil {
		return "", err
	}
	end, err := fingerprint(path)
	if err != nil || st != end {
		return "", errors.New("File changed while recording its comparison identity")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Section: Historical run comparison.

type runDelta struct {
	Path                                                                                                                       string
	FirstName, SecondName, FirstClass, SecondClass, FirstDecision, SecondDecision, FirstWarning, SecondWarning, AIContribution string
}
type runComparison struct {
	First, Second               string
	Files, Changed, NextOffset  int
	FirstMS, SecondMS           int64
	FirstReceipt, SecondReceipt completionReceipt
	FirstAI, SecondAI           map[string]int
	Rows                        []runDelta
}

func (a *application) compareRuns(first, second string, offset int, showNames bool) (runComparison, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy || first == second || first == "" || second == "" {
		return runComparison{}, errors.New("Select two saved runs after the current operation finishes")
	}
	read := func(id string) (*session, error) {
		for _, job := range a.v.Jobs {
			if job.ID == id {
				return openSessionReadOnly(filepath.Join(a.home, "runs", id))
			}
		}
		return nil, errors.New("Saved run could not be found")
	}
	left, err := read(first)
	if err != nil {
		return runComparison{}, err
	}
	defer left.close()
	right, err := read(second)
	if err != nil {
		return runComparison{}, err
	}
	defer right.close()
	if left.fileCount == 0 || left.fileCount != right.fileCount || !left.settings().Scope.VerifyComparison || !right.settings().Scope.VerifyComparison {
		return runComparison{}, errors.New("Both scans need comparison verification and the same starting file set")
	}
	if left.manifest.Root == right.manifest.Root {
		return runComparison{}, errors.New("Run the scan in two separate disposable copies to compare it")
	}
	result := runComparison{First: first, Second: second, Files: left.fileCount, FirstMS: left.clock.ActiveMS, SecondMS: right.clock.ActiveMS,
		FirstReceipt: left.receipt(), SecondReceipt: right.receipt(), FirstAI: make(map[string]int), SecondAI: make(map[string]int), Rows: []runDelta{}}
	relative := func(s *session, path string) string {
		rel, _ := filepath.Rel(s.manifest.Root, path)
		return filepath.ToSlash(rel)
	}
	field := func(s *session, i int) (record, string) {
		if i >= len(s.plans) {
			return record{Decision: "Pending"}, ""
		}
		p := s.plans[i]
		proposed := relative(s, p.Path)
		if p.Move != nil {
			proposed = relative(s, p.Move.To)
		}
		return p, proposed
	}
	for i := 0; i < left.fileCount; i++ {
		l, r := left.manifest.Items[i], right.manifest.Items[i]
		path := relative(left, l.Path)
		if l.Hash == "" || l.Hash != r.Hash || path != relative(right, r.Path) {
			return runComparison{}, errors.New("The starting relative paths or contents differ")
		}
		lp, ln := field(left, i)
		rp, rn := field(right, i)
		if lp.Inspection.AIContribution != "" {
			result.FirstAI[lp.Inspection.AIContribution]++
		}
		if rp.Inspection.AIContribution != "" {
			result.SecondAI[rp.Inspection.AIContribution]++
		}
		if ln == rn && lp.Kind == rp.Kind && planOutcome(lp) == planOutcome(rp) && lp.Note == rp.Note && lp.Inspection.AIContribution == rp.Inspection.AIContribution {
			continue
		}
		if showNames && result.Changed >= offset && len(result.Rows) < 100 {
			result.Rows = append(result.Rows, runDelta{path, ln, rn, lp.Kind, rp.Kind, planOutcome(lp), planOutcome(rp), lp.Note, rp.Note, rp.Inspection.AIContribution})
		}
		result.Changed++
	}
	if offset+len(result.Rows) < result.Changed && len(result.Rows) > 0 {
		result.NextOffset = offset + len(result.Rows)
	}
	return result, nil
}

// Section: CSV report export.

func csvCell(s string) string {
	if len(s) > 0 && strings.ContainsRune("=+-@\t\r\n", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (a *application) exportInspections(w *csv.Writer, unknown bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.s == nil || a.v.Busy && !a.scanActive && a.v.Phase != "reassessing" {
		return errors.New("The inspection report is unavailable during this operation")
	}
	if err := w.Write([]string{"Current path", "Proposed path", "Text or listing", "Metadata", "Image decoded", "Local AI", "Model", "Unknown publication fields", "Read ms", "OCR ms", "Model startup ms", "Local model ms", "Rename ms", "Outcome", "Notes", "Classification", "Naming decision", "Decision reason", "Cache", "AI contribution", "Total ms", "Publication date lookup", "Publication source", "Online lookup ms", "Classification checklist"}); err != nil {
		return err
	}
	for _, r := range a.s.plans {
		if a.s.manifest.Items[r.Index].Folder || unknown && !r.Unknown {
			continue
		}
		var proposed string
		if r.Move != nil {
			proposed = r.Move.To
		}
		checked := r.Inspection
		yesNo := func(b bool) string {
			if b {
				return "Yes"
			}
			return "No"
		}
		if checked.AI == "" {
			checked.AI = "Not recorded"
		}
		outcome, note, renameMS := planOutcome(r), r.Note, int64(0)
		if result, ok := a.s.outcomes[r.Index]; ok {
			outcome, note, renameMS = result.Status, result.Note, result.RenameMS
		}
		if err := w.Write([]string{csvCell(a.s.currentPath(r.Path, r.Index)), csvCell(proposed),
			yesNo(checked.Text), yesNo(checked.Metadata), yesNo(checked.Image), checked.AI, checked.Model, yesNo(r.Unknown),
			strconv.FormatInt(checked.ReadMS, 10), strconv.FormatInt(checked.OCRMS, 10), strconv.FormatInt(checked.StartupMS, 10), strconv.FormatInt(checked.ModelMS, 10),
			strconv.FormatInt(renameMS, 10), csvCell(outcome), csvCell(note), csvCell(r.Kind), csvCell(planOutcome(r)), csvCell(r.Reason), csvCell(checked.Cache), csvCell(checked.AIContribution), strconv.FormatInt(checked.TotalMS, 10), csvCell(checked.PublicationDate), csvCell(checked.PublicationSource), strconv.FormatInt(checked.OnlineMS, 10), csvCell(checklistText(checked.Checklist))}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func (a *application) export(w *csv.Writer) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy {
		return errors.New("Pause the run before exporting flags")
	}
	if a.s == nil {
		return errors.New("There is no run to export")
	}
	if err := w.Write([]string{"Current path", "Action", "Reason"}); err != nil {
		return err
	}
	for _, r := range a.s.flagRows() {
		status := r.Status
		if r.Type == "plan" {
			status = "Review"
		}
		if err := w.Write([]string{csvCell(a.s.currentPath(r.Path, r.Index)), status, csvCell(r.Note)}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// Section: Completion and review reporting.

type batchReport struct {
	Job                                         string
	Phase                                       string
	Revision                                    uint64
	Total, Assessed, Drafts, Cached             int
	Decisions, Classifications                  map[string]int
	AI                                          map[string]int
	ReadMS, OCRMS, StartupMS, ModelMS, RenameMS int64
	PromptMS, GenerationMS                      int64
	Clock                                       scanClock
	AIContribution                              map[string]int
	Receipt                                     completionReceipt
}

func (a *application) report(id string) (batchReport, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if s == nil || id != "" && id != a.v.Job || a.v.Busy && !a.scanActive && a.v.Phase != "reassessing" {
		return batchReport{}, errors.New("No matching live or saved batch report is available")
	}
	report := batchReport{
		Job: a.v.Job, Phase: a.v.Phase, Revision: s.planRevision, Total: s.fileCount,
		Drafts: len(a.cleanup) - a.cleanupThrough, Clock: a.v.Clock.at(time.Now()),
		Decisions: make(map[string]int), Classifications: make(map[string]int), AI: make(map[string]int), AIContribution: make(map[string]int), Receipt: s.receipt(),
	}
	for _, plan := range s.plans[:min(s.fileCount, len(s.plans))] {
		report.Assessed++
		report.Decisions[planOutcome(plan)]++
		if plan.Kind != "" {
			report.Classifications[plan.Kind]++
		}
		check := plan.Inspection
		if check.AI != "" {
			report.AI[check.AI]++
		}
		if check.AIContribution != "" {
			report.AIContribution[check.AIContribution]++
		}
		if check.Cache != "" {
			report.Cached++
		}
		report.ReadMS += check.ReadMS
		report.OCRMS += check.OCRMS
		report.StartupMS += check.StartupMS
		report.ModelMS += check.ModelMS
		report.PromptMS += check.PromptMS
		report.GenerationMS += check.GenerationMS
		report.RenameMS += s.outcomes[plan.Index].RenameMS
	}
	return report, nil
}
