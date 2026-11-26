package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

type inspection struct {
	Text, Metadata, Image                      bool
	AI, Detail, Model                          string
	Cache, AIContribution                      string
	OCR                                        string `json:",omitempty"`
	PublicationDate, PublicationSource         string `json:",omitempty"`
	ReadMS, OCRMS, StartupMS, ModelMS, TotalMS int64
	Checklist                                  []classificationCheck `json:",omitempty"`
	OnlineMS                                   int64                 `json:",omitempty"`
	Location                                   *locationEvidence     `json:",omitempty"`
	PromptMS                                   int64                 `json:",omitempty"`
	GenerationMS                               int64                 `json:",omitempty"`
	ModelTrace                                 bool                  `json:",omitempty"`
}

func localAIIssue(err error) (status, note string) {
	var typed modelFailure
	if errors.As(err, &typed) {
		if typed.kind == modelTimeout && typed.detail == "local model request reached its time limit" {
			return typed.kind, "Local AI timed out after 3 minutes"
		}
		return typed.kind, "Local AI: " + typed.detail
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return "Timed out", fmt.Sprintf("Local AI timed out after %g minutes", modelRequestTimeout.Minutes())
	}
	return modelUnavailable, "Local model could not assess this file: " + err.Error()
}

func recordSuggestion(s *session, i int, d decision, st stamp) error {
	if st.ID != "" && photoExtension(s.manifest.Items[i].Path) {
		d = s.versionImage(i, d)
	}
	return s.append(buildSuggestion(s, i, d, st, false))
}

func buildSuggestion(s *session, i int, d decision, st stamp, revision bool) record {
	item := s.manifest.Items[i]
	if s.manifest.Operation == scanMove {
		return s.buildMoveSuggestion(i, d, st, revision)
	}
	parent := filepath.Dir(item.Path)
	name := d.Name
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		name = filepath.Base(item.Path)
	}
	if d.Name != "" && !revision && item.Folder {
		name = s.namingRules().cleanName(name, true)
	}
	if name != filepath.Base(item.Path) && !validProposedName(name, filepath.Base(item.Path), item.Folder) {
		name = filepath.Base(item.Path)
		d.Note = joinNote(d.Note, "The proposed filename is invalid or too long")
	}
	available := func(from, to string) error {
		if revision && s.plans[i].Move != nil && strings.EqualFold(s.plans[i].Move.To, to) {
			return nameAvailable(from, to)
		}
		return s.availableForPlan(from, to)
	}
	var mv *move
	if !item.Folder && d.Folder != "" && d.Folder != parent {
		proposed := move{From: item.Path, To: filepath.Join(d.Folder, name), Stamp: st}
		if err := s.validateMoveParents(proposed); err != nil {
			d.Note = joinNote(d.Note, err.Error())
		} else if err := available(proposed.From, proposed.To); err != nil {
			d.Note = joinNote(d.Note, err.Error())
		} else {
			source, sourceErr := fingerprint(parent)
			target, targetErr := fingerprint(d.Folder)
			if sourceErr != nil || targetErr != nil || !source.Dir || !target.Dir {
				d.Note = joinNote(d.Note, "Could not confirm both folders for this move")
			} else {
				proposed.SourceParent, proposed.TargetParent = source.ID, target.ID
				mv = &proposed
			}
		}
	}
	if mv == nil && name != filepath.Base(item.Path) {
		to := filepath.Join(parent, name)
		if err := available(item.Path, to); err != nil {
			d.Note = joinNote(d.Note, err.Error())
		} else {
			mv = &move{From: item.Path, To: to, Stamp: st}
		}
	}
	auto := mv != nil && d.Folder == "" && d.Strict && d.AutoEvidence && !d.Unknown && d.Note == "" &&
		!placeholder.MatchString(name) && (d.ReplaceDownloadID || len(removedDetails(filepath.Base(item.Path), name)) == 0)
	if d.Outcome == "" {
		switch {
		case st.ID == "":
			d.Outcome = "Skipped"
		case mv != nil:
			d.Outcome = "Rename proposed"
		default:
			d.Outcome = "Insufficient evidence"
		}
	}
	if mv == nil && (d.Outcome == "Cleanup proposed" || d.Outcome == "Rename proposed") {
		d.Outcome = "Needs review"
	}
	recordType := "plan"
	if revision {
		recordType, auto = "plan-revision", false
	}
	return record{Type: recordType, Index: i, Path: item.Path, Note: d.Note, Move: mv, Kind: d.Kind, Decision: d.Outcome, Reason: d.Reason,
		Inspection: d.Inspection, Unknown: d.Unknown, Metadata: d.Metadata, Stamp: st, Auto: auto, AutoValidated: auto,
		Rough: auto && photoExtension(item.Path) && imageSubject(baseName(filepath.Base(item.Path))) == ""}
}

