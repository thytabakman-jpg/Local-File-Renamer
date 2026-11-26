package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Section: Duplicate group presentation and keeper recommendations.

type duplicateMember struct {
	Index int
	Path  string
}

var duplicateVersion = regexp.MustCompile(`(?i)(?:^|[ _-]+|[\[(])(?:version|rev|draft|v)[ ._-]*([0-9]+(?:\.[0-9]+)*)[\])]?$`)
var duplicateCopySuffix = regexp.MustCompile(`(?i)\s*(?:\([0-9]+\)|[ _-]+copy(?:[ _-]*[0-9]+)?)$`)

func duplicateFilenameParts(stem string) (name, version string, copyName bool) {
	name = stem
	for {
		if suffix := duplicateCopySuffix.FindStringIndex(name); suffix != nil {
			name, copyName = name[:suffix[0]], true
			continue
		}
		match := duplicateVersion.FindStringSubmatchIndex(name)
		if match == nil || version != "" {
			return strings.TrimRight(name, " _-"), version, copyName
		}
		name, version = name[:match[0]], name[match[2]:match[3]]
	}
}

func (s *session) describeDuplicates(plans []record, reason string) duplicateGroup {
	g := duplicateGroup{Grouping: reason}
	for _, p := range plans {
		g.Members = append(g.Members, duplicateMember{Index: p.Index, Path: p.Path})
	}
	slices.SortFunc(g.Members, func(a, b duplicateMember) int { return strings.Compare(a.Path, b.Path) })
	keeper := g.Members[0]
	g.KeeperReason = "Every name has a copy suffix. This path comes first alphabetically."
	for _, member := range g.Members {
		_, _, copyName := duplicateFilenameParts(baseName(filepath.Base(member.Path)))
		if !copyName {
			keeper = member
			g.KeeperReason = "Prefer a name without a copy suffix. Break ties by alphabetical path."
			break
		}
	}
	if reason == "Verified equivalent copies" {
		for _, member := range g.Members {
			stem, _, copyName := duplicateFilenameParts(baseName(filepath.Base(member.Path)))
			author, year, title, venue := citationParts(stem)
			if !copyName && author != "" && year != "" && meaningful(title) && meaningful(venue) {
				keeper = member
				g.KeeperReason = "Its filename includes the author, year, title, and publication venue."
				break
			}
		}
	}
	g.RecommendedKeeper = keeper.Index
	for _, member := range g.Members {
		g.Files = append(g.Files, member.Path)
	}
	return g
}

// Section: Text evidence for duplicate comparison.

