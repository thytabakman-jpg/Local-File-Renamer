package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func Test35MoveGroupsIntakeFormsAndPreservesPunctuation(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "Inbox")
	kept := filepath.Join(root, "Clinical Scheduling")
	for _, folder := range []string{inbox, kept} {
		if err := os.Mkdir(folder, 0700); err != nil {
			t.Fatal(err)
		}
	}
	expected := make(map[string]string)
	pdf := func(dir, title, destination string) {
		name := title + ".pdf"
		writeTestPDFContent(t, filepath.Join(dir, name),
			fmt.Sprintf("BT /F1 12 Tf 72 700 Td (%s) Tj ET BT /F1 12 Tf 72 675 Td (Client information and practical guidance.) Tj ET\n", title),
			fmt.Sprintf("<< /Title (%s) >>", title))
		expected[name] = destination
	}
	for i := range 11 {
		pdf(inbox, fmt.Sprintf("Counseling Intake Form %02d", i+1), "Counseling Intake")
	}
	pdf(inbox, "Intake Form V2", "Counseling Intake")
	pdf(inbox, "Ergonomics & Bullying Guide", "Ergonomics & Bullying")
	pdf(inbox, "Ergonomics & Bullying Notes", "Ergonomics & Bullying")
	pdf(kept, "Clinical Scheduling Clinic", "")
	pdf(inbox, "Unrelated File With No Match", "")

	opts := defaultSettings()
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Operation: scanMove, Preview: true, SortFolders: true, Settings: &opts})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var calls []string
	assess := func(_ context.Context, name string, _ evidence) (assessment, error) {
		calls = append(calls, name)
		return assessment{}, modelFailure{modelTimeout, "local model request reached its time limit"}
	}
	for i := 0; i < s.fileCount; i++ {
		d, st, err := inspect(context.Background(), s, assess, nil, i)
		name := filepath.Base(s.manifest.Items[i].Path)
		plan := s.buildMoveSuggestion(i, d, st, false)
		want := expected[name]
		if err != nil || want == "" && plan.Move != nil || want != "" && (plan.Move == nil || filepath.Dir(plan.Move.To) != filepath.Join(root, want)) {
			t.Fatalf("%s expected destination %q: %+v (%v)", name, want, plan, err)
		}
		if want != "" && (!strings.Contains(plan.Reason, "matches") || !strings.Contains(plan.Reason, want)) {
			t.Fatalf("%s lacks a specific move reason: %s", name, plan.Reason)
		}
	}
	if len(calls) != 1 || calls[0] != "Unrelated File With No Match.pdf" {
		t.Fatalf("expected a model call only for the ambiguous file, got %v", calls)
	}
}

func Test35SharesLocalPDFEvidenceAndKeepsModelTasksSeparate(t *testing.T) {
	ctx := context.Background()
	root, home, cache := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Possible Destination"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTestPDF(t, filepath.Join(root, "document.pdf"))
	renameOpts := defaultSettings()
	renameOpts.AI.Invoke = "all"
	moveOpts := renameOpts
	moveOpts.Evidence.OnlineDates = "off"
	scan := func(op scanOperation, opts scanSettings) *session {
		s, err := createSession(ctx, home, root, manifest{Operation: op, Preview: true, SortFolders: op == scanMove, Settings: &opts})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.close)
		s.cacheDir = cache
		return s
	}
	renamer, mover := scan(scanRename, renameOpts), scan(scanMove, moveOpts)
	calls := 0
	assess := func(context.Context, string, evidence) (assessment, error) {
		calls++
		return assessment{Label: "Trees and Rivers"}, nil
	}
	for _, attempt := range []struct {
		s     *session
		cache string
	}{{renamer, ""}, {mover, "Evidence reused"}, {renamer, "Assessment reused"}, {mover, "Folder topic reused"}} {
		d, _, err := inspect(ctx, attempt.s, assess, nil, 0)
		if err != nil || d.Inspection.Cache != attempt.cache {
			t.Fatalf("%s cache=%q expected %q (%v)", attempt.s.manifest.Operation, d.Inspection.Cache, attempt.cache, err)
		}
	}
	if calls != 2 {
		t.Fatalf("expected one call per model task, got %d", calls)
	}
}

func Test35VerifiesModelTitleSpanningOpeningLines(t *testing.T) {
	title := "Transitions Between Working Memory and Cognitive Flexibility"
	d := decide("document.pdf", evidence{Text: "Transitions Between Working Memory\nand Cognitive Flexibility\nSam Example\nSummary of the project."},
		assessment{Title: title}, true)
	if d.Name != title+".pdf" {
		t.Fatalf("full title not used: %+v", d)
	}
	if titleInFirstLines(title, "Transitions Between Working Memory\nSam Example\nand Cognitive Flexibility") {
		t.Fatal("title verification crossed an unrelated line")
	}
}

