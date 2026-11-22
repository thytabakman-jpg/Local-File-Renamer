package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

type entry struct {
	Path         string
	Folder, Skip bool
	Note         string
	Hash         string `json:",omitempty"`
	Modified     int64  `json:",omitempty"`
}
type manifest struct {
	Inventory                         *inventoryCounts `json:",omitempty"`
	Operation                         scanOperation    `json:",omitempty"`
	Root                              string
	Items                             []entry
	Created                           time.Time
	Preview, RenameFolders            bool
	Auto, RepairMetadata, SortFolders bool
	Partial                           bool
	Rules                             *namingRules  `json:",omitempty"`
	Settings                          *scanSettings `json:",omitempty"`
}
type stamp struct {
	Dir            bool
	Size, Modified int64
	ID             string
}
type move struct {
	From, To                   string
	Stamp                      stamp
	SourceParent, TargetParent string `json:",omitempty"`
	NewFolder                  bool   `json:",omitempty"`
	TargetAncestor             string `json:",omitempty"`
}
type renameMode uint8

const (
	batchRename renameMode = iota
	automaticRename
	singleRename
)

type record struct {
	Type                                         string
	At                                           time.Time `json:",omitzero"`
	Clock                                        scanClock `json:",omitzero"`
	RenameMS                                     int64     `json:",omitempty"`
	Index                                        int
	Status, Note, Path                           string
	Kind, Decision, Reason, OverrideKind         string               `json:",omitempty"`
	Move                                         *move                `json:",omitempty"`
	Inspection                                   inspection           `json:",omitempty"`
	Digest, TextDigest                           string               `json:",omitempty"`
	Stamp                                        stamp                `json:",omitempty"`
	Unknown, Auto, Partial, AutoValidated, Rough bool                 `json:",omitempty"`
	Metadata                                     *metadataEdit        `json:",omitempty"`
	Transaction                                  *metadataTransaction `json:",omitempty"`
	Selection                                    []int                `json:",omitempty"`
	Excluded                                     []int                `json:",omitempty"`
}
type indexedMove struct {
	move     move
	position int
	reason   string
}
type session struct {
	filesAssessed, filesSkipped, filesVerified, filesNotAssessed, filesKept int
	dir, cacheDir                                                           string
	online                                                                  *onlineLookup
	planRevision                                                            uint64
	planVersions                                                            []uint64
	selection                                                               []int
	operationError                                                          string
	undoFailures                                                            int
	clock                                                                   scanClock
	clockLast                                                               time.Time
	outcomes                                                                map[int]record
	manifest                                                                manifest
	log                                                                     *os.File
	index, renamed, kept, skipped, recycled, moved                          int
	flags                                                                   []record
	moves                                                                   []move
	pending                                                                 *record
	undoCount                                                               int
	byItem, automaticMoves                                                  map[int]indexedMove
	folderByPath                                                            map[string]indexedMove
	versionGroups                                                           map[string][]int
	imageVersions                                                           map[int]string
	uncertainImageVersions                                                  map[int]bool
	plans                                                                   []record
	planFlags                                                               []record
	proposedNames                                                           map[string]string
	proposedTargets                                                         map[string]bool
	partial                                                                 bool
	applyThrough, fileCount                                                 int
	resolvedPlanFlags                                                       int
	sortIndex                                                               *folderIndex
	relocationParents                                                       map[string]bool
	autoCompleted                                                           map[int]bool
	autoHeld                                                                map[int]bool
	plansByParent                                                           map[string][]record
	moveIndexByTarget                                                       map[string]int
	excluded                                                                map[int]bool
	metadataIndex, metadataUndone                                           int
	metadataPlans, repaired                                                 []record
	restoredStamps                                                          map[int]stamp
}

func inventory(ctx context.Context, root string) ([]entry, error) {
	return inventoryScope(ctx, root, defaultSettings().Scope)
}

