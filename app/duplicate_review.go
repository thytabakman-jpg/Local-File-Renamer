package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type duplicateMember struct {
	Index         int
	Path, Version string
	Modified      time.Time
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
	numbered := 0
	versions := map[string]bool{}
	distinct := true
	for _, p := range plans {
		item := s.manifest.Items[p.Index]
		member := duplicateMember{Index: p.Index, Path: p.Path}
		if item.Modified != 0 {
			member.Modified = time.Unix(0, item.Modified)
		}
		stem := baseName(filepath.Base(p.Path))
		_, member.Version, _ = duplicateFilenameParts(stem)
		if member.Version != "" {
			numbered++
			versionKey := member.Version
			for strings.HasSuffix(versionKey, ".0") {
				versionKey = strings.TrimSuffix(versionKey, ".0")
			}
			if versions[versionKey] {
				distinct = false
			}
			versions[versionKey] = true
		}
		g.Members = append(g.Members, member)
	}
	if reason == "Identical file contents" {
		slices.SortFunc(g.Members, func(a, b duplicateMember) int { return strings.Compare(a.Path, b.Path) })
		keeper := g.Members[0]
		g.KeeperReason = "If every name looks like a copy, keep the first path alphabetically."
		for _, member := range g.Members {
			_, _, copyName := duplicateFilenameParts(baseName(filepath.Base(member.Path)))
			if !copyName {
				keeper = member
				g.KeeperReason = "Prefer a name without a copy suffix. Break ties by alphabetical path."
				break
			}
		}
		g.RecommendedKeeper = keeper.Index
	} else {
		switch {
		case numbered == len(plans) && distinct:
			g.VersionEvidence = "Order suggested by the version labels in the filenames. Compare contents to confirm the files are revisions."
			slices.SortStableFunc(g.Members, func(a, b duplicateMember) int {
				return compareVersionNumbers(a.Version, b.Version)
			})
		case numbered > 0:
			g.VersionEvidence = "Some version labels are missing or repeated. Version order is uncertain."
		default:
			g.VersionEvidence = "Version order is uncertain. Modification times are shown for reference."
		}
	}
	for _, member := range g.Members {
		g.Files = append(g.Files, member.Path)
	}
	return g
}

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

type duplicateSelection struct {
	Keep    int
	Indices []int
}

type duplicateRecycleOutcome struct {
	Kept      string
	Requested int
	Recycled  int
}

type duplicateRecycleReceipt struct {
	Groups   []duplicateRecycleOutcome
	Recycled int
}

func (a *application) recycleMatchedCopies(ctx context.Context, job string, revision uint64, keep int, selected []int, recycle func(string) error) (int, error) {
	receipt, err := a.recycleDuplicateGroups(ctx, job, revision, []duplicateSelection{{Keep: keep, Indices: selected}}, recycle)
	return receipt.Recycled, err
}

func (a *application) recycleDuplicateGroups(ctx context.Context, job string, revision uint64, groups []duplicateSelection, recycle func(string) error) (duplicateRecycleReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var receipt duplicateRecycleReceipt
	s := a.s
	if s == nil || a.v.Job != job || a.v.Revision != revision || a.v.Busy || a.v.Closing || s.pending != nil || s.manifest.Operation != scanDuplicates || s.settings().Execution.Mode == "preview_only" || len(groups) == 0 {
		return receipt, errors.New("Review exact duplicates in the current scan and choose the copies to recycle")
	}
	seen := make(map[int]bool)
	for _, group := range groups {
		if seen[group.Keep] || !s.liveDuplicate(group.Keep) || len(group.Indices) == 0 {
			return receipt, errors.New("Choose one available keeper and at least one copy in each group")
		}
		seen[group.Keep] = true
		keeper := s.plans[group.Keep]
		for _, index := range group.Indices {
			if seen[index] || !s.liveDuplicate(index) || s.plans[index].Digest != keeper.Digest {
				return receipt, errors.New("Each selected copy must occur once and be byte-identical to its keeper")
			}
			seen[index] = true
		}
		receipt.Groups = append(receipt.Groups, duplicateRecycleOutcome{Kept: keeper.Path, Requested: len(group.Indices)})
	}
	if err := validateRoot(s.manifest.Root); err != nil {
		return receipt, err
	}
	root, err := os.OpenRoot(s.manifest.Root)
	if err != nil {
		return receipt, err
	}
	defer root.Close()
	for _, group := range groups {
		for _, index := range append([]int{group.Keep}, group.Indices...) {
			if _, err := recheckDuplicate(ctx, root, s.manifest.Root, s.plans[index]); err != nil {
				return receipt, fmt.Errorf("Nothing recycled. %s: %w", filepath.Base(s.plans[index].Path), err)
			}
		}
	}
	defer a.syncSession(true)
	for i, group := range groups {
		keeper := s.plans[group.Keep]
		keptStamp, err := recheckDuplicate(ctx, root, s.manifest.Root, keeper)
		if err != nil {
			return receipt, err
		}
		for _, index := range group.Indices {
			if err := ctx.Err(); err != nil {
				return receipt, err
			}
			plan := s.plans[index]
			proof, err := recheckDuplicate(ctx, root, s.manifest.Root, plan)
			if err != nil {
				return receipt, fmt.Errorf("%s: %w", filepath.Base(plan.Path), err)
			}
			currentKept, err := fingerprint(keeper.Path)
			if err != nil || currentKept != keptStamp || safeParent(s.manifest.Root, keeper.Path) != nil {
				return receipt, errors.New("The copy selected to keep is no longer available")
			}
			if err := s.append(record{Type: "recycle-intent", Index: index, Path: plan.Path, Stamp: proof}); err != nil {
				return receipt, err
			}
			recycleErr := recycle(plan.Path)
			recordErr := s.finishRecycle()
			if s.plans[index].Decision == removedFlaggedFile {
				receipt.Groups[i].Recycled++
				receipt.Recycled++
			}
			if err := errors.Join(recycleErr, recordErr); err != nil {
				return receipt, fmt.Errorf("%s: %w", filepath.Base(plan.Path), err)
			}
			if s.plans[index].Decision != removedFlaggedFile {
				return receipt, fmt.Errorf("The Recycle Bin did not remove %s", filepath.Base(plan.Path))
			}
		}
	}
	return receipt, nil
}
