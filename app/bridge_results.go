package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
)

var bridgeResultFilters = []string{"all", "folders", "proposed_moves", "failed_moves", "kept_locations", "exact_duplicates", "verified_duplicates", "possible_duplicates", "errors"}

func (a *application) bridgeRunLocked(id string, readOnly bool) (*session, bool, error) {
	if a.s != nil && (id == "" || id == a.v.Job) {
		return a.s, false, nil
	}
	for _, job := range a.v.Jobs {
		if job.ID == id {
			path := filepath.Join(a.home, "runs", id)
			if readOnly {
				s, err := openSessionReadOnly(path)
				return s, true, err
			}
			s, err := openSession(path)
			return s, true, err
		}
	}
	return nil, false, errors.New("No saved run with that ID")
}

func (a *application) bridgeStatus(id string) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.v
	if (id == "" || id == v.StartJob) && v.StartJob != "" && (v.StartingRoot != "" || v.StartError != "") {
		out := map[string]any{"Job": v.StartJob, "Phase": "indexing", "Mode": v.StartingMode, "Busy": v.Busy}
		if v.StartError != "" {
			out["Phase"], out["Reason"] = "start-failed", "The scan could not start. See local details."
		}
		return out, nil
	}
	if id == "" && a.s == nil {
		return map[string]any{"Phase": v.Phase, "Busy": v.Busy}, nil
	}
	s, owned, err := a.bridgeRunLocked(id, true)
	if err != nil {
		return nil, err
	}
	phase := v.Phase
	busy := !owned && v.Busy
	if owned {
		defer s.close()
		switch {
		case s.applyPending():
			phase = "apply-paused"
		case len(s.plans) < len(s.manifest.Items):
			phase = "paused"
		case s.reviewCount() > 0:
			phase = "preview"
		default:
			phase = "complete"
		}
	}
	receipt := s.receipt()
	pending := s.reviewCount()
	matched := s.fileCount
	if s.manifest.Inventory != nil {
		matched = s.manifest.Inventory.Included
	}
	failed := receipt.Renames.Failed + receipt.Assessments.Failed
	if s.operationError != "" {
		failed++
	}
	operation := s.manifest.Operation
	if operation == "" {
		operation = scanRename
	}
	out := map[string]any{
		"Job": filepath.Base(s.dir), "Phase": phase, "Operation": operation, "Busy": busy, "Revision": s.planRevision,
		"Mode": s.settings().Execution.Mode, "Partial": s.partial, "Matched": matched, "Assessed": s.filesAssessed,
		"Proposed": pending + receipt.Renames.Completed, "Pending": pending, "Applied": receipt.Renames.Completed,
		"Skipped": s.filesSkipped, "Failed": failed, "Reason": s.zeroChangeReason(busy, pending),
	}
	switch operation {
	case scanDuplicates:
		for _, key := range []string{"Proposed", "Pending", "Applied"} {
			delete(out, key)
		}
		out["FilesChecked"] = s.filesAssessed
		if s.recycled != 0 {
			delete(out, "Reason")
		}
		out["ExactGroups"], out["ExtraCopies"], out["VerifiedGroups"], out["PossibleGroups"] = 0, 0, 0, 0
		out["RecycledCopies"], out["Errors"] = s.recycled, failed+s.filesSkipped+len(s.recycleErrors)
		if !busy {
			results := s.duplicateResults()
			_, plan := s.exactBatch(results.Exact)
			out["ExactGroups"], out["ExtraCopies"] = plan.Groups, plan.Copies
			out["VerifiedGroups"], out["PossibleGroups"] = len(results.Verified), results.Unresolved
			if len(s.plans) == s.fileCount && results.CanRecycle && plan.Copies != 0 && s.pending == nil {
				out["ExactBatch"] = plan
			}
		}
	case scanMove:
		out["LocationsKept"] = s.filesKept
		out["MovesProposed"], out["MovesApplied"] = pending+receipt.Renames.Completed, receipt.Renames.Completed
		out["FailedMoves"] = s.failedMoveCount()
	case scanRename:
		out["Verified"] = s.filesVerified
		out["RenamesProposed"], out["RenamesApplied"] = pending+receipt.Renames.Completed, receipt.Renames.Completed
	}
	return out, nil
}

type bridgeResultRow struct {
	Type                                                             string
	Index                                                            *int   `json:",omitempty"`
	Group                                                            string `json:",omitempty"`
	RecommendedKeeper                                                *int   `json:",omitempty"`
	Path, Destination, Parent, Reason, Status, Finding, KeeperReason string `json:",omitempty"`
	New                                                              bool   `json:",omitempty"`
}

type bridgeResultPage struct {
	Job, Filter string
	Revision    uint64
	Total       int
	NextOffset  int `json:",omitempty"`
	Rows        []bridgeResultRow
}

