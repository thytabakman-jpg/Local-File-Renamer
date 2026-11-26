package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Section: Application state and presentation contracts.

type flagRow struct{ Path, Status, Reason string }
type job struct {
	ID, Root, Date string
	Operation      scanOperation
}
type proposalRow struct {
	Index                  int
	Revision               uint64 `json:",omitempty"`
	From, To, Note         string
	Kind, Reason, Decision string
	Draft                  bool
	Metadata               *metadataEdit
}
type inspectionRow struct {
	Path, Proposed, Note                        string
	AutomaticFrom, AutomaticTo, AutomaticReason string
	AutomaticUndone                             bool
	Index                                       int
	Revision                                    uint64 `json:",omitempty"`
	Kind, Reason, Decision                      string
	Inspection                                  inspection
	Unknown                                     bool
	ReviewActions, Refinement                   bool          `json:",omitempty"`
	Metadata                                    *metadataEdit `json:",omitempty"`
	RenameMS                                    int64
	Outcome                                     string
}
type inspectionPage struct {
	Job   string
	Rows  []inspectionRow
	Total int
}

type view struct {
	Inventory                                                               *inventoryCounts
	ScopeDescription                                                        string
	FileTotal, FilesAssessed, FilesSkipped, FilesVerified, FilesNotAssessed int
	StartID, StartJob, StartError, StartingRoot                             string
	StartingScope, StartingMode, ExecutionMode, OutcomeReason               string

	Operation                                                         scanOperation
	ModelName                                                         string
	Clock                                                             scanClock
	BridgeEnabled                                                     bool
	Root, Current, Phase, Message, Job, BackendError                  string
	Total, Done, Renamed, Kept, Skipped, Flagged, UndoLeft, Proposed  int
	ModelReady, Busy, Pausing, Pending, Closing, Partial, PreviewOnly bool
	Downloaded, DownloadSize                                          int64
	MetadataDone, ReviewStart, ReviewEnd, Drafts, ApprovedWaiting     int
	AutoRenamed, ManualChanges, Moved                                 int
	Revision                                                          uint64
	Flags                                                             []flagRow
	Jobs                                                              []job
}

type application struct {
	testMu                  sync.Mutex
	testActive              testReceipt
	testCancel              context.CancelFunc
	testDone                chan struct{}
	testModelPeak           uint64
	modelOwner              *chatSession
	chat                    chatSession
	online                  *onlineLookup
	scanActive              bool
	profiles                profiles
	cleanup                 []proposalRow
	cleanupThrough          int
	bridgeKey               string
	mu                      sync.Mutex
	home, backend, modelDir string
	s                       *session
	v                       view
	cancel, pickerCancel    context.CancelFunc
	jobDone, pickerDone     chan struct{}
	pause                   bool
}

// Section: Application initialization.

func dataHome() (string, error) {
	if home := os.Getenv("LOCAL_RENAMER_HOME"); home != "" {
		return filepath.Abs(home)
	}
	home, err := os.UserConfigDir()
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		home = local
		err = nil
	}
	return filepath.Join(home, "Local File Renamer"), err
}
func newApp(home, backend, modelDir string) (*application, error) {
	p, err := readProfiles(home)
	a := &application{home: home, backend: backend, modelDir: modelDir, profiles: p, online: newOnlineLookup()}
	a.chat.init()
	if err != nil {
		a.v.Message = "Saved profiles could not be read. Using default settings."
	}
	a.v.Phase = "ready"
	name, _, files := modelProfile()
	a.v.ModelName = name
	for _, f := range files {
		a.v.DownloadSize += f.size
	}
	if err := os.MkdirAll(filepath.Join(home, "runs"), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(home, "assessment-cache"), 0700); err != nil {
		return nil, err
	}
	pruneCache(filepath.Join(home, "assessment-cache"))
	a.scanJobs()
	if len(a.v.Jobs) > 0 {
		s, err := openSession(filepath.Join(home, "runs", a.v.Jobs[0].ID))
		if err != nil {
			a.v.Message = err.Error()
		} else {
			a.loadSession(s)
		}
	}
	if err := a.setBridge(true); err != nil {
		a.v.Message = joinNote(a.v.Message, "The local ChatGPT connection could not start: "+err.Error())
	}
	return a, nil
}