func scopeMatch(path string, list []string, subtree bool) bool {
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	for _, candidate := range list {
		candidate = filepath.ToSlash(strings.ReplaceAll(candidate, "\\", "/"))
		if runtime.GOOS == "windows" {
			candidate = strings.ToLower(candidate)
		}
		if path == candidate || subtree && strings.HasPrefix(path, candidate+"/") {
			return true
		}
	}
	return false
}

func inventoryScope(ctx context.Context, root string, scope scanScope) ([]entry, error) {
	items, _, err := scanInventory(ctx, root, scope)
	return items, err
}

func scanInventory(ctx context.Context, root string, scope scanScope) ([]entry, inventoryCounts, error) {
	var files, dirs []entry
	var counts inventoryCounts
	types := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if stop := ctx.Err(); stop != nil {
			return stop
		}
		if path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		excluded := scopeMatch(rel, scope.ExcludePaths, true)
		if d != nil && d.IsDir() && (excluded || !scope.Subfolders) {
			counts.OmittedFolders++
			return filepath.SkipDir
		}
		if d == nil || !d.IsDir() {
			ext := extension(path)
			matchExt := func(list []string) bool {
				return slices.ContainsFunc(list, func(value string) bool { return strings.EqualFold(value, ext) }) || ext != filepath.Ext(path) && slices.ContainsFunc(list, func(value string) bool { return strings.EqualFold(value, filepath.Ext(path)) })
			}
			excluded = excluded || len(scope.IncludePaths) > 0 && !scopeMatch(rel, scope.IncludePaths, true) ||
				len(scope.SelectedPaths) > 0 && !scopeMatch(rel, scope.SelectedPaths, false) ||
				len(scope.IncludeExtensions) > 0 && !matchExt(scope.IncludeExtensions) || matchExt(scope.ExcludeExtensions)
		}
		if err != nil {
			counts.Unavailable++
			if !excluded {
				files = append(files, entry{Path: path, Skip: true, Note: "Unable to inspect: " + err.Error()})
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			counts.Unavailable++
			if !excluded {
				files = append(files, entry{Path: path, Skip: true, Note: "Unable to inspect: " + err.Error()})
			}
			return nil
		}
		if linked(info) {
			counts.Unavailable++
			if !excluded {
				files = append(files, entry{Path: path, Skip: true, Note: "Link, junction or reparse point"})
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			counts.Folders++
			if len(scope.SelectedPaths) == 0 && (len(scope.IncludePaths) == 0 || scopeMatch(rel, scope.IncludePaths, true)) {
				dirs = append(dirs, entry{Path: path, Folder: true})
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			counts.Unavailable++
			if !excluded {
				files = append(files, entry{Path: path, Skip: true, Note: "Special file"})
			}
			return nil
		}
		counts.Found++
		if ext := strings.ToLower(filepath.Ext(path)); ext != "" {
			types[ext] = true
		}
		if excluded || info.Size() < scope.MinBytes || scope.MaxBytes > 0 && info.Size() > scope.MaxBytes {
			counts.Filtered++
			return nil
		}
		counts.Included++
		item := entry{Path: path, Modified: info.ModTime().UnixNano()}
		if scope.VerifyComparison {
			hash, err := comparisonHash(ctx, path)
			if err != nil {
				return err
			}
			item.Hash = hash
		}
		files = append(files, item)
		return nil
	})
	for ext := range types {
		counts.FileTypes = append(counts.FileTypes, ext)
	}
	slices.Sort(counts.FileTypes)
	sort.SliceStable(dirs, func(i, j int) bool {
		return strings.Count(dirs[i].Path, string(os.PathSeparator)) > strings.Count(dirs[j].Path, string(os.PathSeparator))
	})
	return append(files, dirs...), counts, err
}

func createSession(ctx context.Context, home, root string, m manifest) (*session, error) {
	return createNamedSession(ctx, home, root, m, "")
}

func createNamedSession(ctx context.Context, home, root string, m manifest, id string) (*session, error) {
	scope := defaultSettings().Scope
	if m.Settings != nil {
		scope = m.Settings.Scope
	}
	items, counts, err := scanInventory(ctx, root, scope)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}
	var dir string
	if id == "" {
		dir, err = os.MkdirTemp(home, time.Now().UTC().Format("20060102T150405.000000000")+"-")
	} else {
		dir = filepath.Join(home, id)
		err = os.Mkdir(dir, 0700)
	}
	if err != nil {
		return nil, err
	}
	if m.Operation == scanDuplicates {
		items = slices.DeleteFunc(items, func(item entry) bool { return item.Folder })
	}
	m.Root, m.Items, m.Created, m.Inventory = root, items, time.Now(), &counts
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0600); err != nil {
		return nil, err
	}
	return openSession(dir)
}

