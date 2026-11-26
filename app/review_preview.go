package main

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (a *application) previewFile(job string, index int) (string, stamp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if s == nil || job == "" || job != a.v.Job || index < 0 || index >= len(s.manifest.Items) {
		return "", stamp{}, errors.New("Select a file in the current scan")
	}
	item := s.manifest.Items[index]
	if item.Skip || item.Folder {
		return "", stamp{}, errors.New("This item has no file preview")
	}
	path := s.currentPath(item.Path, index)
	if err := safeParent(s.manifest.Root, path); err != nil {
		return "", stamp{}, err
	}
	st, err := fingerprint(path)
	if err != nil || st.Dir {
		return "", stamp{}, errors.New("The file is no longer available")
	}
	return path, st, nil
}

type previewParagraph struct {
	Text  string
	Style string
}

func wordPreview(path string) ([]previewParagraph, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	body, err := z.Open("word/document.xml")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	d := xml.NewDecoder(io.LimitReader(body, 2<<20))
	var paragraphs []previewParagraph
	var inParagraph, inText bool
	var line strings.Builder
	var style string
	for len(paragraphs) < 120 {
		token, err := d.Token()
		if err != nil {
			if len(paragraphs) == 0 && err != io.EOF {
				return nil, err
			}
			break
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				line.Reset()
				style, inParagraph = "", true
			case "pStyle":
				for _, attr := range t.Attr {
					if attr.Name.Local == "val" {
						switch value := strings.ToLower(attr.Value); {
						case value == "title":
							style = "title"
						case strings.HasPrefix(value, "heading"):
							style = "heading"
						}
					}
				}
			case "t":
				inText = inParagraph
			case "tab", "br":
				if inParagraph {
					line.WriteByte(' ')
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				inParagraph = false
				if line.Len() > 0 {
					paragraphs = append(paragraphs, previewParagraph{Text: clean(line.String()), Style: style})
				}
			}
		case xml.CharData:
			if inText && line.Len() < 2500 {
				line.WriteString(chars(string(t), 2500-line.Len()))
			}
		}
	}
	return paragraphs, nil
}

func (a *application) serveFilePreview(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	index, err := strconv.Atoi(query.Get("index"))
	if err != nil {
		http.Error(w, "Choose a file from the scan", http.StatusBadRequest)
		return
	}
	path, before, err := a.previewFile(query.Get("job"), index)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ext := strings.ToLower(extension(path))
	page, _ := strconv.Atoi(query.Get("page"))
	if page < 0 || page > 10000 {
		http.Error(w, "Page outside the preview range", http.StatusBadRequest)
		return
	}
	var jpeg []byte
	var paragraphs []previewParagraph
	var label string
	switch {
	case photoExtension(path):
		var note string
		edge := 1100
		if query.Get("thumbnail") == "1" {
			edge = 96
		}
		jpeg, note = imageEvidenceWith(path, edge, 128)
		if len(jpeg) == 0 {
			err = errors.New(note)
		}
	case ext == ".pdf":
		jpeg, err = renderSamplePage(r.Context(), path, page)
		if err == nil && len(jpeg) == 0 {
			http.Error(w, "No more PDF pages", http.StatusNotFound)
			return
		}
	case ext == ".docx" || ext == ".docm":
		paragraphs, err = wordPreview(path)
		label = "Word document preview · simplified layout"
	case ext == ".txt" || ext == ".md" || ext == ".odt":
		paragraphs = textPreview(readEvidenceWith(path, false, defaultSettings()).Text)
		label = "Text preview"
	case isVideo(path):
		jpeg, err = mediaPreviewFrame(r.Context(), path)
	default:
		err = errors.New("No preview available for this format")
	}
	if err != nil || len(jpeg) == 0 && len(paragraphs) == 0 {
		message := "A preview is unavailable for this file"
		if err != nil {
			message += ": " + err.Error()
		}
		http.Error(w, message, http.StatusUnprocessableEntity)
		return
	}
	after, statErr := fingerprint(path)
	if statErr != nil || before != after || r.Context().Err() != nil {
		http.Error(w, "File changed while preparing the preview", http.StatusConflict)
		return
	}
	if len(jpeg) > 0 {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpeg)
		return
	}
	reply(w, struct {
		Label      string
		Paragraphs []previewParagraph
	}{label, paragraphs})
}

