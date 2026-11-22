package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func Test25PreservesCitationDetails(t *testing.T) {
	name := "Kvam, Schearing, Ziegler, 1999, Eve & Adam.pdf"
	body := evidence{Text: "ISBN 9780253212719\nAcademic Press\nCopyright notice"}
	d := decide(name, body, assessment{}, false)
	if d.Name != name || d.Kind != "academic_book" || !d.Unknown || d.AutoEvidence || !strings.Contains(d.Note, "preserve") {
		t.Fatalf("unsafe citation decision: %+v", d)
	}
	article := "Believers' estimates of God's beliefs are more egocentric than estimates of other people's beliefs"
	body = evidence{Text: "PNAS\n" + article + "\nAuthors: Nicholas Epley; Benjamin A. Converse\nPublished 2009\nDOI: 10.1073/pnas.0908374106"}
	d = decide(article+".pdf", body, assessment{}, false)
	if d.Kind != "article" || d.Unknown || !d.AutoEvidence || !strings.Contains(d.Name, "2009") || !strings.Contains(d.Name, "PNAS") {
		t.Fatalf("published article was not recognized and completed: %+v", d)
	}
	d = decide(article+".pdf", evidence{Text: "PNAS\n" + article + "\nDOI: 10.1073/pnas.0908374106\nVolume 106"}, assessment{}, false)
	if d.Kind != "article" || d.Name != article+".pdf" || !d.Unknown || !strings.Contains(d.Note, "Missing author") {
		t.Fatalf("unresolved citation was not flagged: %+v", d)
	}
}

func Test25EvidenceGatesAndImageAliases(t *testing.T) {
	title := "Are Therapeutic Frameworks Fundamentally Compatible - Psychology and Philosophy Slides.pdf"
	e := evidence{Author: "Tzvi Tabakman", Text: "Independent research presentation\nEvidence on therapeutic frameworks"}
	d := decide(title, e, assessment{Kind: "course_paper", Course: "MCC 642"}, true)
	if d.Kind != "presentation" || d.Name != title {
		t.Fatal(d)
	}
	d = decide("Scope - ICC 118.pdf", evidence{Text: "Independent Research\nTzvi Tabakman"}, assessment{Kind: "course_paper"}, true)
	if d.Kind == "course_paper" {
		t.Fatal(d)
	}
	d = decide("Conceptual Formulation.pdf", evidence{Text: "Conceptual Formulation\nCourse: MCC 642\nTzvi Tabakman"}, assessment{}, false)
	if d.Kind != "course_paper" || !strings.Contains(d.Note, versionPendingNote) || d.AutoEvidence {
		t.Fatal(d)
	}
	d = decide("Reading Stratgeies V2.pdf", evidence{Title: "Reading Strategies"}, assessment{}, false)
	if d.Name != "Reading Strategies V2.pdf" {
		t.Fatal(d)
	}
	d = decide("Reading Strategies for Teens.pdf", evidence{Title: "Reading Strategies"}, assessment{Mismatch: true}, true)
	if d.Name != "Reading Strategies for Teens.pdf" || d.Outcome == "Name verified" {
		t.Fatal(d)
	}
	for _, suffix := range []string{".jfif", ".JPE", ".jpg", ".JPEG"} {
		path := filepath.Join(t.TempDir(), "photo"+suffix)
		write25JPEG(t, path)
		if e := readEvidence(path, true); len(e.Photo) == 0 || e.Note != "" {
			t.Fatalf("%s: %+v", suffix, e)
		}
	}
	if photoExtension("diagram.svg") || photoExtension("photo.heic") {
		t.Fatal("unsupported format advertised as decodable")
	}
}