func Test35ExactBatchThenReplayAfterInterruptedRecycle(t *testing.T) {
	for _, test := range []struct {
		name    string
		recycle func(string) error
	}{{"disposable", os.Remove}, {"Windows Recycle Bin", recycleFile}} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "Windows Recycle Bin" && !recycleAvailable {
				t.Skip("Windows fixture")
			}
			root, home := t.TempDir(), t.TempDir()
			for _, name := range []string{"Original.txt", "Original Copy.txt", "Original Copy 2.txt"} {
				writeScanFile(t, filepath.Join(root, name), "exact duplicate contents")
			}
			newApplication := func() *application {
				a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { a.s.close(); a.chat.cancel() })
				return a
			}
			a := newApplication()
			if err := a.startUIOperation(root, true, false, false, false, false, nil, nil, false, scanDuplicates); err != nil {
				t.Fatal(err)
			}
			v := waitForOperation(t, a)
			batch, plan := a.s.exactBatch(a.s.duplicateResults().Exact)
			if plan.Copies != 2 || plan.Groups != 1 {
				t.Fatalf("unexpected batch: %+v", plan)
			}
			if _, err := a.applyExactBatch(t.Context(), v.Job, plan.PlanID, plan.Revision+1, test.recycle); err == nil {
				t.Fatal("accepted a stale revision")
			}
			// Simulate a process stopping at the precise crash point after the
			// first Recycle Bin call, before its durable result append.
			if err := a.s.append(record{Type: "exact-batch-approved", ExactBatch: &batch}); err != nil {
				t.Fatal(err)
			}
			first := a.s.plans[batch.Groups[0].Indices[0]]
			if err := a.s.append(record{Type: "recycle-intent", Index: first.Index, Path: first.Path, Stamp: first.Stamp, BatchID: batch.ID}); err != nil {
				t.Fatal(err)
			}
			if err := test.recycle(first.Path); err != nil {
				t.Fatal(err)
			}
			a.s.close()
			a.s = nil
			reopened := newApplication()
			var paths []string
			recycle := func(path string) error {
				paths = append(paths, path)
				return test.recycle(path)
			}
			receipt, err := reopened.applyExactBatch(t.Context(), v.Job, plan.PlanID, plan.Revision, recycle)
			if err != nil || receipt.Error != "" || receipt.PreviouslyRecycled != 1 || receipt.NewlyRecycled != 1 ||
				receipt.Remaining != 0 || len(paths) != 1 || paths[0] == first.Path {
				t.Fatalf("retry: %+v (%v), recycle calls %v", receipt, err, paths)
			}
			receipt, err = reopened.applyExactBatch(t.Context(), v.Job, plan.PlanID, plan.Revision, recycle)
			if err != nil || receipt.PreviouslyRecycled != 2 || receipt.NewlyRecycled != 0 || len(paths) != 1 {
				t.Fatalf("replay: %+v (%v), recycle calls %v", receipt, err, paths)
			}
			status, err := reopened.bridgeStatus(v.Job)
			if err != nil || status["FilesChecked"] != 3 || status["RecycledCopies"] != 2 || status["ExtraCopies"] != 0 || status["Errors"] != 0 {
				t.Fatalf("status=%v (%v)", status, err)
			}
			if _, err := os.Stat(filepath.Join(root, "Original.txt")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func Test35BridgeResultsPageExactAndUnresolvedMembers(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	for i := range 136 {
		writeScanFile(t, filepath.Join(root, fmt.Sprintf("Exact Copy %03d.txt", i)), "same bytes")
	}
	writeScanFile(t, filepath.Join(root, "Similar V1.txt"), "first revision")
	writeScanFile(t, filepath.Join(root, "Similar V2.txt"), "second revision")
	a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close(); a.chat.cancel() }()
	if err := a.startUIOperation(root, true, false, false, false, false, nil, nil, false, scanDuplicates); err != nil {
		t.Fatal(err)
	}
	v := waitForOperation(t, a)
	for filter, count := range map[string]int{"exact_duplicates": 136, "possible_duplicates": 2} {
		seen := make(map[int]bool)
		cmd := bridgeCommand{Job: v.Job, Filter: filter, Limit: 25, IncludeNames: true}
		for {
			page, err := a.bridgeResults(cmd)
			if err != nil || page.Total != count {
				t.Fatalf("%s %+v (%v)", filter, page, err)
			}
			body, _ := json.Marshal(page)
			if _, err := readBridgeResponse(http.StatusOK, bytes.NewReader(body)); err != nil {
				t.Fatal(err)
			}
			for _, row := range page.Rows {
				if row.Index == nil || seen[*row.Index] || row.Path == "" {
					t.Fatalf("missing or repeated member: %+v", row)
				}
				seen[*row.Index] = true
			}
			if page.NextOffset == 0 {
				break
			}
			cmd.Offset, cmd.Revision = page.NextOffset, &page.Revision
		}
		if len(seen) != count {
			t.Fatalf("%s got %d of %d", filter, len(seen), count)
		}
	}
	status, err := a.bridgeStatus(v.Job)
	data, _ := json.Marshal(status)
	if err != nil || status["ExactGroups"] != 1 || status["ExtraCopies"] != 135 || status["PossibleGroups"] != 1 || bytes.Contains(data, []byte("Exact Copy")) || strings.Contains(string(data), "Root") {
		t.Fatalf("status=%s (%v)", data, err)
	}
	if _, err := readBridgeResponse(http.StatusOK, strings.NewReader(strings.Repeat(" ", bridgeResponseLimit)+"{}")); err == nil {
		t.Fatal("accepted an oversized response")
	}
	if _, err := readBridgeResponse(http.StatusOK, strings.NewReader(`{"Rows":[`)); err == nil {
		t.Fatal("accepted cut-off JSON")
	}
}

