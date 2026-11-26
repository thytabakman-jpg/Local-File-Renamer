package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

type scanStartOptions struct {
	ScopeMode  string
	Subfolders *bool
	Fresh      bool
	RequestID  string
	JobID      string
	FallbackAI bool
}

func (start scanStartOptions) configure(opts *scanSettings, include []string) error {
	switch start.ScopeMode {
	case "":
		if len(include) > 0 {
			opts.Scope.IncludeExtensions = include
		}
	case "all", "selected":
		opts.Scope = scanScope{Subfolders: opts.Scope.Subfolders, VerifyComparison: opts.Scope.VerifyComparison}
		if start.ScopeMode == "selected" {
			if len(include) == 0 {
				return errors.New("Select a file type or choose All file types")
			}
			opts.Scope.IncludeExtensions = include
		}
	case "profile":
	default:
		return errors.New("Choose All file types, Selected file types, or Use profile scope")
	}
	if start.Subfolders != nil {
		opts.Scope.Subfolders = *start.Subfolders
	}
	if start.Fresh {
		opts.Cache = cacheSettings{}
	}
	return nil
}

type inventoryCounts struct {
	Found, Included, Filtered, Folders, Unavailable, OmittedFolders int
	FileTypes                                                       []string `json:",omitempty"`
}

func (c inventoryCounts) summary() string {
	line := fmt.Sprintf("%d files found, %d included, %d filtered out, %d subfolders", c.Found, c.Included, c.Filtered, c.Folders)
	if c.Unavailable != 0 {
		line += fmt.Sprintf(", %d unavailable entries", c.Unavailable)
	}
	if c.OmittedFolders != 0 {
		line += fmt.Sprintf(", %d subfolders outside scope", c.OmittedFolders)
	}
	return line
}

func (scope scanScope) description() string {
	parts := []string{"All file types"}
	if len(scope.IncludeExtensions) > 0 {
		parts[0] = "Included extensions: " + strings.Join(scope.IncludeExtensions, ", ")
	}
	if len(scope.ExcludeExtensions) > 0 {
		parts = append(parts, "Excluded extensions: "+strings.Join(scope.ExcludeExtensions, ", "))
	}
	if scope.Subfolders {
		parts = append(parts, "Subfolders included")
	} else {
		parts = append(parts, "Subfolders excluded")
	}
	if len(scope.IncludePaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d included paths", len(scope.IncludePaths)))
	}
	if len(scope.ExcludePaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d excluded paths", len(scope.ExcludePaths)))
	}
	if len(scope.SelectedPaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d individually selected paths", len(scope.SelectedPaths)))
	}
	if scope.MinBytes > 0 {
		parts = append(parts, fmt.Sprintf("Minimum size: %d bytes", scope.MinBytes))
	}
	if scope.MaxBytes > 0 {
		parts = append(parts, fmt.Sprintf("Maximum size: %d bytes", scope.MaxBytes))
	}
	return strings.Join(parts, " · ")
}

func (s *session) availableTypes() []string {
	if s.manifest.Inventory != nil {
		return s.manifest.Inventory.FileTypes
	}
	return nil
}

func (a *application) start(root string, partial, folders, auto, repair, sortFiles bool) error {
	return a.startFiltered(root, partial, folders, auto, repair, sortFiles, nil)
}
func (a *application) startFiltered(root string, partial, folders, auto, repair, sortFiles bool, include []string) error {
	return a.startFromUI(root, partial, folders, auto, repair, sortFiles, include, nil, false)
}

func (a *application) startFromUI(root string, partial, folders, auto, repair, sortFiles bool, include []string, media *bool, previewOnly bool) error {
	return a.startUIOperation(root, partial, folders, auto, repair, sortFiles, include, media, previewOnly, "")
}

func (a *application) startUIOperation(root string, partial, folders, auto, repair, sortFiles bool, include []string, media *bool, previewOnly bool, operation scanOperation) error {
	return a.startConfiguredScan(root, partial, folders, auto, repair, sortFiles, include, media, previewOnly, operation, scanStartOptions{})
}

func (a *application) startConfiguredScan(root string, partial, folders, auto, repair, sortFiles bool, include []string, media *bool, previewOnly bool, operation scanOperation, setup scanStartOptions) error {
	return a.launchScan(root, partial, "", operation, setup, func(opts *scanSettings) error {
		opts.Execution.FolderRenames, opts.Execution.Metadata, opts.Execution.Moves = folders, repair, sortFiles
		if err := setup.configure(opts, include); err != nil {
			return err
		}
		if media != nil {
			opts.Evidence.MediaSamples = *media
		}
		if previewOnly {
			opts.Execution.Mode = "preview_only"
		}
		if opts.Execution.Mode != "preview_only" {
			opts.Execution.Mode = "review"
			if auto {
				opts.Execution.Mode = "automatic"
			}
		}
		return validateSettings(*opts)
	})
}

func (a *application) startWith(root string, partial bool, profile string, prepare func(*scanSettings) error) error {
	return a.startWithOperation(root, partial, profile, "", prepare)
}

