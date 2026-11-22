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

type moveFolder struct {
	Path, Parent string
	New          bool
	Proposals    []proposalRow
}

type duplicateGroup struct {
	Files                     []string
	Members                   []duplicateMember
	Grouping, VersionEvidence string
}

type operationResults struct {
	Folders           []moveFolder
	Exact             []duplicateGroup
	Possible          []duplicateGroup
	Skipped, Recycled int
	CanRecycle        bool
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

func (s *session) moveFolders() []moveFolder {
	root := s.manifest.Root
	folders := map[string]*moveFolder{root: {Path: root}}
	for _, item := range s.manifest.Items {
		if item.Folder && !item.Skip && within(root, item.Path) && item.Path != root {
			folders[item.Path] = &moveFolder{Path: item.Path, Parent: filepath.Dir(item.Path)}
		}
	}
	for _, plan := range s.plans {
		if plan.Move == nil || !s.reviewable(plan) || s.excluded[plan.Index] {
			continue
		}
		m := plan.Move
		path := filepath.Dir(m.To)
		folder := folders[path]
		if folder == nil {
			folder = &moveFolder{Path: path, Parent: filepath.Dir(path), New: m.NewFolder}
			folders[path] = folder
		}
		if folder.New && safeParent(root, m.To) == nil {
			folder.New = false
		}
		note := plan.Note
		if failure := s.outcomes[plan.Index]; failure.Type == "single-failed" {
			note = failure.Note
		}
		folder.Proposals = append(folder.Proposals, proposalRow{
			Index: plan.Index, Revision: s.planVersions[plan.Index], From: m.From, To: m.To, Note: note, Reason: plan.Reason,
		})
	}
	paths := make([]string, 0, len(folders))
	for path := range folders {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	rows := make([]moveFolder, 0, len(paths))
	for _, path := range paths {
		row := folders[path]
		if path != root && folders[row.Parent] == nil {
			row.Parent = root
		}
		rows = append(rows, *row)
	}
	return rows
}

func (s *session) buildMoveSuggestion(index int, d decision, st stamp, revision bool) record {
	item := s.manifest.Items[index]
	r := record{Type: "plan", Index: index, Path: item.Path, Kind: d.Kind,
		Inspection: d.Inspection, Reason: d.Reason, Note: d.Note, Stamp: st, Decision: "Location kept"}
	if revision {
		r.Type = "plan-revision"
	}
	if item.Folder {
		r.Decision = "Folder indexed"
		return r
	}
	if item.Skip || st.ID == "" {
		r.Decision = "Skipped"
		return r
	}
	if d.Folder == "" || samePath(d.Folder, filepath.Dir(item.Path)) {
		return r
	}
	if !within(s.manifest.Root, d.Folder) {
		r.Note = "The destination is outside the selected folder"
		return r
	}
	m := move{From: item.Path, To: filepath.Join(d.Folder, filepath.Base(item.Path)), Stamp: st, NewFolder: d.NewFolder}
	source, err := fingerprint(filepath.Dir(m.From))
	if err != nil || !source.Dir {
		r.Note = "The source folder could not be verified"
		return r
	}
	m.SourceParent = source.ID
	if m.NewFolder {
		ancestor, err := fingerprint(filepath.Dir(d.Folder))
		if err != nil || !ancestor.Dir {
			r.Note = "The proposed folder's parent could not be verified"
			return r
		}
		m.TargetAncestor = ancestor.ID
	} else {
		target, err := fingerprint(d.Folder)
		if err != nil || !target.Dir {
			r.Note = "The destination folder could not be verified"
			return r
		}
		m.TargetParent = target.ID
	}
	if err := s.validateMoveParents(m); err != nil {
		r.Note = err.Error()
		return r
	}
	available := s.availableForPlan
	if revision && s.plans[index].Move != nil && samePath(s.plans[index].Move.To, m.To) {
		available = nameAvailable
	}
	if err := available(m.From, m.To); err != nil {
		r.Note = err.Error()
		return r
	}
	r.Move, r.Decision = &m, "Move proposed"
	return r
}

func (s *session) validateProposedFolder(m move) error {
	root, folder := s.manifest.Root, filepath.Dir(m.To)
	if err := safeParent(root, m.From); err != nil {
		return err
	}
	source, err := fingerprint(filepath.Dir(m.From))
	if err != nil || !source.Dir || source.ID != m.SourceParent {
		return errors.New("The source folder changed after the scan")
	}
	if err := safeParent(root, folder); err != nil {
		return err
	}
	if !validProposedName(filepath.Base(folder), "", true) {
		return errors.New("The proposed folder name is invalid")
	}
	ancestor, err := fingerprint(filepath.Dir(folder))
	if err != nil || !ancestor.Dir || ancestor.ID != m.TargetAncestor {
		return errors.New("The proposed folder's parent changed after the scan")
	}
	return nil
}

func (s *session) prepareMoveFolder(m *move) error {
	if err := s.validateProposedFolder(*m); err != nil {
		return err
	}
	folder := filepath.Dir(m.To)
	if err := os.Mkdir(folder, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	target, err := fingerprint(folder)
	if err != nil || !target.Dir {
		return errors.New("The proposed folder is unavailable or is a link")
	}
	m.TargetParent = target.ID
	return s.validateMoveParents(*m)
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

var copySuffix = regexp.MustCompile(`(?i)\s*(?:\([0-9]+\)|[ _-]+(?:copy(?:[ _-]*[0-9]+)?|(?:version|rev|draft|v)[ _-]*[0-9]+(?:\.[0-9]+)*))$`)

func similarFilename(path string) string {
	stem := baseName(filepath.Base(path))
	for trimmed := copySuffix.ReplaceAllString(stem, ""); trimmed != stem; trimmed = copySuffix.ReplaceAllString(stem, "") {
		stem = trimmed
	}
	stem = norm(stem)
	if !meaningful(stem) {
		return ""
	}
	return stem + strings.ToLower(extension(path))
}

func (s *session) duplicateResults() operationResults {
	exact := make(map[string][]record)
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
	// A group with the same members only needs one entry. Prefer the text evidence.
	seen := make(map[string]bool)
	for _, data := range []struct {
		groups map[string][]record
		label  string
	}{
		{byText, "Matching text samples"}, {byName, "Similar filenames"},
	} {
		for _, group := range data.groups {
			if len(group) < 2 || !slices.ContainsFunc(group[1:], func(p record) bool { return p.Digest != group[0].Digest }) {
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
			result.Possible = append(result.Possible, s.describeDuplicates(group, data.label))
		}
	}
	for _, groups := range [][]duplicateGroup{result.Exact, result.Possible} {
		slices.SortFunc(groups, func(a, b duplicateGroup) int { return strings.Compare(a.Files[0], b.Files[0]) })
	}
	return result
}
