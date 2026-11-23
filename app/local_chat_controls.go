package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var proposalExplanation = regexp.MustCompile(`(?i)^(?:explain|show why for) (?:proposal|file) (\d+)[.?!]?$`)
var folderPickerRequest = regexp.MustCompile(`(?i)^(?:open|show) (?:the )?folder picker(?: (?:at|in) (.+))?[.?!]?$`)
var explicitDefaultsRequest = regexp.MustCompile(`(?i)\b(?:default to|always scan|(?:make|save|set|use|change|remember|keep)\b.{0,120}\b(?:defaults?|future scans|every new scan|from now on))\b`)
var retryModelRequest = regexp.MustCompile(`(?i)\bif (?:the )?(?:local )?(?:ai|model) (?:failed|timed out|crashed)\b.*\b(?:try again|retry|restart|fresh)\b`)
var allTypesRequest = regexp.MustCompile(`(?i)\b(?:all|every) file(?: type)?s?\b`)
var inspectScopeRequest = regexp.MustCompile(`(?i)^(?:find out|tell me|show me|explain|why|what)\b.*\b(?:filters?|scope|file types?|subfolders|found|(?:only (?:found|scanned|listed|included)))\b`)
var recentFolderRequest = regexp.MustCompile(`(?i)^use (?:my|the) last (.+?) folder,`)
var freshScanRequest = regexp.MustCompile(`(?i)^(?:please )?(?:start|run|do|try|check|scan|find out|why)\b.*\b(?:fresh|from scratch|again)\b`)

func extendedImmediateChatPlan(message string) (chatPlan, bool) {
	lower := strings.Trim(strings.ToLower(message), " .!?")
	switch lower {
	case "scan settings", "scan scope", "show scan settings", "show scan scope":
		return chatPlan{Action: "scope"}, true
	case "scan diagnostics", "diagnose scan", "why did it pause", "why did the scan pause", "why did the scan fail":
		return chatPlan{Action: "diagnostics"}, true
	case "show results", "show scan results", "show exact duplicates", "show duplicate groups":
		return chatPlan{Action: "results", ExactOnly: lower == "show exact duplicates"}, true
	case "scan history", "show scan history", "show history":
		return chatPlan{Action: "history"}, true
	case "open last scan", "open latest scan":
		return chatPlan{Action: "open_run", Run: "latest"}, true
	case "open previous scan":
		return chatPlan{Action: "open_run", Run: "previous"}, true
	case "use last folder", "use last scan folder", "use the last scan folder":
		return chatPlan{Action: "use_folder", Run: "latest"}, true
	case "compare the last two scans", "compare last two scans":
		return chatPlan{Action: "compare", Run: "previous", OtherRun: "latest"}, true
	case "make that my default", "make this my default", "save this scope as my default":
		return chatPlan{Action: "save_defaults"}, true
	case "default to all file types", "scan all file types by default", "always scan all file types":
		return chatPlan{Action: "save_defaults", ScopeMode: "all"}, true
	case "scan all files", "scan every file", "scan all file types":
		return chatPlan{Action: "start", ScopeMode: "all"}, true
	case "check for duplicates", "duplicate check", "scan for duplicates", "run a duplicate check":
		return chatPlan{Action: "start", Operation: scanDuplicates}, true
	case "start file renamer", "run file renamer":
		return chatPlan{Action: "start", Operation: scanRename}, true
	case "start file mover", "run file mover", "suggest where these files belong":
		return chatPlan{Action: "start", Operation: scanMove}, true
	}
	if m := folderPickerRequest.FindStringSubmatch(strings.TrimSpace(message)); m != nil {
		return chatPlan{Action: "pick_folder", Folder: strings.TrimSpace(m[1])}, true
	}
	if m := proposalExplanation.FindStringSubmatch(strings.TrimSpace(message)); m != nil {
		if i, err := strconv.Atoi(m[1]); err == nil {
			return chatPlan{Action: "explain", Indices: []int{i}}, true
		}
	}
	// Common recovery requests work before a local language model is installed.
	// An ambiguous path or restriction goes through the normal request planner.
	if retryModelRequest.MatchString(lower) && (strings.Contains(lower, "built-in analysis") || strings.Contains(lower, "without local ai")) {
		return chatPlan{Action: "start", RetryModelFailure: true, WithoutAI: true, Fresh: true}, true
	}
	diagnose := inspectScopeRequest.MatchString(lower)
	action := lower
	if _, rest, ok := strings.Cut(lower, "then "); ok && diagnose {
		action = rest
	}
	fresh := freshScanRequest.MatchString(action)
	recent := recentFolderRequest.FindStringSubmatch(message)
	duplicates := strings.Contains(action, "duplicates")
	all := allTypesRequest.MatchString(action)
	fallback := strings.Contains(action, "without local ai") || strings.Contains(action, "built-in analysis")
	if (fresh && (all || fallback || action == "start fresh" || action == "run a fresh scan") ||
		recent != nil && duplicates && all) && !looksLikeLocalPath(message) &&
		!strings.Contains(action, "except ") && !strings.Contains(action, "only scan") && !strings.Contains(action, "apply ") &&
		!explicitDefaultsRequest.MatchString(action) {
		plan := chatPlan{Action: "start", Fresh: fresh, Diagnose: diagnose, WithoutAI: fallback,
			ShowResults: strings.Contains(action, "show")}
		if recent != nil {
			plan.Run, plan.Folder = "latest", strings.TrimSpace(recent[1])
		}
		if all {
			plan.ScopeMode = "all"
		}
		if strings.Contains(action, "subfolder") {
			plan.Subfolders = "include"
			if strings.Contains(action, "without subfolder") || strings.Contains(action, "exclude subfolder") {
				plan.Subfolders = "exclude"
			}
		}
		if duplicates {
			plan.Operation, plan.ExactOnly = scanDuplicates, strings.Contains(action, "exact")
		}
		return plan, true
	}
	if diagnose && !strings.Contains(lower, "then ") && !strings.Contains(lower, " and ") && !strings.Contains(lower, "restart") {
		return chatPlan{Action: "scope"}, true
	}
	return chatPlan{}, false
}

