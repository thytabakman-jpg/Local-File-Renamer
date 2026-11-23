package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func Test36ModelPreservesLocalFolderEvidence(t *testing.T) {
	e := evidence{Title: "Client Consent", Text: "MCC 642 Coursework\nTreatment Planning",
		FollowupText: "Counselling Intake Interview"}
	local := topicFromEvidence(e, assessment{}, false)
	withModel := topicFromEvidence(e, assessment{Title: "Client Consent", Label: "Clinical Forms"}, true)
	if !slices.Equal(local, withModel[:len(local)]) ||
		!slices.Contains(withModel, "coursework") || !slices.Contains(withModel, "interview") || !slices.Contains(withModel, "clinical") {
		t.Fatalf("local=%v with model=%v", local, withModel)
	}
}

func Test36MoverUsesLaterEvidenceBeforeModel(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"Inbox", "Clinical Intake", "Training Review"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "Inbox", "Read This.pdf")
	writeTestPDFContent(t, path, "BT /F1 12 Tf 72 700 Td (Clinical Intake Client Documentation) Tj ET\n", "<< >>")
	opts := defaultSettings()
	opts.Evidence.PDFPages = 0 // Simulate the opening sample having omitted the relevant page.
	opts.Evidence.ExtraPDFPages = 1
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Operation: scanMove, SortFolders: true, Settings: &opts})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	d := inspectLocation(context.Background(), s, func(context.Context, string, evidence) (assessment, error) {
		t.Fatal("a model is unnecessary once the later evidence distinguishes the folder")
		return assessment{}, nil
	}, opts, path, filepath.Base(path), "", preparedEvidence{}, evidence{Text: "General document"}, "", 0, nil, false)
	if d.Folder != filepath.Join(root, "Clinical Intake") || !strings.Contains(d.Inspection.Detail, "resolved the folder decision") {
		t.Fatalf("unexpected decision: %+v", d)
	}
}

func Test36TestPlanThroughMCPAndReportHistory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "Private Test Input")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	body := "Specimen File\nDistinctive text for this test."
	inbox := filepath.Join(root, "Inbox")
	destination := filepath.Join(root, "Clinical Intake")
	for _, dir := range []string{inbox, destination} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(root, "SPECIMEN_FILE.txt"), body)
	writeTestFile(t, filepath.Join(root, "Specimen File Copy.txt"), body)
	writeTestFile(t, filepath.Join(root, "empty.txt"), "")
	writeTestFile(t, filepath.Join(inbox, "Intake form.txt"), "Clinical Intake\nClient admission form and procedure.")
	a, err := newApp(filepath.Join(base, "data"), filepath.Join(base, "engine"), filepath.Join(base, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.chat.cancel()
	ref := "Specimen File.txt"
	moveRef := "Clinical Intake"
	plan := testPlan{Name: "File comparison", Root: root, KeeperRule: testKeeperRule{Mode: "no_copy_suffix"},
		Expected: []testExpectation{{File: "SPECIMEN_FILE.txt", Name: &ref}, {File: "Inbox/Intake form.txt", Folder: &moveRef}},
		Variants: []testVariant{
			{Name: "Rename", Operation: scanRename, Apply: true, Settings: []chatSettingChange{{Key: "ai.model", Value: "off"}}},
			{Name: "Retry", Operation: scanRename, From: "Rename", Select: "unresolved"},
			{Name: "Duplicates", Operation: scanDuplicates, Apply: true},
			{Name: "Move", Operation: scanMove, Apply: true, Settings: []chatSettingChange{{Key: "ai.model", Value: "off"}}},
		},
	}
	if _, err := a.bridgeAction(bridgeCommand{Action: "test_start", Test: &plan}); err == nil {
		t.Fatal("MCP started without approval")
	}
	response, err := a.bridgeAction(bridgeCommand{Action: "test_start", Authorized: true, Test: &plan})
	if err != nil {
		t.Fatal(err)
	}
	receipt := response.(testReceipt)
	a.testMu.Lock()
	done := a.testDone
	a.testMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("test did not finish")
		}
	}
	job, err := a.readTest(receipt.Job)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "finished" || len(job.Findings) != 4 || job.Findings[0].Correct != 1 ||
		len(job.Findings[2].Groups) != 1 || job.Findings[3].Correct != 1 {
		for _, f := range job.Findings {
			t.Logf("%s: %s %s %d correct %+v %+v", f.Name, f.Status, f.Error, f.Correct, f.Groups, f.Rows)
		}
		t.Fatalf("test status=%s, input error=%q", job.Status, job.InputError)
	}
	if job.Findings[1].Selected >= job.Findings[0].Selected || job.Findings[1].Selected == 0 {
		t.Fatalf("conditional case selected %d of %d files", job.Findings[1].Selected, job.Findings[0].Selected)
	}
	if !slices.ContainsFunc(job.Findings[2].Rows, func(r testFileResult) bool { return r.Applied == "Removed disposable exact copy" }) {
		t.Fatal("exact duplicate apply did not use the disposable copies")
	}
	if !slices.ContainsFunc(job.Findings[3].Rows, func(r testFileResult) bool { return r.Applied != "" && r.File == "Inbox/Intake form.txt" }) {
		t.Fatal("move apply was not recorded")
	}
	for _, file := range []string{"SPECIMEN_FILE.txt", "Specimen File Copy.txt", "Inbox/Intake form.txt"} {
		if _, err := os.Stat(filepath.Join(root, file)); err != nil {
			t.Fatalf("original file changed: %s: %v", file, err)
		}
	}
	handler := server(a).Handler
	home := httptest.NewRecorder()
	handler.ServeHTTP(home, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:39173/", nil))
	response, err = a.bridgeAction(bridgeCommand{Action: "test_status", Job: receipt.Job})
	if err != nil {
		t.Fatal(err)
	}
	finished := response.(testReceipt)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:39173"+finished.Report, nil)
	req.AddCookie(home.Result().Cookies()[0])
	report := httptest.NewRecorder()
	handler.ServeHTTP(report, req)
	if report.Code != http.StatusOK || !strings.Contains(report.Body.String(), "Duplicate group") ||
		strings.Contains(report.Body.String(), root) || strings.Contains(report.Body.String(), body) {
		t.Fatalf("incorrect shareable report response: %d %s", report.Code, report.Body.String())
	}
	raw, _ := json.Marshal(finished)
	if len(raw) > 450 || strings.Contains(string(raw), "SPECIMEN_FILE") {
		t.Fatalf("MCP returned details: %s", raw)
	}
}