// Section: Saved jobs and session projections.

func (a *application) scanJobs() {
	var jobs []job
	dirs, _ := os.ReadDir(filepath.Join(a.home, "runs"))
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if !d.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(a.home, "runs", d.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var m struct {
			Root      string
			Created   time.Time
			Operation scanOperation
		}
		if json.Unmarshal(b, &m) == nil {
			jobs = append(jobs, job{d.Name(), m.Root, m.Created.Local().Format("2006-01-02 15:04"), m.Operation})
		}
	}
	a.v.Jobs = jobs
}

func (a *application) syncSession(full bool) {
	s := a.s
	if s == nil {
		return
	}
	a.v.Root = s.manifest.Root
	a.v.Operation = s.manifest.Operation
	a.v.Clock = s.clock
	if !a.scanActive && a.v.Clock.Stopped.IsZero() && !a.v.Clock.Started.IsZero() {
		a.v.Clock = stoppedClock(a.v.Clock, s.clockLast, true)
	}
	a.v.Partial = s.partial
	a.v.ExecutionMode = s.settings().Execution.Mode
	a.v.PreviewOnly = a.v.ExecutionMode == "preview_only"
	jobID := filepath.Base(s.dir)
	if a.v.Job != jobID {
		a.v.ScopeDescription = s.settings().Scope.description()
	}
	a.v.Job = jobID
	a.v.Total = len(s.manifest.Items)
	a.v.FileTotal = s.fileCount
	a.v.FilesAssessed, a.v.FilesSkipped, a.v.FilesVerified = s.filesAssessed, s.filesSkipped, s.filesVerified
	a.v.FilesNotAssessed = s.filesNotAssessed
	a.v.Inventory = s.manifest.Inventory
	a.v.Done = s.index
	if s.manifest.Preview {
		a.v.Done = len(s.plans)
	}
	a.v.Renamed = s.renamed - s.moved
	a.v.Moved = s.moved
	a.v.AutoRenamed = len(s.automaticMoves)
	a.v.ManualChanges = s.renamed - a.v.AutoRenamed
	a.v.Kept = s.filesKept
	if s.applyThrough > 0 || !s.manifest.Preview {
		a.v.Kept = s.kept
	}
	a.v.Skipped = s.filesSkipped
	if s.applyThrough > 0 || !s.manifest.Preview {
		a.v.Skipped = s.skipped
	}
	a.v.Flagged = len(s.flags) + len(s.planFlags) - s.resolvedPlanFlags
	a.v.MetadataDone = len(s.repaired) - s.metadataUndone
	a.v.UndoLeft = len(s.moves) - s.undoCount + a.v.MetadataDone
	a.v.Pending = s.pending != nil
	a.v.ApprovedWaiting = len(s.selection)
	a.v.Revision = s.planRevision
	if full {
		a.v.ReviewStart, a.v.ReviewEnd = s.applyThrough, s.reviewEnd()
		a.v.Proposed = s.reviewCount()
		a.v.OutcomeReason = s.zeroChangeReason(a.v.Busy, a.v.Proposed)
		flags := s.flagRows()
		a.v.Flags = nil
		for _, r := range flags[max(0, len(flags)-100):] {
			path := s.currentPath(r.Path, r.Index)
			status := r.Status
			if r.Type == "plan" {
				status = planOutcome(r)
			}
			a.v.Flags = append(a.v.Flags, flagRow{path, status, r.Note})
		}
	}
	if !a.v.Busy {
		switch {
		case (s.undoCount > 0 || s.metadataUndone > 0) && a.v.UndoLeft == 0:
			a.v.Phase = "undone"
		case s.undoCount > 0 || s.metadataUndone > 0 || s.pending != nil && (s.pending.Type == "undo-intent" || s.pending.Type == "metadata-undo-intent"):
			a.v.Phase = "undo-paused"
		case s.manifest.Preview && s.applyPending():
			a.v.Phase = "apply-paused"
		case s.manifest.Preview && len(s.plans) == len(s.manifest.Items) && !s.automaticRenamePending():
			a.v.Phase = "complete"
			if a.v.Proposed > 0 {
				a.v.Phase = "preview"
			}
		case !s.manifest.Preview:
			a.v.Phase = "complete"
			if s.index < len(s.manifest.Items) {
				a.v.Phase = "legacy-paused"
			}
		default:
			a.v.Phase = "paused"
		}
	}
}
func (a *application) snapshot() view {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.v
	if a.scanActive && a.s != nil {
		v.Proposed = a.s.reviewCount()
		v.Drafts = max(0, len(a.cleanup)-a.cleanupThrough)
		flags := a.s.liveFlags()
		v.Flags = make([]flagRow, 0, len(flags))
		for _, r := range flags {
			v.Flags = append(v.Flags, flagRow{Path: r.Path, Status: planOutcome(r), Reason: r.Note})
		}
	}
	v.ModelName, _, _ = modelProfileFor(a.activeSettings().AI.Model)
	v.ModelReady = modelReadyFor(a.modelDir, a.activeSettings().AI.Model)
	_, _, assets := modelProfileFor(a.activeSettings().AI.Model)
	v.DownloadSize = 0
	for _, asset := range assets {
		v.DownloadSize += asset.size
	}
	v.Clock = v.Clock.at(time.Now())
	return v
}
func (a *application) selectJob(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy || a.v.Closing {
		return errors.New("Pause the active run first")
	}
	for _, j := range a.v.Jobs {
		if j.ID != id {
			continue
		}
		s, err := openSession(filepath.Join(a.home, "runs", id))
		if err != nil {
			return err
		}
		a.loadSession(s)
		a.v.StartError = ""
		return nil
	}
	return errors.New("Run could not be found")
}
func (a *application) loadSession(s *session) {
	a.s.close()
	a.s = s
	s.cacheDir = filepath.Join(a.home, "assessment-cache")
	s.online = a.online
	a.cleanup, a.cleanupThrough = nil, 0
	a.v.Message = ""
	if s.pending != nil && (s.pending.Type == "single-intent" || s.pending.Type == "recycle-intent") {
		err := validateRoot(s.manifest.Root)
		if err == nil {
			err = s.finishPending()
		}
		if err != nil {
			a.v.Message = err.Error()
		}
	}
	a.syncSession(true)
}

