package main

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

type batchReport struct {
	Job                                         string
	Phase                                       string
	Revision                                    uint64
	Total, Assessed, Drafts, Cached             int
	Decisions, Classifications                  map[string]int
	AI                                          map[string]int
	ReadMS, OCRMS, StartupMS, ModelMS, RenameMS int64
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
		Drafts: len(a.cleanup) - a.cleanupThrough, Clock: a.v.Clock,
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
		report.RenameMS += s.outcomes[plan.Index].RenameMS
	}
	return report, nil
}

var classifications = []string{"article", "academic_book", "book", "course_paper", "presentation", "other"}

func (a *application) editable(id string, revision uint64) (*session, error) {
	s := a.s
	if s == nil || id != a.v.Job || revision != a.v.Revision || a.v.Busy || a.v.Closing ||
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
	if kind == "" && name == nil || kind != "" && !slices.Contains(classifications, kind) {
		return errors.New("Supply a supported classification or a proposed filename")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.editable(id, revision)
	if err != nil {
		return err
	}
	if s.manifest.Operation == scanMove {
		return errors.New("Use File Renamer to change filenames or classifications")
	}
	prior, err := s.checkEditable(index)
	if err != nil {
		return err
	}
	item := s.manifest.Items[index]
	if item.Folder && kind != "" {
		return errors.New("Only files have a classification")
	}
	if err := safeParent(s.manifest.Root, prior.Path); err != nil {
		return err
	}
	st, err := fingerprint(prior.Path)
	if err != nil || st.Dir != item.Folder || prior.Stamp.ID != "" && st != prior.Stamp {
		return errors.New("The original changed since inspection. Reassess it first.")
	}
	if name == nil {
		prior.Move, prior.Metadata, prior.Auto = nil, nil, false
		prior.Unknown = true
		prior.Note, prior.Decision = "Classification corrected. Reassess to generate a proposal under that rule.", "Needs review"
	} else {
		old := filepath.Base(prior.Path)
		to := *name
		if !validProposedName(to, old, item.Folder) || windowsNameLength(to) > s.namingRules().MaxLength {
			return errors.New("Supply a valid Windows filename with the original extension")
		}
		d := decision{Name: to, Outcome: "Rename proposed", Reason: "Proposed filename corrected by the operator", Inspection: prior.Inspection, Metadata: prior.Metadata, Unknown: prior.Unknown}
		if to == old {
			d.Outcome, d.Reason = "Name kept by operator", "The operator selected the original filename"
		}
		if prior.Move != nil {
			d.Folder = filepath.Dir(prior.Move.To)
		}
		plan := buildSuggestion(s, index, d, st, true)
		if d.Folder != "" && d.Folder != filepath.Dir(prior.Path) && (plan.Move == nil || filepath.Dir(plan.Move.To) != d.Folder) {
			return errors.New(plan.Note)
		}
		if to != old && plan.Move == nil {
			return errors.New(plan.Note)
		}
		plan.Kind, plan.OverrideKind = prior.Kind, prior.OverrideKind
		prior = plan
	}
	prior.Type, prior.Reason = "plan-revision", "The operator corrected the assessment"
	if kind != "" {
		prior.Kind, prior.OverrideKind = kind, kind
	}
	if err := s.append(prior); err != nil {
		return err
	}
	a.syncSession(true)
	return nil
}

func (a *application) reassess(id string, revision uint64, indexes []int, group, value string, refresh bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
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
			engine.timings(&d.Inspection, partial)
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

func (a *application) applySelected(id string, revision uint64, indexes []int, resume bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if s == nil || id != a.v.Job || revision != a.v.Revision || a.v.Busy || a.v.Closing {
		return errors.New("Read the current proposals and revision again before applying")
	}
	if resume {
		if a.v.Phase != "apply-paused" || len(indexes) != 0 {
			return errors.New("Resume applies the saved selection and takes no new indices")
		}
		return a.applyLocked(id, 0, 0, nil)
	}
	if a.v.Phase != "preview" && a.v.Phase != "paused" {
		return errors.New("There is no current selection to apply")
	}
	seen := make(map[int]bool, len(indexes))
	for _, i := range indexes {
		if i < s.applyThrough || i >= s.reviewEnd() || seen[i] || !s.reviewable(s.plans[i]) {
			return errors.New("The selection contains a duplicate or ineligible proposal")
		}
		seen[i] = true
	}
	if len(seen) == 0 {
		return errors.New("Select at least one proposal for apply")
	}
	if s.manifest.Operation == scanMove {
		return a.applyMoveSelection(s, indexes)
	}
	var excluded []int
	for _, plan := range s.plans[s.applyThrough:s.reviewEnd()] {
		if s.reviewable(plan) && !seen[plan.Index] {
			excluded = append(excluded, plan.Index)
		}
	}
	return a.applyLocked(id, s.applyThrough, s.reviewEnd(), excluded)
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

func (a *application) changeLocation(id string, revision uint64, index int, destination string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.editable(id, revision)
	if err != nil {
		return err
	}
	prior, err := s.checkEditable(index)
	if err != nil {
		return err
	}
	if s.manifest.Items[index].Folder || s.settings().Execution.Mode == "preview_only" {
		return errors.New("This run does not allow editing file destinations")
	}
	if !s.manifest.SortFolders && prior.Move == nil {
		return errors.New("Enable folder moves when starting the scan")
	}
	parent := filepath.Dir(prior.Path)
	destination = strings.TrimSpace(destination)
	if destination == "" {
		destination = parent
	}
	if !filepath.IsAbs(destination) {
		return errors.New("Choose an existing folder inside the scan folder")
	}
	destination = filepath.Clean(destination)
	if !within(s.manifest.Root, destination) {
		return errors.New("Choose a destination inside the selected folder")
	}
	if destination != parent && !s.manifest.SortFolders {
		return errors.New("Enable folder moves when starting the scan")
	}
	if err := safeParent(s.manifest.Root, prior.Path); err != nil {
		return err
	}
	st, err := fingerprint(prior.Path)
	if err != nil || st.Dir || st != prior.Stamp {
		return errors.New("The original changed since inspection. Reassess it first.")
	}
	name := filepath.Base(prior.Path)
	if prior.Move != nil {
		name = filepath.Base(prior.Move.To)
	}
	reason := "The operator selected the destination folder"
	if destination == parent {
		reason = "The operator kept the file in its current folder"
	}
	d := decision{Name: name, Folder: destination, Metadata: prior.Metadata, Note: prior.Note, Kind: prior.Kind,
		Inspection: prior.Inspection, Unknown: prior.Unknown, Outcome: "Rename proposed", Reason: reason}
	if destination == parent && name == filepath.Base(prior.Path) {
		d.Outcome = "Name kept by operator"
	}
	plan := buildSuggestion(s, index, d, st, true)
	if (destination != parent || name != filepath.Base(prior.Path)) &&
		(plan.Move == nil || filepath.Dir(plan.Move.To) != destination) {
		return errors.New(plan.Note)
	}
	plan.OverrideKind = prior.OverrideKind
	if err := s.append(plan); err != nil {
		return err
	}
	a.syncSession(true)
	return nil
}