func Test36RecommendationRespectsPublicationEvidence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "document.pdf")
	writeTestPDF(t, path)
	opts := defaultSettings()
	opts.AI.Model = "off"
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Operation: scanRename, Preview: true, Settings: &opts})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	st, err := fingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.append(record{Type: "plan", Index: 0, Path: path, Stamp: st,
		Kind: "article", Decision: "Needs review", Note: "Missing journal"}); err != nil {
		t.Fatal(err)
	}
	a := &application{s: s}
	a.chat.init()
	defer a.chat.cancel()
	a.syncSession(true)
	names, err := a.recommendedNames(context.Background(), a.v.Job, a.v.Revision, 0)
	if err != nil || len(names) != 0 {
		t.Fatalf("publication recommendation bypassed citation evidence: %v, %v", names, err)
	}
}

func Test36ApplyAllAcrossReviewPagesAndRejectStaleCount(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 106; i++ {
		name := fmt.Sprintf("EXAMPLE_FILE_%03d.txt", i)
		writeTestFile(t, filepath.Join(root, name), "Original bytes for the rename")
	}
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Operation: scanRename, Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for i, row := range s.manifest.Items {
		st, err := fingerprint(row.Path)
		if err != nil {
			t.Fatal(err)
		}
		proposed := strings.ReplaceAll(filepath.Base(row.Path), "_", " ")
		plan := buildSuggestion(s, i, decision{Name: proposed}, st, false)
		if err := s.append(plan); err != nil {
			t.Fatal(err)
		}
	}
	a := &application{s: s}
	a.syncSession(true)
	id, rev := a.v.Job, a.v.Revision
	if err := a.applyAllReviewed(id, rev, "all", "", 100); err == nil {
		t.Fatal("a confirmation for a partial count applied")
	}
	if err := a.applyAllReviewed(id, rev, "all", "", 106); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "complete")
	if _, err := os.Stat(filepath.Join(root, "EXAMPLE FILE 105.txt")); err != nil {
		t.Fatal("the file beyond the first review page was not renamed", err)
	}
}

func Test36ApplyChosenNamesValidatesEveryFileBeforeEditing(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "First.txt"), "First source")
	writeTestFile(t, filepath.Join(root, "Second.txt"), "Second source")
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Operation: scanRename, Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for i, row := range s.manifest.Items {
		st, err := fingerprint(row.Path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.append(record{Type: "plan", Index: i, Path: row.Path, Stamp: st, Decision: "Needs review"}); err != nil {
			t.Fatal(err)
		}
	}
	a := &application{s: s}
	a.syncSession(true)
	batch := []chosenName{{0, s.planVersions[0], "New First.txt"}, {1, s.planVersions[1], "New First.txt"}}
	if err := a.applyChosenNames(a.v.Job, batch); err == nil || s.plans[0].Move != nil {
		t.Fatalf("the duplicate destination partially updated the batch: %v", err)
	}
	batch[1].Name = "New Second.txt"
	if err := a.applyChosenNames(a.v.Job, batch); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "complete")
	for _, path := range []string{"New First.txt", "New Second.txt"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
}
