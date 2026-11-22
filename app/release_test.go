package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationEvidenceAndExplicitVersions(t *testing.T) {
	name := "Rube Goldberg Project_ Task 1-7 (V2).pdf"
	e := evidence{Text: "Rube Goldberg Project: Task 1-7\nAbstract\nA classroom project about a chain reaction."}
	if d := decide(name, e, assessment{Kind: "article"}, true); d.Name != name || d.Strict {
		t.Fatalf("course project flagged as publication or version lost: %+v", d)
	}
	name = "Assignment (V2) - MCC 502.docx"
	if d := decide(name, evidence{}, assessment{}, false); d.Name != "Assignment - MCC 502 - 2.docx" || d.Note != "" {
		t.Fatalf("bracketed version was lost: %+v", d)
	}
	name = "Project Notes - Completed.docx"
	if d := decide(name, evidence{Text: "Project Notes\nA progress summary."}, assessment{Keep: true}, true); d.Name != name {
		t.Fatalf("descriptive completion word removed: %+v", d)
	}
	e = evidence{Kind: "book", Text: "Chapter 1\nThis text is a book.", Publisher: "University Press"}
	d := decide("download.epub", e, assessment{}, false)
	want := "Unknown author - Unknown year - Unknown title - University Press.epub"
	if d.Name != want || !d.Unknown || !d.Strict {
		t.Fatalf("missing fields not tracked: %+v", d)
	}
	if d = decide(want, e, assessment{}, false); d.Name != want || !d.Unknown {
		t.Fatalf("placeholder fields mistaken for facts: %+v", d)
	}
}

func TestSelectedChangesAndIncompleteCitationPreview(t *testing.T) {
	root := t.TempDir()
	book := filepath.Join(root, "book.epub")
	writeTestZip(t, book, map[string]string{"book.opf": "<package><metadata><title>Ancient Trees</title></metadata></package>"})
	writeTestFile(t, filepath.Join(root, "document.txt"), "Field Notes on Trees\nText body.")
	writeTestFile(t, filepath.Join(root, "download.txt"), "Field Notes on Rivers\nText body.")
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true, Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.manifest.Items {
		if err = process(context.Background(), s, nil, i); err != nil {
			t.Fatal(err)
		}
		if err = s.autoApply(s.plans[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(book); err != nil || s.reviewCount() != 3 || !s.plans[0].Unknown || s.plans[0].Auto {
		t.Fatalf("incomplete citation must remain a preview: %v, %+v", err, s.plans)
	}
	if err = s.append(record{Type: "approved", Excluded: []int{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err = s.applyNext(); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.close()
	s, err = openSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for s.index < len(s.plans) {
		if err = s.applyNext(); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{book, filepath.Join(root, "document.txt"), filepath.Join(root, "Field Notes on Rivers.txt")} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if s.renamed != 1 || !s.plans[1].Inspection.Text || s.plans[1].Inspection.AI != "Off" {
		t.Fatalf("unexpected selection or inspection: %+v", s)
	}
}

func TestMetadataRepairRoundTrip(t *testing.T) {
	root := t.TempDir()
	doc := filepath.Join(root, "download.docx")
	writeTestZip(t, doc, map[string]string{
		"docProps/core.xml": `<?xml version="1.0"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Old title</dc:title><cp:keywords>keep this</cp:keywords></cp:coreProperties>`,
		"word/document.xml": `<w:document xmlns:w="urn:w"><w:p><w:t>Title: Trees and Rivers</w:t></w:p><w:p><w:t>Author: Jane &amp; John Doe</w:t></w:p></w:document>`,
	})
	pdf := filepath.Join(root, "field notes.pdf")
	writeTestPDF(t, pdf)
	files := []string{doc, pdf}
	originals := map[string][]byte{}
	for _, path := range files {
		var err error
		originals[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true, RepairMetadata: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.manifest.Items {
		if err = process(context.Background(), s, nil, i); err != nil {
			t.Fatal(err)
		}
		if p := s.plans[i]; p.Metadata == nil || p.Metadata.Title != "Trees and Rivers" || p.Metadata.Author != "Jane & John Doe" {
			t.Fatalf("metadata proposal for %s: %+v", p.Path, p.Metadata)
		}
	}
	if err = s.append(record{Type: "approved"}); err != nil {
		t.Fatal(err)
	}
	for s.index < len(s.plans) {
		if err = s.applyNext(); err != nil {
			t.Fatal(err)
		}
	}
	for s.metadataIndex < len(s.metadataPlans) {
		if err = s.metadataNext(); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.repaired) != 2 {
		t.Fatalf("metadata not repaired: %+v", s.flags)
	}
	for i, plan := range s.plans {
		path := s.currentPath(plan.Path, i)
		e := readEvidence(path, false)
		if clean(e.Title) != "Trees and Rivers" || clean(e.Author) != "Jane & John Doe" || !strings.Contains(e.Text, "Trees and Rivers") {
			t.Fatalf("metadata or body at %s: %+v", path, e)
		}
	}
	dir := s.dir
	s.close()
	s, err = openSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for s.metadataUndone < len(s.repaired) || s.undoCount < len(s.moves) {
		if err = s.undoNext(); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range files {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, originals[path]) {
			t.Fatalf("original contents not restored for %s: %v", path, err)
		}
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeTestZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for path, data := range entries {
		w, err := z.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, b.String())
}

func writeTestPDF(t *testing.T, path string) {
	t.Helper()
	content := "BT /F1 12 Tf 72 700 Td (Title: Trees and Rivers) Tj ET BT /F1 12 Tf 72 675 Td (Author: Jane & John Doe) Tj ET\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		"<< /Title (Old Title) >>",
	}
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	var offsets []int
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info 6 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	writeTestFile(t, path, b.String())
}
