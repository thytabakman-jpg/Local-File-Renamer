package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type flagRow struct{ Path, Status, Reason string }
type job struct {
	ID, Root, Date string
	Operation      scanOperation
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
	if err := pruneConfiguredCache(home); err != nil {
		a.v.Message = joinNote(a.v.Message, "Cache cleanup: "+err.Error())
	}
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

func within(parent, path string) bool {
	r, err := filepath.Rel(parent, path)
	return err == nil && (r == "." || (r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator))))
}
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
func (s *session) draftCleanupProposals(ctx context.Context, startIndex int) []proposalRow {
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
	return cleanups
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
	cleanups := s.draftCleanupProposals(ctx, startIndex)
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