// Section: Scan admission and launch.

func within(parent, path string) bool {
	r, err := filepath.Rel(parent, path)
	return err == nil && (r == "." || (r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator))))
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
// Section: Scan resume and pause.

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
func (a *application) pauseRun() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pauseLocked()
}

func (a *application) pauseLocked() {
	if a.v.Busy {
		a.pause = true
		a.v.Pausing = true
		if a.cancel != nil {
			a.cancel()
		}
	}
}

// Section: Execution lifecycle and scan worker.

func (a *application) begin(phase string) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.jobDone = make(chan struct{})
	a.v.Phase = phase
	a.v.Busy = true
	a.v.Pausing = false
	a.v.Message = ""
	a.pause = false
	return ctx
}
func (a *application) startClock(s *session, now time.Time) error {
	c := s.clock
	if c.Stopped.IsZero() && !c.Started.IsZero() {
		c = stoppedClock(c, s.clockLast, true)
	}
	if !c.Stopped.IsZero() {
		c.PausedMS += durationMS(now.Sub(c.Stopped))
	}
	c.Started, c.Stopped, c.Paused = now, time.Time{}, false
	if err := s.append(record{Type: "scan-start", Clock: c}); err != nil {
		return err
	}
	a.v.Clock, a.scanActive = c, true
	return nil
}

