package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Evidence format version is independent of the application release.
const evidenceVersion = "3.6.0"
const assessmentVersion = "2.8.0-llama-b12588"

type assessmentCache struct {
	Deepened                           bool   `json:",omitempty"`
	MediaSampled                       bool   `json:",omitempty"`
	MediaTools                         string `json:",omitempty"`
	Stamp                              stamp
	EvidenceVersion, AssessmentVersion string
	Evidence                           evidence
	OCR                                string
	Model                              *assessment
	Kind                               string
	Rules                              namingRules
	EvidenceConfig                     evidenceSettings
	AIConfig                           aiSettings
	DecodeMemory                       int
	OCRModel                           string
	Move                               *cachedMoveTopic `json:",omitempty"`
}

type cachedMoveTopic struct {
	Version, ModelKey string
	AI                aiSettings
	Assessment        assessment
}

const moveTopicVersion = "2"

func localEvidenceSettings(config evidenceSettings) evidenceSettings {
	config.OnlineDates = ""
	return config
}

func evidenceCache(pre preparedEvidence, e evidence, ocr string, opts scanSettings) assessmentCache {
	if pre.Reused {
		return pre.Cached
	}
	cached := assessmentCache{Deepened: pre.Deepened, Stamp: pre.Stamp, EvidenceVersion: evidenceVersion, Evidence: e, OCR: ocr,
		EvidenceConfig: opts.Evidence, DecodeMemory: opts.Resources.DecodeMemoryMiB, AIConfig: opts.AI}
	if ocr == "Read sampled pages" {
		cached.OCRModel = modelCacheKey(opts.AI.Model)
	}
	return cached
}

func cachePath(dir, path string, vision bool) string {
	if dir == "" {
		return ""
	}
	mode := "text:"
	if vision {
		mode = "vision:"
	}
	key := sha256.Sum256([]byte(mode + path))
	return filepath.Join(dir, hex.EncodeToString(key[:])+".json")
}

func readCache(path string, st stamp) (assessmentCache, bool) {
	f, err := os.Open(path)
	if err != nil {
		return assessmentCache{}, false
	}
	defer f.Close()
	var c assessmentCache
	if json.NewDecoder(io.LimitReader(f, 2<<20)).Decode(&c) != nil || c.Stamp != st || c.EvidenceVersion != evidenceVersion {
		return assessmentCache{}, false
	}
	return c, true
}

func pruneCache(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -90)
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
}

func modelCacheKey(selection string) string {
	label, _, resources := modelProfileFor(selection)
	for _, resource := range resources {
		label += ":" + resource.sum
	}
	return label
}
