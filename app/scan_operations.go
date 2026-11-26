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
	"slices"
	"strings"
)

type scanOperation string

const (
	scanRename     scanOperation = "rename"
	scanMove       scanOperation = "move"
	scanDuplicates scanOperation = "duplicates"
)

func (operation scanOperation) valid() bool {
	return operation == "" || operation == scanRename || operation == scanMove || operation == scanDuplicates
}

func samePath(a, b string) bool {
	relative, err := filepath.Rel(a, b)
	return err == nil && relative == "."
}

type duplicateGroup struct {
	Files                           []string
	Members                         []duplicateMember
	Grouping, KeeperReason, Finding string
	RecommendedKeeper               int
}

type operationResults struct {
	Folders           []moveFolder
	Exact             []duplicateGroup
	Verified          []duplicateGroup
	Unresolved        int
	Skipped, Recycled int
	CanRecycle        bool
	possible          []duplicateGroup
}

func (a *application) scanResults(id string) (operationResults, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if id == "" || id != a.v.Job || s == nil || a.v.Busy {
		return operationResults{}, errors.New("Open the saved scan or wait for the current scan to pause or finish")
	}
	switch s.manifest.Operation {
	case scanMove:
		return operationResults{Folders: s.moveFolders()}, nil
	case scanDuplicates:
		return s.duplicateResults(), nil
	default:
		return operationResults{}, errors.New("This run uses the filename review")
	}
}

func (a *application) runDuplicates(ctx context.Context, s *session) {
	if err := validateRoot(s.manifest.Root); err != nil {
		a.finish(err)
		return
	}
	root, err := os.OpenRoot(s.manifest.Root)
	if err != nil {
		a.finish(err)
		return
	}
	defer root.Close()
	a.mu.Lock()
	index := len(s.plans)
	a.mu.Unlock()
	var failure error
	for ; index < len(s.manifest.Items) && ctx.Err() == nil; index++ {
		item := s.manifest.Items[index]
		a.mu.Lock()
		a.v.Phase = "files"
		a.v.Current = item.Path
		a.mu.Unlock()

		r := record{Type: "plan", Index: index, Path: item.Path, Decision: "Checked", Note: item.Note}
		switch {
		case item.Skip:
			r.Decision = "Skipped"
		default:
			err := safeParent(s.manifest.Root, item.Path)
			if err == nil {
				relative, relErr := filepath.Rel(s.manifest.Root, item.Path)
				if relErr == nil {
					r.Digest, err = scopedDuplicateHash(ctx, root, relative)
					if err == nil {
						r.Stamp, err = fingerprint(item.Path)
					}
					if err == nil {
						r.EquivalentDigest = duplicateEquivalence(ctx, root, relative, r.Stamp, r.Digest)
						if text := duplicateText(item.Path, r.Stamp); text != "" {
							digest := sha256.Sum256([]byte(text))
							r.TextDigest = hex.EncodeToString(digest[:])
						}
					}
				} else {
					err = relErr
				}
			}
			if err != nil {
				r.Decision, r.Note = "Skipped", err.Error()
				r.Digest, r.TextDigest = "", ""
			}
		}
		if ctx.Err() != nil {
			break
		}
		a.mu.Lock()
		failure = s.append(r)
		a.syncSession(false)
		a.mu.Unlock()
		if failure != nil {
			break
		}
	}
	a.finish(failure)
}

func scopedDuplicateHash(ctx context.Context, root *os.Root, path string) (string, error) {
	before, err := root.Lstat(path)
	if err != nil {
		return "", err
	}
	if linked(before) || !before.Mode().IsRegular() {
		return "", errors.New("The file is a link or special file")
	}
	f, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	matches := func(info os.FileInfo) bool {
		return info.Mode().IsRegular() && !linked(info) && os.SameFile(before, info) &&
			before.Size() == info.Size() && before.ModTime().Equal(info.ModTime())
	}
	opened, err := f.Stat()
	if err != nil || !matches(opened) {
		return "", errors.New("The file changed before duplicate checking")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx, f}); err != nil {
		return "", err
	}
	after, err := root.Lstat(path)
	if err != nil || !matches(after) {
		return "", errors.New("The file changed during duplicate checking")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func similarFilename(path string) string {
	stem, _, _ := duplicateFilenameParts(baseName(filepath.Base(path)))
	stem = norm(stem)
	if !meaningful(stem) {
		return ""
	}
	return stem + strings.ToLower(extension(path))
}

func (s *session) duplicateResults() operationResults {
	return s.duplicateResultsWithCandidates(false)
}

func (s *session) duplicateResultsWithCandidates(details bool) operationResults {
	exact := make(map[string][]record)
	equivalent := make(map[string][]record)
	byName := make(map[string][]record)
	byText := make(map[string][]record)
	result := operationResults{Recycled: s.recycled, CanRecycle: recycleAvailable && s.settings().Execution.Mode != "preview_only"}
	for _, plan := range s.plans {
		if plan.Decision == removedFlaggedFile {
			continue
		}
		if plan.Digest == "" {
			result.Skipped++
			continue
		}
		exact[plan.Digest] = append(exact[plan.Digest], plan)
		if plan.EquivalentDigest != "" {
			equivalent[plan.EquivalentDigest] = append(equivalent[plan.EquivalentDigest], plan)
		}
		if key := similarFilename(plan.Path); key != "" {
			byName[key] = append(byName[key], plan)
		}
		if plan.TextDigest != "" {
			byText[plan.TextDigest] = append(byText[plan.TextDigest], plan)
		}
	}
	for _, group := range exact {
		if len(group) > 1 {
			result.Exact = append(result.Exact, s.describeDuplicates(group, "Identical file contents"))
		}
	}
	verified := make(map[int]string)
	for key, group := range equivalent {
		// Finish overlapping exact cleanup first so one approval cannot recycle
		// the keeper of another recommendation.
		if len(group) < 2 || slices.ContainsFunc(group, func(p record) bool { return len(exact[p.Digest]) != 1 }) {
			continue
		}
		recommendation := s.describeDuplicates(group, "Verified equivalent copies")
		recommendation.Finding = equivalenceFinding(key)
		result.Verified = append(result.Verified, recommendation)
		for _, p := range group {
			verified[p.Index] = key
		}
	}
	// Count a candidate group once even when both signals found it.
	seen := make(map[string]bool)
	for _, groups := range []map[string][]record{byText, byName} {
		for _, group := range groups {
			if len(group) < 2 || !slices.ContainsFunc(group[1:], func(p record) bool { return p.Digest != group[0].Digest }) {
				continue
			}
			proof := verified[group[0].Index]
			if proof != "" && !slices.ContainsFunc(group[1:], func(p record) bool { return verified[p.Index] != proof }) {
				continue
			}
			var key strings.Builder
			for _, p := range group {
				fmt.Fprintf(&key, "%d,", p.Index)
			}
			if seen[key.String()] {
				continue
			}
			seen[key.String()] = true
			result.Unresolved++
			if details {
				candidate := s.describeDuplicates(group, "Unresolved candidate")
				result.possible = append(result.possible, candidate)
			}
		}
	}
	for _, groups := range [][]duplicateGroup{result.Exact, result.Verified, result.possible} {
		slices.SortFunc(groups, func(a, b duplicateGroup) int { return slices.Compare(a.Files, b.Files) })
	}
	return result
}