type assessFile func(context.Context, string, evidence) (assessment, error)

func suggest(ctx context.Context, s *session, m *localModel, i int) (decision, stamp, error) {
	if m == nil {
		return inspect(ctx, s, nil, nil, i)
	}
	assess := m.assess
	if s.manifest.Operation == scanMove {
		assess = m.assessTopic
	}
	return inspect(ctx, s, assess, m.transcribe, i)
}

func inspect(ctx context.Context, s *session, assess assessFile, transcribe transcribePage, i int) (result decision, original stamp, failure error) {
	return inspectFile(ctx, s, assess, transcribe, i, "", false, s.settings())
}

func inspectFile(ctx context.Context, s *session, assess assessFile, transcribe transcribePage, i int, override string, refresh bool, opts scanSettings) (result decision, original stamp, failure error) {
	return inspectPrepared(ctx, s, assess, transcribe, i, override, refresh, opts, nil)
}

type preparedEvidence struct {
	Evidence evidence
	Stamp    stamp
	Cached   assessmentCache
	Reused   bool
	Deepened bool
	ReadMS   int64
}

func prepareEvidence(ctx context.Context, path, name, cacheName string, st stamp, vision bool, opts scanSettings) preparedEvidence {
	if opts.Cache.ReuseEvidence {
		if c, ok := readCache(cacheName, st); ok && localEvidenceSettings(c.EvidenceConfig) == localEvidenceSettings(opts.Evidence) && c.DecodeMemory == opts.Resources.DecodeMemoryMiB &&
			(c.OCRModel == "" || opts.Evidence.OCREngine != "tesseract" && c.OCRModel == modelCacheKey(opts.AI.Model) && c.AIConfig == opts.AI) {
			return preparedEvidence{Evidence: c.Evidence, Stamp: st, Cached: c, Reused: true, Deepened: c.Deepened}
		}
	}
	started := time.Now()
	e := readEvidenceWith(path, vision, opts)
	sparse := len(e.Text) < 300
	deepen := opts.Evidence.DeepenOn == "always" || opts.Evidence.DeepenOn == "sparse" && sparse ||
		opts.Evidence.DeepenOn == "sparse_or_ambiguous" && (sparse || len(e.Text) < 1600 && publicationKind(".pdf", e, assessment{}) == "" || !meaningful(baseName(name)))
	pre := preparedEvidence{Evidence: e, Stamp: st}
	if deepen {
		addPDFEvidence(ctx, path, opts, &pre)
	}
	pre.ReadMS = elapsedMS(started)
	return pre
}

// Read another page range only once. Retain the opening text independently
// so a later sample cannot suppress it when the folder candidates are ranked.
func addPDFEvidence(ctx context.Context, path string, opts scanSettings, pre *preparedEvidence) bool {
	if pre.Deepened || !strings.EqualFold(extension(path), ".pdf") || opts.Evidence.ExtraPDFPages == 0 ||
		opts.Evidence.DeepenOn == "never" || ctx.Err() != nil {
		return false
	}
	pre.Deepened = true
	last := opts.Evidence.PDFPages
	extra := pdfSample(path, last+1, last+opts.Evidence.ExtraPDFPages)
	if extra.Text == "" {
		return false
	}
	pre.Evidence.FollowupText = chars(extra.Text, opts.Evidence.TextBytes/2)
	e := &pre.Evidence
	e.Text = chars(strings.TrimSpace(chars(e.Text, opts.Evidence.TextBytes/2)+"\n"+e.FollowupText), opts.Evidence.TextBytes)
	if len(e.Text) >= 35 {
		e.Note = ""
	}
	return true
}

