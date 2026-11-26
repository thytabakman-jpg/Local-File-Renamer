package main

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

var classifications = []string{"article", "academic_book", "book", "course_paper", "presentation", "other"}

func (a *application) liveReview() bool {
	return a.scanActive && (a.v.Phase == "files" || a.v.Phase == "folders")
}

func (s *session) rowUnchanged(index int, revision uint64) bool {
	return index >= 0 && index < len(s.planVersions) && revision >= s.planVersions[index] && revision <= s.planRevision
}

func (a *application) editable(id string, revision uint64) (*session, error) {
	s := a.s
	if s == nil || id != a.v.Job || revision != a.v.Revision || a.v.Busy && !a.liveReview() || a.v.Closing ||
		s.manifest.Operation == scanDuplicates || !s.manifest.Preview || s.pending != nil || s.applyPending() || s.automaticRenamePending() ||
		s.undoCount != 0 || s.metadataUndone != 0 {
		return nil, errors.New("Review the current run and its revision before changing a proposal")
	}
	return s, nil
}

func (s *session) checkEditable(index int) (record, error) {
	if index < s.applyThrough || index >= len(s.plans) || s.autoCompleted[index] {
		return record{}, errors.New("The item is not an editable, assessed proposal")
	}
	plan := s.plans[index]
	if plan.Decision == removedFlaggedFile {
		return record{}, errors.New("The file was removed from the scan")
	}
	if s.manifest.Items[index].Skip {
		return record{}, errors.New("The scan skipped this item")
	}
	return plan, nil
}

func (a *application) correct(id string, revision uint64, index int, kind string, name *string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.correctLocked(id, revision, index, kind, name)
}

func (a *application) correctLocked(id string, revision uint64, index int, kind string, name *string) error {
	plan, err := a.prepareCorrectionLocked(id, revision, index, kind, name)
	if err != nil {
		return err
	}
	if err := a.s.append(plan); err != nil {
		return err
	}
	a.syncSession(true)
	return nil
}

func (a *application) prepareCorrectionLocked(id string, revision uint64, index int, kind string, name *string) (record, error) {
	if kind == "" && name == nil || kind != "" && !slices.Contains(classifications, kind) {
		return record{}, errors.New("Supply a supported classification or a proposed filename")
	}
	if a.s != nil && a.s.rowUnchanged(index, revision) {
		revision = a.v.Revision
	}
	s, err := a.editable(id, revision)
	if err != nil {
		return record{}, err
	}
	if s.manifest.Operation == scanMove {
		return record{}, errors.New("Use File Renamer to change filenames or classifications")
	}
	if name != nil && kind == "" && s.canRefine(index) {
		plan := s.plans[index]
		path := s.currentPath(plan.Path, index)
		if err := safeParent(s.manifest.Root, path); err != nil {
			return record{}, err
		}
		st, err := fingerprint(path)
		if err != nil || st != plan.Stamp {
			return record{}, errors.New("The image changed since the scan")
		}
		old := filepath.Base(path)
		if *name == old || !validProposedName(*name, old, false) || windowsNameLength(*name) > s.namingRules().MaxLength {
			return record{}, errors.New("Supply a different valid filename with the original extension")
		}
		target := filepath.Join(filepath.Dir(path), *name)
		if err := s.availableForPlan(path, target); err != nil {
			return record{}, err
		}
		plan.Type, plan.Decision, plan.Reason = "image-refinement", "Rename proposed", "Selected a more specific image name"
		plan.Move, plan.Auto, plan.AutoValidated, plan.Rough = &move{From: path, To: target, Stamp: st}, false, false, false
		return plan, nil
	}
	prior, err := s.checkEditable(index)
	if err != nil {
		return record{}, err
	}
	item := s.manifest.Items[index]
	if item.Folder && kind != "" {
		return record{}, errors.New("Only files have a classification")
	}
	if err := safeParent(s.manifest.Root, prior.Path); err != nil {
		return record{}, err
	}
	st, err := fingerprint(prior.Path)
	if err != nil || st.Dir != item.Folder || prior.Stamp.ID != "" && st != prior.Stamp {
		return record{}, errors.New("The original changed since inspection. Reassess it first.")
	}
	if name == nil {
		prior.Move, prior.Metadata, prior.Auto = nil, nil, false
		prior.Unknown = true
		prior.Note, prior.Decision = "Classification corrected. Reassess to generate a proposal under that rule.", "Needs review"
	} else {
		old := filepath.Base(prior.Path)
		to := *name
		if !validProposedName(to, old, item.Folder) || windowsNameLength(to) > s.namingRules().MaxLength {
			return record{}, errors.New("Supply a valid Windows filename with the original extension")
		}
		d := decision{Name: to, Outcome: "Rename proposed", Reason: "Proposed filename corrected by the operator", Inspection: prior.Inspection, Metadata: prior.Metadata, Unknown: prior.Unknown}
		if to == old {
			d.Outcome, d.Reason = "Name kept by operator", "The operator selected the original filename"
			d.Metadata, d.Unknown = nil, false
		}
		if prior.Move != nil {
			d.Folder = filepath.Dir(prior.Move.To)
		}
		plan := buildSuggestion(s, index, d, st, true)
		if d.Folder != "" && d.Folder != filepath.Dir(prior.Path) && (plan.Move == nil || filepath.Dir(plan.Move.To) != d.Folder) {
			return record{}, errors.New(plan.Note)
		}
		if to != old && plan.Move == nil {
			return record{}, errors.New(plan.Note)
		}
		plan.Kind, plan.OverrideKind = prior.Kind, prior.OverrideKind
		prior = plan
	}
	prior.Type, prior.Reason = "plan-revision", "The operator corrected the assessment"
	if kind != "" {
		prior.Kind, prior.OverrideKind = kind, kind
	}
	return prior, nil
}

