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