func inspectPrepared(ctx context.Context, s *session, assess assessFile, transcribe transcribePage, i int, override string, refresh bool, opts scanSettings, pre *preparedEvidence) (result decision, original stamp, failure error) {
	started := time.Now()
	rules := opts.Naming
	defer func() {
		result.Inspection.TotalMS = max(elapsedMS(started), result.Inspection.ReadMS+result.Inspection.OCRMS+result.Inspection.ModelMS)
	}()
	item := s.manifest.Items[i]
	if item.Skip {
		return decision{Note: item.Note, Inspection: inspection{AI: "Not attempted", Detail: item.Note}}, stamp{}, nil
	}
	if item.Folder && s.manifest.Preview && !s.manifest.RenameFolders {
		return decision{}, stamp{}, nil
	}
	if err := safeParent(s.manifest.Root, item.Path); err != nil {
		return decision{Note: err.Error()}, stamp{}, nil
	}
	st, err := fingerprint(item.Path)
	if err != nil {
		return decision{Note: err.Error()}, stamp{}, nil
	}
	if st.Dir != item.Folder {
		return decision{Note: "Item type changed after the scan started"}, stamp{}, nil
	}
	name := filepath.Base(item.Path)
	if item.Hash != "" {
		hash, err := comparisonHash(ctx, item.Path)
		if err != nil || hash != item.Hash {
			return decision{Name: name, Outcome: "Analysis failed", Note: "The file changed after its starting comparison identity was recorded"}, st, ctx.Err()
		}
	}
	if item.Folder {
		if s.relocationParents[item.Path] {
			return decision{Name: name, Note: "Review this folder name after sorting its files"}, st, ctx.Err()
		}
		return folderDecision(ctx, item.Path, assess, s), st, ctx.Err()
	}
	vision := assess != nil
	cacheName := cachePath(s.cacheDir, item.Path, vision)
	if pre == nil || pre.Stamp != st {
		p := prepareEvidence(ctx, item.Path, name, cacheName, st, vision, opts)
		pre = &p
	}
	cached, reused, e, readMS := pre.Cached, pre.Reused, pre.Evidence, pre.ReadMS
	e, ocrStatus, ocrMS, ocrErr := inspectOCR(ctx, s, transcribe, opts, item, name, override, pre, e)

	if s.manifest.Operation == scanMove {
		return inspectLocation(ctx, s, assess, opts, item.Path, name, cacheName, *pre, e, ocrStatus, ocrMS, ocrErr, refresh), st, ctx.Err()
	}
	if isMedia(name) {
		if opts.Evidence.MediaSamples && assess != nil {
			return inspectSampledMedia(ctx, item.Path, name, cacheName, st, e, readMS, assess, opts, refresh), st, ctx.Err()
		}
		d := inspectMediaTitle(name, e)
		d.Inspection.ReadMS = readMS
		if cacheName != "" && opts.Cache.ReuseEvidence && !reused && ctx.Err() == nil {
			_ = atomicJSON(cacheName, assessmentCache{Stamp: st, EvidenceVersion: evidenceVersion, Evidence: e, EvidenceConfig: opts.Evidence, DecodeMemory: opts.Resources.DecodeMemoryMiB})
		}
		return d, st, ctx.Err()
	}
	check := inspection{Text: strings.TrimSpace(e.Text) != "", Image: len(e.Photo) > 0,
		Metadata: e.Title != "" || e.Author != "" || e.Year != "" || e.Venue != "" || e.Publisher != "" || e.Kind != "" || e.Summary != "",
		AI:       "Off", Detail: e.Note, OCR: ocrStatus, ReadMS: readMS, OCRMS: ocrMS}
	if reused {
		check.Cache = "Evidence reused"
	}
	var a assessment
	used := false
	modelErr := ocrErr
	known := decision{Name: name, Note: e.Note}
	if e.Note == "" || check.Text || check.Metadata || check.Image || override != "" {
		known = decideWithRules(name, e, a, false, rules, override)
	}
	if assess != nil && modelErr == nil {
		check.AI = "No readable input"
		if check.Text || check.Metadata || check.Image {
			ruleResolved := known.Strict && known.Note == "" || known.Outcome == "Name verified" ||
				known.Name != name && known.Reason == "Used a more specific or corrected title supported by the file" && !known.Strict
			cleanup := rules.cleanName(name, false)
			// Explicit structured fields are deterministic. A substantive
			// text decision also suffices unless the scan requested extra work.
			if opts.AI.Invoke == "generic_names" && meaningful(baseName(name)) && !known.Unknown {
				check.AI = "Not needed under the scan settings"
			} else if opts.AI.Invoke != "all" && ruleResolved && !s.manifest.SortFolders && !s.manifest.RepairMetadata {
				check.AI = "Rule resolved name"
			} else if opts.AI.Invoke != "all" && cleanup != name && !check.Image && !known.Unknown &&
				known.Name == name && !s.manifest.SortFolders && !s.manifest.RepairMetadata &&
				(slices.Contains([]string{".txt", ".md", ".csv", ".log"}, strings.ToLower(extension(name))) || known.Outcome == "Name verified") {
				check.AI = "Formatting only"
			} else if opts.Cache.ReuseAssessment && !refresh && reused && cached.Model != nil && cached.AIConfig == opts.AI && cached.AssessmentVersion == assessmentVersion &&
				cached.Kind == modelCacheKey(opts.AI.Model) && reflect.DeepEqual(cached.Rules, rules) {
				a, used, check.AI, check.Cache = *cached.Model, true, "Used", "Assessment reused"
			} else {
				inference := time.Now()
				a, modelErr = assess(ctx, name, e)
				check.ModelMS = elapsedMS(inference)
				if modelErr == nil {
					used, check.AI = true, "Used"
				}
			}
		}
	}
	if cacheName != "" && opts.Cache.ReuseEvidence && ctx.Err() == nil && ocrErr == nil && (!reused || used && check.Cache != "Assessment reused") {
		cached := evidenceCache(*pre, e, ocrStatus, opts)
		if used && opts.Cache.ReuseAssessment {
			cached.Model, cached.AssessmentVersion, cached.Kind, cached.Rules, cached.AIConfig = &a, assessmentVersion, modelCacheKey(opts.AI.Model), rules, opts.AI
		}
		_ = atomicJSON(cacheName, cached)
	}
	d := known
	if used {
		d = decideWithRules(name, e, a, true, rules, override)
		if photoExtension(name) && opts.Evidence.UseOCR && a.SpeciesVisibleText && meaningful(a.Species) &&
			len(a.Species) <= 100 && transcribe != nil && ctx.Err() == nil {
			ocrStart := time.Now()
			visible, speciesErr := transcribe(ctx, e.Photo)
			check.OCRMS += elapsedMS(ocrStart)
			if speciesErr == nil && hasEvidence(a.Species, visible) {
				d = identifyImageSpecies(name, d, a, rules)
				check.OCR = "Species name verified against visible text"
			} else {
				d.Note = joinNote(d.Note, "The species could not be verified from visible text")
			}
		}
		check.AIContribution = evidenceBenefit(known, d)
	}
	if s.online != nil && opts.Evidence.OnlineDates != "off" && localPublicationYear(e) == "" &&
		(d.Kind == "article" || d.Kind == "book" || d.Kind == "academic_book" || extractDOI(chars(e.Text, 2200)) != "") {
		started := time.Now()
		confirmed, err := s.online.publicationDate(ctx, name, e)
		check.OnlineMS = elapsedMS(started)
		if err != nil {
			check.PublicationDate = err.Error()
		} else {
			check.PublicationDate, check.PublicationSource = confirmed.Reason, confirmed.Source.URL
			if confirmed.Year != "" {
				e.Year = confirmed.Year
				d = decideWithRules(name, e, a, used, rules, override)
				d.AutoEvidence = false
				check.PublicationDate = confirmed.Year + " confirmed by " + confirmed.Provider
			}
		}
	}
	cleaned := rules.cleanName(d.Name, false)
	formattingOnlyPDF := s.manifest.Auto && d.Name == name && strings.EqualFold(extension(name), ".pdf")
	if !formattingOnlyPDF && cleaned != d.Name && (cleaned == name || windowsNameLength(cleaned) <= rules.MaxLength) {
		if d.Name == name {
			d.Outcome = "Cleanup proposed"
			d.Reason = "Applied filename formatting and configured spelling rules"
		}
		d.Name = cleaned
	}
	if d.Name != name && d.Name != "" {
		d.Name = rules.normalizeGeneratedName(name, d.Name)
	}
	if photoExtension(name) && !vision {
		check.AI = "Off"
		d.Note = ""
		d.Outcome = "Not assessed"
		d.Reason = "Image content was not assessed because local AI is off. Any formatting proposal uses the filename only."
	}
	if e.Note != "" && d.Outcome != "Not assessed" && !strings.Contains(d.Note, e.Note) {
		d.Note = joinNote(d.Note, e.Note)
	}
	if d.Outcome == "" {
		switch {
		case d.Unknown || d.Name == name && d.Note != "":
			d.Outcome = "Insufficient evidence"
		case d.Name != name:
			d.Outcome = "Rename proposed"
		case d.Strict && (check.Text || check.Metadata):
			d.Outcome = "Name verified"
		default:
			d.Outcome = "Insufficient evidence"
		}
	}
	if modelErr != nil {
		var note string
		check.AI, note = localAIIssue(modelErr)
		d.Outcome = "Analysis failed"
		check.AIContribution = "Analysis failed"
		d.Note = joinNote(d.Note, note)
		d.Strict = false
	}
	if !check.Text && !check.Image && !check.Metadata {
		d.Strict = false
	}
	if d.Name != name && !rules.protect(name, d.Name) {
		d.Name, d.Strict, d.Outcome = name, false, "Needs review"
		d.Note = joinNote(d.Note, "The alternative name would remove a protected word or identifier")
	}
	if (d.Outcome == "Insufficient evidence" || d.Outcome == "Analysis failed" || d.Unknown || d.Note != "") && opts.Recovery.Uncertainty != "propose_with_warning" && d.Outcome != "Cleanup proposed" {
		d.Name, d.Strict = name, false
		if opts.Recovery.Uncertainty == "keep" && d.Outcome != "Analysis failed" && !d.Unknown {
			d.Outcome = "Name kept by preference"
		}
	}
	check.Checklist = d.Checklist
	d.Inspection = check
	destination, sortNote := s.sortIndex.suggest(item.Path, e, a, used)
	d.Folder, d.Note = destination, joinNote(d.Note, sortNote)
	if s.manifest.RepairMetadata {
		d.Metadata = suggestMetadata(name, e, a)
		if d.Metadata != nil && d.Metadata.Title != "" {
			d.Metadata.Title = readableTitle(d.Metadata.Title)
			if d.Metadata.Title == d.Metadata.OldTitle {
				d.Metadata.Title = ""
			}
			if d.Metadata.Title == "" && d.Metadata.Author == "" {
				d.Metadata = nil
			}
		}
	}
	if modelErr != nil && check.AI == modelTimeout && opts.AI.TimeoutSeconds != 180 && hasModelFailure(modelErr, modelTimeout) {
		d.Note = strings.ReplaceAll(d.Note, "timed out after 3 minutes", fmt.Sprintf("timed out after %d seconds", opts.AI.TimeoutSeconds))
	}
	if strings.Contains(d.Note, versionPendingNote) {
		d.Note = joinNote(d.Note, s.compareVersionCopies(i, d.Name))
	}
	return d, st, ctx.Err()
}

