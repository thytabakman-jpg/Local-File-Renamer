package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type moveDestinationOverride struct {
	Index       *int   `json:"index"`
	Destination string `json:"destination"`
}

type approvedMoveDestination struct {
	Index int
	Move  move
}

type moveApplyFailure struct {
	Index  int
	Reason string
}

type moveApplyReceipt struct {
	Requested, Moved, Failed, Remaining int
	Failures                            []moveApplyFailure `json:",omitempty"`
}

func (a *application) bridgeApplySelection(job string, revision uint64, indices []int, resume bool, destinations []moveDestinationOverride) (any, error) {
	a.mu.Lock()
	if a.s == nil || job != a.v.Job || a.s.manifest.Operation != scanMove {
		a.mu.Unlock()
		if len(destinations) != 0 {
			return nil, errors.New("Destination overrides require the selected Move File run")
		}
		return map[string]bool{"ok": true}, a.applySelected(job, revision, indices, resume)
	}
	s, done, indices, receipt, err := a.startBridgeMoveBatchLocked(job, revision, indices, resume, destinations)
	a.mu.Unlock()
	if err != nil || done == nil {
		return receipt, err
	}
	<-done
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, index := range indices {
		outcome := s.outcomes[index]
		switch {
		case outcome.Move != nil:
			receipt.Moved++
		case outcome.Type == "single-failed" || outcome.Type == "single-error":
			receipt.Failures = append(receipt.Failures, moveApplyFailure{Index: index, Reason: outcome.Note})
		}
	}
	receipt.Failed = len(receipt.Failures)
	receipt.Remaining = receipt.Requested - receipt.Moved - receipt.Failed
	return receipt, nil
}

func (a *application) startBridgeMoveBatchLocked(job string, revision uint64, indices []int, resume bool,
	destinations []moveDestinationOverride) (*session, <-chan struct{}, []int, moveApplyReceipt, error) {
	s := a.s
	receipt := moveApplyReceipt{Requested: len(indices)}
	if revision != s.planRevision || a.v.Busy || a.v.Closing || !s.manifest.Preview ||
		s.settings().Execution.Mode == "preview_only" || s.undoCount != 0 {
		return s, nil, indices, receipt, errors.New("The Move File review changed or is unavailable for applying")
	}
	if resume {
		if a.v.Phase != "apply-paused" || len(indices) != 0 || len(destinations) != 0 || len(s.selection) == 0 {
			return s, nil, indices, receipt, errors.New("Resume takes the saved selection and destinations")
		}
		indices = append([]int(nil), s.selection...)
		receipt.Requested = len(indices)
	} else {
		if s.applyPending() || s.pending != nil || len(indices) == 0 ||
			(a.v.Phase != "preview" && a.v.Phase != "paused" && a.v.Phase != "complete") {
			return s, nil, indices, receipt, errors.New("Select at least one reviewed move when the run is idle")
		}
		overrides, failures, err := s.preflightMoveBatch(indices, destinations)
		if err != nil {
			return s, nil, indices, receipt, err
		}
		if len(failures) != 0 {
			receipt.Failed, receipt.Failures = len(failures), failures
			receipt.Remaining = receipt.Requested - receipt.Failed
			return s, nil, indices, receipt, nil
		}
		if err := s.append(record{Type: "apply-selection", Selection: indices, MoveOverrides: overrides}); err != nil {
			return s, nil, indices, receipt, err
		}
	}
	if err := a.applyLocked(job, 0, 0, nil); err != nil {
		return s, nil, indices, receipt, err
	}
	return s, a.jobDone, indices, receipt, nil
}

func (s *session) preflightMoveBatch(indices []int, destinations []moveDestinationOverride) ([]approvedMoveDestination, []moveApplyFailure, error) {
	if err := validateRoot(s.manifest.Root); err != nil {
		return nil, nil, err
	}
	selected := make(map[int]bool)
	for _, index := range indices {
		if index < s.applyThrough || index >= s.reviewEnd() || selected[index] ||
			!s.reviewable(s.plans[index]) || s.plans[index].Move == nil {
			return nil, nil, errors.New("Every approved file ID must be an available move from this revision and appear once")
		}
		selected[index] = true
	}
	requested := make(map[int]string)
	for _, override := range destinations {
		if override.Index == nil || !selected[*override.Index] || requested[*override.Index] != "" || strings.TrimSpace(override.Destination) == "" {
			return nil, nil, errors.New("Each destination override must identify one approved file and specify one folder")
		}
		requested[*override.Index] = strings.TrimSpace(override.Destination)
	}
	var overrides []approvedMoveDestination
	var failures []moveApplyFailure
	targets := make(map[string]bool)
	for _, index := range indices {
		plan := s.plans[index]
		move := *plan.Move
		destination := requested[index]
		var err error
		if destination != "" {
			move, err = s.overrideMoveDestination(move, destination)
		}
		if err == nil {
			err = s.validateMoveParents(move)
		}
		if err == nil {
			if original, err := fingerprint(move.From); err != nil || original != move.Stamp {
				failures = append(failures, moveApplyFailure{Index: index, Reason: "The original changed or became unavailable after review"})
				continue
			}
			err = nameAvailable(move.From, move.To)
		}
		key := strings.ToLower(filepath.Clean(move.To))
		if err == nil && targets[key] {
			err = errors.New("Another approved file has the same destination filename")
		}
		if err != nil {
			failures = append(failures, moveApplyFailure{Index: index, Reason: err.Error()})
			continue
		}
		targets[key] = true
		if destination != "" {
			overrides = append(overrides, approvedMoveDestination{Index: index, Move: move})
		}
	}
	return overrides, failures, nil
}

