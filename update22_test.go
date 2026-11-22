package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitForPhase(t *testing.T, a *application, phase string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a.snapshot().Phase == phase {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiting for %s: %+v", phase, a.snapshot())
}

func TestApplyPausedPreviewReopenAndResume(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "document.txt")
	pending := filepath.Join(root, "file.txt")
	writeTestFile(t, original, "Reading and Study Skills\nSample body.")
	writeTestFile(t, pending, "Lecture on Another Topic\nSample body.")
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = process(context.Background(), s, nil, 0); err != nil {
		t.Fatal(err)
	}
	a := &application{s: s}
	a.syncSession(true)
	rows, err := a.previewPage(0)
	if err != nil || len(rows) != 1 || a.v.Phase != "paused" {
		t.Fatalf("paused preview=%+v, error=%v, state=%+v", rows, err, a.v)
	}
	v := a.snapshot()
	if err = a.apply(v.Job, v.ReviewStart, v.ReviewEnd, nil); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "paused")
	dir := s.dir
	s.close()
	s, err = openSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a = &application{s: s}
	a.syncSession(true)
	if a.v.Done != 1 || a.v.Proposed != 0 || s.index != 1 || s.applyPending() {
		t.Fatalf("unexpected reopened state %+v", a.v)
	}
	if err = a.resume(a.snapshot().Job, true); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "preview")
	v = a.snapshot()
	if v.Done != 2 || v.Proposed != 1 {
		t.Fatalf("unexpected finished scan %+v", v)
	}
	if err = a.apply(v.Job, 0, v.ReviewEnd, nil); err == nil {
		t.Fatal("stale selection accepted after resume")
	}
	if err = a.apply(v.Job, v.ReviewStart, v.ReviewEnd, nil); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "complete")
	if err = a.undo(); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "undone")
	for _, path := range []string{original, pending} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUndoEarlyMetadataAfterLaterFolderRename(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "New Folder")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(folder, "download.docx")
	writeTestZip(t, doc, map[string]string{
		"docProps/core.xml": `<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Old Title</dc:title></cp:coreProperties>`,
		"word/document.xml": `<w:document xmlns:w="urn:w"><w:p><w:t>Title: Trees and Rivers</w:t></w:p></w:document>`,
	})
	original, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true, RenameFolders: true, RepairMetadata: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &application{s: s}
	if err = process(context.Background(), s, nil, 0); err != nil {
		t.Fatal(err)
	}
	a.syncSession(true)
	v := a.snapshot()
	if err = a.apply(v.Job, v.ReviewStart, v.ReviewEnd, nil); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "paused")
	if a.snapshot().MetadataDone != 1 {
		t.Fatalf("early metadata not repaired: %+v", a.snapshot())
	}
	if err = a.resume(a.snapshot().Job, true); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "preview")
	v = a.snapshot()
	if err = a.apply(v.Job, v.ReviewStart, v.ReviewEnd, nil); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "complete")
	if _, err = os.Stat(filepath.Join(root, "Old Title", "Old Title.docx")); err != nil {
		t.Fatal(err)
	}
	if err = a.undo(); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "undone")
	got, err := os.ReadFile(doc)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("original document did not survive undo: %v", err)
	}
}