func openSession(dir string) (*session, error) {
	return readSession(dir, false)
}

func openSessionReadOnly(dir string) (*session, error) {
	return readSession(dir, true)
}

func readSession(dir string, readOnly bool) (*session, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	s := &session{dir: dir, outcomes: make(map[int]record), byItem: make(map[int]indexedMove), automaticMoves: make(map[int]indexedMove), folderByPath: make(map[string]indexedMove), proposedNames: make(map[string]string), proposedTargets: make(map[string]bool), autoCompleted: make(map[int]bool), autoHeld: make(map[int]bool),
		excluded: make(map[int]bool), plansByParent: make(map[string][]record), moveIndexByTarget: make(map[string]int), restoredStamps: make(map[int]stamp), relocationParents: make(map[string]bool)}
	if err = json.Unmarshal(b, &s.manifest); err != nil {
		return nil, err
	}
	s.partial = s.manifest.Partial
	s.versionGroups = groupVersionCopies(s.manifest.Items)
	s.imageVersions, s.uncertainImageVersions = imageVersionGroups(s.manifest.Items)
	s.fileCount = len(s.manifest.Items)
	for i, item := range s.manifest.Items {
		if item.Folder {
			s.fileCount = i
			break
		}
	}
	if s.manifest.SortFolders {
		s.sortIndex = indexFolders(s.manifest.Root, s.manifest.Items)
		if s.manifest.Operation == scanMove {
			s.sortIndex.enableMoveScan(s.manifest.Items)
		}
	}
	mode := os.O_CREATE | os.O_RDWR
	if readOnly {
		mode = os.O_RDONLY
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), mode, 0600)
	if err != nil {
		return nil, err
	}
	s.log = f
	reader := bufio.NewReader(f)
	var offset int64
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// An interrupted append may leave a partial trailing record.
			if len(line) > 0 && !readOnly {
				if err := f.Truncate(offset); err != nil {
					s.close()
					return nil, err
				}
			}
			break
		}
		if err != nil {
			s.close()
			return nil, err
		}
		var r record
		if err = json.Unmarshal(line, &r); err != nil {
			s.close()
			return nil, fmt.Errorf("journal is unreadable at byte %d: %w", offset, err)
		}
		if err := s.validateRecord(r); err != nil {
			s.close()
			return nil, fmt.Errorf("invalid journal at byte %d: %w", offset, err)
		}
		s.accept(r)
		offset += int64(len(line))
	}
	_, err = f.Seek(0, 2)
	if err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *session) close() {
	if s != nil {
		_ = s.log.Close()
	}
}
func (s *session) validateRecord(r record) error {
	switch r.Type {
	case "plan":
		if r.Index != len(s.plans) || r.Index >= len(s.manifest.Items) {
			return errors.New("plan index does not match scan inventory")
		}
	case "image-refinement":
		if !s.canRefine(r.Index) || r.Move == nil || r.Path != s.plans[r.Index].Path ||
			r.Move.From != s.byItem[r.Index].move.To {
			return errors.New("refinement does not match the earlier image rename")
		}
	case "plan-revision":
		if r.Index < s.applyThrough || r.Index >= len(s.plans) || s.autoCompleted[r.Index] ||
			r.Path != s.manifest.Items[r.Index].Path {
			return errors.New("revision does not match an unapplied plan")
		}
	case "recycle-intent":
		if s.pending != nil || r.Index < s.applyThrough || r.Index >= len(s.plans) || s.autoCompleted[r.Index] ||
			r.Path != s.manifest.Items[r.Index].Path || r.Stamp.ID == "" || r.Stamp.Dir {
			return errors.New("recycle request does not match an unapplied file")
		}
	case "recycle-result":
		if s.pending == nil || s.pending.Type != "recycle-intent" || s.pending.Index != r.Index ||
			s.pending.Path != r.Path || (r.Status != "present" && r.Status != "absent") {
			return errors.New("recycle result does not match its request")
		}
	case "apply-selection":
		if len(s.selection) != 0 || len(r.Selection) == 0 {
			return errors.New("invalid pending selection")
		}
		for _, i := range r.Selection {
			if i < s.applyThrough || i >= len(s.plans) {
				return errors.New("selection index does not match an assessed item")
			}
		}
	case "selected-metadata":
		if len(s.selection) == 0 || r.Index != s.selection[0] || s.plans[r.Index].Metadata == nil {
			return errors.New("metadata approval does not match the selected item")
		}
	case "selection-done":
		if len(s.selection) == 0 || r.Index != s.selection[0] {
			return errors.New("selection result does not match its approval")
		}
	case "approved", "apply-batch":
		if r.Type == "apply-batch" && (r.Index <= s.applyThrough || r.Index > len(s.plans)) {
			return errors.New("invalid approval range")
		}
	case "result":
		if r.Index < 0 || r.Index >= len(s.manifest.Items) || r.Auto && r.Index >= len(s.plans) {
			return errors.New("invalid result index")
		}
	case "single-failed", "automatic-failed":
		if r.Index < 0 || r.Index >= len(s.plans) {
			return errors.New("invalid single rename result index")
		}
	case "intent", "undo-intent", "single-intent":
		if r.Move == nil {
			return errors.New("missing recorded move")
		}
	case "metadata-intent", "metadata-undo-intent":
		if r.Transaction == nil {
			return errors.New("missing recorded metadata operation")
		}
	}
	return nil
}

