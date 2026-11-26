package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type exactRecycleBatch struct {
	ID             string
	Revision       uint64
	Groups         []duplicateSelection
	approvedCopies map[int]bool
}

func (b *exactRecycleBatch) includes(index int) bool {
	return b.approvedCopies[index]
}

type exactBatchSummary struct {
	PlanID         string
	Revision       uint64
	Groups, Copies int
}

type exactBatchReceipt struct {
	PlanID                                                            string
	Requested, PreviouslyRecycled, NewlyRecycled, Recycled, Remaining int
	Error                                                             string `json:",omitempty"`
}

func exactBatchID(job string, revision uint64, groups []duplicateSelection) string {
	payload, _ := json.Marshal(struct {
		Job      string
		Revision uint64
		Groups   []duplicateSelection
	}{job, revision, groups})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:16])
}

func (s *session) exactBatch(groups []duplicateGroup) (exactRecycleBatch, exactBatchSummary) {
	batch := exactRecycleBatch{Revision: s.planRevision}
	summary := exactBatchSummary{Revision: batch.Revision}
	for _, group := range groups {
		selection := duplicateSelection{Keep: group.RecommendedKeeper}
		for _, member := range group.Members {
			if member.Index != selection.Keep {
				selection.Indices = append(selection.Indices, member.Index)
			}
		}
		summary.Copies += len(selection.Indices)
		batch.Groups = append(batch.Groups, selection)
	}
	summary.Groups = len(batch.Groups)
	if summary.Groups > 0 {
		batch.ID = exactBatchID(filepath.Base(s.dir), batch.Revision, batch.Groups)
		summary.PlanID = batch.ID
	}
	return batch, summary
}

func (a *application) applyExactBatch(ctx context.Context, job, id string, revision uint64, recycle func(string) error) (exactBatchReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy || a.v.Closing || job == "" || id == "" {
		return exactBatchReceipt{}, errors.New("Choose the exact duplicate plan from a saved run when the app is idle")
	}
	s, owned, err := a.bridgeRunLocked(job, false)
	if err != nil {
		return exactBatchReceipt{}, err
	}
	if owned {
		defer s.close()
	} else {
		defer a.syncSession(true)
	}
	if s.manifest.Operation != scanDuplicates || s.settings().Execution.Mode == "preview_only" || len(s.plans) != s.fileCount {
		return exactBatchReceipt{}, errors.New("Exact duplicate approval requires a completed duplicate scan in Review mode")
	}
	batch := s.exactBatches[id]
	if batch == nil {
		if s.pending != nil || revision != s.planRevision {
			return exactBatchReceipt{}, errors.New("The run changed. Review its current exact duplicate plan before approving")
		}
		proposed, _ := s.exactBatch(s.duplicateResults().Exact)
		if proposed.ID == "" || id != proposed.ID {
			return exactBatchReceipt{}, errors.New("The exact duplicate plan does not match this run and revision")
		}
		batch = &proposed
		if err := s.append(record{Type: "exact-batch-approved", ExactBatch: batch}); err != nil {
			return exactBatchReceipt{}, err
		}
	} else if batch.Revision != revision {
		return exactBatchReceipt{}, errors.New("Retry with the original approved revision")
	}

	receipt := exactBatchReceipt{PlanID: id}
	for _, group := range batch.Groups {
		receipt.Requested += len(group.Indices)
	}
	completed := s.batchRecycled[id]
	receipt.PreviouslyRecycled = len(completed)
	if s.pending != nil {
		if s.pending.Type != "recycle-intent" || s.pending.BatchID != id {
			return receipt, errors.New("Resolve the other pending file operation before retrying the exact duplicate plan")
		}
		if err := s.finishRecycle(); err != nil {
			return receipt, err
		}
		receipt.PreviouslyRecycled = len(completed)
	}

	var remaining []duplicateSelection
	for _, group := range batch.Groups {
		next := duplicateSelection{Keep: group.Keep}
		for _, index := range group.Indices {
			if !completed[index] {
				next.Indices = append(next.Indices, index)
			}
		}
		if len(next.Indices) != 0 {
			remaining = append(remaining, next)
		}
	}
	if len(remaining) != 0 {
		_, err = s.recycleCopies(ctx, remaining, false, id, recycle)
		if err != nil {
			receipt.Error = err.Error()
		}
	}
	receipt.Recycled = len(completed)
	receipt.NewlyRecycled = receipt.Recycled - receipt.PreviouslyRecycled
	receipt.Remaining = receipt.Requested - receipt.Recycled
	return receipt, nil
}