func (p chatPlan) scanSetup() (scanStartOptions, error) {
	setup := scanStartOptions{ScopeMode: p.ScopeMode, Fresh: p.Fresh, FallbackAI: !p.UseImageScanner}
	switch p.Subfolders {
	case "", "default":
	case "include", "exclude":
		value := p.Subfolders == "include"
		setup.Subfolders = &value
	default:
		return setup, errors.New("Choose whether to include subfolders")
	}
	return setup, nil
}

func (a *application) describeScanScope(v view) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	settings := a.activeSettings()
	active := "Active profile " + a.profiles.Active + " · " + settings.Scope.description()
	if v.Job == "" {
		return active + ". Choose a folder to start a scan.", nil
	}
	if a.s == nil || v.Job != a.v.Job {
		return "", errors.New("The selected run changed. Ask for scan settings again.")
	}
	saved := a.s.settings()
	lines := []string{"Saved run in " + v.Root, saved.Scope.description(), active}
	if v.Inventory != nil {
		lines = append(lines, v.Inventory.summary()+".")
	} else {
		lines = append(lines, fmt.Sprintf("%d entries on the saved list. This older run did not record counts before filtering.", v.Total))
	}
	lines = append(lines, fmt.Sprintf("%d files assessed, %d skipped, %d proposals. Status: %s.", v.FilesAssessed, v.FilesSkipped, v.Proposed, v.Phase))
	if v.Partial && v.Operation != scanDuplicates {
		lines = append(lines, "The saved run uses built-in analysis.")
	}
	if v.StartError != "" {
		lines = append(lines, "Most recent start error: "+v.StartError)
	} else if v.Message != "" {
		lines = append(lines, v.Message)
	}
	return strings.Join(lines, "\n"), nil
}