func (a *application) finish(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.scanActive {
		now := time.Now()
		c := stoppedClock(a.s.clock, now, len(a.s.plans) < len(a.s.manifest.Items) || a.s.automaticRenamePending())
		var note string
		if err != nil && !errors.Is(err, context.Canceled) {
			note = err.Error()
		}
		if saveErr := a.s.append(record{Type: "scan-stop", Clock: c, Note: note}); saveErr != nil {
			err = errors.Join(err, saveErr)
		}
		a.scanActive = false
	} else if a.s != nil && err != nil && !errors.Is(err, context.Canceled) && (a.v.Phase == "applying" || a.v.Phase == "metadata" || a.v.Phase == "undo") {
		err = errors.Join(err, a.s.append(record{Type: "operation-error", Note: err.Error()}))
	}
	a.v.Busy = false
	a.cleanup, a.cleanupThrough = nil, 0
	a.v.Pausing = false
	a.v.Current = ""
	if a.s != nil {
		a.syncSession(true)
	} else {
		a.v.Phase = "ready"
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		a.v.Message = err.Error()
	}
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	if a.jobDone != nil {
		close(a.jobDone)
		a.jobDone = nil
	}
}
func (a *application) run(ctx context.Context, s *session, partial bool) {
	if s.manifest.Operation == scanDuplicates {
		a.runDuplicates(ctx, s)
		return
	}
	if ctx.Err() != nil {
		a.finish(nil)
		return
	}
	if err := validateRoot(s.manifest.Root); err != nil {
		a.finish(err)
		return
	}
	a.mu.Lock()
	var failure error
	if s.pending != nil {
		failure = s.finishPending()
	}
	if failure == nil && len(s.plans) > 0 {
		failure = s.autoApply(s.plans[len(s.plans)-1])
	}
	startIndex := len(s.plans)
	a.mu.Unlock()
	if failure != nil {
		a.finish(failure)
		return
	}
	rules := s.namingRules()
	var cleanups []proposalRow
	for i := startIndex; s.manifest.Operation != scanMove && i < s.fileCount && ctx.Err() == nil; i++ {
		item := s.manifest.Items[i]
		name := filepath.Base(item.Path)
		if s.manifest.Auto && strings.EqualFold(extension(name), ".pdf") {
			continue
		}
		cleaned := rules.cleanName(name, false)
		if !item.Skip && cleaned != name && windowsNameLength(cleaned) <= rules.MaxLength && validProposedName(cleaned, name, false) {
			cleanups = append(cleanups, proposalRow{Index: i, From: item.Path,
				To: filepath.Join(filepath.Dir(item.Path), cleaned), Draft: true,
				Decision: "Cleanup proposed", Reason: "Formatting checked. File inspection and conflict checks are pending."})
		}
	}
	a.mu.Lock()
	a.cleanup, a.cleanupThrough = cleanups, 0
	a.mu.Unlock()
	opts := s.settings()
	engine := scanModel{app: a, session: s, settings: opts}
	assess, transcribe := engine.handlers(partial)
	var prefetched map[int]preparedEvidence
	failures := 0
	for index := startIndex; index < len(s.manifest.Items) && ctx.Err() == nil; index++ {
		item := s.manifest.Items[index]
		a.mu.Lock()
		if item.Folder {
			a.v.Phase = "folders"
		} else {
			a.v.Phase = "files"
		}
		a.v.Current = item.Path
		a.mu.Unlock()

		if opts.Resources.Workers > 1 && (index-startIndex)%opts.Resources.Workers == 0 {
			prefetched = prefetchEvidence(ctx, s, index, opts, assess != nil)
		}
		var pre *preparedEvidence
		if p, ok := prefetched[index]; ok {
			pre = &p
		}
		d, st, err := inspectPrepared(ctx, s, assess, transcribe, index, "", false, opts, pre)
		engine.timings(&d.Inspection, partial, index)
		a.mu.Lock()
		if err == nil {
			failure = recordSuggestion(s, index, d, st)
			if failure == nil {
				failure = s.autoApply(s.plans[index])
			}
			if a.cleanupThrough < len(a.cleanup) && a.cleanup[a.cleanupThrough].Index == index {
				a.cleanupThrough++
			}
			a.syncSession(false)
		} else {
			failure = s.append(record{Type: "scan-interrupted", Index: index, Path: item.Path,
				Status: "Interrupted", Note: "Inspection paused before completion", Inspection: d.Inspection})
			if failure == nil && !errors.Is(err, context.Canceled) {
				failure = err
			}
		}
		a.mu.Unlock()
		if err != nil || failure != nil {
			break
		}
		if d.Outcome == "Analysis failed" {
			failures++
			if opts.Recovery.PauseAfter > 0 && failures >= opts.Recovery.PauseAfter && index+1 < len(s.manifest.Items) {
				failure = errors.New("Scan paused after consecutive local AI failures. Completed work was saved.")
				break
			}
		} else {
			failures = 0
		}
	}
	engine.end()
	a.finish(failure)
}