func (a *application) startWithOperation(root string, partial bool, profile string, operation scanOperation, prepare func(*scanSettings) error) error {
	return a.launchScan(root, partial, profile, operation, scanStartOptions{}, prepare)
}

func (a *application) launchScan(root string, partial bool, profile string, operation scanOperation, setup scanStartOptions, prepare func(*scanSettings) error) error {
	if !operation.valid() {
		return errors.New("Choose File Renamer, File Mover, or Duplicate Check")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return errors.New("Choose a folder for the new scan")
	}
	p, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if within(p, a.home) || within(a.home, p) || within(p, a.backend) || within(p, a.modelDir) || within(a.modelDir, p) {
		return errors.New("Choose a folder that is separate from the app and its data")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy || a.v.Closing {
		return errors.New("The app is busy or closing")
	}
	if profile == "" {
		profile = a.profiles.Active
	}
	opts, ok := a.profiles.Profiles[profile]
	if !ok {
		return errors.New("No profile with that name")
	}
	if err := prepare(&opts); err != nil {
		return err
	}
	switch operation {
	case scanRename:
		opts.Execution.Moves = false
	case scanMove, scanDuplicates:
		opts.Execution.FolderRenames, opts.Execution.Metadata = false, false
		opts.Execution.Moves = operation == scanMove
		opts.Evidence.OnlineDates = "off"
		if opts.Execution.Mode == "automatic" {
			opts.Execution.Mode = "review"
		}
		if operation == scanDuplicates {
			opts.Scope.VerifyComparison = false
		}
	}
	return a.startLocked(p, partial, opts, operation, setup)
}

func (a *application) startLocked(p string, partial bool, opts scanSettings, operation scanOperation, setup scanStartOptions) error {
	partial = operation == scanDuplicates || partial || opts.AI.Model == "off" || opts.AI.Invoke == "never"
	if !partial && !modelReadyFor(a.modelDir, opts.AI.Model) {
		if !setup.FallbackAI {
			return errors.New("Download the local model or select Run without local AI")
		}
		partial = true
	}
	started := time.Now()
	a.v.StartID, a.v.StartJob, a.v.StartError, a.v.StartingRoot = setup.RequestID, setup.JobID, "", p
	a.v.StartingScope, a.v.StartingMode = opts.Scope.description(), opts.Execution.Mode
	ctx := a.begin("indexing")
	failStart := func(err error) {
		message := err.Error()
		if errors.Is(err, context.Canceled) {
			message = "Scan cancelled before indexing finished"
		}
		a.mu.Lock()
		a.v.StartingRoot, a.v.StartError = "", message
		a.mu.Unlock()
		a.finish(err)
	}
	go func() {
		if err := validateRoot(p); err != nil {
			failStart(err)
			return
		}
		s, err := createNamedSession(ctx, filepath.Join(a.home, "runs"), p, manifest{
			Preview: true, RenameFolders: opts.Execution.FolderRenames, Auto: opts.Execution.Mode == "automatic", RepairMetadata: opts.Execution.Metadata, SortFolders: opts.Execution.Moves, Partial: partial, Settings: &opts, Operation: operation,
		}, setup.JobID)
		if err != nil {
			failStart(err)
			return
		}
		a.mu.Lock()
		a.s.close()
		a.s = s
		a.v.StartJob, a.v.StartingRoot = filepath.Base(s.dir), ""
		a.v.Phase = "starting"
		s.cacheDir = filepath.Join(a.home, "assessment-cache")
		s.online = a.online
		a.cleanup, a.cleanupThrough = nil, 0
		a.scanJobs()
		err = a.startClock(s, started)
		a.syncSession(true)
		a.mu.Unlock()
		if err != nil {
			a.finish(err)
			return
		}
		a.run(ctx, s, partial)
	}()
	return nil
}
func (a *application) resume(id string, partial bool) error {
	return a.resumeWithFallback(id, partial, false)
}
func (a *application) resumeWithFallback(id string, partial, fallback bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resumeLocked(id, partial, fallback)
}
func (a *application) resumeLocked(id string, partial, fallback bool) error {
	if a.v.Busy || a.v.Closing || a.s == nil || a.v.Job != id || a.v.Phase != "paused" || (a.s.undoCount > 0 || a.s.metadataUndone > 0) {
		return errors.New("There is no scan to resume")
	}
	s := a.s
	opts := s.settings()
	partial = s.manifest.Operation == scanDuplicates || partial || opts.AI.Model == "off" || opts.AI.Invoke == "never"
	if !partial && !modelReadyFor(a.modelDir, opts.AI.Model) {
		if !fallback {
			return errors.New("Download the local model or select Run without local AI")
		}
		partial = true
	}
	if !s.manifest.Preview || s.applyPending() {
		return errors.New("Start a new scan to preview changes")
	}
	if s.partial != partial {
		if err := s.append(record{Type: "scan-mode", Partial: partial}); err != nil {
			return err
		}
		a.v.Partial = partial
	}
	if err := a.startClock(s, time.Now()); err != nil {
		return err
	}
	ctx := a.begin("starting")
	go a.run(ctx, s, partial)
	return nil
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