// Section: Recycling approval and receipt contracts.

type duplicateSelection struct {
	Keep    int
	Indices []int
}

type duplicateRecycleOutcome struct {
	Kept      string
	Requested int
	Recycled  int
}

type duplicateRecycleReceipt struct {
	Groups   []duplicateRecycleOutcome
	Recycled int
}

// Section: Approved duplicate recycling.

func (a *application) recycleMatchedCopies(ctx context.Context, job string, revision uint64, keep int, selected []int, recycle func(string) error) (int, error) {
	receipt, err := a.recycleDuplicateGroups(ctx, job, revision, []duplicateSelection{{Keep: keep, Indices: selected}}, recycle)
	return receipt.Recycled, err
}

func (a *application) recycleDuplicateGroups(ctx context.Context, job string, revision uint64, groups []duplicateSelection, recycle func(string) error) (duplicateRecycleReceipt, error) {
	return a.recycleApprovedCopies(ctx, job, revision, groups, false, recycle)
}

func (a *application) recycleApprovedCopies(ctx context.Context, job string, revision uint64, groups []duplicateSelection, equivalent bool, recycle func(string) error) (duplicateRecycleReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if s == nil || a.v.Job != job || a.v.Revision != revision || a.v.Busy || a.v.Closing || s.pending != nil || s.manifest.Operation != scanDuplicates || s.settings().Execution.Mode == "preview_only" || len(groups) == 0 {
		return duplicateRecycleReceipt{}, errors.New("Open the current duplicate scan and choose the copies to recycle")
	}
	defer a.syncSession(true)
	return s.recycleCopies(ctx, groups, equivalent, "", recycle)
}