// Section: Proposal retrieval.

func (a *application) previewPage(offset int) ([]proposalRow, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.previewPageLocked(offset)
}

type proposalPage struct {
	Job               string
	Revision          uint64
	Total, NextOffset int
	Rows              []proposalRow
}

func (a *application) proposalPage(id string, offset int) (proposalPage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id != "" && id != a.v.Job {
		return proposalPage{}, errors.New("The selected run changed")
	}
	rows, err := a.previewPageLocked(offset)
	if err != nil {
		return proposalPage{}, err
	}
	total := a.s.reviewCount() + len(a.cleanup) - a.cleanupThrough
	next := offset + len(rows)
	if next >= total || len(rows) == 0 {
		next = 0
	}
	return proposalPage{Job: a.v.Job, Revision: a.s.planRevision, Total: total, NextOffset: next, Rows: rows}, nil
}

func (a *application) previewPageLocked(offset int) ([]proposalRow, error) {
	if a.s == nil || a.v.Busy && !a.scanActive || !a.scanActive && (a.v.Phase != "preview" && a.v.Phase != "paused" && a.v.Phase != "complete") || a.s.pending != nil || a.s.automaticRenamePending() {
		return nil, errors.New("No preview is available")
	}
	rows := make([]proposalRow, 0, 100)
	for _, plan := range a.s.plans[a.s.applyThrough:a.s.reviewEnd()] {
		hasRename := plan.Move != nil && !a.s.autoCompleted[plan.Index]
		if !a.s.reviewable(plan) {
			continue
		}
		if offset > 0 {
			offset--
			continue
		}
		row := proposalRow{Index: plan.Index, Revision: a.s.planVersions[plan.Index], From: a.s.currentPath(plan.Path, plan.Index), Note: plan.Note, Metadata: plan.Metadata, Kind: plan.Kind, Reason: plan.Reason, Decision: planOutcome(plan)}
		if hasRename {
			row.To = plan.Move.To
			if a.s.manifest.Auto && photoExtension(plan.Path) && !a.s.canAutoApply(plan) {
				if plan.Auto && !a.s.autoHeld[plan.Index] {
					row.Reason = joinNote(row.Reason, "Enable this category for automatic changes or review it manually")
				} else {
					row.Reason = joinNote(row.Reason, "Review required before this image name can be applied")
				}
			}
			if failure := a.s.outcomes[plan.Index]; failure.Type == "single-failed" || failure.Type == "automatic-failed" {
				row.Note = failure.Note
			}
		}
		rows = append(rows, row)
		if len(rows) == 100 {
			break
		}
	}
	if a.scanActive && len(rows) < 100 {
		start := min(len(a.cleanup), a.cleanupThrough+offset)
		rows = append(rows, a.cleanup[start:min(len(a.cleanup), start+100-len(rows))]...)
	}
	return rows, nil
}

