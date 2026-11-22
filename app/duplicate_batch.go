package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