func (s *session) recycleCopies(ctx context.Context, groups []duplicateSelection, equivalent bool, batchID string, recycle func(string) error) (duplicateRecycleReceipt, error) {
	var receipt duplicateRecycleReceipt
	verified := make(map[int]int)
	if equivalent {
		for _, group := range s.duplicateResults().Verified {
			for _, member := range group.Members {
				verified[member.Index] = group.RecommendedKeeper
			}
		}
	}
	seen := make(map[int]bool)
	for _, group := range groups {
		if seen[group.Keep] || !s.liveDuplicate(group.Keep) || len(group.Indices) == 0 {
			return receipt, errors.New("Choose one available keeper and at least one copy in each group")
		}
		seen[group.Keep] = true
		keeper := s.plans[group.Keep]
		for _, index := range group.Indices {
			if seen[index] || !s.liveDuplicate(index) {
				return receipt, errors.New("Each selected copy must occur once and be available")
			}
			if chosen, ok := verified[index]; equivalent && (!ok || chosen != group.Keep) {
				return receipt, errors.New("Each selected copy must belong to the keeper's current verified recommendation")
			}
			if !equivalent && s.plans[index].Digest != keeper.Digest {
				return receipt, errors.New("Each selected copy must be byte-identical to its keeper")
			}
			seen[index] = true
		}
		receipt.Groups = append(receipt.Groups, duplicateRecycleOutcome{Kept: keeper.Path, Requested: len(group.Indices)})
	}
	if err := validateRoot(s.manifest.Root); err != nil {
		return receipt, err
	}
	root, err := os.OpenRoot(s.manifest.Root)
	if err != nil {
		return receipt, err
	}
	defer root.Close()
	for _, group := range groups {
		for _, index := range append([]int{group.Keep}, group.Indices...) {
			plan := s.plans[index]
			st, err := recheckDuplicate(ctx, root, s.manifest.Root, plan)
			if err != nil {
				return receipt, fmt.Errorf("Nothing recycled. %s: %w", filepath.Base(s.plans[index].Path), err)
			}
			if equivalent {
				relative, err := filepath.Rel(s.manifest.Root, plan.Path)
				if err != nil || duplicateEquivalence(ctx, root, relative, st, plan.Digest) != plan.EquivalentDigest {
					return receipt, fmt.Errorf("Nothing recycled. Equivalence could not be reverified for %s", filepath.Base(plan.Path))
				}
			}
		}
	}
	for i, group := range groups {
		keeper := s.plans[group.Keep]
		keptStamp, err := recheckDuplicate(ctx, root, s.manifest.Root, keeper)
		if err != nil {
			return receipt, err
		}
		for _, index := range group.Indices {
			if err := ctx.Err(); err != nil {
				return receipt, err
			}
			plan := s.plans[index]
			proof, err := recheckDuplicate(ctx, root, s.manifest.Root, plan)
			if err != nil {
				return receipt, fmt.Errorf("%s: %w", filepath.Base(plan.Path), err)
			}
			currentKept, err := fingerprint(keeper.Path)
			if err != nil || currentKept != keptStamp || safeParent(s.manifest.Root, keeper.Path) != nil {
				return receipt, errors.New("The copy selected to keep is no longer available")
			}
			if err := s.append(record{Type: "recycle-intent", Index: index, Path: plan.Path, Stamp: proof, BatchID: batchID}); err != nil {
				return receipt, err
			}
			recycleErr := recycle(plan.Path)
			recordErr := s.finishRecycle()
			if s.plans[index].Decision == removedFlaggedFile {
				receipt.Groups[i].Recycled++
				receipt.Recycled++
			}
			if err := errors.Join(recycleErr, recordErr); err != nil {
				return receipt, fmt.Errorf("%s: %w", filepath.Base(plan.Path), err)
			}
			if s.plans[index].Decision != removedFlaggedFile {
				return receipt, fmt.Errorf("The Recycle Bin did not remove %s", filepath.Base(plan.Path))
			}
		}
	}
	return receipt, nil
}

// Section: Journal replay for exact batch approval.

func (s *session) acceptExactBatchApproval(r record) {
	if s.exactBatches == nil {
		s.exactBatches = make(map[string]*exactRecycleBatch)
		s.batchRecycled = make(map[string]map[int]bool)
	}
	r.ExactBatch.approvedCopies = make(map[int]bool)
	for _, group := range r.ExactBatch.Groups {
		for _, index := range group.Indices {
			r.ExactBatch.approvedCopies[index] = true
		}
	}
	s.exactBatches[r.ExactBatch.ID] = r.ExactBatch
	s.batchRecycled[r.ExactBatch.ID] = make(map[int]bool)
}

// Section: Journal replay for recycle result.

func (s *session) acceptRecycleResult(r record) {
	batchID := s.pending.BatchID
	s.pending = nil
	s.planRevision++
	s.planVersions[r.Index] = s.planRevision
	if r.Status == "present" {
		if s.recycleErrors == nil {
			s.recycleErrors = make(map[int]bool)
		}
		s.recycleErrors[r.Index] = true
	}
	if r.Status == "absent" {
		delete(s.recycleErrors, r.Index)
		if batchID != "" {
			s.batchRecycled[batchID][r.Index] = true
		}
		s.recycled++
		plan := s.plans[r.Index]
		s.countFileAssessment(plan, -1)
		plan.Move, plan.Metadata = nil, nil
		plan.Digest, plan.TextDigest = "", ""
		plan.Note, plan.Decision, plan.Reason = "", removedFlaggedFile, "The file is no longer in the scanned folder after a Recycle Bin request"
		plan.Auto, plan.AutoValidated, plan.Unknown = false, false, false
		s.countFileAssessment(plan, 1)
		s.plans[r.Index] = plan
		delete(s.outcomes, r.Index)
		if s.manifest.Operation != scanDuplicates {
			s.reindexPlans()
		}
	}
}