// Section: Apply and undo operations.

func (a *application) apply(id string, start, end int, excluded []int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.applyLocked(id, start, end, excluded)
}

func (a *application) applyLocked(id string, start, end int, excluded []int) error {
	s := a.s
	if a.v.Busy || a.v.Closing || s == nil || id != a.v.Job || !s.manifest.Preview || s.settings().Execution.Mode == "preview_only" {
		return errors.New("No matching preview is ready to apply")
	}
	if a.v.Phase != "apply-paused" && len(s.selection) == 0 {
		if (a.v.Phase != "preview" && a.v.Phase != "paused") || s.pending != nil || s.automaticRenamePending() ||
			start != s.applyThrough || end != s.reviewEnd() || a.v.Proposed == 0 {
			return errors.New("The preview changed. Review it again before applying")
		}
		seen := make(map[int]bool)
		for _, i := range excluded {
			if i < start || i >= end || seen[i] || !s.reviewable(s.plans[i]) {
				return errors.New("The selection does not match this preview")
			}
			seen[i] = true
		}
		if len(excluded) == a.v.Proposed {
			return errors.New("Select at least one proposed change")
		}
		if err := s.append(record{Type: "apply-batch", Index: end, Excluded: excluded}); err != nil {
			return err
		}
	}
	a.begin("applying")
	go func() {
		err := validateRoot(s.manifest.Root)
		a.mu.Lock()
		if err == nil && s.pending != nil {
			err = s.finishPending()
		}
		var writeFailures []error
		for err == nil && len(s.selection) > 0 && !a.pause {
			a.v.Current = s.plans[s.selection[0]].Path
			var failure error
			failure, err = s.applyNextSelection()
			if failure != nil {
				writeFailures = append(writeFailures, failure)
			}
			a.syncSession(false)
			a.mu.Unlock()
			a.mu.Lock()
		}
		for err == nil && s.index < s.applyThrough && !a.pause {
			a.v.Current = s.manifest.Items[s.index].Path
			err = s.applyNext()
			a.syncSession(false)
			a.mu.Unlock()
			a.mu.Lock()
		}
		for err == nil && s.index == s.applyThrough && s.metadataIndex < len(s.metadataPlans) && !a.pause {
			plan := s.metadataPlans[s.metadataIndex]
			a.v.Current = s.currentPath(plan.Path, plan.Index)
			a.v.Phase = "metadata"
			err = s.metadataNext()
			a.syncSession(false)
			a.mu.Unlock()
			a.mu.Lock()
		}
		a.mu.Unlock()
		a.finish(errors.Join(err, errors.Join(writeFailures...)))
	}()
	return nil
}
func (a *application) applyOne(id string, index int) error { return a.applyOneRevision(id, index, nil) }
func (a *application) applyOneRevision(id string, index int, revision *uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if revision == nil {
		revision = &a.v.Revision
	}
	return a.applySelectionLocked(id, *revision, []int{index}, nil)
}

func (a *application) undo() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.undoLocked()
}

func (a *application) undoLocked() error {
	if a.v.Busy || a.v.Closing || a.s == nil || a.v.UndoLeft == 0 && !a.v.Pending {
		return errors.New("There are no changes to undo")
	}
	s := a.s
	if len(s.selection) > 0 {
		return errors.New("Resume applying the approved selection before undoing changes")
	}
	a.begin("undo")
	go func() {
		err := validateRoot(s.manifest.Root)
		a.mu.Lock()
		if err == nil && s.pending != nil {
			err = s.finishPending()
		}
		for err == nil && (len(s.moves) > s.undoCount || len(s.repaired) > s.metadataUndone) && !a.pause {
			err = s.undoNext()
			a.syncSession(false)
			a.mu.Unlock()
			a.mu.Lock()
		}
		a.mu.Unlock()
		a.finish(err)
	}()
	return nil
}