func folderDecision(ctx context.Context, path string, assess assessFile, s *session) decision {
	name := filepath.Base(path)
	if meaningful(name) {
		return decision{Name: name}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return decision{Name: name, Note: err.Error()}
	}
	var labels []string
	for _, e := range entries {
		if len(labels) == 35 {
			break
		}
		label := e.Name()
		if candidate, ok := s.proposedNames[filepath.Join(path, label)]; ok {
			label = filepath.Base(candidate)
		}
		if !e.IsDir() {
			label = baseName(label)
		}
		if meaningful(label) && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	switch len(labels) {
	case 0:
		return decision{Name: name, Note: "Folder contents do not identify a subject"}
	case 1:
		// A lone, clearly named file can identify the folder it sits in.
		if len(entries) == 1 {
			return decision{Name: makeName([]string{labels[0]}, "")}
		}
	}
	if assess == nil {
		return decision{Name: name, Note: "Folder needs a clearer name"}
	}
	start := time.Now()
	e := evidence{Text: "Use these names to identify a shared subject of this folder. Use an empty label if the items have no shared topic.\n" + strings.Join(labels, "\n")}
	a, err := assess(ctx, name, e)
	if err != nil {
		status, note := localAIIssue(err)
		return decision{Name: name, Note: note, Inspection: inspection{AI: status, ModelMS: elapsedMS(start)}}
	}
	if !meaningful(a.Label) || len(strings.Fields(a.Label)) > 10 || strings.EqualFold(a.Label, fmt.Sprintf("%d files", len(entries))) {
		return decision{Name: name, Note: "Folder contents do not identify a subject", Inspection: inspection{AI: "Used", ModelMS: elapsedMS(start)}}
	}
	return decision{Name: makeName([]string{a.Label}, ""), Inspection: inspection{AI: "Used", ModelMS: elapsedMS(start)}}
}

func (s *session) currentPath(path string, index int) string {
	active := len(s.moves) - s.undoCount
	if im, ok := s.byItem[index]; ok {
		path = im.move.To
		for position := im.position; position >= active; position-- {
			if candidate := s.moves[position]; candidate.To == path {
				path = candidate.From
			}
		}
	}
	for p := filepath.Dir(path); p != s.manifest.Root && p != filepath.Dir(p); p = filepath.Dir(p) {
		if im, ok := s.folderByPath[p]; ok && im.position < active {
			path = im.move.To + strings.TrimPrefix(path, p)
		}
	}
	return path
}

func prefetchEvidence(ctx context.Context, s *session, start int, opts scanSettings, vision bool) map[int]preparedEvidence {
	end := min(s.fileCount, start+opts.Resources.Workers)
	out := make(map[int]preparedEvidence)
	if start >= end {
		return out
	}
	type indexed struct {
		index int
		data  preparedEvidence
	}
	results := make(chan indexed, end-start)
	for i := start; i < end; i++ {
		go func(index int) {
			item := s.manifest.Items[index]
			result := indexed{index: index}
			defer func() { results <- result }()
			if item.Skip || ctx.Err() != nil || safeParent(s.manifest.Root, item.Path) != nil {
				return
			}
			st, err := fingerprint(item.Path)
			if err != nil || st.Dir {
				return
			}
			result.data = prepareEvidence(ctx, item.Path, filepath.Base(item.Path), cachePath(s.cacheDir, item.Path, vision), st, vision, opts)
		}(i)
	}
	for i := start; i < end; i++ {
		r := <-results
		if r.data.Stamp.ID != "" {
			out[r.index] = r.data
		}
	}
	return out
}