func TestSortBoundedPreviewAndUndo(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "Downloads")
	music := filepath.Join(root, "Music")
	unfiled := filepath.Join(music, "Unfiled")
	tax := filepath.Join(root, "Tax Receipts")
	for _, dir := range []string{unfiled, tax} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(music, "Property Statements.txt")
	dest := filepath.Join(tax, filepath.Base(source))
	writeTestFile(t, source, "Tax Receipts for Property and Income\nDocuments for the accountant.")
	tooFar := filepath.Join(unfiled, "More Statements.txt")
	writeTestFile(t, tooFar, "Tax Receipts for Property and Income\nDocuments for the accountant.")
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true, SortFolders: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for i := range s.manifest.Items {
		if err = process(context.Background(), s, nil, i); err != nil {
			t.Fatal(err)
		}
		if r := s.plans[i]; r.Path == tooFar && r.Move != nil {
			t.Fatalf("three-hop move proposed: %+v", r)
		}
	}
	if _, err = os.Stat(dest); !os.IsNotExist(err) || s.reviewCount() != 1 {
		t.Fatalf("expected one unexecuted proposal: %v, %+v", err, s.plans)
	}
	if err = s.append(record{Type: "apply-batch", Index: len(s.plans)}); err != nil {
		t.Fatal(err)
	}
	for s.index < s.applyThrough {
		if err = s.applyNext(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(dest); err != nil {
		t.Fatal(err)
	}
	if err = s.undoNext(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(source); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(tooFar); err != nil {
		t.Fatal(err)
	}
	if err = s.validateMoveParents(move{From: source, To: filepath.Join(parent, "outside.txt")}); err == nil {
		t.Fatal("accepted a destination outside the selected folder")
	}
	if err = s.validateMoveParents(move{From: source, To: filepath.Join(root, "top.txt")}); err != nil {
		t.Fatalf("selected root is a valid destination: %v", err)
	}
	jump := filepath.Join(root, "Linked outside")
	if err := os.Symlink(parent, jump); err == nil {
		if err = s.validateMoveParents(move{From: source, To: filepath.Join(jump, "outside.txt")}); err == nil {
			t.Fatal("accepted a linked destination outside the selected folder")
		}
	}
}

func TestAppliedConflictStaysFlagged(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "Trees - MCC 502 (V2).txt")
	writeTestFile(t, p, "Trees\nFactual body.")
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true, Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = process(context.Background(), s, nil, 0); err != nil {
		t.Fatal(err)
	}
	plan := s.plans[0]
	if !plan.Auto {
		t.Fatalf("expected eligible course paper: %+v", plan)
	}
	writeTestFile(t, plan.Move.To, "An unrelated file.")
	if err = s.autoApply(plan); err != nil {
		t.Fatal(err)
	}
	a := &application{s: s}
	a.syncSession(true)
	v := a.snapshot()
	if v.Flagged != 1 || len(v.Flags) != 1 || !strings.Contains(v.Flags[0].Reason, "Name conflict") {
		t.Fatalf("runtime conflict missing from report: %+v", v)
	}
	content, err := os.ReadFile(plan.Move.To)
	if err != nil || string(content) != "An unrelated file." {
		t.Fatal("existing destination changed")
	}
}

type countReads struct {
	r     io.ReaderAt
	bytes int
}

func (r *countReads) ReadAt(p []byte, at int64) (int, error) {
	r.bytes += len(p)
	return r.r.ReadAt(p, at)
}

func mp4TestBox(kind string, data []byte) []byte {
	b := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(b[:4], uint32(len(b)))
	copy(b[4:8], kind)
	copy(b[8:], data)
	return b
}

func TestMediaTitleReadsSkipPayloadAndAI(t *testing.T) {
	root := t.TempDir()
	video := filepath.Join(root, "Unexpected Title.mp4")
	const payloadSize = 8 << 20
	f, err := os.Create(video)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header, payloadSize)
	copy(header[4:], "mdat")
	if _, err = f.Write(header); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(payloadSize, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	title := append([]byte{0, 0, 0, 1, 0, 0, 0, 0}, []byte("Reading and Study Skills")...)
	tail := mp4TestBox("moov", mp4TestBox("udta", mp4TestBox("meta", append(make([]byte, 4),
		mp4TestBox("ilst", mp4TestBox("\xa9nam", mp4TestBox("data", title)))...))))
	if _, err = f.Write(tail); err != nil {
		t.Fatal(err)
	}
	counter := &countReads{r: f}
	reader := mediaReader{file: counter, remaining: 8192}
	var got evidence
	reader.titles(0, payloadSize+int64(len(tail)), 0, &got)
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if got.Title != "Reading and Study Skills" || counter.bytes > 512 {
		t.Fatalf("title=%q, read %d bytes in an 8 MiB video", got.Title, counter.bytes)
	}
	t.Logf("Read %d bytes to find a title after an 8 MiB video payload", counter.bytes)

	mp3 := filepath.Join(root, "lecture.mp3")
	body := []byte{1, 0xff, 0xfe, 'C', 0, 'a', 0, 'f', 0, 0xe9, 0}
	frame := make([]byte, 10)
	copy(frame, "TIT2")
	binary.BigEndian.PutUint32(frame[4:], uint32(len(body)))
	tag := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, byte(len(frame) + len(body))}
	writeTestFile(t, mp3, string(append(append(tag, frame...), body...)))
	if e := readEvidence(mp3, false); e.Title != "Café" {
		t.Fatalf("UTF-16 title: %+v", e)
	}
	s := &session{manifest: manifest{Root: root, Items: []entry{{Path: video}, {Path: mp3}}}}
	for i := range s.manifest.Items {
		result, _, err := suggest(context.Background(), s, &localModel{}, i)
		if err != nil || !strings.Contains(result.Note, "differs from embedded title") ||
			!result.Inspection.Metadata || result.Inspection.AI != "Skipped (media tags only)" ||
			result.Name != filepath.Base(s.manifest.Items[i].Path) {
			t.Fatalf("media decision: %+v, %v", result, err)
		}
	}
}

func TestMediaOnlyRunDoesNotStartModel(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Some Video.mp4"), "")
	s, err := createSession(context.Background(), t.TempDir(), root, manifest{Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &application{s: s, home: t.TempDir(), modelDir: t.TempDir()}
	ctx := a.begin("starting")
	a.run(ctx, s, false)
	got := a.snapshot()
	if got.Done != 1 || got.Phase != "complete" || got.Message != "" || got.Flagged != 1 {
		t.Fatalf("media-only scan used the model: %+v", got)
	}
}