func (s *session) countFileAssessment(r record, increment int) {
	if r.Index >= s.fileCount {
		return
	}
	switch planOutcome(r) {
	case "Skipped":
		s.filesSkipped += increment
	case "Not assessed":
		s.filesNotAssessed += increment
	case "Analysis failed":
	case "Name verified":
		s.filesVerified += increment
		s.filesKept += increment
		s.filesAssessed += increment
	case "Kept existing name", "Name kept by operator", "Name kept by preference":
		s.filesKept += increment
		s.filesAssessed += increment
	default:
		s.filesAssessed += increment
	}
}

func (s *session) accept(r record) {
	if !r.At.IsZero() {
		s.clockLast = r.At
	}
	if (r.Type == "result" || r.Type == "single-error" || r.Type == "single-failed" || r.Type == "automatic-failed") && s.outcomes != nil {
		s.outcomes[r.Index] = r
	}

	switch r.Type {
	case "scan-start", "scan-stop":
		s.clock = r.Clock
		s.operationError = r.Note
	case "operation-error":
		s.operationError = r.Note
	case "scan-mode":
		s.partial = r.Partial
	case "image-refinement", "plan-revision":
		if r.Type == "image-refinement" {
			delete(s.autoCompleted, r.Index)
		}
		s.countFileAssessment(s.plans[r.Index], -1)
		s.countFileAssessment(r, 1)
		s.plans[r.Index] = r
		delete(s.outcomes, r.Index)
		s.planRevision++
		s.planVersions[r.Index] = s.planRevision
		s.reindexPlans()
	case "plan":
		s.countFileAssessment(r, 1)
		s.plans = append(s.plans, r)
		s.planRevision++
		s.planVersions = append(s.planVersions, s.planRevision)
		s.indexPlan(r)
	case "apply-selection":
		s.selection = append([]int(nil), r.Selection...)
	case "selection-done":
		s.selection = s.selection[1:]
	case "selected-metadata":
		s.metadataPlans = append(s.metadataPlans, s.plans[r.Index])
	case "approved", "apply-batch":
		s.planRevision++
		end := r.Index
		if r.Type == "approved" { // Releases 2.0 and 2.1 saved a single full-run approval.
			end = len(s.plans)
		}
		for _, i := range r.Excluded {
			s.excluded[i] = true
		}
		for _, plan := range s.plans[s.applyThrough:end] {
			if plan.Metadata != nil && !s.excluded[plan.Index] {
				s.metadataPlans = append(s.metadataPlans, plan)
			}
		}
		s.applyThrough = end
	case "advance":
		s.index = r.Index + 1
	case "intent", "undo-intent", "single-intent", "metadata-intent", "metadata-undo-intent", "recycle-intent":
		s.pending = &r
	case "recycle-result":
		s.pending = nil
		s.planRevision++
		s.planVersions[r.Index] = s.planRevision
		if r.Status == "absent" {
			s.recycled++
			plan := s.plans[r.Index]
			s.countFileAssessment(plan, -1)
			plan.Move, plan.Metadata = nil, nil
			plan.Digest, plan.TextDigest = "", ""
			plan.Note, plan.Decision, plan.Reason = "", removedFlaggedFile, "The file is no longer in the scanned folder after a Recycle Bin request"
			plan.Auto, plan.AutoValidated, plan.Unknown = false, false, false
			s.countFileAssessment(plan, 1)
			s.plans[r.Index] = plan
			delete(s.outcomes, r.Index)
			s.reindexPlans()
		}
	case "plan-auto-blocked":
		s.pending = nil
		s.flags = append(s.flags, r)
	case "automatic-failed":
		s.autoHeld[r.Index] = true
		s.flags = append(s.flags, r)
		s.pending = nil
	case "single-failed":
		if s.pending != nil && s.pending.Type == "single-intent" && s.pending.Index == r.Index {
			s.pending = nil
		}
	case "result":
		if r.Move != nil && s.automaticMoves != nil && r.Auto && s.pending != nil && s.pending.Type == "intent" {
			reason := "Matched the automatic naming rules"
			if r.Index < len(s.plans) && s.plans[r.Index].Reason != "" {
				reason = s.plans[r.Index].Reason
			}
			s.automaticMoves[r.Index] = indexedMove{move: *r.Move, position: len(s.moves), reason: reason}
		}
		s.planRevision++
		if r.Index < len(s.planVersions) {
			s.planVersions[r.Index] = s.planRevision
		}
		s.pending = nil
		if r.Auto {
			s.autoCompleted[r.Index] = true
		} else if r.Index >= s.index {
			s.index = r.Index + 1
		}
		if r.Index >= 0 && r.Index < len(s.plans) && s.plans[r.Index].Note != "" {
			s.resolvedPlanFlags++
		}
		switch r.Status {
		case "renamed":
			s.renamed++
		case "moved":
			s.renamed++
			s.moved++
		case "kept":
			s.kept++
		case "skipped":
			s.skipped++
		}
		if r.Note != "" {
			s.flags = append(s.flags, r)
		}
		if r.Move != nil {
			im := indexedMove{move: *r.Move, position: len(s.moves)}
			s.byItem[r.Index] = im
			s.moveIndexByTarget[r.Move.To] = r.Index
			if r.Move.Stamp.Dir {
				s.folderByPath[r.Move.From] = im
			}
			s.moves = append(s.moves, *r.Move)
		}
	case "metadata-result":
		s.pending = nil
		s.metadataIndex++
		if r.Transaction != nil {
			s.repaired = append(s.repaired, r)
		}
		if r.Note != "" {
			s.flags = append(s.flags, r)
		}
	case "metadata-undo-result":
		s.pending = nil
		s.metadataUndone++
		s.restoredStamps[r.Index] = r.Stamp
	case "undo-result":
		s.pending = nil
		s.undoCount++
	case "undo-failed":
		s.pending = nil
		s.undoFailures++
	}
}
func (s *session) namingRules() namingRules { return s.settings().Naming }