func (a *application) saveChatDefaults(plan chatPlan, message string) (string, error) {
	if !explicitDefaultsRequest.MatchString(message) {
		return "", errors.New("Say explicitly if these settings are for future scans")
	}
	setup, err := plan.scanSetup()
	if err != nil {
		return "", err
	}
	setup.Fresh = false
	info, err := a.settingsInfo("")
	if err != nil {
		return "", err
	}
	settings := info.Effective
	if plan.ScopeMode == "" && setup.Subfolders == nil && len(plan.Include) == 0 && len(plan.Exclude) == 0 && !plan.WithoutAI && plan.ScanAI == "" && len(plan.Settings) == 0 {
		if info.Run == nil {
			return "", errors.New("Specify the default settings to save or select a saved run first")
		}
		settings.Scope = info.Run.Scope
	} else {
		if err := applyChatScope(&settings, plan, setup); err != nil {
			return "", err
		}
	}
	if err := a.saveProfile(info.Revision, info.Active, &settings, true); err != nil {
		return "", err
	}
	summary := "Saved defaults for " + info.Active + ": " + settings.Scope.description() + "."
	if plan.WithoutAI || plan.ScanAI == "off" {
		summary += " New scans will use built-in analysis."
	}
	for _, change := range plan.Settings {
		if spec, ok := settingsSpecs[change.Key]; ok {
			summary += "\n" + spec.Label + " = " + change.Value
		}
	}
	return summary, nil
}

func applyChatScope(opts *scanSettings, plan chatPlan, setup scanStartOptions) error {
	if err := setup.configure(opts, plan.Include); err != nil {
		return err
	}
	if len(plan.Exclude) > 0 {
		opts.Scope.ExcludeExtensions = append(slices.Clone(opts.Scope.ExcludeExtensions), plan.Exclude...)
	}
	switch plan.ScanAI {
	case "":
	case "off":
		opts.AI.Invoke = "never"
	case "on":
		if opts.AI.Invoke == "never" {
			opts.AI.Invoke = "unresolved"
		}
		if opts.AI.Model == "off" {
			opts.AI.Model = "configured"
		}
	default:
		return errors.New("Choose whether to use local AI")
	}
	if err := applyChatSettings(opts, plan.Settings); err != nil {
		return err
	}
	if plan.WithoutAI {
		opts.AI.Invoke = "never"
	}
	return nil
}

type chatSettingChange struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func applyChatSettings(opts *scanSettings, changes []chatSettingChange) error {
	if len(changes) == 0 {
		return nil
	}
	if len(changes) > 8 {
		return errors.New("Change up to eight settings at a time")
	}
	raw, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	var sections map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sections); err != nil {
		return err
	}
	for _, change := range changes {
		group, key, _ := strings.Cut(change.Key, ".")
		current, exists := sections[group][key]
		spec, known := settingsSpecs[change.Key]
		if !exists || !known {
			return fmt.Errorf("Unknown scan setting %q", change.Key)
		}
		value := []byte(strings.TrimSpace(change.Value))
		if current[0] == '"' {
			value, _ = json.Marshal(change.Value)
		}
		if !json.Valid(value) || string(value) == "null" {
			return fmt.Errorf("Provide a value for %s in the correct format", spec.Label)
		}
		sections[group][key] = value
	}
	raw, err = json.Marshal(sections)
	if err != nil {
		return err
	}
	var updated scanSettings
	if err := json.Unmarshal(raw, &updated); err != nil {
		return errors.New("One of the settings used a value with the wrong type")
	}
	*opts = updated
	return nil
}

