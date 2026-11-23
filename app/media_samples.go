package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func isVideo(name string) bool {
	switch strings.ToLower(extension(name)) {
	case ".mp4", ".mov", ".m4v", ".mkv", ".webm", ".avi":
		return true
	}
	return false
}

func mediaCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	configureChild(cmd)
	return cmd
}

func mediaTool(name string) (string, error) {
	return exec.LookPath(name)
}

func videoFrame(ctx context.Context, path string, second, edge int) ([]byte, error) {
	ffmpeg, err := mediaTool("ffmpeg")
	if err != nil {
		return nil, errors.New("FFmpeg is required for video frames")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	size := strconv.Itoa(edge)
	return mediaCommand(ctx, ffmpeg, "-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe",
		"-ss", strconv.Itoa(second), "-threads", "2", "-i", path, "-map", "0:v:0", "-an", "-frames:v", "1",
		"-vf", "scale="+size+":"+size+":force_original_aspect_ratio=decrease", "-filter_threads", "1", "-threads", "1",
		"-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1").Output()
}

func videoSamples(ctx context.Context, path string, edge int) ([]byte, int, error) {
	var frames []image.Image
	thumb := min(edge/2, 300)
	for _, at := range []int{0, 12, 30} {
		jpg, err := videoFrame(ctx, path, at, thumb)
		if err != nil || len(jpg) == 0 {
			if at == 0 {
				return nil, 0, errors.New("Video frames unavailable; check FFmpeg and the input format")
			}
			break
		}
		frame, err := jpeg.Decode(bytes.NewReader(jpg))
		if err == nil {
			frames = append(frames, frame)
		}
	}
	if len(frames) == 0 {
		return nil, 0, errors.New("No readable video frames")
	}
	sheet := image.NewRGBA(image.Rect(0, 0, min(2, len(frames))*thumb, ((len(frames)+1)/2)*thumb))
	for i, frame := range frames {
		rect := frame.Bounds().Add(image.Pt(i%2*thumb, i/2*thumb))
		draw.Draw(sheet, rect, frame, frame.Bounds().Min, draw.Src)
	}
	data, note := encodeVision(sheet, edge)
	if len(data) == 0 {
		return nil, 0, errors.New(note)
	}
	return data, len(frames), nil
}

func speechSample(ctx context.Context, path string, threads int) (string, error) {
	ffmpeg, ffErr := mediaTool("ffmpeg")
	whisper, whErr := mediaTool("whisper-cli")
	model := os.Getenv("LOCAL_RENAMER_WHISPER_MODEL")
	if ffErr != nil || whErr != nil || !filepath.IsAbs(model) {
		return "", errors.New("Speech sampling requires FFmpeg, whisper-cli, and LOCAL_RENAMER_WHISPER_MODEL")
	}
	if info, err := os.Stat(model); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("The configured Whisper model is unavailable")
	}
	dir, err := os.MkdirTemp("", "renamer-speech-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	wave, output := filepath.Join(dir, "sample.wav"), filepath.Join(dir, "speech")
	if err := mediaCommand(ctx, ffmpeg, "-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe",
		"-threads", "2", "-i", path, "-map", "0:a:0", "-vn", "-t", "25", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", wave).Run(); err != nil {
		return "", errors.New("No audio track could be sampled")
	}
	if err := mediaCommand(ctx, whisper, "-m", model, "-f", wave, "-t", strconv.Itoa(threads), "-l", "auto",
		"-ng", "-nt", "-otxt", "-of", output).Run(); err != nil {
		return "", errors.New("Local speech transcription failed or timed out")
	}
	text, err := os.ReadFile(output + ".txt")
	if err != nil {
		return "", errors.New("No speech transcript was returned")
	}
	return strings.TrimSpace(chars(string(text), 4000)), nil
}

func mediaToolKey() string {
	var key []string
	ffmpeg, _ := mediaTool("ffmpeg")
	whisper, _ := mediaTool("whisper-cli")
	for _, path := range []string{ffmpeg, whisper, os.Getenv("LOCAL_RENAMER_WHISPER_MODEL")} {
		if info, err := os.Stat(path); err == nil {
			key = append(key, fmt.Sprintf("%s:%d:%d", path, info.Size(), info.ModTime().UnixNano()))
		}
	}
	return strings.Join(key, "\x00")
}

func inspectSampledMedia(ctx context.Context, path, name, cacheName string, st stamp, e evidence, readMS int64, assess assessFile, opts scanSettings, refresh bool) decision {
	tools := mediaToolKey()
	cached, found := readCache(cacheName, st)
	reuse := found && opts.Cache.ReuseEvidence && cached.EvidenceConfig == opts.Evidence && cached.MediaTools == tools && cached.MediaSampled
	if reuse {
		e = cached.Evidence
	} else {
		started := time.Now()
		var details []string
		if isVideo(name) {
			photo, count, err := videoSamples(ctx, path, opts.Evidence.ImageEdge)
			if err != nil {
				details = append(details, err.Error())
			} else {
				e.Photo = photo
				details = append(details, fmt.Sprintf("%d video frames sampled", count))
			}
		}
		text, err := speechSample(ctx, path, opts.Resources.Threads)
		if err != nil {
			details = append(details, err.Error())
		} else if text != "" {
			e.Text = text
			details = append(details, "25-second audio excerpt transcribed")
		} else {
			details = append(details, "No speech identified in the sample")
		}
		e.Note = strings.Join(details, ". ")
		readMS += elapsedMS(started)
	}
	d := decision{Name: name, Kind: "other", Outcome: "Insufficient evidence", Reason: "Samples did not establish a useful topic name"}
	check := inspection{Text: e.Text != "", Image: len(e.Photo) > 0, Metadata: e.Title != "", ReadMS: readMS, Detail: e.Note, AI: "No usable media sample"}
	if reuse {
		check.Cache = "Media samples reused"
	}
	if !check.Text && !check.Image {
		d.Note, d.Inspection = e.Note, check
		return d
	}
	var result assessment
	var err error
	if reuse && !refresh && opts.Cache.ReuseAssessment && cached.Model != nil && cached.AIConfig == opts.AI &&
		cached.AssessmentVersion == assessmentVersion && cached.Kind == modelCacheKey(opts.AI.Model) {
		result, check.Cache = *cached.Model, "Media assessment reused"
	} else {
		started := time.Now()
		result, err = assess(ctx, name, e)
		check.ModelMS = elapsedMS(started)
	}
	if opts.Cache.ReuseEvidence && ctx.Err() == nil && check.Cache != "Media assessment reused" {
		out := assessmentCache{Stamp: st, EvidenceVersion: evidenceVersion, Evidence: e, EvidenceConfig: opts.Evidence,
			DecodeMemory: opts.Resources.DecodeMemoryMiB, MediaSampled: true, MediaTools: tools, AIConfig: opts.AI}
		if err == nil && opts.Cache.ReuseAssessment {
			out.Model, out.AssessmentVersion, out.Kind = &result, assessmentVersion, modelCacheKey(opts.AI.Model)
		}
		_ = atomicJSON(cacheName, out)
	}
	check.AI, check.AIContribution = "Used", "No useful change"
	switch {
	case err != nil:
		check.AI, d.Note = localAIIssue(err)
		d.Outcome = "Analysis failed"
		check.AIContribution = "Analysis failed"
	case imageSubject(result.Label) == "" || timestampInLabel.MatchString(result.Label):
		d.Note = "Media samples did not identify a usable topic."
	case norm(baseName(name)) == norm(result.Label):
		d.Outcome, d.Reason = "Name verified", "The sampled media supports the existing topic name"
		check.AIContribution = "Verified name"
	case imageSubject(baseName(name)) == "" || result.Mismatch:
		label := imageSubject(result.Label)
		current := baseName(name)
		if match := explicitMediaVersion.FindStringIndex(current); match != nil && !hasEvidence(current[match[0]:], label) {
			label += current[match[0]:]
		}
		candidate := opts.Naming.makeName([]string{label}, extension(name))
		if candidate != "" && opts.Naming.protect(name, candidate) {
			d.Name, d.Outcome, d.Reason = candidate, "Rename proposed", "Named for the topic supported by the sampled media"
			check.AIContribution = "Supplied a topic name"
		}
	default:
		d.Note = "The existing title has useful wording. Review the sample before changing it."
	}
	d.Inspection = check
	return d
}
