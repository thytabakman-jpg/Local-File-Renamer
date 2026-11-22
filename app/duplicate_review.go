package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type duplicateMember struct {
	Index         int
	Path, Version string
	Modified      time.Time
}

var duplicateVersion = regexp.MustCompile(`(?i)(?:^|[ _\-\[(])(?:version|rev|draft|v)[ ._\-]*([1-9][0-9]*(?:\.[0-9]+)*)[\])]?$`)

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
		if match := duplicateVersion.FindStringSubmatch(stem); match != nil {
			member.Version = match[1]
		} else if version, explicit := readVersion(stem); explicit && version != pendingVersion {
			member.Version = version
		}
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
	if reason != "Identical file contents" {
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
	Verdict, Detail string
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
		return duplicateComparison{"Exact duplicate", "The full-file SHA-256 hashes match."}, nil
	}
	ltext, rtext := duplicateText(left.Path, lst), duplicateText(right.Path, rst)
	lafter, lerr := fingerprint(left.Path)
	rafter, rerr := fingerprint(right.Path)
	if lerr != nil || rerr != nil || lafter != lst || rafter != rst {
		return duplicateComparison{}, errors.New("A file changed during comparison. Run a fresh duplicate scan.")
	}
	switch {
	case ltext == "" || rtext == "":
		return duplicateComparison{"Different files", "The file bytes differ. A text comparison is unavailable for this pair."}, nil
	case ltext == rtext:
		return duplicateComparison{"Matching text samples", "The file bytes differ. The text samples match. Check the previews before treating these as copies."}, nil
	default:
		return duplicateComparison{"Different text samples", "The file bytes and the extracted text samples differ. Review both previews to check whether these are revisions."}, nil
	}
}

func (a *application) recycleExactDuplicates(ctx context.Context, job string, revision uint64, keep int, selected []int) (int, error) {
	return a.recycleMatchedCopies(ctx, job, revision, keep, selected, recycleFile)
}

func (a *application) recycleMatchedCopies(ctx context.Context, job string, revision uint64, keep int, selected []int, recycle func(string) error) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if s == nil || a.v.Job != job || a.v.Revision != revision || a.v.Busy || a.v.Closing || s.pending != nil || s.manifest.Operation != scanDuplicates || s.settings().Execution.Mode == "preview_only" || !s.liveDuplicate(keep) || len(selected) == 0 {
		return 0, errors.New("Review the current exact duplicate group and confirm a copy to keep")
	}
	keeper := s.plans[keep]
	seen := map[int]bool{keep: true}
	for _, index := range selected {
		if seen[index] || !s.liveDuplicate(index) || s.plans[index].Digest != keeper.Digest {
			return 0, errors.New("Every selected file must be an exact copy of the file you chose to keep")
		}
		seen[index] = true
	}
	if err := validateRoot(s.manifest.Root); err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(s.manifest.Root)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	keptStamp, err := recheckDuplicate(ctx, root, s.manifest.Root, keeper)
	if err != nil {
		return 0, err
	}
	defer a.syncSession(true)
	moved := 0
	for _, index := range selected {
		plan := s.plans[index]
		proof, err := recheckDuplicate(ctx, root, s.manifest.Root, plan)
		if err != nil {
			return moved, err
		}
		currentKept, err := fingerprint(keeper.Path)
		if err != nil || currentKept != keptStamp || safeParent(s.manifest.Root, keeper.Path) != nil {
			return moved, errors.New("The copy selected to keep is no longer available")
		}
		if err := ctx.Err(); err != nil {
			return moved, err
		}
		if err := s.append(record{Type: "recycle-intent", Index: index, Path: plan.Path, Stamp: proof}); err != nil {
			return moved, err
		}
		recycleErr := recycle(plan.Path)
		recordErr := s.finishRecycle()
		if err := errors.Join(recycleErr, recordErr); err != nil {
			return moved, err
		}
		if s.plans[index].Decision != removedFlaggedFile {
			return moved, fmt.Errorf("The Recycle Bin did not remove %s", filepath.Base(plan.Path))
		}
		moved++
	}
	return moved, nil
}