// Section: External application controls.

func (a *application) pick(ctx context.Context, folder string) (string, error) {
	a.mu.Lock()
	if a.v.Closing || a.v.Busy || a.pickerDone != nil {
		a.mu.Unlock()
		return "", errors.New("The app is busy or a folder picker is already open")
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	a.pickerCancel, a.pickerDone = cancel, done
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.pickerCancel, a.pickerDone = nil, nil
		close(done)
		a.mu.Unlock()
	}()
	return pickFolder(ctx, folder)
}

func (a *application) requestClose() (work, picker <-chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v.Closing = true
	if a.chat.cancel != nil {
		a.chat.cancel()
	}
	a.pauseLocked()
	if a.pickerCancel != nil {
		a.pickerCancel()
	}
	return a.jobDone, a.pickerDone
}

func (a *application) download() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy || a.v.Closing {
		return errors.New("The app is busy or closing")
	}
	selection := a.activeSettings().AI.Model
	if modelReadyFor(a.modelDir, selection) {
		return nil
	}
	ctx := a.begin("download")
	a.v.Downloaded = 0
	go func() {
		err := fetchModelsFor(ctx, a.modelDir, selection, func(n int64) { a.mu.Lock(); a.v.Downloaded = n; a.mu.Unlock() })
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		a.finish(err)
	}()
	return nil
}
// Section: Inspection views and CSV export.

func (a *application) inspections(offset int, unknown bool) (inspectionPage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inspectionPageLocked(offset, unknown)
}

func (a *application) reviewInspections(offset int, unknown bool, filter string) (inspectionPage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inspectionPageFiltered(offset, unknown, filter)
}

func (a *application) inspectionPageLocked(offset int, unknown bool) (inspectionPage, error) {
	return a.inspectionPageFiltered(offset, unknown, "")
}

func (a *application) inspectionPageFiltered(offset int, unknown bool, filter string) (inspectionPage, error) {
	page := inspectionPage{Job: a.v.Job, Rows: make([]inspectionRow, 0, 100)}
	if a.s == nil || a.v.Busy && !a.scanActive && a.v.Phase != "reassessing" {
		return page, errors.New("The inspection report is unavailable during this operation")
	}
	for _, r := range a.s.plans {
		if a.s.manifest.Items[r.Index].Folder || unknown && !r.Unknown {
			continue
		}
		if !reviewMatches(a.s, r, filter) {
			continue
		}
		if page.Total >= offset && len(page.Rows) < 100 {
			row := inspectionRow{Path: a.s.currentPath(r.Path, r.Index), Note: r.Note, Unknown: r.Unknown, Inspection: r.Inspection, Index: r.Index, Revision: a.s.planVersions[r.Index], Kind: r.Kind, Reason: r.Reason, Decision: planOutcome(r)}
			if filter == "flagged" || filter == "auto" || filter == "all" {
				_, _, err := a.flaggedFile(a.v.Job, a.v.Revision, r.Index)
				row.ReviewActions = err == nil
				row.Refinement = a.s.canRefine(r.Index)
				if a.s.reviewable(r) {
					row.Metadata = r.Metadata
				}
			}
			if automatic, ok := a.s.automaticMoves[r.Index]; ok {
				row.AutomaticFrom, row.AutomaticTo = automatic.move.From, automatic.move.To
				row.AutomaticReason = automatic.reason
				row.AutomaticUndone = automatic.position >= len(a.s.moves)-a.s.undoCount
			}
			row.Outcome = planOutcome(r)
			if result, ok := a.s.outcomes[r.Index]; ok {
				row.RenameMS, row.Outcome = result.RenameMS, result.Status
				row.Note = joinNote(r.Note, result.Note)
			}
			if r.Move != nil && (!a.s.autoCompleted[r.Index] || a.s.byItem[r.Index].move.To != r.Move.To) {
				row.Proposed = r.Move.To
			}
			page.Rows = append(page.Rows, row)
		}
		page.Total++
	}
	if filter == "all" || filter == "unassessed" {
		for index := len(a.s.plans); index < a.s.fileCount; index++ {
			if page.Total >= offset && len(page.Rows) < 100 {
				item := a.s.manifest.Items[index]
				page.Rows = append(page.Rows, inspectionRow{Path: item.Path, Index: index,
					Decision: "Not assessed", Outcome: "Not assessed", Reason: "Waiting for assessment"})
			}
			page.Total++
		}
	}

	return page, nil
}

