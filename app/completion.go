package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

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
	default:
		return "Assessment produced no proposals"
	}
}