func (a *application) reassess(id string, revision uint64, indexes []int, group, value string, refresh bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy {
		return errors.New("Pause the scan before reassessing files")
	}
	s, err := a.editable(id, revision)
	if err != nil {
		return err
	}
	if len(indexes) > 0 && group != "" {
		return errors.New("Select indices or one group")
	}
	if group != "" {
		if !slices.Contains([]string{"classification", "extension", "decision"}, group) || value == "" {
			return errors.New("Choose a classification, extension or decision group and its value")
		}
		for i := s.applyThrough; i < len(s.plans); i++ {
			p := s.plans[i]
			field := p.Kind
			switch group {
			case "extension":
				field = extension(p.Path)
			case "decision":
				field = planOutcome(p)
			}
			if strings.EqualFold(field, value) && p.Decision != removedFlaggedFile && !s.autoCompleted[i] && !s.manifest.Items[i].Folder && !s.manifest.Items[i].Skip {
				indexes = append(indexes, i)
			}
		}
	}
	if len(indexes) == 0 || len(indexes) > 200 {
		return errors.New("Select between 1 and 200 assessed files per reassessment")
	}
	seen := make(map[int]bool, len(indexes))
	for _, i := range indexes {
		if _, err := s.checkEditable(i); err != nil || seen[i] || s.manifest.Items[i].Folder {
			return errors.New("The selection contains a duplicate or ineligible file")
		}
		seen[i] = true
	}
	opts := a.activeSettings()
	if s.manifest.Operation == scanMove {
		opts.Evidence.OnlineDates = "off"
	}
	partial := s.partial || opts.AI.Model == "off" || opts.AI.Invoke == "never"
	if !partial && !modelReadyFor(a.modelDir, opts.AI.Model) {
		return errors.New("The configured local model is unavailable")
	}
	ctx := a.begin("reassessing")
	go func() {
		engine := scanModel{app: a, session: s, settings: opts}
		assess, transcribe := engine.handlers(partial)
		var failure error
		for _, i := range indexes {
			if ctx.Err() != nil {
				break
			}
			a.mu.Lock()
			prior := s.plans[i]
			a.v.Current = prior.Path
			a.mu.Unlock()
			d, st, err := inspectFile(ctx, s, assess, transcribe, i, prior.OverrideKind, refresh, opts)
			engine.timings(&d.Inspection, partial, i)
			if err != nil {
				failure = err
				break
			}
			a.mu.Lock()
			plan := buildSuggestion(s, i, d, st, true)
			plan.OverrideKind = prior.OverrideKind
			failure = s.append(plan)
			a.syncSession(false)
			a.mu.Unlock()
			if failure != nil {
				break
			}
		}
		engine.end()
		a.finish(failure)
	}()
	return nil
}

func (a *application) undo() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.undoLocked()
}

func (a *application) undoLocked() error {
	if a.v.Busy || a.v.Closing || a.s == nil || a.v.UndoLeft == 0 && !a.v.Pending {
		return errors.New("There are no changes to undo")
	}
	s := a.s
	if len(s.selection) > 0 {
		return errors.New("Resume applying the approved selection before undoing changes")
	}
	a.begin("undo")
	go func() {
		err := validateRoot(s.manifest.Root)
		a.mu.Lock()
		if err == nil && s.pending != nil {
			err = s.finishPending()
		}
		for err == nil && (len(s.moves) > s.undoCount || len(s.repaired) > s.metadataUndone) && !a.pause {
			err = s.undoNext()
			a.syncSession(false)
			a.mu.Unlock()
			a.mu.Lock()
		}
		a.mu.Unlock()
		a.finish(err)
	}()
	return nil
}

func (a *application) undoJob(id string) error { return a.undoJobRevision(id, nil) }
func (a *application) undoJobRevision(id string, revision *uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id == "" || id != a.v.Job || revision != nil && *revision != a.v.Revision {
		return errors.New("Select the run to undo")
	}
	return a.undoLocked()
}

