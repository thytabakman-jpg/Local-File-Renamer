package main

import (
	"context"
	"strings"
	"time"
)

func inspectLocation(ctx context.Context, s *session, assess assessFile, opts scanSettings, path, name, cacheName string,
	pre preparedEvidence, e evidence, ocr string, ocrMS int64, ocrErr error, refresh bool) decision {
	choice := s.sortIndex.proposedLocation(path, e, assessment{}, false)
	var followup string
	if !choice.Clear && opts.Evidence.DeepenOn == "sparse_or_ambiguous" && !pre.Deepened &&
		strings.EqualFold(extension(path), ".pdf") && opts.Evidence.ExtraPDFPages > 0 && ctx.Err() == nil {
		started := time.Now()
		pre.Evidence = e
		added := addPDFEvidence(ctx, path, opts, &pre)
		pre.ReadMS += elapsedMS(started)
		if added {
			e = pre.Evidence
			choice = s.sortIndex.proposedLocation(path, e, assessment{}, false)
			followup = "Read later PDF pages"
			if choice.Clear {
				followup += " and resolved the folder decision"
			}
		} else {
			followup = "Later PDF pages supplied no readable text"
		}
		// The earlier cached assessment was based on different input.
		pre.Reused = false
	} else if e.FollowupText != "" {
		followup = "Used later PDF pages"
	}
	check := inspection{Text: strings.TrimSpace(e.Text) != "", Metadata: e.Title != "" || e.Author != "" || e.Summary != "",
		Image: len(e.Photo) != 0, AI: "Off", Detail: joinNote(e.Note, followup), ReadMS: pre.ReadMS, OCR: ocr, OCRMS: ocrMS}
	if pre.Reused {
		check.Cache = "Evidence reused"
	}
	modelErr := ocrErr
	var topic assessment
	var fetched bool
	switch {
	case choice.Clear:
		check.AI = "Location resolved from local evidence"
		modelErr = nil
	case assess == nil || modelErr != nil:
	case opts.AI.Invoke == "generic_names" && meaningful(baseName(name)):
		check.AI = "Not needed under the scan settings"
	case !check.Text && !check.Metadata && !check.Image:
		check.AI = "No readable input"
	default:
		cached := pre.Cached.Move
		if pre.Reused && opts.Cache.ReuseAssessment && !refresh && cached != nil &&
			cached.Version == moveTopicVersion && cached.ModelKey == modelCacheKey(opts.AI.Model) && cached.AI == opts.AI {
			topic, check.Cache = cached.Assessment, "Folder topic reused"
		} else {
			started := time.Now()
			topic, modelErr = assess(ctx, name, e)
			check.ModelMS = elapsedMS(started)
			fetched = modelErr == nil
		}
		if modelErr == nil {
			check.AI = "Used"
			check.AIContribution = "No location change"
			if next := s.sortIndex.proposedLocation(path, e, topic, true); next.Clear {
				choice = next
				if next.Path == "" {
					check.AIContribution = "Confirmed the current folder"
				} else {
					check.AIContribution = "Identified a destination"
				}
			}
		}
	}
	if cacheName != "" && opts.Cache.ReuseEvidence && ctx.Err() == nil && ocrErr == nil &&
		(!pre.Reused || fetched && opts.Cache.ReuseAssessment) {
		cached := evidenceCache(pre, e, ocr, opts)
		if fetched && opts.Cache.ReuseAssessment {
			cached.Move = &cachedMoveTopic{moveTopicVersion, modelCacheKey(opts.AI.Model), opts.AI, topic}
		}
		_ = atomicJSON(cacheName, cached)
	}
	d := decision{Name: name, Folder: choice.Path, NewFolder: choice.New, Reason: choice.Reason}
	if !choice.Clear {
		d.Outcome = "Needs review"
	}
	if modelErr != nil {
		status, detail := localAIIssue(modelErr)
		if opts.AI.TimeoutSeconds != 180 {
			detail = strings.ReplaceAll(detail, "timed out after 3 minutes", "reached the configured request timeout")
		}
		check.AI, check.Detail, check.AIContribution = status, joinNote(check.Detail, detail), "Analysis failed"
		d.Outcome = "Analysis failed"
		d.Reason = joinNote(d.Reason, detail)
	}
	check.Location = choice.Evidence
	d.Inspection = check
	return d
}