func chatRun(jobs []job, ref, folder string) (job, error) {
	ref, folder = strings.TrimSpace(ref), strings.TrimSpace(folder)
	for _, candidate := range jobs {
		if candidate.ID == ref {
			return candidate, nil
		}
	}
	var matching []job
	for _, candidate := range jobs {
		if folder == "" || strings.EqualFold(filepath.Base(candidate.Root), folder) || samePath(folder, candidate.Root) {
			matching = append(matching, candidate)
		}
	}
	if len(matching) == 0 && folder != "" && !filepath.IsAbs(folder) {
		prefix := strings.ToLower(folder) + " "
		var root string
		for _, candidate := range jobs {
			if !strings.HasPrefix(strings.ToLower(filepath.Base(candidate.Root)), prefix) {
				continue
			}
			if root != "" && !samePath(root, candidate.Root) {
				return job{}, errors.New("Several saved folders match. Give the full folder path or choose it in New scan.")
			}
			root = candidate.Root
			matching = append(matching, candidate)
		}
	}
	switch strings.ToLower(ref) {
	case "", "latest":
		if len(matching) > 0 {
			return matching[0], nil
		}
	case "previous":
		if len(matching) > 1 {
			return matching[1], nil
		}
	case "today", "yesterday":
		date := time.Now()
		if strings.EqualFold(ref, "yesterday") {
			date = date.AddDate(0, 0, -1)
		}
		for _, candidate := range matching {
			if strings.HasPrefix(candidate.Date, date.Format("2006-01-02")) {
				return candidate, nil
			}
		}
	default:
		return chatRun(matching, "latest", ref)
	}
	return job{}, errors.New("No matching saved scan was found. Show scan history to see the available runs.")
}

