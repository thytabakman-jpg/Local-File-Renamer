package main

import (
	"archive/zip"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNamingExamples(t *testing.T) {
	cases := []struct {
		name       string
		e          evidence
		a          assessment
		want, flag string
	}{
		{"To Help, You Must Know It All - 1.2 - MCC 502.docx", evidence{}, assessment{}, "To Help, You Must Know It All - MCC 502 - 1.2.docx", ""},
		{"To Help, You Must Know It All [take two] - MCC 502.pdf", evidence{}, assessment{}, "To Help, You Must Know It All - MCC 502 - 2.pdf", ""},
		{"How Do You Pick - MCC 502 Final Final.pdf", evidence{}, assessment{}, "How Do You Pick - MCC 502 - Version Pending.pdf", "Version ? (unresolved)"},
		{"How Do You Pick - MCC 502 - 1.2 - Final.pdf", evidence{}, assessment{}, "How Do You Pick - MCC 502 - 1.2.pdf", ""},
		{"How Do You Pick - Final - MCC 502.pdf", evidence{}, assessment{}, "How Do You Pick - MCC 502 - Version Pending.pdf", "Version ? (unresolved)"},
		{"How Do You Pick - MCC 502 - Version Pending.pdf", evidence{}, assessment{}, "How Do You Pick - MCC 502 - Version Pending.pdf", "Version ? (unresolved)"},
		{"download.pdf", evidence{Text: "Journal of Counselling\nResearch Approaches in Practice\nJane Doe\nCopyright 2024"}, assessment{Kind: "article", Title: "Research Approaches in Practice", Authors: []string{"Jane Doe"}, Year: "2024", Venue: "Journal of Counselling"}, "Doe - 2024 - Research Approaches in Practice - Journal of Counselling.pdf", ""},
		{"download.pdf", evidence{Text: "Research Approaches in Practice\nJane Doe\nDOI: 10.1234/example"}, assessment{Kind: "article", Title: "Research Approaches in Practice", Authors: []string{"Jane Doe"}, Venue: "Journal of Counselling"}, "Doe - Unknown year - Research Approaches in Practice - Unknown journal.pdf", "Missing publication year"},
		{"book.epub", evidence{Kind: "book", Title: "The Night Sky", Author: "Jane Doe; John Brown", Year: "2018", Publisher: "Moon House"}, assessment{}, "Doe and Brown - 2018 - The Night Sky.epub", ""},
		{"book.epub", evidence{Kind: "book", Title: "Principles of Counselling", Author: "Jane Doe", Year: "2020", Publisher: "University Press"}, assessment{Kind: "academic_book"}, "Doe - 2020 - Principles of Counselling - University Press.epub", ""},
		{"IMG_5041.jpg", evidence{Photo: []byte{1}}, assessment{Label: "Cat on a wooden chair"}, "Cat on a wooden chair.jpg", ""},
		{"Tax Receipt.pdf", evidence{Text: "Business expenses"}, assessment{}, "Tax Receipt.pdf", ""},
	}
	for _, tc := range cases {
		d := decide(tc.name, tc.e, tc.a, true)
		if d.Name != tc.want || !strings.Contains(d.Note, tc.flag) {
			t.Errorf("%s: got %+v want %q, flag %q", tc.name, d, tc.want, tc.flag)
		}
	}
}

func process(ctx context.Context, s *session, m *localModel, i int) error {
	d, st, err := suggest(ctx, s, m, i)
	if err != nil {
		return err
	}
	return recordSuggestion(s, i, d, st)
}

func scanAndApply(t *testing.T, s *session) {
	t.Helper()
	for len(s.plans) < len(s.manifest.Items) {
		if err := process(context.Background(), s, nil, len(s.plans)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.append(record{Type: "apply-batch", Index: len(s.plans)}); err != nil {
		t.Fatal(err)
	}
	for s.index < s.applyThrough {
		if err := s.applyNext(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunResumeCollisionAndUndo(t *testing.T) {
	root := t.TempDir()
	data := t.TempDir()
	p := filepath.Join(root, "New Folder", "New Folder")
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(p, "untitled.txt")
	if err := os.WriteFile(original, []byte("Basics of course planning\n\nLonger context on education."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "document.txt"), []byte("A name we already have\nMore info."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "A name we already have.txt"), []byte("Existing file."), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := createSession(context.Background(), data, root, manifest{Preview: true, RenameFolders: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	if len(s.manifest.Items) != 5 {
		t.Fatal(s.manifest.Items)
	}
	if err := process(context.Background(), s, nil, 0); err != nil {
		t.Fatal(err)
	}
	s.close()
	s, err = openSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if len(s.plans) != 1 {
		t.Fatal(len(s.plans))
	}
	scanAndApply(t, s)
	if len(s.flags) != 2 || !strings.Contains(s.flags[1].Note, "Name conflict") {
		t.Fatal(s.flags)
	}
	if s.renamed != 3 {
		t.Fatal(s.moves)
	}
	latest := filepath.Join(root, "Basics of course planning", "Basics of course planning", "Basics of course planning.txt")
	if _, err := os.Stat(latest); err != nil {
		t.Fatal(err)
	}
	for len(s.moves) > s.undoCount {
		if err := s.undoNext(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "A name we already have.txt")); err != nil || string(b) != "Existing file." {
		t.Fatal(string(b), err)
	}
}

func TestPendingVersionComparesCopiesAcrossSubfolders(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "Drafts")
	if err := os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(root, "Case Analysis - MCC 502 - Final.txt"),
		filepath.Join(old, "Case Analysis - MCC 502 - 1.2.txt"),
		filepath.Join(old, "Case Analysis - MCC 502 - 1.10.txt"),
		filepath.Join(old, "Case Analysis - MCC 648 - 3.txt"),
	}
	for i, path := range paths {
		if err := os.WriteFile(path, []byte("Course paper contents."), 0600); err != nil {
			t.Fatal(err)
		}
		ts := time.Date(2026, 1, 5-i, 17, 30, 0, 0, time.Local)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true, RenameFolders: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	scanAndApply(t, s)
	path := filepath.Join(root, "Case Analysis - MCC 502 - Version Pending.txt")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if len(s.flags) != 1 {
		t.Fatalf("expected one version flag, got %+v", s.flags)
	}
	note := s.flags[0].Note
	high, low := strings.Index(note, "(version 1.10,"), strings.Index(note, "(version 1.2,")
	if high < 0 || low <= high || !strings.Contains(note, "2026-01-05 17:30:00") ||
		!strings.Contains(note, "2026-01-04 17:30:00") || strings.Contains(note, "MCC 648") {
		t.Fatalf("unexpected version comparison: %s", note)
	}
}

func TestRecoveryAfterRenameBeforeResult(t *testing.T) {
	root := t.TempDir()
	old, next := filepath.Join(root, "old.txt"), filepath.Join(root, "new.txt")
	if err := os.WriteFile(old, []byte("text"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{})
	if err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	stamp, err := fingerprint(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.append(record{Type: "intent", Index: 0, Move: &move{From: old, To: next, Stamp: stamp}}); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(old, next); err != nil {
		t.Fatal(err)
	}
	s.close()
	s, err = openSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.finishPending(); err != nil {
		t.Fatal(err)
	}
	if s.index != 1 || s.renamed != 1 {
		t.Fatal(s.index, s.renamed)
	}
	if err := s.undoNext(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal(err)
	}
}

func TestOfficeAndBookExtraction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "book.epub")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(file)
	files := map[string]string{
		"content.opf":   `<package xmlns:dc="http://purl.org/dc/elements/1.1/"><metadata><dc:title>A Book about Counselling</dc:title><dc:creator>Jane Doe</dc:creator><dc:date>2020-01-01</dc:date><dc:publisher>University Press</dc:publisher></metadata></package>`,
		"chapter.xhtml": `<html><body><h1>What counsellors do</h1><p>An example paragraph.</p></body></html>`,
	}
	for name, value := range files {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	e := readEvidence(path, false)
	if e.Kind != "book" || e.Title != "A Book about Counselling" || e.Author != "Jane Doe" || e.Year != "2020" || e.Publisher != "University Press" || !strings.Contains(e.Text, "An example paragraph") {
		t.Fatalf("%+v", e)
	}
}
func TestRealLocalModel(t *testing.T) {
	engine, models := os.Getenv("LOCAL_RENAMER_ENGINE"), os.Getenv("LOCAL_RENAMER_MODELS")
	if engine == "" || models == "" {
		t.Skip("Set the local engine and model directories for the model integration test")
	}
	tmp := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := startModel(ctx, engine, models, filepath.Join(tmp, "model.log"))
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(tmp, "model.log"))
		t.Fatalf("%v\n%s", err, chars(string(log), 9000))
	}
	defer m.Close()
	a, err := m.assess(ctx, "download.pdf", evidence{Text: "Journal of Counselling Research\nResearch Approaches in Practice\nJane Doe and Mark Watson\nPublished 2024\nAbstract: This study tests different research approaches to counselling psychology. DOI: 10.1234/test."})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "article" || a.Year != "2024" || !strings.Contains(a.Title, "Research Approaches") || len(a.Authors) != 2 {
		t.Fatalf("%+v", a)
	}
	t.Logf("PDF text: %+v", a)
	photo := filepath.Join(tmp, "Japan flag.png")
	f, err := os.Create(photo)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 256, 192))
	for x := 0; x < 256; x++ {
		for y := 0; y < 192; y++ {
			c := color.RGBA{R: 242, G: 240, B: 225, A: 255}
			if (x-128)*(x-128)+(y-96)*(y-96) < 55*55 {
				c = color.RGBA{R: 222, G: 32, B: 46, A: 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	if err = png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s := &session{manifest: manifest{Root: tmp, Items: []entry{{Path: photo}}}}
	d, _, err := suggest(ctx, s, m, 0)
	if err != nil || !d.Inspection.Image || d.Inspection.AI != "Used" {
		t.Fatalf("descriptively named image was not inspected: %+v, %v", d, err)
	}
	t.Logf("Image: %+v", d)
}

func TestVersion2AuditCases(t *testing.T) {
	for _, tc := range []struct{ input, want, flag string }{
		{"Assignment - MCC 502 - Final (2).docx", "Assignment - MCC 502 - Version Pending.docx", versionPendingNote},
		{"The Final - MCC 502 - 2.docx", "The Final - MCC 502 - 2.docx", ""},
		{"Assignment - MCC 502 - 1.2 Final.docx", "Assignment - MCC 502 - 1.2.docx", ""},
		{"Assignment (Final) - MCC 502.docx", "Assignment - MCC 502 - Version Pending.docx", versionPendingNote},
		{"A title mentioning Version 2 in Context - MCC 502 - 3.docx", "A title mentioning Version 2 in Context - MCC 502 - 3.docx", ""},
		{"Draft - MCC 502 - Take Two - 3.docx", "Draft - MCC 502 - Version Pending.docx", "Conflicting version labels"},
		{"Family Trip.jpg", "Family Trip.jpg", ""},
	} {
		d := decide(tc.input, evidence{Photo: []byte{1}}, assessment{Label: "People beside a tree", Keep: false}, true)
		if d.Name != tc.want || !strings.Contains(d.Note, tc.flag) {
			t.Errorf("%s: got %+v", tc.input, d)
		}
	}
}

func TestVersion2PreviewAndApply(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "New Folder")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(sub, "untitled.txt")
	unreadable := filepath.Join(root, "MCC 502 - Assignment - 1.2.docx")
	stale := filepath.Join(root, "file.txt")
	for path, data := range map[string]string{
		good:       "Building Strong Reading Habits\nExtra content.",
		unreadable: "This fails to open as a DOCX file.",
		stale:      "A Subject For The File\nMore text.",
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.manifest.Items {
		if err = process(context.Background(), s, nil, i); err != nil {
			t.Fatal(err)
		}
	}
	if s.reviewCount() != 2 || len(s.planFlags) != 3 || !strings.Contains(s.planFlags[0].Note, "could not be opened") {
		t.Fatalf("unexpected preview: %+v", s.plans)
	}
	if err = s.applyNext(); err == nil {
		t.Fatal("unapproved preview was applied")
	}
	for _, path := range []string{good, unreadable, stale} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	dir := s.dir
	s.close()
	s, err = openSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = os.WriteFile(stale, []byte("Changed after preview."), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.append(record{Type: "approved"}); err != nil {
		t.Fatal(err)
	}
	for s.index < len(s.manifest.Items) {
		if err = s.applyNext(); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(sub, "Building Strong Reading Habits.txt"), unreadable, stale} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if s.renamed != 1 || len(s.flags) != 3 {
		t.Fatalf("renames=%d flags=%+v", s.renamed, s.flags)
	}
	if err = s.undoNext(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(good); err != nil {
		t.Fatal(err)
	}
}