func (a *application) bridgeResults(cmd bridgeCommand) (bridgeResultPage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var page bridgeResultPage
	if cmd.Job == "" || cmd.Offset < 0 || cmd.Limit < 0 || cmd.Limit > 100 {
		return page, errors.New("Provide a job ID, a nonnegative offset, and a page size up to 100")
	}
	s, owned, err := a.bridgeRunLocked(cmd.Job, true)
	if err != nil {
		return page, err
	}
	if owned {
		defer s.close()
	}
	if !owned && a.v.Busy {
		return page, errors.New("Wait for the move or duplicate scan to finish or pause")
	}
	if cmd.Offset > 0 && cmd.Revision == nil || cmd.Revision != nil && *cmd.Revision != s.planRevision {
		return page, errors.New("The results revision changed or is missing. Restart paging from the first page")
	}
	filter := cmd.Filter
	if filter == "" {
		filter = "all"
	}
	limit := cmd.Limit
	if limit == 0 {
		limit = 50
	}
	page = bridgeResultPage{Job: cmd.Job, Filter: filter, Revision: s.planRevision, Rows: []bridgeResultRow{}}
	bytes := 0
	var large bool
	add := func(row bridgeResultRow) {
		if page.Total >= cmd.Offset && len(page.Rows) < limit && !large {
			if !cmd.IncludeNames {
				row.Path, row.Destination, row.Parent, row.Reason = "", "", "", ""
			}
			data, err := json.Marshal(row)
			if err != nil || bytes+len(data)+1 > bridgeResponseLimit/2 {
				large = true
			} else {
				bytes += len(data) + 1
				page.Rows = append(page.Rows, row)
			}
		}
		page.Total++
	}
	switch s.manifest.Operation {
	case scanMove:
		switch filter {
		case "all", "folders", "proposed_moves":
			for _, folder := range s.moveFolders() {
				if filter != "proposed_moves" {
					add(bridgeResultRow{Type: "folder", Path: folder.Path, Parent: folder.Parent, New: folder.New})
				}
				if filter != "folders" {
					for _, p := range folder.Proposals {
						add(bridgeResultRow{Type: "proposed_move", Index: &p.Index, Path: p.From, Destination: p.To, Reason: p.Reason, New: folder.New})
					}
				}
			}
		case "failed_moves", "kept_locations", "errors":
		default:
			return page, errors.New("Use a Move result filter")
		}
		if filter == "kept_locations" {
			for _, plan := range s.plans {
				if plan.Decision == "Location kept" && s.moveFailure(plan) == "" {
					reason := plan.Reason
					if reason == "" {
						reason = "The scan kept the file in its current folder"
					}
					add(bridgeResultRow{Type: "kept_location", Index: &plan.Index, Path: plan.Path, Reason: reason})
				}
			}
		}
		if filter == "all" || filter == "failed_moves" || filter == "errors" {
			for _, plan := range s.plans {
				if note := s.moveFailure(plan); note != "" {
					var destination string
					if plan.Move != nil {
						destination = plan.Move.To
					}
					add(bridgeResultRow{Type: "failed_move", Index: &plan.Index, Path: plan.Path, Destination: destination, Reason: note})
				}
			}
		}
	case scanDuplicates:
		switch filter {
		case "all", "exact_duplicates", "verified_duplicates", "possible_duplicates", "errors":
		default:
			return page, errors.New("Use a Duplicate Check result filter")
		}
		if filter != "errors" {
			results := s.duplicateResultsWithCandidates(filter == "all" || filter == "possible_duplicates")
			for _, set := range []struct {
				name   string
				groups []duplicateGroup
			}{{"exact_duplicates", results.Exact}, {"verified_duplicates", results.Verified}, {"possible_duplicates", results.possible}} {
				if filter != "all" && filter != set.name {
					continue
				}
				for i, group := range set.groups {
					id := set.name + ":" + strconv.Itoa(i)
					for _, member := range group.Members {
						row := bridgeResultRow{Type: set.name, Group: id, Index: &member.Index, Path: member.Path, Finding: group.Finding}
						if set.name != "possible_duplicates" {
							row.RecommendedKeeper = &group.RecommendedKeeper
							row.KeeperReason = group.KeeperReason
						}
						add(row)
					}
				}
			}
		}
		if filter == "all" || filter == "errors" {
			for _, plan := range s.plans {
				if plan.Decision == "Skipped" {
					add(bridgeResultRow{Type: "file_error", Index: &plan.Index, Path: plan.Path, Status: "Skipped", Reason: plan.Note})
				} else if s.recycleErrors[plan.Index] {
					add(bridgeResultRow{Type: "file_error", Index: &plan.Index, Path: plan.Path, Status: "Recycle failed", Reason: "The original remained at its scanned path"})
				}
			}
		}
	default:
		return page, errors.New("Use filename proposals for Rename results")
	}
	if large && len(page.Rows) == 0 {
		return page, errors.New("A result is too large. Request the page without paths")
	}
	if count := cmd.Offset + len(page.Rows); count < page.Total {
		page.NextOffset = count
	}
	return page, nil
}

func (s *session) moveFailure(plan record) string {
	outcome := s.outcomes[plan.Index]
	switch {
	case outcome.Type == "single-error" || outcome.Type == "single-failed" ||
		outcome.Type == "result" && (strings.Contains(outcome.Note, "Rename failed:") || plan.Move != nil && outcome.Move == nil && !s.excluded[plan.Index]):
		if outcome.Note != "" {
			return outcome.Note
		}
		return outcome.Status
	case plan.Decision == "Location kept" && plan.Note != "":
		return plan.Note
	default:
		return ""
	}
}

func (s *session) failedMoveCount() int {
	count := 0
	for _, plan := range s.plans {
		if s.moveFailure(plan) != "" {
			count++
		}
	}
	return count
}