// Text matching supplements byte identity. It compares a bounded sample and
// never contributes to the decision to recycle a file.
func duplicateText(path string, st stamp) string {
	if st.Size > 16<<20 {
		return ""
	}
	switch strings.ToLower(extension(path)) {
	case ".pdf", ".docx", ".docm", ".txt", ".md", ".odt", ".rtf":
	default:
		return ""
	}
	text := readEvidenceWith(path, false, defaultSettings()).Text
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// Section: Pair selection and identity revalidation.

type duplicateComparison struct {
	Verdict, Detail, Recommendation string
}

func (a *application) duplicatePair(job string, first, second int) (string, record, record, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if s == nil || a.v.Job != job || a.v.Busy || s.manifest.Operation != scanDuplicates {
		return "", record{}, record{}, errors.New("Open the duplicate scan before comparing files")
	}
	if first == second || !s.liveDuplicate(first) || !s.liveDuplicate(second) {
		return "", record{}, record{}, errors.New("Choose two available files from this duplicate scan")
	}
	return s.manifest.Root, s.plans[first], s.plans[second], nil
}

func (s *session) liveDuplicate(index int) bool {
	return index >= 0 && index < len(s.plans) && s.plans[index].Digest != "" && s.plans[index].Decision != removedFlaggedFile
}

func recheckDuplicate(ctx context.Context, root *os.Root, rootPath string, plan record) (stamp, error) {
	if err := safeParent(rootPath, plan.Path); err != nil {
		return stamp{}, err
	}
	rel, err := filepath.Rel(rootPath, plan.Path)
	if err != nil {
		return stamp{}, err
	}
	before, err := fingerprint(plan.Path)
	if err != nil || before.Dir || (plan.Stamp.ID != "" && before != plan.Stamp) {
		return stamp{}, errors.New("A file changed since the scan. Run a fresh duplicate scan.")
	}
	digest, err := scopedDuplicateHash(ctx, root, rel)
	if err != nil {
		return stamp{}, err
	}
	after, err := fingerprint(plan.Path)
	if err != nil || before != after || digest != plan.Digest {
		return stamp{}, errors.New("A file changed since the scan. Run a fresh duplicate scan.")
	}
	return after, nil
}

// Section: Verified pair comparison.

func (a *application) compareDuplicateFiles(ctx context.Context, job string, first, second int) (duplicateComparison, error) {
	rootPath, left, right, err := a.duplicatePair(job, first, second)
	if err != nil {
		return duplicateComparison{}, err
	}
	if err := validateRoot(rootPath); err != nil {
		return duplicateComparison{}, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return duplicateComparison{}, err
	}
	defer root.Close()
	lst, err := recheckDuplicate(ctx, root, rootPath, left)
	if err != nil {
		return duplicateComparison{}, err
	}
	rst, err := recheckDuplicate(ctx, root, rootPath, right)
	if err != nil {
		return duplicateComparison{}, err
	}
	if left.Digest == right.Digest {
		return duplicateComparison{"Exact duplicate", "The full-file SHA-256 hashes match.", "Keep one"}, nil
	}
	if left.EquivalentDigest != "" && left.EquivalentDigest == right.EquivalentDigest {
		return duplicateComparison{"Verified equivalent copies", equivalenceFinding(left.EquivalentDigest), "Keep one"}, nil
	}
	ltext, lfull, err := comparisonText(ctx, root, rootPath, left, lst)
	if err != nil {
		return duplicateComparison{}, err
	}
	rtext, rfull, err := comparisonText(ctx, root, rootPath, right, rst)
	if err != nil {
		return duplicateComparison{}, err
	}
	lafter, lerr := fingerprint(left.Path)
	rafter, rerr := fingerprint(right.Path)
	if lerr != nil || rerr != nil || lafter != lst || rafter != rst {
		return duplicateComparison{}, errors.New("A file changed during comparison. Run a fresh duplicate scan.")
	}
	switch {
	case ltext == "" || rtext == "":
		return duplicateComparison{"Different bytes", "Readable text was unavailable for at least one file.", "Needs your judgment"}, nil
	case !lfull || !rfull:
		if strings.EqualFold(ltext, rtext) {
			return duplicateComparison{"Matching text samples", "The samples match. Unread portions or formatting may differ.", "Needs your judgment"}, nil
		}
		return duplicateComparison{"Different text samples", "The samples differ. A complete content comparison is unavailable.", "Needs your judgment"}, nil
	case ltext == rtext:
		if strings.EqualFold(extension(left.Path), extension(right.Path)) {
			return duplicateComparison{"Matching complete text", "The complete text matches after normalizing line endings. This is an advisory comparison.", "Keep one"}, nil
		}
		return duplicateComparison{"Matching complete text", "The text matches, and the files use different formats.", "Needs your judgment"}, nil
	}
	leftWords, rightWords := strings.Fields(strings.ToLower(ltext)), strings.Fields(strings.ToLower(rtext))
	shared := duplicateTextOverlap(leftWords, rightWords)
	_, lv, _ := duplicateFilenameParts(baseName(filepath.Base(left.Path)))
	_, rv, _ := duplicateFilenameParts(baseName(filepath.Base(right.Path)))
	if lv != "" && rv != "" && compareVersionNumbers(lv, rv) != 0 && shared >= .35 {
		return duplicateComparison{"Different complete text", "The complete texts share passages and the filenames carry different version labels.", "Keep both as versions"}, nil
	}
	if shared < .10 && min(len(leftWords), len(rightWords)) >= 5 {
		return duplicateComparison{"Different complete text", "The full texts have little shared wording.", "Keep both as distinct files"}, nil
	}
	return duplicateComparison{"Different complete text", "The full texts differ. There is insufficient evidence to establish how they are related.", "Needs your judgment"}, nil
}

// Section: Text comparison helpers.

// Complete comparison is limited to bounded, valid UTF-8 text files. Document
// extraction is sampling only, so it cannot establish content equivalence.
func comparisonText(ctx context.Context, root *os.Root, rootPath string, plan record, before stamp) (string, bool, error) {
	const fullLimit = 1 << 20
	ext := strings.ToLower(extension(plan.Path))
	if (ext == ".txt" || ext == ".md") && before.Size <= fullLimit {
		rel, err := filepath.Rel(rootPath, plan.Path)
		if err != nil {
			return "", false, err
		}
		f, err := root.Open(rel)
		if err != nil {
			return "", false, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, fullLimit+1))
		if err != nil {
			return "", false, err
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != plan.Digest {
			return "", false, errors.New("A file changed during comparison. Run a fresh duplicate scan.")
		}
		if len(data) <= fullLimit && utf8.Valid(data) && !strings.ContainsRune(string(data), 0) {
			text := strings.TrimPrefix(string(data), "\uFEFF")
			text = strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
			if strings.TrimSpace(text) != "" {
				return text, true, nil
			}
		}
	}
	return duplicateText(plan.Path, before), false, nil
}

// A shared three-word sequence is stronger revision evidence than a shared word.
func duplicateTextOverlap(left, right []string) float64 {
	sequences := func(words []string) map[[3]string]bool {
		seen := make(map[[3]string]bool)
		for i := 2; i < len(words); i++ {
			seen[[3]string{words[i-2], words[i-1], words[i]}] = true
		}
		return seen
	}
	l, r := sequences(left), sequences(right)
	if len(l) == 0 || len(r) == 0 {
		return 0
	}
	shared := 0
	for sequence := range l {
		if r[sequence] {
			shared++
		}
	}
	return float64(shared) / float64(min(len(l), len(r)))
}