func write25JPEG(t *testing.T, path string) {
	t.Helper()
	buf := bytes.Buffer{}
	img := image.NewRGBA(image.Rect(0, 0, 96, 64))
	img.Set(40, 24, color.RGBA{60, 140, 210, 255})
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func Test25BudgetAndRetryRecovery(t *testing.T) {
	var sizes []int
	var mode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var payload struct {
			Messages  []struct{ Content string }
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		size := 128 + payload.MaxTokens
		for _, m := range payload.Messages {
			size += len(m.Content)
		}
		sizes = append(sizes, size)
		if len(sizes) == 1 {
			if mode == "context" {
				http.Error(w, "request exceeds context limit", 400)
				return
			}
			if mode == "invalid" {
				_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"{}"}}]}`)
				return
			}
			if mode == "unavailable" {
				http.Error(w, "service unavailable", 503)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"kind":"article","title":"Study on Rivers","authors":["Jane Doe"],"year":"2009","venue":"PNAS"}`}}}})
	}))
	defer server.Close()
	model := localModel{url: server.URL}
	for _, scenario := range []string{"context", "invalid", "unavailable"} {
		mode = scenario
		sizes = nil
		_, err := model.assess(context.Background(), "download.pdf", evidence{Text: "PNAS\nPublished 2009\n" + strings.Repeat("Test 字 文 §\n", 1200)})
		if sizes[0] > modelContextTokens {
			t.Fatalf("oversized request sent: %v", sizes)
		}
		if scenario == "unavailable" {
			if !hasModelFailure(err, modelUnavailable) || len(sizes) != 1 {
				t.Fatalf("%s: %v %v", scenario, err, sizes)
			}
			continue
		}
		if err != nil || len(sizes) != 2 || sizes[1] >= sizes[0] {
			t.Fatalf("%s: %v %v", scenario, err, sizes)
		}
	}
}

func Test25CacheInvalidation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	name := "Hydrology presentation.txt"
	path := filepath.Join(root, name)
	writeTestFile(t, path, "A hidden meaning in typical outputs\nGeneral text evidence.")
	opts := defaultSettings()
	opts.AI.Invoke = "all"
	s, err := createSession(ctx, t.TempDir(), root, manifest{Preview: true, Settings: &opts})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.cacheDir = t.TempDir()
	count := 0
	model := func(context.Context, string, evidence) (assessment, error) {
		count++
		return assessment{Kind: "other"}, nil
	}
	for round := 1; round <= 4; round++ {
		if round == 3 {
			opts.Naming.Spelling = map[string]string{"reciept": "receipt"}
		}
		if round == 4 {
			writeTestFile(t, path, "Text was changed.")
		}
		d, _, err := inspectFile(ctx, s, model, nil, 0, "", false, opts)
		if err != nil {
			t.Fatal(err)
		}
		expected := []int{1, 1, 2, 3}[round-1]
		if count != expected || round == 2 && d.Inspection.Cache != "Assessment reused" || round == 3 && d.Inspection.Cache != "Evidence reused" || round == 4 && d.Inspection.Cache != "" {
			t.Fatalf("round %d cache=%+v model calls=%d", round, d.Inspection, count)
		}
	}
}

