package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestImageExportLabelsAreNotSubjects(t *testing.T) {
	name := "Untitled design 2 remove background preview.png"
	for _, label := range []string{"Untitled Design 2 Remove Background Preview", "UNTITLED DESIGN (2) - REMOVE-BACKGROUND-PREVIEW", "No visible subject", "Preview"} {
		d := decide(name, evidence{Photo: []byte{1}}, assessment{Label: label}, true)
		if d.Name != name || d.Note == "" {
			t.Fatalf("%q: %+v", label, d)
		}
	}
	d := decide(name, evidence{Photo: []byte{1}}, assessment{Label: "A fern beside a window - remove background preview"}, true)
	if d.Name != "A fern beside a window.png" {
		t.Fatal(d)
	}
	d = decide("Sunday walk - remove background preview.jpg", evidence{}, assessment{}, false)
	if d.Name != "Sunday walk.jpg" || d.Outcome != "Cleanup proposed" {
		t.Fatal(d)
	}
	if got := defaultRules.cleanName("UNTITLED_DESIGN_2_REMOVE_BACKGROUND_PREVIEW.PNG", false); got != "UNTITLED_DESIGN_2_REMOVE_BACKGROUND_PREVIEW.PNG" {
		t.Fatal(got)
	}
}

func TestGeneratedNamesDoNotIntroduceAllCaps(t *testing.T) {
	rules := defaultRules
	rules.Capitalization = "preserve"
	current := "Reading Notes.pdf"
	if got := rules.normalizeGeneratedName(current, "READING NOTES.pdf"); got != current {
		t.Fatal(got)
	}
	got := rules.normalizeGeneratedName("scan.pdf", "EFT AND THE ROLE OF EMOTION - MCC 642.pdf")
	if got != "EFT and the Role of Emotion - MCC 642.pdf" {
		t.Fatal(got)
	}
}

func TestImageChatUsesImageScannerAndRunInstructions(t *testing.T) {
	message := "Go through every image file and use the image scanner. Name each image for the visible subject."
	plan, ok := immediateChatPlan(message)
	if !ok || !plan.UseImageScanner || plan.Action != "start" || plan.ScanInstructions != message {
		t.Fatalf("%+v %t", plan, ok)
	}
	opts := defaultSettings()
	opts.AI.Model = "off"
	opts.AI.Invoke = "never"
	opts.Execution.Mode = "automatic"
	opts.Scope.IncludeExtensions = []string{".pdf"}
	opts.Scope.ExcludeExtensions = []string{".png", ".csv"}
	plan.model = "qwen2.5-vl-3b"
	if err := configureChatScan(&opts, plan); err != nil {
		t.Fatal(err)
	}
	if opts.AI.Model != plan.model || opts.AI.Invoke != "all" || opts.Execution.Mode != "review" ||
		!slices.Equal(opts.Scope.IncludeExtensions, imageExtensions) ||
		!slices.Equal(opts.Scope.ExcludeExtensions, []string{".csv"}) || !strings.Contains(opts.AI.Instructions, message) {
		t.Fatalf("%+v", opts)
	}
	if _, ok := immediateChatPlan("Start a scan but skip images."); ok {
		t.Fatal("the image scanner ignored the exclusion")
	}
}

func TestRunFilterIncludesOnlyRequestedExtension(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "some_document.txt"), "Some Document")
	writeTestFile(t, filepath.Join(root, "some_image.png"), "Dummy file. Model disabled.")
	base := t.TempDir()
	a, err := newApp(filepath.Join(base, "home"), filepath.Join(base, "engine"), filepath.Join(base, "model"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.chat.cancel()
	if err = a.startFiltered(root, true, false, false, false, false, []string{".txt"}); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "preview")
	defer a.s.close()
	v := a.snapshot()
	if v.Total != 1 {
		t.Fatalf("%+v", v)
	}
	rows, err := a.inspections(0, false)
	if err != nil || len(rows.Rows) != 1 || !strings.HasSuffix(rows.Rows[0].Path, "some_document.txt") {
		t.Fatal(rows, err)
	}
}