func textPreview(content string) []previewParagraph {
	var paragraphs []previewParagraph
	for _, line := range strings.Split(content, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paragraphs = append(paragraphs, previewParagraph{Text: line})
		}
	}
	return paragraphs
}

func mediaPreviewFrame(ctx context.Context, path string) ([]byte, error) {
	return videoFrame(ctx, path, 0, 850)
}

// Section: Review projection data contracts.

type proposalRow struct {
	Index                  int
	Revision               uint64 `json:",omitempty"`
	From, To, Note         string
	Kind, Reason, Decision string
	Draft                  bool
	Metadata               *metadataEdit
}
type inspectionRow struct {
	Path, Proposed, Note                        string
	AutomaticFrom, AutomaticTo, AutomaticReason string
	AutomaticUndone                             bool
	Index                                       int
	Revision                                    uint64 `json:",omitempty"`
	Kind, Reason, Decision                      string
	Inspection                                  inspection
	Unknown                                     bool
	ReviewActions, Refinement                   bool          `json:",omitempty"`
	Metadata                                    *metadataEdit `json:",omitempty"`
	RenameMS                                    int64
	Outcome                                     string
}
type inspectionPage struct {
	Job   string
	Rows  []inspectionRow
	Total int
}

// Section: Proposal retrieval.

func (a *application) previewPage(offset int) ([]proposalRow, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.previewPageLocked(offset)
}

type proposalPage struct {
	Job               string
	Revision          uint64
	Total, NextOffset int
	Rows              []proposalRow
}

func (a *application) proposalPage(id string, offset int) (proposalPage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id != "" && id != a.v.Job {
		return proposalPage{}, errors.New("The selected run changed")
	}
	rows, err := a.previewPageLocked(offset)
	if err != nil {
		return proposalPage{}, err
	}
	total := a.s.reviewCount() + len(a.cleanup) - a.cleanupThrough
	next := offset + len(rows)
	if next >= total || len(rows) == 0 {
		next = 0
	}
	return proposalPage{Job: a.v.Job, Revision: a.s.planRevision, Total: total, NextOffset: next, Rows: rows}, nil
}

func (a *application) previewPageLocked(offset int) ([]proposalRow, error) {
	if a.s == nil || a.v.Busy && !a.scanActive || !a.scanActive && (a.v.Phase != "preview" && a.v.Phase != "paused" && a.v.Phase != "complete") || a.s.pending != nil || a.s.automaticRenamePending() {
		return nil, errors.New("No preview is available")
	}
	rows := make([]proposalRow, 0, 100)
	for _, plan := range a.s.plans[a.s.applyThrough:a.s.reviewEnd()] {
		hasRename := plan.Move != nil && !a.s.autoCompleted[plan.Index]
		if !a.s.reviewable(plan) {
			continue
		}
		if offset > 0 {
			offset--
			continue
		}
		row := proposalRow{Index: plan.Index, Revision: a.s.planVersions[plan.Index], From: a.s.currentPath(plan.Path, plan.Index), Note: plan.Note, Metadata: plan.Metadata, Kind: plan.Kind, Reason: plan.Reason, Decision: planOutcome(plan)}
		if hasRename {
			row.To = plan.Move.To
			if a.s.manifest.Auto && photoExtension(plan.Path) && !a.s.canAutoApply(plan) {
				if plan.Auto && !a.s.autoHeld[plan.Index] {
					row.Reason = joinNote(row.Reason, "Enable this category for automatic changes or review it manually")
				} else {
					row.Reason = joinNote(row.Reason, "Review required before this image name can be applied")
				}
			}
			if failure := a.s.outcomes[plan.Index]; failure.Type == "single-failed" || failure.Type == "automatic-failed" {
				row.Note = failure.Note
			}
		}
		rows = append(rows, row)
		if len(rows) == 100 {
			break
		}
	}
	if a.scanActive && len(rows) < 100 {
		start := min(len(a.cleanup), a.cleanupThrough+offset)
		rows = append(rows, a.cleanup[start:min(len(a.cleanup), start+100-len(rows))]...)
	}
	return rows, nil
}