func csvCell(s string) string {
	if len(s) > 0 && strings.ContainsRune("=+-@\t\r\n", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (a *application) exportInspections(w *csv.Writer, unknown bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.s == nil || a.v.Busy && !a.scanActive && a.v.Phase != "reassessing" {
		return errors.New("The inspection report is unavailable during this operation")
	}
	if err := w.Write([]string{"Current path", "Proposed path", "Text or listing", "Metadata", "Image decoded", "Local AI", "Model", "Unknown publication fields", "Read ms", "OCR ms", "Model startup ms", "Local model ms", "Rename ms", "Outcome", "Notes", "Classification", "Naming decision", "Decision reason", "Cache", "AI contribution", "Total ms", "Publication date lookup", "Publication source", "Online lookup ms", "Classification checklist"}); err != nil {
		return err
	}
	for _, r := range a.s.plans {
		if a.s.manifest.Items[r.Index].Folder || unknown && !r.Unknown {
			continue
		}
		var proposed string
		if r.Move != nil {
			proposed = r.Move.To
		}
		checked := r.Inspection
		yesNo := func(b bool) string {
			if b {
				return "Yes"
			}
			return "No"
		}
		if checked.AI == "" {
			checked.AI = "Not recorded"
		}
		outcome, note, renameMS := planOutcome(r), r.Note, int64(0)
		if result, ok := a.s.outcomes[r.Index]; ok {
			outcome, note, renameMS = result.Status, result.Note, result.RenameMS
		}
		if err := w.Write([]string{csvCell(a.s.currentPath(r.Path, r.Index)), csvCell(proposed),
			yesNo(checked.Text), yesNo(checked.Metadata), yesNo(checked.Image), checked.AI, checked.Model, yesNo(r.Unknown),
			strconv.FormatInt(checked.ReadMS, 10), strconv.FormatInt(checked.OCRMS, 10), strconv.FormatInt(checked.StartupMS, 10), strconv.FormatInt(checked.ModelMS, 10),
			strconv.FormatInt(renameMS, 10), csvCell(outcome), csvCell(note), csvCell(r.Kind), csvCell(planOutcome(r)), csvCell(r.Reason), csvCell(checked.Cache), csvCell(checked.AIContribution), strconv.FormatInt(checked.TotalMS, 10), csvCell(checked.PublicationDate), csvCell(checked.PublicationSource), strconv.FormatInt(checked.OnlineMS, 10), csvCell(checklistText(checked.Checklist))}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func (a *application) export(w *csv.Writer) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy {
		return errors.New("Pause the run before exporting flags")
	}
	if a.s == nil {
		return errors.New("There is no run to export")
	}
	if err := w.Write([]string{"Current path", "Action", "Reason"}); err != nil {
		return err
	}
	for _, r := range a.s.flagRows() {
		status := r.Status
		if r.Type == "plan" {
			status = "Review"
		}
		if err := w.Write([]string{csvCell(a.s.currentPath(r.Path, r.Index)), status, csvCell(r.Note)}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
