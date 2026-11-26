package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

type chosenName struct {
	Index    int    `json:"index"`
	Revision uint64 `json:"revision"`
	Name     string `json:"name"`
}

func (a *application) apply(id string, start, end int, excluded []int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.applyLocked(id, start, end, excluded)
}

func (a *application) applyLocked(id string, start, end int, excluded []int) error {
	s := a.s
	if a.v.Busy || a.v.Closing || s == nil || id != a.v.Job || !s.manifest.Preview || s.settings().Execution.Mode == "preview_only" {
		return errors.New("No matching preview is ready to apply")
	}
	if a.v.Phase != "apply-paused" && len(s.selection) == 0 {
		if (a.v.Phase != "preview" && a.v.Phase != "paused") || s.pending != nil || s.automaticRenamePending() ||
			start != s.applyThrough || end != s.reviewEnd() || a.v.Proposed == 0 {
			return errors.New("The preview changed. Review it again before applying")
		}
		seen := make(map[int]bool)
		for _, i := range excluded {
			if i < start || i >= end || seen[i] || !s.reviewable(s.plans[i]) {
				return errors.New("The selection does not match this preview")
			}
			seen[i] = true
		}
		if len(excluded) == a.v.Proposed {
			return errors.New("Select at least one proposed change")
		}
		if err := s.append(record{Type: "apply-batch", Index: end, Excluded: excluded}); err != nil {
			return err
		}
	}
	a.begin("applying")
	go func() {
		err := validateRoot(s.manifest.Root)
		a.mu.Lock()
		if err == nil && s.pending != nil {
			err = s.finishPending()
		}
		var writeFailures []error
		for err == nil && len(s.selection) > 0 && !a.pause {
			a.v.Current = s.plans[s.selection[0]].Path
			var failure error
			failure, err = s.applyNextSelection()
			if failure != nil {
				writeFailures = append(writeFailures, failure)
			}
			a.syncSession(false)
			a.mu.Unlock()
			a.mu.Lock()
		}
		for err == nil && s.index < s.applyThrough && !a.pause {
			a.v.Current = s.manifest.Items[s.index].Path
			err = s.applyNext()
			a.syncSession(false)
			a.mu.Unlock()
			a.mu.Lock()
		}
		for err == nil && s.index == s.applyThrough && s.metadataIndex < len(s.metadataPlans) && !a.pause {
			plan := s.metadataPlans[s.metadataIndex]
			a.v.Current = s.currentPath(plan.Path, plan.Index)
			a.v.Phase = "metadata"
			err = s.metadataNext()
			a.syncSession(false)
			a.mu.Unlock()
			a.mu.Lock()
		}
		a.mu.Unlock()
		a.finish(errors.Join(err, errors.Join(writeFailures...)))
	}()
	return nil
}
func (a *application) applyOne(id string, index int) error { return a.applyOneRevision(id, index, nil) }
func (a *application) applyOneRevision(id string, index int, revision *uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if revision == nil {
		revision = &a.v.Revision
	}
	return a.applySelectionLocked(id, *revision, []int{index}, nil)
}

func (a *application) applySelected(id string, revision uint64, indexes []int, resume bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if resume {
		if a.v.Phase != "apply-paused" || revision != a.v.Revision || len(indexes) != 0 {
			return errors.New("Resume applies the saved selection and takes no new indices")
		}
		return a.applyLocked(id, 0, 0, nil)
	}
	return a.applySelectionLocked(id, revision, indexes, nil)
}

func (a *application) applySelectionLocked(id string, revision uint64, indexes []int, rowRevisions map[int]uint64) error {
	s := a.s
	if s == nil || id != a.v.Job || a.v.Busy && !a.liveReview() || a.v.Closing ||
		s.settings().Execution.Mode == "preview_only" || s.pending != nil || s.applyPending() ||
		s.undoCount != 0 || s.metadataUndone != 0 {
		return errors.New("The current run is unavailable for applying changes")
	}
	if !a.liveReview() && a.v.Phase != "preview" && a.v.Phase != "paused" && a.v.Phase != "complete" {
		return errors.New("There is no current selection to apply")
	}
	seen := make(map[int]bool, len(indexes))
	for _, i := range indexes {
		seenRevision := revision
		if rowRevisions != nil {
			seenRevision = rowRevisions[i]
		}
		if i < s.applyThrough || i >= s.reviewEnd() || !s.rowUnchanged(i, seenRevision) || seen[i] || !s.reviewable(s.plans[i]) {
			return errors.New("The selection contains a duplicate, changed or ineligible proposal")
		}
		seen[i] = true
	}
	if len(seen) == 0 {
		return errors.New("Select at least one proposal for apply")
	}
	if err := validateRoot(s.manifest.Root); err != nil {
		return err
	}
	if err := s.append(record{Type: "apply-selection", Selection: indexes}); err != nil {
		return err
	}
	if a.liveReview() || len(indexes) == 1 {
		err := s.finishSelection()
		a.syncSession(true)
		return err
	}
	return a.applyLocked(id, 0, 0, nil)
}