func Test35LiveReportAndDiagnosticsUseRunningClock(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.chat.cancel()
	s, err := createSession(t.Context(), filepath.Join(home, "runs"), root, manifest{Operation: scanMove})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a.s = s
	a.scanJobs()
	a.v.Busy = true
	if err := a.startClock(s, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	a.syncSession(true)
	report, reportErr := a.report(a.v.Job)
	diagnostic, diagnosticErr := a.diagnostic(a.v.Job, false)
	if reportErr != nil || diagnosticErr != nil || report.Clock.ActiveMS < 60000 ||
		diagnostic.Clock.ActiveMS < report.Clock.ActiveMS || diagnostic.Clock.ActiveMS-report.Clock.ActiveMS > 1000 {
		t.Fatalf("report=%+v (%v) diagnostic=%+v (%v)", report.Clock, reportErr, diagnostic.Clock, diagnosticErr)
	}
}

func Test35BridgeApplyMovesAllowsReviewedFolderOverride(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	inbox := filepath.Join(root, "Inbox")
	if err := os.Mkdir(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Counseling Intake Form 01.pdf", "Intake Form V2.pdf", "Optional Intake Copy.pdf"} {
		writeTestPDF(t, filepath.Join(inbox, name))
	}
	a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close(); a.chat.cancel() }()
	opts := defaultSettings()
	s, err := createSession(t.Context(), filepath.Join(home, "runs"), root, manifest{Operation: scanMove, Preview: true, SortFolders: true, Settings: &opts})
	if err != nil {
		t.Fatal(err)
	}
	a.s = s
	indices := make(map[string]int)
	counseling := filepath.Join(root, "Counseling Intake")
	separate := filepath.Join(root, "Intake Form")
	for index, item := range s.manifest.Items {
		name := filepath.Base(item.Path)
		st, err := fingerprint(item.Path)
		if err != nil {
			t.Fatal(err)
		}
		d := decision{}
		if !item.Folder {
			indices[name] = index
			d.NewFolder, d.Folder = true, counseling
			if name == "Intake Form V2.pdf" {
				d.Folder = separate
			}
		}
		if err := s.append(s.buildMoveSuggestion(index, d, st, false)); err != nil {
			t.Fatal(err)
		}
	}
	a.scanJobs()
	a.syncSession(true)
	index := indices["Intake Form V2.pdf"]
	first := indices["Counseling Intake Form 01.pdf"]
	selected := []int{first, index}
	job, revision := a.v.Job, a.v.Revision
	if _, err := a.bridgeAction(bridgeCommand{Action: "apply", Job: job, Revision: &revision, Indices: selected}); err == nil {
		t.Fatal("unauthorised move")
	}
	stale := revision - 1
	if _, err := a.bridgeAction(bridgeCommand{Action: "apply", Job: job, Revision: &stale, Indices: selected, Authorized: true}); err == nil {
		t.Fatal("accepted stale review")
	}
	cmd := bridgeCommand{Action: "apply", Job: job, Revision: &revision, Indices: selected, Authorized: true,
		Destinations: []moveDestinationOverride{{Index: &index, Destination: "Unknown Folder"}}}
	result, err := a.bridgeAction(cmd)
	if err != nil {
		t.Fatal(err)
	}
	rejected := result.(moveApplyReceipt)
	if rejected.Moved != 0 || rejected.Failed != 1 || rejected.Remaining != 1 || rejected.Failures[0].Index != index {
		t.Fatalf("invalid destination receipt: %+v", rejected)
	}
	if _, err := os.Stat(counseling); !os.IsNotExist(err) {
		t.Fatalf("preflight created the folder: %v", err)
	}
	cmd.Destinations[0].Destination = "Counseling Intake"
	result, err = a.bridgeAction(cmd)
	if err != nil {
		t.Fatal(err)
	}
	receipt := result.(moveApplyReceipt)
	if receipt.Moved != 2 || receipt.Failed != 0 || receipt.Remaining != 0 {
		t.Fatalf("receipt: %+v", receipt)
	}
	for _, name := range []string{"Counseling Intake Form 01.pdf", "Intake Form V2.pdf"} {
		if _, err := os.Stat(filepath.Join(counseling, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(inbox, "Optional Intake Copy.pdf")); err != nil {
		t.Fatal("moved an unapproved file:", err)
	}
	if _, err := os.Stat(separate); !os.IsNotExist(err) {
		t.Fatalf("created the superseded destination: %v", err)
	}
	reopened, err := openSessionReadOnly(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if reopened.outcomes[index].Move.To != filepath.Join(counseling, "Intake Form V2.pdf") {
		t.Fatal("overridden move was not saved durably")
	}
}