func (s *session) overrideMoveDestination(m move, destination string) (move, error) {
	if !filepath.IsAbs(destination) {
		if !filepath.IsLocal(destination) {
			return m, errors.New("Choose a folder inside the selected scan root")
		}
		destination = filepath.Join(s.manifest.Root, destination)
	}
	destination = filepath.Clean(destination)
	if !within(s.manifest.Root, destination) || samePath(filepath.Dir(m.From), destination) {
		return m, errors.New("Choose a different destination inside the selected scan root")
	}
	m.To = filepath.Join(destination, filepath.Base(m.From))
	m.TargetAncestor, m.TargetParent, m.NewFolder = "", "", false
	folder, err := fingerprint(destination)
	switch {
	case err == nil && folder.Dir:
		m.TargetParent = folder.ID
		return m, nil
	case errors.Is(err, os.ErrNotExist):
		for _, plan := range s.plans {
			if plan.Move != nil && plan.Move.NewFolder && s.reviewable(plan) &&
				samePath(filepath.Dir(plan.Move.To), destination) {
				m.NewFolder, m.TargetAncestor = true, plan.Move.TargetAncestor
				return m, nil
			}
		}
		return m, errors.New("A missing destination must be a folder proposed in the reviewed scan")
	default:
		return m, errors.New("The destination is not an available folder")
	}
}

type moveFolder struct {
	Path, Parent string
	New          bool
	Proposals    []proposalRow
}

func (s *session) moveFolders() []moveFolder {
	root := s.manifest.Root
	folders := map[string]*moveFolder{root: {Path: root}}
	for _, item := range s.manifest.Items {
		if item.Folder && !item.Skip && within(root, item.Path) && item.Path != root {
			folders[item.Path] = &moveFolder{Path: item.Path, Parent: filepath.Dir(item.Path)}
		}
	}
	for _, plan := range s.plans {
		if plan.Move == nil || !s.reviewable(plan) || s.excluded[plan.Index] {
			continue
		}
		m := plan.Move
		path := filepath.Dir(m.To)
		folder := folders[path]
		if folder == nil {
			folder = &moveFolder{Path: path, Parent: filepath.Dir(path), New: m.NewFolder}
			folders[path] = folder
		}
		if folder.New && safeParent(root, m.To) == nil {
			folder.New = false
		}
		note := plan.Note
		if failure := s.outcomes[plan.Index]; failure.Type == "single-failed" {
			note = failure.Note
		}
		folder.Proposals = append(folder.Proposals, proposalRow{
			Index: plan.Index, Revision: s.planVersions[plan.Index], From: m.From, To: m.To, Note: note, Reason: plan.Reason,
		})
	}
	paths := make([]string, 0, len(folders))
	for path := range folders {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	rows := make([]moveFolder, 0, len(paths))
	for _, path := range paths {
		row := folders[path]
		if path != root && folders[row.Parent] == nil {
			row.Parent = root
		}
		rows = append(rows, *row)
	}
	return rows
}

func (s *session) validateProposedFolder(m move) error {
	root, folder := s.manifest.Root, filepath.Dir(m.To)
	if err := safeParent(root, m.From); err != nil {
		return err
	}
	source, err := fingerprint(filepath.Dir(m.From))
	if err != nil || !source.Dir || source.ID != m.SourceParent {
		return errors.New("The source folder changed after the scan")
	}
	if err := safeParent(root, folder); err != nil {
		return err
	}
	if !validProposedName(filepath.Base(folder), "", true) {
		return errors.New("The proposed folder name is invalid")
	}
	ancestor, err := fingerprint(filepath.Dir(folder))
	if err != nil || !ancestor.Dir || ancestor.ID != m.TargetAncestor {
		return errors.New("The proposed folder's parent changed after the scan")
	}
	return nil
}

func (s *session) prepareMoveFolder(m *move) error {
	if err := s.validateProposedFolder(*m); err != nil {
		return err
	}
	folder := filepath.Dir(m.To)
	if err := os.Mkdir(folder, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	target, err := fingerprint(folder)
	if err != nil || !target.Dir {
		return errors.New("The proposed folder is unavailable or is a link")
	}
	m.TargetParent = target.ID
	return s.validateMoveParents(*m)
}