// Section: Inspection views.

func (a *application) inspections(offset int, unknown bool) (inspectionPage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inspectionPageLocked(offset, unknown)
}

func (a *application) reviewInspections(offset int, unknown bool, filter string) (inspectionPage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inspectionPageFiltered(offset, unknown, filter)
}

func (a *application) inspectionPageLocked(offset int, unknown bool) (inspectionPage, error) {
	return a.inspectionPageFiltered(offset, unknown, "")
}

func (a *application) inspectionPageFiltered(offset int, unknown bool, filter string) (inspectionPage, error) {
	page := inspectionPage{Job: a.v.Job, Rows: make([]inspectionRow, 0, 100)}
	if a.s == nil || a.v.Busy && !a.scanActive && a.v.Phase != "reassessing" {
		return page, errors.New("The inspection report is unavailable during this operation")
	}
	for _, r := range a.s.plans {
		if a.s.manifest.Items[r.Index].Folder || unknown && !r.Unknown {
			continue
		}
		if !reviewMatches(a.s, r, filter) {
			continue
		}
		if page.Total >= offset && len(page.Rows) < 100 {
			row := inspectionRow{Path: a.s.currentPath(r.Path, r.Index), Note: r.Note, Unknown: r.Unknown, Inspection: r.Inspection, Index: r.Index, Revision: a.s.planVersions[r.Index], Kind: r.Kind, Reason: r.Reason, Decision: planOutcome(r)}
			if filter == "flagged" || filter == "auto" || filter == "all" {
				_, _, err := a.flaggedFile(a.v.Job, a.v.Revision, r.Index)
				row.ReviewActions = err == nil
				row.Refinement = a.s.canRefine(r.Index)
				if a.s.reviewable(r) {
					row.Metadata = r.Metadata
				}
			}
			if automatic, ok := a.s.automaticMoves[r.Index]; ok {
				row.AutomaticFrom, row.AutomaticTo = automatic.move.From, automatic.move.To
				row.AutomaticReason = automatic.reason
				row.AutomaticUndone = automatic.position >= len(a.s.moves)-a.s.undoCount
			}
			row.Outcome = planOutcome(r)
			if result, ok := a.s.outcomes[r.Index]; ok {
				row.RenameMS, row.Outcome = result.RenameMS, result.Status
				row.Note = joinNote(r.Note, result.Note)
			}
			if r.Move != nil && (!a.s.autoCompleted[r.Index] || a.s.byItem[r.Index].move.To != r.Move.To) {
				row.Proposed = r.Move.To
			}
			page.Rows = append(page.Rows, row)
		}
		page.Total++
	}
	if filter == "all" || filter == "unassessed" {
		for index := len(a.s.plans); index < a.s.fileCount; index++ {
			if page.Total >= offset && len(page.Rows) < 100 {
				item := a.s.manifest.Items[index]
				page.Rows = append(page.Rows, inspectionRow{Path: item.Path, Index: index,
					Decision: "Not assessed", Outcome: "Not assessed", Reason: "Waiting for assessment"})
			}
			page.Total++
		}
	}

	return page, nil
}

// Section: Review filter matching.

func reviewMatches(s *session, row record, filter string) bool {
	decision := planOutcome(row)
	switch filter {
	case "kept":
		return decision == "Name verified" || decision == "Kept existing name" || strings.HasPrefix(decision, "Name kept")
	case "auto":
		_, ok := s.automaticMoves[row.Index]
		return ok
	case "flagged":
		return flaggedPlan(row) || s.canRefine(row.Index) || strings.Contains(s.outcomes[row.Index].Note, "Rename failed:")
	case "unassessed":
		return decision == "Not assessed"
	default:
		return true
	}
}
