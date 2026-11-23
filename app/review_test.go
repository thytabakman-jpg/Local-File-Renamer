package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageTitlesAndVersionPairing(t *testing.T) {
	for _, label := range []string{"Background Remove", "Untitled Design 2 Preview", "Preview (7)", "Remove Background Preview"} {
		if got := imageSubject(label); got != "" {
			t.Fatalf("export label %q passed as a title: %q", label, got)
		}
	}
	for in, want := range map[string]string{
		"Sunday Walk - Preview": "Sunday Walk", "Sunday Walk (Preview)": "Sunday Walk",
		"Sunday Walk - Background Remove": "Sunday Walk", "Course Preview": "Course Preview",
	} {
		if got := imageSubject(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
	root := t.TempDir()
	items := []entry{{Path: filepath.Join(root, "A space to talk.png")},
		{Path: filepath.Join(root, "A space to talk - background remove.png")},
		{Path: filepath.Join(root, "Yellow butterfly.png")}}
	versions, uncertain := imageVersionGroups(items)
	s := session{manifest: manifest{Items: items}, imageVersions: versions, uncertainImageVersions: uncertain}
	original := s.versionImage(0, decision{Name: "A space to talk.png"})
	export := s.versionImage(1, decision{Name: "A space to talk.png"})
	single := s.versionImage(2, decision{Name: "Yellow butterfly.png"})
	if original.Name != "A space to talk - Version 1.png" || export.Name != "A space to talk - Version 2.png" ||
		single.Name != "Yellow butterfly.png" || original.AutoEvidence || export.AutoEvidence {
		t.Fatalf("%+v %+v %+v", original, export, single)
	}
	items = append(items, entry{Path: filepath.Join(root, "A space to talk - background removed (2).png")})
	versions, uncertain = imageVersionGroups(items)
	if len(versions) != 0 || !uncertain[1] || !uncertain[3] {
		t.Fatalf("ambiguous versions: %v %v", versions, uncertain)
	}
	generic := "202604422222.png"
	d := decide(generic, evidence{Photo: []byte{1}}, assessment{Label: "A fern 202602033333"}, true)
	if d.Name != generic || d.Note == "" {
		t.Fatal(d)
	}
}

func TestLocationRevisionAndUndo(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	in := filepath.Join(root, "Incoming")
	out := filepath.Join(root, "Reflections")
	for _, dir := range []string{in, out} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	name := "REFLECTION_DRAFT.txt"
	origin := filepath.Join(in, name)
	writeTestFile(t, origin, "Reflection Draft\nThe week in review.")
	a, err := newApp(filepath.Join(data, "home"), filepath.Join(data, "engine"), filepath.Join(data, "model"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.chat.cancel()
	if err := a.startFiltered(root, true, false, false, false, true, nil); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "preview")
	defer a.s.close()
	v := a.snapshot()
	rows, err := a.previewPage(0)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	i := rows[0].Index
	if err := a.changeLocation(v.Job, v.Revision, i, data); err == nil {
		t.Fatal("accepted a folder outside the scan")
	}
	if err := a.changeLocation(v.Job, v.Revision, i, out); err != nil {
		t.Fatal(err)
	}
	v = a.snapshot()
	custom := "Internship Reflection.txt"
	if err := a.correct(v.Job, v.Revision, i, "", &custom); err != nil {
		t.Fatal(err)
	}
	v = a.snapshot()
	rows, err = a.previewPage(0)
	destination := filepath.Join(out, custom)
	if err != nil || len(rows) != 1 || rows[0].To != destination {
		t.Fatal("rename correction lost destination", rows, err)
	}
	if err := a.applySelected(v.Job, v.Revision, []int{i}, false); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "complete")
	if _, err := os.Stat(destination); err != nil {
		t.Fatal(err)
	}
	if err := a.undo(); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "undone")
	if _, err := os.Stat(origin); err != nil {
		t.Fatal(err)
	}
}

func TestSampledVideoUsesFramesAndCache(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg unavailable")
	}
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mp4")
	cmd := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=blue:s=320x240:d=1", "-c:v", "mpeg4", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	st, err := fingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := defaultSettings()
	opts.Evidence.MediaSamples = true
	var calls int
	assess := func(_ context.Context, _ string, e evidence) (assessment, error) {
		calls++
		if len(e.Photo) == 0 || !strings.Contains(e.Note, "video frames sampled") {
			t.Fatalf("missing video evidence: %+v", e)
		}
		return assessment{Label: "Blue Test Pattern"}, nil
	}
	for i := range 2 {
		d := inspectSampledMedia(ctx, path, "clip.mp4", filepath.Join(dir, "cached.json"), st, evidence{}, 0, assess, opts, false)
		if d.Name != "Blue Test Pattern.mp4" || calls != 1 || i != 0 && d.Inspection.Cache != "Media assessment reused" {
			t.Fatalf("%+v (calls: %d)", d, calls)
		}
	}
}