func (s *session) indexPlan(r record) {
	if r.Note != "" {
		s.planFlags = append(s.planFlags, r)
	}
	if r.Move != nil {
		s.proposedNames[r.Move.From] = r.Move.To
		s.proposedTargets[strings.ToLower(r.Move.To)] = true
		parent := filepath.Dir(r.Path)
		s.plansByParent[parent] = append(s.plansByParent[parent], r)
		if target := filepath.Dir(r.Move.To); parent != target {
			s.relocationParents[parent] = true
			s.relocationParents[target] = true
		}
	}
}

func (s *session) reindexPlans() {
	s.planFlags = nil
	clear(s.proposedNames)
	clear(s.proposedTargets)
	clear(s.plansByParent)
	clear(s.relocationParents)
	for _, plan := range s.plans {
		s.indexPlan(plan)
	}
}

func (s *session) append(r record) error {
	r.At = time.Now()

	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err = s.log.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = s.log.Sync(); err != nil {
		return err
	}
	s.accept(r)
	return nil
}

func fingerprint(path string) (stamp, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return stamp{}, err
	}
	if linked(info) || !(info.IsDir() || info.Mode().IsRegular()) {
		return stamp{}, errors.New("path is a link or special file")
	}
	id, err := identity(path, info)
	if err != nil {
		return stamp{}, err
	}
	st := stamp{Dir: info.IsDir(), ID: id}
	if !st.Dir {
		st.Size = info.Size()
		st.Modified = info.ModTime().UnixNano()
	}
	return st, nil
}
func safeParent(root, path string) error {
	if !within(root, path) || filepath.Clean(path) == root {
		return errors.New("path is outside the selected folder")
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if linked(info) || !info.IsDir() {
			return fmt.Errorf("parent is a link or unavailable: %s", parent)
		}
		if parent == root {
			return nil
		}
		if parent == filepath.Dir(parent) {
			return errors.New("path is outside the selected folder")
		}
	}
}
func validateRoot(root string) error {
	for p := root; ; p = filepath.Dir(p) {
		f, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if linked(f) || !f.IsDir() {
			return fmt.Errorf("folder is unavailable or a link: %s", p)
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	if root == filepath.Dir(root) {
		return errors.New("Choose a folder inside the drive")
	}
	return nil
}

func (s *session) result(index int, path, status, note string) error {
	return s.append(record{Type: "result", Index: index, Path: path, Status: status, Note: note})
}

func (s *session) renamePlan(plan record, mode renameMode) error {
	m := *plan.Move
	started := time.Now()
	auto := mode != batchRename
	result := record{Type: "result", Index: plan.Index, Path: m.From, Status: "skipped", Note: plan.Note, Auto: auto}
	fail := func() error {
		switch mode {
		case singleRename:
			result.Type, result.Status = "single-failed", "Rename failed"
		case automaticRename:
			result.Type, result.Status = "automatic-failed", "Rename failed"
		}
		result.RenameMS = elapsedMS(started)
		return s.append(result)
	}
	if err := s.validateMoveParents(m); err != nil {
		result.Note = joinNote(result.Note, "Rename failed: "+err.Error())
		return fail()
	}
	st, err := fingerprint(m.From)
	if err != nil || st != m.Stamp {
		result.Note = joinNote(result.Note, "Rename failed: The original changed or became unavailable during the scan")
		return fail()
	}
	if err := nameAvailable(m.From, m.To); err != nil {
		result.Status = "kept"
		result.Note = joinNote(result.Note, "Rename failed: "+err.Error())
		return fail()
	}
	r := record{Type: "intent", Index: plan.Index, Path: m.From, Move: &m, Note: plan.Note, Auto: auto}
	if mode == singleRename {
		r.Type = "single-intent"
	}
	if err := s.append(r); err != nil {
		return err
	}
	return s.finishPending()
}

func (s *session) validateMoveParents(m move) error {
	if m.NewFolder && m.TargetParent == "" {
		return s.validateProposedFolder(m)
	}
	for _, side := range []struct{ path, parentID string }{{m.From, m.SourceParent}, {m.To, m.TargetParent}} {
		if err := safeParent(s.manifest.Root, side.path); err != nil {
			return err
		}
		if side.parentID != "" {
			parent := filepath.Dir(side.path)
			st, err := fingerprint(parent)
			if err != nil || st.ID != side.parentID {
				return fmt.Errorf("folder changed after the move was planned: %s", parent)
			}
		}
	}
	return nil
}

func (s *session) reviewEnd() int {
	if len(s.plans) == len(s.manifest.Items) {
		return len(s.plans)
	}
	return min(len(s.plans), s.fileCount)
}

func (s *session) reviewable(plan record) bool {
	return plan.Metadata != nil && !s.metadataApproved(plan.Index) || plan.Move != nil && !s.autoCompleted[plan.Index]
}

func (s *session) reviewCount() int {
	count := 0
	for i := s.applyThrough; i < s.reviewEnd(); i++ {
		if s.reviewable(s.plans[i]) {
			count++
		}
	}
	return count
}

func (s *session) applyPending() bool {
	return len(s.selection) > 0 || s.index < s.applyThrough || s.metadataIndex < len(s.metadataPlans)
}

func (s *session) automaticRenamePending() bool {
	if n := len(s.plans); s.manifest.Auto && n > 0 {
		plan := s.plans[n-1]
		return s.canAutoApply(plan)
	}
	return false
}

func (s *session) autoApply(plan record) error {
	if s.canAutoApply(plan) {
		return s.renamePlan(plan, automaticRename)
	}
	return nil
}

func (s *session) liveFlags() []record {
	rows := make([]record, 0, 100)
	for i := len(s.planFlags) - 1; i >= 0 && len(rows) < 100; i-- {
		plan := s.planFlags[i]
		if plan.Index >= s.index && !s.autoCompleted[plan.Index] {
			rows = append(rows, plan)
		}
	}
	return rows
}

func (s *session) flagRows() []record {
	resolved := s.index
	flags := append([]record(nil), s.flags...)
	for _, r := range s.planFlags {
		if r.Index >= resolved && !s.autoCompleted[r.Index] {
			flags = append(flags, r)
		}
	}
	return flags
}

func (s *session) availableForPlan(from, to string) error {
	if s.proposedTargets[strings.ToLower(to)] {
		return fmt.Errorf("Proposed name conflict: %s", filepath.Base(to))
	}
	return nameAvailable(from, to)
}

func (s *session) applyNext() error {
	if !s.manifest.Preview || s.index >= s.applyThrough {
		return errors.New("There are no approved proposals left to apply")
	}
	plan := s.plans[s.index]
	if s.autoCompleted[plan.Index] || plan.Decision == removedFlaggedFile {
		return s.append(record{Type: "advance", Index: plan.Index})
	}
	if plan.Move != nil && !s.excluded[plan.Index] {
		if plan.Move.Stamp.Dir && s.folderHasUnappliedChild(plan.Path) {
			return s.result(plan.Index, plan.Path, "kept", joinNote(plan.Note, "Folder name needs a fresh scan after a child rename was skipped"))
		}
		return s.renamePlan(plan, batchRename)
	}
	status := "kept"
	if s.manifest.Items[plan.Index].Skip {
		status = "skipped"
	}
	return s.result(plan.Index, plan.Path, status, plan.Note)
}

func (s *session) folderHasUnappliedChild(path string) bool {
	for _, child := range s.plansByParent[path] {
		if _, applied := s.byItem[child.Index]; !applied {
			return true
		}
	}
	return false
}

func nameAvailable(from, to string) error {
	if info, err := os.Lstat(to); err == nil {
		old, _ := os.Lstat(from)
		if !strings.EqualFold(from, to) || old == nil || !os.SameFile(info, old) {
			return fmt.Errorf("Name conflict: %s", filepath.Base(to))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func actualSpelling(path string) bool {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(path) {
			return true
		}
	}
	return false
}

func (s *session) finishPending() error {
	if s.pending == nil {
		return nil
	}
	r := *s.pending
	if r.Type == "recycle-intent" {
		return s.finishRecycle()
	}
	if r.Transaction != nil {
		return s.finishMetadata(r)
	}
	m := *r.Move
	renameStart := time.Now()
	undo := r.Type == "undo-intent"
	if err := s.validateMoveParents(m); err != nil {
		return err
	}
	current, sourceErr := fingerprint(m.From)
	target, targetErr := fingerprint(m.To)
	sameCase := strings.EqualFold(m.From, m.To)
	moved := targetErr == nil && target == m.Stamp &&
		(errors.Is(sourceErr, os.ErrNotExist) || (sameCase && actualSpelling(m.To) && !actualSpelling(m.From)))
	var problem error
	switch {
	case moved:
	case sourceErr != nil || current != m.Stamp:
		// Neither location uniquely identifies the original. Recover only after inspecting the folder.
		return fmt.Errorf("Could not verify the original or renamed item: %s", m.From)
	default:
		if r.Type == "intent" && r.Auto && (r.Index >= len(s.plans) || !s.plans[r.Index].AutoValidated) {
			return s.append(record{Type: "plan-auto-blocked", Index: r.Index, Path: m.From, Note: "Recovered an old automatic proposal without current safety validation. Review it before applying."})
		}
		if m.NewFolder && m.TargetParent == "" {
			problem = s.prepareMoveFolder(&m)
		}
		if problem == nil {
			problem = nameAvailable(m.From, m.To)
		}
		if problem == nil {
			problem = renameNoReplace(m.From, m.To)
		}
	}
	if problem != nil {
		if undo {
			if err := s.append(record{Type: "undo-failed", Note: problem.Error()}); err != nil {
				return err
			}
			return problem
		}
		failure := record{Type: "result", Index: r.Index, Path: m.From, Status: "kept",
			Note: joinNote(r.Note, "Rename failed: "+problem.Error()), Auto: r.Auto, RenameMS: elapsedMS(renameStart)}
		if r.Type == "single-intent" {
			failure.Type, failure.Status = "single-failed", "Rename failed"
		} else if r.Auto {
			failure.Type, failure.Status = "automatic-failed", "Rename failed"
		}
		return s.append(failure)
	}
	if undo {
		return s.append(record{Type: "undo-result"})
	}
	if m.NewFolder && m.TargetParent == "" {
		targetParent, err := fingerprint(filepath.Dir(m.To))
		if err != nil || !targetParent.Dir {
			return errors.New("The move destination is unavailable")
		}
		m.TargetParent = targetParent.ID
	}
	status := "renamed"
	if filepath.Dir(m.From) != filepath.Dir(m.To) {
		status = "moved"
	}
	return s.append(record{Type: "result", Index: r.Index, Path: m.To, Note: r.Note, Status: status, Move: &m, Auto: r.Auto, RenameMS: elapsedMS(renameStart)})
}

func (s *session) undoNext() error {
	if s.pending != nil {
		return s.finishPending()
	}
	if s.metadataUndone < len(s.repaired) {
		return s.undoMetadata()
	}
	if s.undoCount >= len(s.moves) {
		return nil
	}
	m := s.moves[len(s.moves)-s.undoCount-1]
	if index, found := s.moveIndexByTarget[m.To]; found {
		if restored, ok := s.restoredStamps[index]; ok {
			m.Stamp = restored
		}
	}
	if err := s.validateMoveParents(m); err != nil {
		return err
	}
	st, err := fingerprint(m.To)
	if err != nil || st != m.Stamp {
		return fmt.Errorf("Cannot undo a changed or missing item: %s", m.To)
	}
	if err := nameAvailable(m.To, m.From); err != nil {
		return err
	}
	m.From, m.To = m.To, m.From
	m.SourceParent, m.TargetParent = m.TargetParent, m.SourceParent
	if err := s.append(record{Type: "undo-intent", Move: &m}); err != nil {
		return err
	}
	return s.finishPending()
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

func (s *session) canRefine(index int) bool {
	return index >= s.applyThrough && index >= 0 && index < len(s.plans) && s.plans[index].Rough &&
		s.autoCompleted[index] && s.byItem[index].move.To != "" && s.undoCount == 0
}