func Test25ProfilesPermissionsAndComparison(t *testing.T) {
	base := t.TempDir()
	a, err := newApp(filepath.Join(base, "app"), filepath.Join(base, "engine"), filepath.Join(base, "models"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close() }()
	p, err := a.settingsInfo("")
	if err != nil || p.Defaults.AI.Model != "configured" || len(p.Fields) < 35 {
		t.Fatalf("%+v %v", p, err)
	}
	opts := p.Effective
	opts.Scope.VerifyComparison = true
	opts.Scope.IncludeExtensions = []string{".txt", ".jfif"}
	opts.Scope.ExcludePaths = []string{"Ignored"}
	opts.Resources.Workers = 3
	opts.Execution.Mode = "preview_only"
	opts.AI.Model = "off"
	if err = a.saveProfile(p.Revision, "Controlled", &opts, true); err != nil {
		t.Fatal(err)
	}
	if err = a.saveProfile(p.Revision, "Default", nil, true); err == nil {
		t.Fatal("stale profile update accepted")
	}
	p, err = a.settingsInfo("")
	if err != nil || p.Active != "Controlled" || p.Effective.Resources.Workers != 3 {
		t.Fatalf("%+v %v", p, err)
	}
	var jobs []string
	for round := 0; round < 2; round++ {
		root := filepath.Join(base, fmt.Sprint(round))
		if err = os.MkdirAll(filepath.Join(root, "Ignored"), 0700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 27; i++ {
			file := filepath.Join(root, fmt.Sprintf("Reading_Guide_%02d.txt", i))
			writeTestFile(t, file, fmt.Sprintf("Reading Guide %02d\nFixture notes for this file.", i))
		}
		write25JPEG(t, filepath.Join(root, "Butterfly.jfif"))
		writeTestFile(t, filepath.Join(root, "Ignored", "excluded.txt"), "Excluded from both scans.")
		writeTestFile(t, filepath.Join(root, "unsupported.bin"), "Excluded by extension.")
		start := time.Now()
		err = a.startWith(root, false, "", func(*scanSettings) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		waitForPhase(t, a, "preview")
		v := a.snapshot()
		report, err := a.report(v.Job)
		if err != nil || report.Total != 28 || report.Receipt.NotAssessed != 1 || report.Receipt.Assessments.Failed != 0 || report.Receipt.Renames.Pending != 27 {
			t.Fatalf("incorrect report: %+v %v", report, err)
		}
		t.Logf("disposable 28-input comparison run %d: wall=%s scan=%d ms", round+1, time.Since(start), report.Clock.ActiveMS)
		jobs = append(jobs, v.Job)
		if err := a.apply(v.Job, v.ReviewStart, v.ReviewEnd, nil); err == nil {
			t.Fatal("preview-only profile allowed a rename")
		}
	}
	diff, err := a.compareRuns(jobs[0], jobs[1], 0, true)
	if err != nil || diff.Files != 28 || diff.Changed != 0 {
		t.Fatalf("%+v %v", diff, err)
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("Go heap after comparison: %d bytes, total allocated %d bytes", m.HeapAlloc, m.TotalAlloc)
}

func Test25AuthenticatedControlApplyUndoAndJournal(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "input")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, "Research_Note_01.txt")
	writeTestFile(t, original, "Research Note 01\nIndependent work.")
	a, err := newApp(filepath.Join(base, "app"), filepath.Join(base, "engine"), filepath.Join(base, "model"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.s.close() }()
	if err = a.setBridge(true); err != nil {
		t.Fatal(err)
	}
	h := server(a).Handler
	call := func(c bridgeCommand, key, origin string) (int, []byte) {
		d, _ := json.Marshal(c)
		request := httptest.NewRequest("POST", "http://127.0.0.1:39173/api/bridge", bytes.NewReader(d))
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request)
		return w.Code, w.Body.Bytes()
	}
	key := a.bridgeKey
	if status, _ := call(bridgeCommand{Action: "start", Root: root, Partial: true}, "", ""); status != 403 {
		t.Fatal(status)
	}
	if status, _ := call(bridgeCommand{Action: "capabilities"}, key, "https://example.test"); status != 403 {
		t.Fatal(status)
	}
	if status, b := call(bridgeCommand{Action: "capabilities"}, key, ""); status != 200 || !strings.Contains(string(b), "renamer_save_profile") {
		t.Fatalf("%d %s", status, b)
	}
	if status, b := call(bridgeCommand{Action: "start", Root: root, Partial: true}, key, ""); status != 200 {
		t.Fatalf("%d %s", status, b)
	}
	waitForPhase(t, a, "preview")
	v := a.snapshot()
	if status, b := call(bridgeCommand{Action: "proposals", Job: v.Job}, key, ""); status != 200 || !strings.Contains(string(b), "Research Note 01") {
		t.Fatalf("%d %s", status, b)
	}
	i := 0
	candidate := "Updated Research Note 01.txt"
	if status, b := call(bridgeCommand{Action: "correct", Job: v.Job, Revision: &v.Revision, Index: &i, Name: &candidate}, key, ""); status != 200 {
		t.Fatalf("%d %s", status, b)
	}
	old := v.Revision
	v = a.snapshot()
	for _, cmd := range []bridgeCommand{
		{Action: "apply", Job: v.Job, Revision: &old, Indices: []int{0}, Authorized: true},
		{Action: "apply", Job: v.Job, Revision: &v.Revision, Indices: []int{0}},
	} {
		if status, _ := call(cmd, key, ""); status != 409 {
			t.Fatalf("invalid approval accepted: %+v", cmd)
		}
	}
	if status, b := call(bridgeCommand{Action: "apply", Job: v.Job, Revision: &v.Revision, Indices: []int{0}, Authorized: true}, key, ""); status != 200 {
		t.Fatalf("%d %s", status, b)
	}
	waitForPhase(t, a, "complete")
	if _, err = os.Stat(filepath.Join(root, candidate)); err != nil {
		t.Fatal(err)
	}
	if status, b := call(bridgeCommand{Action: "undo", Job: v.Job, Authorized: true}, key, ""); status != 200 {
		t.Fatalf("%d %s", status, b)
	}
	waitForPhase(t, a, "undone")
	if _, err = os.Stat(original); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSession(a.s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	receipt := reopened.receipt()
	if receipt.Renames.Completed != 1 || receipt.Undo.Completed != 1 {
		t.Fatalf("%+v", receipt)
	}
}
