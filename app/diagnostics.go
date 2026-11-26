package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

func elapsedMS(start time.Time) int64 { return durationMS(time.Since(start)) }

func durationMS(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return max(1, d.Milliseconds())
}

type scanClock struct {
	Started, Stopped              time.Time
	ActiveMS, PausedMS, ElapsedMS int64
	Paused                        bool
}

func (c scanClock) at(now time.Time) scanClock {
	if !c.Started.IsZero() && c.Stopped.IsZero() {
		c.ActiveMS += durationMS(now.Sub(c.Started))
	} else if c.Paused && !c.Stopped.IsZero() {
		c.PausedMS += durationMS(now.Sub(c.Stopped))
	}
	c.ElapsedMS = c.ActiveMS + c.PausedMS
	return c
}

func stoppedClock(clock scanClock, stopped time.Time, paused bool) scanClock {
	if stopped.IsZero() || stopped.Before(clock.Started) {
		stopped = clock.Started
	}
	c := clock.at(stopped)
	c.Stopped, c.Paused = stopped, paused
	return c
}

type batchReport struct {
	Job                                         string
	Phase                                       string
	Revision                                    uint64
	Total, Assessed, Drafts, Cached             int
	Decisions, Classifications                  map[string]int
	AI                                          map[string]int
	ReadMS, OCRMS, StartupMS, ModelMS, RenameMS int64
	PromptMS, GenerationMS                      int64
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
		Drafts: len(a.cleanup) - a.cleanupThrough, Clock: a.v.Clock.at(time.Now()),
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
		report.PromptMS += check.PromptMS
		report.GenerationMS += check.GenerationMS
		report.RenameMS += s.outcomes[plan.Index].RenameMS
	}
	return report, nil
}

type timingRow struct {
	Path                     string
	Status, Note             string
	Inspection               inspection
	RenameMS                 int64
	RenameStatus, RenameNote string
	Index                    int
}

type diagnosticReport struct {
	Job                                         string
	Clock                                       scanClock
	ReadMS, OCRMS, StartupMS, ModelMS, RenameMS int64
	PromptMS, GenerationMS                      int64
	Slowest                                     []timingRow
	RecentIssues                                []timingRow
	LastError                                   string
}

func (a *application) diagnostic(jobID string, includeNames bool) (diagnosticReport, error) {
	a.mu.Lock()
	if jobID == "" {
		jobID = a.v.Job
	}
	live := a.scanActive && a.v.Job == jobID
	found := slices.ContainsFunc(a.v.Jobs, func(j job) bool { return j.ID == jobID })
	a.mu.Unlock()
	if !found {
		return diagnosticReport{}, errors.New("No matching scan history")
	}
	f, err := os.Open(filepath.Join(a.home, "runs", jobID, "journal.jsonl"))
	if err != nil {
		return diagnosticReport{}, err
	}
	defer f.Close()
	report := diagnosticReport{Job: jobID}
	plans := make(map[int]timingRow)
	var interrupted []timingRow
	var last time.Time
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64<<10), 4<<20)
	for scan.Scan() {
		var r record
		if json.Unmarshal(scan.Bytes(), &r) != nil {
			break
		} // A live writer may be finishing this line.
		if !r.At.IsZero() {
			last = r.At
		}
		switch r.Type {
		case "scan-start", "scan-stop":
			report.Clock = r.Clock
			if r.Type == "scan-stop" && r.Note != "" {
				report.LastError = r.Note
			}
		case "operation-error":
			report.LastError = r.Note
		case "plan", "plan-revision", "image-refinement", "scan-interrupted":
			report.ReadMS += r.Inspection.ReadMS
			report.OCRMS += r.Inspection.OCRMS
			report.ModelMS += r.Inspection.ModelMS
			report.PromptMS += r.Inspection.PromptMS
			report.GenerationMS += r.Inspection.GenerationMS
			report.StartupMS += r.Inspection.StartupMS
			row := timingRow{Index: r.Index, Path: r.Path, Status: planOutcome(r), Note: r.Note, Inspection: r.Inspection}
			if r.Type == "scan-interrupted" {
				row.Status = r.Status
				interrupted = append(interrupted, row)
			} else {
				plans[r.Index] = row
			}
		case "result", "single-error", "single-failed", "automatic-failed":
			report.RenameMS += r.RenameMS
			row, ok := plans[r.Index]
			if !ok {
				continue
			}
			row.RenameMS += r.RenameMS
			row.RenameStatus, row.RenameNote = r.Status, r.Note
			if r.Move != nil {
				row.Path = r.Move.To
			}
			plans[r.Index] = row
		}
	}
	if err := scan.Err(); err != nil {
		return diagnosticReport{}, err
	}
	if !live && report.Clock.Stopped.IsZero() && !report.Clock.Started.IsZero() {
		report.Clock = stoppedClock(report.Clock, last, true)
	}
	report.Clock = report.Clock.at(time.Now())

	if !includeNames && report.LastError != "" {
		report.LastError = "The scan stopped with an error. See the local scan history for details."
	}
	report.Slowest = make([]timingRow, 0, 5)
	report.RecentIssues = make([]timingRow, 0, 10)
	collect := func(row timingRow) {
		if row.Inspection.TotalMS+row.RenameMS > 0 {
			report.Slowest = keepRows(report.Slowest, row, 5, func(a, b timingRow) bool {
				x, y := a.Inspection.TotalMS+a.RenameMS, b.Inspection.TotalMS+b.RenameMS
				return x > y || x == y && a.Index < b.Index
			})
		}
		if row.Note != "" || row.RenameNote != "" {
			report.RecentIssues = keepRows(report.RecentIssues, row, 10, func(a, b timingRow) bool { return a.Index > b.Index })
		}
	}
	for _, row := range plans {
		collect(row)
	}
	for _, row := range interrupted {
		collect(row)
	}
	if !includeNames {
		for _, rows := range [][]timingRow{report.Slowest, report.RecentIssues} {
			for i := range rows {
				row := &rows[i]
				row.Path, row.Inspection.Detail = "", ""
				row.Inspection.PublicationSource = ""
				row.Inspection.Checklist = nil
				row.Inspection.Location = nil
				switch {
				case row.Status == "Interrupted":
					row.Note = "Inspection paused or interrupted"
				case strings.Contains(row.Note, "conflict"):
					row.Note = "Naming conflict"
				case row.Inspection.AI == "Timed out":
					row.Note = "Local model timed out"
				case row.Note != "":
					row.Note = "Flagged for review"
				}
				if strings.Contains(row.RenameNote, "Rename failed:") {
					row.RenameNote = "Rename failed"
				} else {
					row.RenameNote = ""
				}
			}
		}
	}
	return report, nil
}

