package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type testHistoryRow struct {
	testReceipt
	Name string `json:"name"`
}

func (a *application) listTests() []testHistoryRow {
	rows := make([]testHistoryRow, 0)
	dirs, _ := os.ReadDir(filepath.Join(a.home, "tests"))
	for i := len(dirs) - 1; i >= 0; i-- {
		if !dirs[i].IsDir() {
			continue
		}
		job, err := a.readTest(dirs[i].Name())
		if err != nil {
			continue
		}
		receipt, err := a.testStatus(job.Job)
		if err == nil {
			rows = append(rows, testHistoryRow{receipt, job.Plan.Name})
		}
	}
	return rows
}

func (a *application) registerTestRoutes(mux *http.ServeMux, auth func(http.ResponseWriter, *http.Request) bool) {
	mux.HandleFunc("GET /api/tests", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			reply(w, a.listTests())
		}
	})
	mux.HandleFunc("GET /api/test-plan", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		job, err := a.readTest(r.URL.Query().Get("job"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		reply(w, job.Plan)
	})
	mux.HandleFunc("POST /api/test-plan", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var request struct {
			Plan  *testPlan `json:"plan"`
			Job   string    `json:"job"`
			Start bool      `json:"start"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
		decoder.DisallowUnknownFields()
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || decoder.Decode(&request) != nil ||
			(request.Plan == nil) == (request.Job == "") || request.Job != "" && !request.Start {
			http.Error(w, "Supply a test plan or an approved saved job ID", http.StatusBadRequest)
			return
		}
		var receipt testReceipt
		var err error
		if request.Plan != nil {
			var job savedTest
			job, err = a.saveTestPlan(*request.Plan)
			receipt = job.testReceipt
		} else {
			receipt.Job = request.Job
		}
		if err == nil && request.Start {
			receipt, err = a.startTest(receipt.Job)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		reply(w, receipt)
	})
	mux.HandleFunc("POST /api/test-cancel", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		a.testMu.Lock()
		defer a.testMu.Unlock()
		if a.testCancel == nil || a.testActive.Job != r.URL.Query().Get("job") {
			http.Error(w, "The test is no longer running", http.StatusConflict)
			return
		}
		a.testCancel()
		reply(w, a.testActive)
	})
	mux.HandleFunc("GET /api/test-report", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		id := r.URL.Query().Get("job")
		receipt, err := a.testStatus(id)
		if err == nil && receipt.Report == "" {
			err = errors.New("The test report is not available yet")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		job, err := a.readTest(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		job.testReceipt = receipt
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"Renamer-Test-%s.md\"", id))
		_, _ = w.Write([]byte(renderTestReport(job, r.URL.Query().Get("full") == "1")))
	})
}

var testMarkdown = strings.NewReplacer("\\", "\\\\", "\n", " ", "\r", " ", "|", "\\|", "<", "&lt;", ">", "&gt;",
	"`", "\\`", "[", "\\[", "]", "\\]", "*", "\\*", "_", "\\_")

func renderTestReport(job savedTest, full bool) string {
	var out strings.Builder
	md := testMarkdown.Replace
	fmt.Fprintf(&out, "# Local File Renamer test report\n\n- Job ID: %s\n- Renamer version: %s\n- Started: %s\n- Status: %s\n- Frozen input: %d files, SHA-256 %s\n",
		job.Job, job.Version, job.Created.Format("2006-01-02 15:04:05 MST"), job.Status, job.InputFiles, job.InputHash)
	if full {
		fmt.Fprintf(&out, "- Plan: %s\n- Folder: %s\n- Success criteria: %s\n", md(job.Plan.Name), md(job.Plan.Root), md(job.Plan.Success))
	} else {
		out.WriteString("- Shareable report: retains relative filenames and measured evidence fields. Free-text evidence notes, absolute paths and document excerpts are excluded.\n")
	}
	if job.InputError != "" {
		out.WriteString("\nThe test could not complete its input or reporting step.\n")
		if full {
			fmt.Fprintf(&out, "\n%s\n", md(job.InputError))
		}
	}
	out.WriteString("\n## Variant results\n\n| Variant | Workflow | Model | Status | Files | Correct | Wrong | Deferred | Time (ms) | App peak (MiB) | Model peak (MiB) |\n")
	out.WriteString("|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for i, v := range job.Findings {
		fmt.Fprintf(&out, "| %s | %s | %s | %s | %d | %d | %d | %d | %d | %s | %s |\n",
			reportVariantName(job, i, full), v.Operation, md(v.Model), v.Status, v.Selected, v.Correct, v.Wrong, v.Deferred,
			v.ElapsedMS, testMemoryDisplay(v.PeakAppBytes), testMemoryDisplay(v.PeakModelBytes))
	}
	out.WriteString("\nCounts of correct, wrong, and deferred use supplied reference answers. App memory is sampled resident memory. Model memory is the peak reported by the OS for the model process. Variant time excludes staging the input copies.\n")
	if job.Summary != "" {
		fmt.Fprintf(&out, "\n## Local AI interpretation\n\n%s\n", md(job.Summary))
	}
	for i, v := range job.Findings {
		writeTestVariant(&out, job, i, v, full)
	}
	return out.String()
}

func reportVariantName(job savedTest, index int, full bool) string {
	if full {
		return testMarkdown.Replace(job.Plan.Variants[index].Name)
	}
	return fmt.Sprintf("Variant %d", index+1)
}

func testMemoryDisplay(bytes uint64) string {
	if bytes == 0 {
		return "unavailable"
	}
	return fmt.Sprintf("%.1f", float64(bytes)/(1<<20))
}

func writeTestVariant(out *strings.Builder, job savedTest, index int, finding testFinding, full bool) {
	md := testMarkdown.Replace
	variant := job.Plan.Variants[index]
	fmt.Fprintf(out, "\n### %s\n\n- Status: %s\n- Selection: %s\n- Apply on disposable copies: %t (%d ms)\n",
		reportVariantName(job, index, full), finding.Status, variant.Select, variant.Apply, finding.ApplyMS)
	if finding.Error != "" && full {
		fmt.Fprintf(out, "- Issue: %s\n", md(finding.Error))
	}
	if finding.Error != "" && !full {
		fmt.Fprintf(out, "- Outcome: %s. The full local report contains the issue detail.\n", finding.Status)
	}
	if variant.BaselineJob != "" {
		fmt.Fprintf(out, "- Source: saved Rename scan %s. Relative paths and bytes were verified. This scan predates the test runner's version and memory logging.\n", variant.BaselineJob)
	}
	writeTestCriteria(out, variant.Criteria, finding)
	modelRows, reusedRows, evidenceRows := 0, 0, 0
	for _, row := range finding.Rows {
		i := row.Inspection
		if i.ModelMS > 0 { modelRows++ }
		if i.Cache == "Assessment reused" || i.Cache == "Folder topic reused" { reusedRows++ }
		if i.Text || i.Metadata || i.Image { evidenceRows++ }
	}
	fmt.Fprintf(out, "- Rows with recorded model time: %d; cached assessment rows: %d; rows with readable evidence: %d. These are row counts, not exact inference request counts.\n", modelRows, reusedRows, evidenceRows)
	opts := job.Settings[index]
	if !full {
		opts.AI.Instructions = ""
		opts.Naming.Spelling = nil
		opts.Naming.Protected = nil
		opts.Scope = scanScope{Subfolders: opts.Scope.Subfolders}
	}
	settings, _ := json.MarshalIndent(opts, "", "  ")
	fmt.Fprintf(out, "\n#### Effective settings\n\n```json\n%s\n```\n", settings)
	baseline := slices.IndexFunc(job.Findings[:index], func(f testFinding) bool { return f.Name == variant.From && f.Operation == finding.Operation })
	if variant.From == "" {
		baseline = slices.IndexFunc(job.Findings[:index], func(f testFinding) bool { return f.Operation == finding.Operation && f.Status == "finished" })
	}
	previous := make(map[string]testFileResult)
	if baseline >= 0 {
		for _, row := range job.Findings[baseline].Rows {
			previous[testPathKey(row.File)] = row
		}
		fmt.Fprintf(out, "\nCompared with %s on overlapping files.\n", reportVariantName(job, baseline, full))
	}
	if len(finding.Rows) > 0 {
		out.WriteString("\n| File | Result | Reference | Verdict | Status | Change from comparison | Evidence and timing |\n|---|---|---|---|---|---|---|\n")
	}
	for _, row := range finding.Rows {
		delta := ""
		if before, ok := previous[testPathKey(row.File)]; ok {
			if before.Result == row.Result && before.Status == row.Status && before.Verdict == row.Verdict {
				delta = "Same result"
			} else {
				delta = fmt.Sprintf("%s (%s) → %s (%s)", before.Result, before.Status, row.Result, row.Status)
			}
			delta += fmt.Sprintf(", %+.0f ms", float64(row.Inspection.TotalMS-before.Inspection.TotalMS))
		}
		evidence := testEvidenceSummary(row, full)
		fmt.Fprintf(out, "| %s | %s | %s | %s | %s | %s | %s |\n", md(row.File), md(row.Result), md(row.Expected),
			row.Verdict, md(row.Status), md(delta), md(evidence))
	}
	if len(finding.Groups) > 0 {
		out.WriteString("\n| Duplicate group | Members | App keeper | Rule keeper | Comparison |\n|---|---|---|---|---|\n")
		for _, group := range finding.Groups {
			fmt.Fprintf(out, "| %s | %s | %s | %s | %s |\n", group.Kind, md(strings.Join(group.Members, ", ")),
				md(group.Keeper), md(group.Expected), md(joinNote(group.Verdict, group.Reason)))
		}
	}
}

func testEvidenceSummary(row testFileResult, full bool) string {
	i := row.Inspection
	result := fmt.Sprintf("text=%t, metadata=%t, image=%t, read=%d ms, OCR=%d ms, model=%d ms, online=%d ms",
		i.Text, i.Metadata, i.Image, i.ReadMS, i.OCRMS, i.ModelMS, i.OnlineMS)
	for _, detail := range []string{i.AIContribution, i.Cache, i.OCR} {
		result = joinNote(result, detail)
	}
	for _, check := range i.Checklist {
		value := check.Type + "=" + check.Result
		if full {
			value = joinNote(value, check.Reason)
		}
		result = joinNote(result, value)
	}
	if loc := i.Location; loc != nil {
		result = joinNote(result, fmt.Sprintf("current score %d", loc.Current.Score))
		for _, match := range loc.Alternatives {
			result = joinNote(result, fmt.Sprintf("%s scored %d", match.Path, match.Score))
		}
	}
	if full {
		for _, detail := range []string{row.Reason, row.Note, row.Applied, i.Detail, i.PublicationDate, i.PublicationSource} {
			result = joinNote(result, detail)
		}
	}
	return result
}

func writeTestCriteria(out *strings.Builder, criteria testCriteria, result testFinding) {
	for _, check := range []struct {
		name, relation string
		target         *int
		actual         int
		satisfied      func(int, int) bool
	}{
		{"correct", "at least", criteria.MinCorrect, result.Correct, func(actual, target int) bool { return actual >= target }},
		{"wrong", "at most", criteria.MaxWrong, result.Wrong, func(actual, target int) bool { return actual <= target }},
		{"deferred", "at most", criteria.MaxDeferred, result.Deferred, func(actual, target int) bool { return actual <= target }},
	} {
		if check.target == nil {
			continue
		}
		status := "unverified"
		if result.Status == "finished" && result.Correct+result.Wrong+result.Deferred > 0 {
			status = testCriterionOutcome(check.satisfied(check.actual, *check.target))
		}
		fmt.Fprintf(out, "- Criterion: %s %d %s on labelled files. Observed %d (%s).\n", check.relation, *check.target, check.name, check.actual, status)
	}
	if criteria.MaxElapsedMS != nil {
		status := "unverified"
		if result.Status == "finished" {
			status = testCriterionOutcome(result.ElapsedMS <= *criteria.MaxElapsedMS)
		}
		fmt.Fprintf(out, "- Criterion: at most %d ms. Observed %d ms (%s).\n", *criteria.MaxElapsedMS, result.ElapsedMS, status)
	}
}

func testCriterionOutcome(ok bool) string {
	if ok {
		return "met"
	}
	return "missed"
}