func (a *application) compareChatRuns(left, right job) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy || a.v.Closing {
		return "", errors.New("Wait for the current operation to finish before comparing scans")
	}
	if left.ID == right.ID {
		return "", errors.New("Choose two distinct saved scans")
	}
	lines := make([]string, 0, 2)
	for _, run := range []job{left, right} {
		s, err := openSession(filepath.Join(a.home, "runs", run.ID))
		if err != nil {
			return "", err
		}
		receipt, scope := s.receipt(), s.settings().Scope.description()
		line := fmt.Sprintf("%s · %s · %d file entries, %d assessed, %d skipped, %d pending, %d pending changes. %s.",
			run.Date, run.Root, s.fileCount, receipt.Assessments.Completed, receipt.Assessments.Skipped, receipt.Assessments.Pending, receipt.Renames.Pending, scope)
		s.close()
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

func (a *application) chatProposal(v view, plan chatPlan) (proposalRow, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if s == nil || v.Job != a.v.Job || v.Revision != a.v.Revision {
		return proposalRow{}, errors.New("Open the run and ask for the proposal again")
	}
	index := -1
	if len(plan.Indices) == 1 {
		index = plan.Indices[0]
	} else if name := strings.TrimSpace(plan.Query); name != "" {
		for _, row := range s.plans {
			if strings.EqualFold(filepath.Base(row.Path), name) || samePath(row.Path, name) {
				if index != -1 {
					return proposalRow{}, errors.New("Several files match that name. Use the proposal number or full path.")
				}
				index = row.Index
			}
		}
	}
	if index < 0 || index >= len(s.plans) {
		return proposalRow{}, errors.New("Supply one assessed file name or proposal number")
	}
	row := s.plans[index]
	proposal := proposalRow{Index: index, From: s.currentPath(row.Path, index), Decision: planOutcome(row), Reason: row.Reason, Note: row.Note}
	if row.Move != nil {
		proposal.To = row.Move.To
	}
	return proposal, nil
}

func (a *application) chatReassess(v view, plan chatPlan) error {
	indices := plan.Indices
	if plan.Flagged || len(indices) == 0 && len(plan.Include) > 0 {
		a.mu.Lock()
		if a.s == nil || v.Job != a.v.Job || v.Revision != a.v.Revision {
			a.mu.Unlock()
			return errors.New("The run changed. Review the current run before reassessing.")
		}
		for _, row := range a.s.plans {
			if row.Index >= a.s.fileCount || plan.Flagged && !flaggedPlan(row) {
				continue
			}
			if len(plan.Include) > 0 && !slices.ContainsFunc(plan.Include, func(ext string) bool { return strings.EqualFold(ext, extension(row.Path)) }) {
				continue
			}
			if _, err := a.s.checkEditable(row.Index); err == nil {
				indices = append(indices, row.Index)
			}
		}
		a.mu.Unlock()
	}
	return a.reassess(v.Job, v.Revision, indices, "", "", true)
}

func (a *application) chatResults(v view, exact bool) (chatReply, error) {
	if v.Operation != scanMove && v.Operation != scanDuplicates {
		if v.Job == "" {
			return chatReply{}, errors.New("Start a scan or open a saved run first")
		}
		page, err := a.proposalPage(v.Job, 0)
		return chatReply{Message: fmt.Sprintf("%d proposals for %s.", page.Total, v.Root), Proposals: page.Rows}, err
	}
	results, err := a.scanResults(v.Job)
	if err != nil {
		return chatReply{}, err
	}
	if v.Operation == scanDuplicates {
		response := chatReply{Message: fmt.Sprintf("%d exact duplicate groups. %d files skipped.", len(results.Exact), results.Skipped)}
		if !exact {
			response.Message += fmt.Sprintf(" %d cleanup recommendations. %d candidate groups left alone.", len(results.Verified), results.Unresolved)
		}
		for _, group := range results.Exact[:min(25, len(results.Exact))] {
			response.Groups = append(response.Groups, chatResultGroup{"Exact duplicate", group.Files})
		}
		if !exact {
			for _, group := range results.Verified[:min(25, len(results.Verified))] {
				response.Groups = append(response.Groups, chatResultGroup{group.Finding, group.Files})
			}
		}
		if len(results.Exact) > 25 || !exact && len(results.Verified) > 25 {
			response.Message += " Open Review for all groups."
		}
		return response, nil
	}
	response := chatReply{}
	total := 0
	for _, folder := range results.Folders {
		total += len(folder.Proposals)
		room := 50 - len(response.Proposals)
		response.Proposals = append(response.Proposals, folder.Proposals[:min(room, len(folder.Proposals))]...)
	}
	response.Message = fmt.Sprintf("%d proposed moves for %s.", total, v.Root)
	if total > len(response.Proposals) {
		response.Message += " Open Review for all moves."
	}
	return response, nil
}

func (a *application) chatScanUpdate(id string, exact, results bool) (chatReply, error) {
	v := a.snapshot()
	if id == "" || v.StartID != id {
		return chatReply{}, errors.New("A different scan has started. Open History to see the earlier run.")
	}
	if v.Busy {
		return chatReply{ScanID: id}, nil
	}
	if v.StartError != "" {
		return chatReply{Message: "Scan could not start. " + v.StartError}, nil
	}
	if v.StartJob == "" || v.Job != v.StartJob {
		return chatReply{}, errors.New("The selected run changed. Open History to see the scan.")
	}
	summary := fmt.Sprintf("Scan %s in %s. %d files assessed, %d skipped, %d proposals.", v.Phase, v.Root, v.FilesAssessed, v.FilesSkipped, v.Proposed)
	if v.Inventory != nil {
		summary += "\n" + v.Inventory.summary() + "."
	}
	if v.Message != "" {
		summary += "\n" + v.Message
	}
	if !results || v.Phase == "paused" {
		return chatReply{Message: summary}, nil
	}
	response, err := a.chatResults(v, exact)
	if err != nil {
		return chatReply{}, err
	}
	response.Message = summary + "\n" + response.Message
	return response, nil
}

func chatPickerStart(requested, selected, saved string) string {
	if filepath.IsAbs(requested) {
		return requested
	}
	for _, common := range []string{"Desktop", "Documents", "Downloads", "Pictures", "Videos"} {
		if strings.EqualFold(strings.TrimSpace(requested), common) {
			if home, err := os.UserHomeDir(); err == nil {
				return filepath.Join(home, common)
			}
		}
	}
	if selected != "" {
		return selected
	}
	return saved
}

func chatHistory(jobs []job) string {
	if len(jobs) == 0 {
		return "There are no saved scans yet."
	}
	lines := []string{"Recent scans"}
	for _, run := range jobs[:min(10, len(jobs))] {
		operation := string(run.Operation)
		if operation == "" {
			operation = string(scanRename)
		}
		lines = append(lines, fmt.Sprintf("%s · %s · %s · %s", run.Date, operation, run.Root, run.ID))
	}
	return strings.Join(lines, "\n")
}

func (a *application) chatDiagnostics(v view) (chatReply, error) {
	if v.StartError != "" {
		return chatReply{Message: "The latest scan could not start. " + v.StartError}, nil
	}
	if v.Job == "" {
		return chatReply{Message: "There is no saved scan to diagnose."}, nil
	}
	d, err := a.diagnostic(v.Job, true)
	if err != nil {
		return chatReply{}, err
	}
	lines := []string{fmt.Sprintf("Scan %s in %s. %d of %d items processed.", v.Phase, v.Root, v.Done, v.Total)}
	if d.LastError != "" {
		lines = append(lines, d.LastError)
	} else if v.Message != "" {
		lines = append(lines, v.Message)
	}
	for _, issue := range d.RecentIssues[:min(3, len(d.RecentIssues))] {
		note := strings.TrimSpace(issue.Note + " " + issue.Inspection.Detail)
		if note != "" {
			lines = append(lines, filepath.Base(issue.Path)+" · "+note)
		}
	}
	lines = append(lines, fmt.Sprintf("Reading %.1f s · model startup %.1f s · model analysis %.1f s.",
		float64(d.ReadMS)/1000, float64(d.StartupMS)/1000, float64(d.ModelMS)/1000))
	return chatReply{Message: strings.Join(lines, "\n")}, nil
}

func (a *application) editChatProposal(v view, plan chatPlan) (chatReply, error) {
	row, err := a.chatProposal(v, plan)
	if err != nil {
		return chatReply{}, err
	}
	switch plan.Action {
	case "explain":
		message := strings.Join([]string{row.Decision, row.Reason, row.Note}, " ")
		return chatReply{Message: strings.TrimSpace(message), Proposals: []proposalRow{row}}, nil
	case "keep_name", "rename_proposal":
		name := strings.TrimSpace(plan.Name)
		if plan.Action == "keep_name" {
			name = filepath.Base(row.From)
		}
		if err := a.correct(v.Job, v.Revision, row.Index, "", &name); err != nil {
			return chatReply{}, err
		}
		return chatReply{Message: fmt.Sprintf("Updated the proposed name for file %d to %s.", row.Index, name)}, nil
	case "change_destination":
		destination := strings.TrimSpace(plan.Destination)
		if destination != "" && !filepath.IsAbs(destination) {
			destination = filepath.Join(v.Root, destination)
		}
		if err := a.changeLocation(v.Job, v.Revision, row.Index, destination); err != nil {
			return chatReply{}, err
		}
		return chatReply{Message: fmt.Sprintf("Updated the proposed destination for file %d.", row.Index)}, nil
	}
	return chatReply{}, errors.New("Choose how to edit the proposal")
}

func (a *application) startChatScan(v view, plan chatPlan, selected string) (chatReply, error) {
	folder := strings.TrimSpace(selected)
	var lines []string
	var recoveryScope *scanScope
	if plan.RetryModelFailure {
		diagnosis, err := a.chatDiagnostics(v)
		if err != nil {
			return chatReply{}, err
		}
		if !a.savedModelFailure(v) {
			diagnosis.Message += "\nThe saved run has no recorded model failure."
			return diagnosis, nil
		}
		info, err := a.settingsInfo("")
		if err != nil || info.Run == nil {
			return chatReply{}, errors.New("Open the failed run before retrying it")
		}
		recoveryScope = &info.Run.Scope
		plan.WithoutAI = true
		plan.Operation = v.Operation
		plan.Run, plan.Folder = "", ""
		folder = v.Root
		lines = append(lines, diagnosis.Message)
	}
	switch {
	case plan.Run != "":
		run, err := chatRun(v.Jobs, plan.Run, plan.Folder)
		if err != nil {
			return chatReply{}, err
		}
		folder = run.Root
	case strings.TrimSpace(plan.Folder) != "":
		folder = strings.TrimSpace(plan.Folder)
		normalise := func(value string) string { return strings.ToLower(strings.ReplaceAll(value, "\\", "/")) }
		if plan.userText != "" && !samePath(folder, selected) && !samePath(folder, v.Root) &&
			!strings.Contains(normalise(plan.userText), normalise(folder)) {
			return chatReply{}, errors.New("Choose the folder or give its full path in your message")
		}
	case folder == "":
		folder = v.Root
	}
	if !filepath.IsAbs(folder) {
		return chatReply{}, errors.New("Choose the full folder path in New scan or give it in Local chat")
	}
	if plan.Operation == "" {
		plan.Operation = v.Operation
		if plan.Operation == "" {
			plan.Operation = scanRename
		}
	}
	setup, err := plan.scanSetup()
	if err != nil {
		return chatReply{}, err
	}
	setup.RequestID = token()
	if plan.Diagnose {
		message, err := a.describeScanScope(v)
		if err != nil {
			return chatReply{}, err
		}
		lines = append(lines, message)
	}
	if plan.SaveDefaults {
		message, err := a.saveChatDefaults(plan, plan.userText)
		if err != nil {
			return chatReply{}, err
		}
		lines = append(lines, message)
	}

	err = a.launchScan(folder, plan.WithoutAI, "", plan.Operation, setup, func(opts *scanSettings) error {
		if recoveryScope != nil {
			opts.Scope = *recoveryScope
		}
		if err := applyChatScope(opts, plan, setup); err != nil {
			return err
		}
		// The shared configurator also accepts extension rules from older callers.
		config := plan
		config.Include, config.Exclude = nil, nil
		if err := configureChatScan(opts, config); err != nil {
			return err
		}
		if plan.UseImageScanner && !modelReadyFor(a.modelDir, opts.AI.Model) {
			name, _, _ := modelProfileFor(opts.AI.Model)
			return fmt.Errorf("Download %s in Scan settings before starting an image scan", name)
		}
		return nil
	})
	if err != nil {
		if plan.SaveDefaults {
			lines = append(lines, "Scan could not start. "+err.Error())
			return chatReply{Message: strings.Join(lines, "\n\n"), SettingsChanged: true}, nil
		}
		return chatReply{}, err
	}
	description := "The preview scan started for " + folder + "."
	if plan.Fresh {
		description = "Started a fresh preview scan in " + folder + "."
	}
	if plan.UseImageScanner {
		description = "Started an image-only preview in " + folder + " using the image scanner and your naming instructions."
	}
	lines = append(lines, description)
	return chatReply{Message: strings.Join(lines, "\n\n"), Folder: folder, ScanID: setup.RequestID,
		SettingsChanged: plan.SaveDefaults, ExactOnly: plan.ExactOnly, ShowResults: plan.ShowResults || plan.ExactOnly}, nil
}

func (a *application) savedModelFailure(v view) bool {
	if v.Job == "" || v.Busy || v.Partial {
		return false
	}
	report, err := a.diagnostic(v.Job, true)
	if err != nil {
		return false
	}
	if strings.Contains(strings.ToLower(report.LastError), "local ai failures") {
		return true
	}
	for _, issue := range report.RecentIssues {
		switch issue.Inspection.AI {
		case modelTimeout, modelUnavailable, modelProcess, modelInvalid:
			return true
		}
	}
	return false
}