func (s *session) applyNextSelection() (writeFailure, journalFailure error) {
	i := s.selection[0]
	plan := s.plans[i]
	if plan.Move != nil && !s.autoCompleted[i] {
		if plan.Move.Stamp.Dir && s.folderHasUnappliedChild(plan.Path) {
			if err := s.append(record{Type: "single-failed", Index: i, Status: "Rename failed", Note: "Apply the child renames before renaming the folder"}); err != nil {
				return nil, err
			}
		} else if err := s.renamePlan(plan, singleRename); err != nil {
			return nil, err
		}
		if outcome := s.outcomes[i]; outcome.Type == "single-failed" {
			writeFailure = errors.New(outcome.Note)
		}
	}
	if plan.Metadata != nil && (plan.Move == nil || s.autoCompleted[i]) && !s.metadataApproved(i) {
		if err := s.append(record{Type: "selected-metadata", Index: i}); err != nil {
			return writeFailure, err
		}
	}
	return writeFailure, s.append(record{Type: "selection-done", Index: i})
}

func (s *session) finishSelection() error {
	var failures []error
	for len(s.selection) > 0 {
		failure, err := s.applyNextSelection()
		if err != nil {
			return err
		}
		failures = append(failures, failure)
	}
	for s.metadataIndex < len(s.metadataPlans) {
		if err := s.metadataNext(); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

func (a *application) useReviewName(id string, revision uint64, index int, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.correctLocked(id, revision, index, "", &name); err != nil {
		return err
	}
	if a.s.plans[index].Move == nil {
		return nil
	}
	return a.applySelectionLocked(id, a.v.Revision, []int{index}, nil)
}

func (a *application) applyAllIndices(id string, revision uint64, filter, folder string) ([]int, error) {
	s, err := a.editable(id, revision)
	if err != nil || s.settings().Execution.Mode == "preview_only" {
		return nil, errors.New("Review the current assessed proposals before applying all")
	}
	if filter == "folder" && (s.manifest.Operation != scanMove || !within(s.manifest.Root, folder)) {
		return nil, errors.New("Select a destination folder from the current move scan")
	}
	switch filter {
	case "all", "proposed", "flagged", "kept", "unassessed", "folder":
	default:
		return nil, errors.New("Choose a review filter with pending proposals")
	}
	indices := make([]int, 0, s.reviewCount())
	for i := s.applyThrough; i < s.reviewEnd(); i++ {
		row := s.plans[i]
		if !s.reviewable(row) || !reviewMatches(s, row, filter) {
			continue
		}
		if filter == "folder" && (row.Move == nil || !samePath(filepath.Dir(row.Move.To), folder)) {
			continue
		}
		indices = append(indices, i)
	}
	return indices, nil
}

func (a *application) applyAllReviewed(id string, revision uint64, filter, folder string, count int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	indices, err := a.applyAllIndices(id, revision, filter, folder)
	if err != nil {
		return err
	}
	if len(indices) == 0 || count != len(indices) {
		return errors.New("The batch changed. Confirm the current count before applying")
	}
	return a.applySelectionLocked(id, revision, indices, nil)
}

func (a *application) applyChosenNames(id string, names []chosenName) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(names) == 0 || a.s == nil || id != a.v.Job || a.v.PreviewOnly {
		return errors.New("Choose at least one name from the current writable review")
	}
	plans := make([]record, 0, len(names))
	indices := make([]int, 0, len(names))
	seen := make(map[int]bool, len(names))
	targets := make(map[string]bool, len(names))
	for _, entry := range names {
		if seen[entry.Index] || !a.s.rowUnchanged(entry.Index, entry.Revision) {
			return errors.New("The chosen names contain a duplicate or changed file")
		}
		seen[entry.Index] = true
		if _, _, err := a.flaggedFile(id, entry.Revision, entry.Index); err != nil {
			return err
		}
		plan, err := a.prepareCorrectionLocked(id, entry.Revision, entry.Index, "", &entry.Name)
		if err != nil {
			return fmt.Errorf("Chosen name for file %d: %w", entry.Index, err)
		}
		if plan.Move == nil {
			return errors.New("A chosen name is already in use by its file")
		}
		target := strings.ToLower(filepath.Clean(plan.Move.To))
		if targets[target] {
			return errors.New("Two chosen names use the same destination")
		}
		targets[target] = true
		plans = append(plans, plan)
		indices = append(indices, entry.Index)
	}
	for _, plan := range plans {
		if err := a.s.append(plan); err != nil {
			a.syncSession(true)
			return err
		}
	}
	a.syncSession(true)
	return a.applySelectionLocked(id, a.v.Revision, indices, nil)
}