func planOutcome(r record) string {
	if r.Decision != "" {
		return r.Decision
	}
	switch {
	case r.Stamp.ID == "":
		return "Skipped"
	case r.Move != nil:
		return "Proposed change"
	case r.Note != "":
		return "Flagged"
	default:
		return "Kept existing name"
	}
}

func keepRows(rows []timingRow, row timingRow, limit int, before func(timingRow, timingRow) bool) []timingRow {
	for i, other := range rows {
		if before(row, other) {
			rows = slices.Insert(rows, i, row)
			return rows[:min(len(rows), limit)]
		}
	}
	if len(rows) < limit {
		return append(rows, row)
	}
	return rows
}

func csvCell(s string) string {
	if len(s) > 0 && strings.ContainsRune("=+-@\t\r\n", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (a *application) exportInspections(w *csv.Writer, unknown bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.s == nil || a.v.Busy && !a.scanActive && a.v.Phase != "reassessing" {
		return errors.New("The inspection report is unavailable during this operation")
	}
	if err := w.Write([]string{"Current path", "Proposed path", "Text or listing", "Metadata", "Image decoded", "Local AI", "Model", "Unknown publication fields", "Read ms", "OCR ms", "Model startup ms", "Local model ms", "Rename ms", "Outcome", "Notes", "Classification", "Naming decision", "Decision reason", "Cache", "AI contribution", "Total ms", "Publication date lookup", "Publication source", "Online lookup ms", "Classification checklist"}); err != nil {
		return err
	}
	for _, r := range a.s.plans {
		if a.s.manifest.Items[r.Index].Folder || unknown && !r.Unknown {
			continue
		}
		var proposed string
		if r.Move != nil {
			proposed = r.Move.To
		}
		checked := r.Inspection
		yesNo := func(b bool) string {
			if b {
				return "Yes"
			}
			return "No"
		}
		if checked.AI == "" {
			checked.AI = "Not recorded"
		}
		outcome, note, renameMS := planOutcome(r), r.Note, int64(0)
		if result, ok := a.s.outcomes[r.Index]; ok {
			outcome, note, renameMS = result.Status, result.Note, result.RenameMS
		}
		if err := w.Write([]string{csvCell(a.s.currentPath(r.Path, r.Index)), csvCell(proposed),
			yesNo(checked.Text), yesNo(checked.Metadata), yesNo(checked.Image), checked.AI, checked.Model, yesNo(r.Unknown),
			strconv.FormatInt(checked.ReadMS, 10), strconv.FormatInt(checked.OCRMS, 10), strconv.FormatInt(checked.StartupMS, 10), strconv.FormatInt(checked.ModelMS, 10),
			strconv.FormatInt(renameMS, 10), csvCell(outcome), csvCell(note), csvCell(r.Kind), csvCell(planOutcome(r)), csvCell(r.Reason), csvCell(checked.Cache), csvCell(checked.AIContribution), strconv.FormatInt(checked.TotalMS, 10), csvCell(checked.PublicationDate), csvCell(checked.PublicationSource), strconv.FormatInt(checked.OnlineMS, 10), csvCell(checklistText(checked.Checklist))}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func (a *application) export(w *csv.Writer) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy {
		return errors.New("Pause the run before exporting flags")
	}
	if a.s == nil {
		return errors.New("There is no run to export")
	}
	if err := w.Write([]string{"Current path", "Action", "Reason"}); err != nil {
		return err
	}
	for _, r := range a.s.flagRows() {
		status := r.Status
		if r.Type == "plan" {
			status = "Review"
		}
		if err := w.Write([]string{csvCell(a.s.currentPath(r.Path, r.Index)), status, csvCell(r.Note)}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
