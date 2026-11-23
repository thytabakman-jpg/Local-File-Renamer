package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const bridgeURL = "http://127.0.0.1:39173/api/bridge"

type bridgeCommand struct {
	Action        string                     `json:"action"`
	Operation     scanOperation              `json:"operation,omitempty"`
	Job           string                     `json:"job,omitempty"`
	IncludeNames  bool                       `json:"include_names,omitempty"`
	Root          string                     `json:"root,omitempty"`
	Partial       bool                       `json:"without_ai,omitempty"`
	Folders       *bool                      `json:"rename_folders,omitempty"`
	Repair        *bool                      `json:"repair_metadata,omitempty"`
	Sort          *bool                      `json:"suggest_moves,omitempty"`
	Filter        string                     `json:"filter,omitempty"`
	Limit         int                        `json:"limit,omitempty"`
	PlanID        string                     `json:"plan_id,omitempty"`
	ClientVersion string                     `json:"client_version,omitempty"`
	Offset        int                        `json:"offset,omitempty"`
	Index         *int                       `json:"index,omitempty"`
	Indices       []int                      `json:"indices,omitempty"`
	Destinations  []moveDestinationOverride  `json:"destinations,omitempty"`
	Revision      *uint64                    `json:"revision,omitempty"`
	Rules         *namingRules               `json:"rules,omitempty"`
	Name          *string                    `json:"filename,omitempty"`
	Kind          string                     `json:"classification,omitempty"`
	Group         string                     `json:"group,omitempty"`
	Value         string                     `json:"value,omitempty"`
	Refresh       bool                       `json:"refresh_assessment,omitempty"`
	Resume        bool                       `json:"resume_saved_selection,omitempty"`
	Profile       string                     `json:"profile,omitempty"`
	Settings      *scanSettings              `json:"settings,omitempty"`
	Activate      bool                       `json:"activate,omitempty"`
	Authorized    bool                       `json:"authorized,omitempty"`
	First         string                     `json:"first,omitempty"`
	Second        string                     `json:"second,omitempty"`
	Destination   string                     `json:"destination,omitempty"`
	Fields        []string                   `json:"fields,omitempty"`
	Changes       map[string]json.RawMessage `json:"changes,omitempty"`
	Reset         bool                       `json:"reset,omitempty"`
	ScopeMode     string                     `json:"scope_mode,omitempty"`
	Extensions    []string                   `json:"include_extensions,omitempty"`
	Subfolders    *bool                      `json:"subfolders,omitempty"`
	Fresh         bool                       `json:"fresh,omitempty"`
	MediaSamples  *bool                      `json:"media_samples,omitempty"`
}

const appVersion = "3.5"
const bridgeVersion = 4
const bridgeResponseLimit = 512 << 10

func bridgeKeyPath(home string) string { return filepath.Join(home, "desktop-connection.key") }

func (a *application) setBridge(enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Closing {
		return errors.New("Renamer is closing")
	}
	if enabled == a.v.BridgeEnabled {
		return nil
	}
	path := bridgeKeyPath(a.home)
	if !enabled {
		a.bridgeKey, a.v.BridgeEnabled = "", false
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	key := token()
	f, err := os.CreateTemp(a.home, "desktop-connection-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(key)
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	a.bridgeKey, a.v.BridgeEnabled = key, true
	return nil
}

func (a *application) bridge(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	allowed := a.bridgeKey != "" && !a.v.Closing && r.Header.Get("Origin") == "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.bridgeKey)) == 1
	a.mu.Unlock()
	if !allowed {
		http.Error(w, "Allow the local ChatGPT connection in the renamer first.", 403)
		return
	}
	var cmd bridgeCommand
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&cmd) != nil {
		http.Error(w, "Invalid renamer command", 400)
		return
	}
	response, err := a.bridgeAction(cmd)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	reply(w, response)
}

func (a *application) bridgeAction(cmd bridgeCommand) (any, error) {
	var response any = map[string]bool{"ok": true}
	var err error
	revision := func() uint64 {
		if cmd.Revision != nil {
			return *cmd.Revision
		}
		return 0
	}
	switch cmd.Action {
	case "status":
		response, err = a.bridgeStatus(cmd.Job)
	case "capabilities":
		names := make([]string, 0, len(bridgeTools))
		for _, tool := range bridgeTools {
			names = append(names, tool["name"].(string))
		}
		response = map[string]any{"version": appVersion, "running_app_version": appVersion, "stdio_bridge_version": cmd.ClientVersion,
			"connection_api": bridgeVersion, "tools": names, "page_size": 100, "max_reassessment": 200}
	case "start":
		if !filepath.IsAbs(cmd.Root) {
			return nil, errors.New("Provide the full path of the folder to preview")
		}
		jobID := time.Now().UTC().Format("20060102T150405.000000000") + "-" + token()[:12]
		setup := scanStartOptions{ScopeMode: cmd.ScopeMode, Subfolders: cmd.Subfolders, Fresh: cmd.Fresh, FallbackAI: true, JobID: jobID}
		err = a.launchScan(cmd.Root, cmd.Partial, cmd.Profile, cmd.Operation, setup, func(opts *scanSettings) error {
			updated, patchErr := patchSettings(*opts, cmd.Changes)
			if patchErr != nil {
				return patchErr
			}
			*opts = updated
			if err := setup.configure(opts, cmd.Extensions); err != nil {
				return err
			}
			if opts.Execution.Mode == "automatic" && cmd.Operation != scanMove && cmd.Operation != scanDuplicates && !cmd.Authorized {
				return errors.New("This profile applies some naming rules automatically. Use an authorised start or a preview profile.")
			}
			if cmd.Folders != nil {
				opts.Execution.FolderRenames = *cmd.Folders
			}
			if cmd.Repair != nil {
				opts.Execution.Metadata = *cmd.Repair
			}
			if cmd.Sort != nil {
				opts.Execution.Moves = *cmd.Sort
			}
			if cmd.MediaSamples != nil {
				opts.Evidence.MediaSamples = *cmd.MediaSamples
			}
			return validateSettings(*opts)
		})
		response = map[string]any{"job": jobID, "phase": "indexing"}
	case "get_settings":
		info, infoErr := a.settingsInfo(cmd.Profile)
		if infoErr != nil || len(cmd.Fields) == 0 {
			return info, infoErr
		}
		fields := make(map[string]any)
		for _, field := range cmd.Fields {
			spec, ok := info.Fields[field]
			if !ok {
				return nil, errors.New("Unknown setting: " + field)
			}
			if field == "execution.auto_kinds" {
				spec.Values = classifications
			}
			value, err := settingValue(info.Effective, field)
			if err != nil {
				return nil, err
			}
			def, err := settingValue(info.Defaults, field)
			if err != nil {
				return nil, err
			}
			fields[field] = map[string]any{"value": value, "default": def, "spec": spec}
		}
		response = map[string]any{"revision": info.Revision, "active": info.Active, "fields": fields}
	case "patch_settings":
		if !cmd.Authorized || cmd.Revision == nil {
			return nil, errors.New("Saving profile changes requires authorization and the current settings revision")
		}
		value, patchErr := a.patchProfile(revision(), cmd.Profile, cmd.Changes, cmd.Reset)
		response, err = map[string]uint64{"revision": value}, patchErr
	case "save_profile":
		if cmd.Revision == nil || cmd.Settings == nil {
			return nil, errors.New("Provide the current profile revision and complete settings")
		}
		err = a.saveProfile(revision(), cmd.Profile, cmd.Settings, cmd.Activate)
	case "restore_profile":
		if cmd.Revision == nil {
			return nil, errors.New("Provide the current profile revision")
		}
		err = a.saveProfile(revision(), cmd.Profile, nil, true)
	case "scan_results":
		response, err = a.bridgeResults(cmd)
	case "proposals":
		response, err = a.proposalPage(cmd.Job, max(0, cmd.Offset))
	case "inspections":
		a.mu.Lock()
		if cmd.Job != "" && cmd.Job != a.v.Job {
			err = errors.New("The selected run changed")
		} else if !cmd.IncludeNames {
			err = errors.New("Set include_names after the user requests per-file details")
		} else {
			response, err = a.inspectionPageLocked(max(0, cmd.Offset), false)
		}
		a.mu.Unlock()
	case "report":
		report, reportErr := a.report(cmd.Job)
		if !cmd.IncludeNames && report.Receipt.LastError != "" {
			report.Receipt.LastError = "The run reported an error. See local details."
		}
		response, err = report, reportErr
	case "compare_runs":
		report, reportErr := a.compareRuns(cmd.First, cmd.Second, max(0, cmd.Offset), cmd.IncludeNames)
		if !cmd.IncludeNames {
			report.FirstReceipt.LastError = ""
			report.SecondReceipt.LastError = ""
		}
		response, err = report, reportErr
	case "get_rules":
		response = a.getRules()
	case "update_rules":
		if cmd.Rules == nil {
			return nil, errors.New("Provide the complete naming rules and their revision")
		}
		err = a.updateRules(revision(), *cmd.Rules)
	case "correct":
		if cmd.Index == nil || cmd.Revision == nil {
			return nil, errors.New("Provide the assessed file index and current revision")
		}
		err = a.correct(cmd.Job, revision(), *cmd.Index, cmd.Kind, cmd.Name)
	case "change_location":
		if cmd.Index == nil || cmd.Revision == nil {
			return nil, errors.New("Provide the assessed file index and current revision")
		}
		err = a.changeLocation(cmd.Job, revision(), *cmd.Index, cmd.Destination)
	case "reassess":
		if cmd.Revision == nil {
			return nil, errors.New("Provide the current proposal revision")
		}
		err = a.reassess(cmd.Job, revision(), cmd.Indices, cmd.Group, cmd.Value, cmd.Refresh)
	case "apply":
		if cmd.PlanID != "" {
			if !cmd.Authorized || cmd.Revision == nil || len(cmd.Indices) != 0 || len(cmd.Destinations) != 0 || cmd.Resume {
				return nil, errors.New("Apply the exact duplicate plan only with explicit authorization, its original revision, and no other selection")
			}
			if !recycleAvailable {
				return nil, errors.New("The Windows Recycle Bin is unavailable")
			}
			return a.applyExactBatch(context.Background(), cmd.Job, cmd.PlanID, revision(), recycleFile)
		}
		if !cmd.Authorized || cmd.Revision == nil {
			return nil, errors.New("Apply requires an authorized selection and the current proposal revision")
		}
		return a.bridgeApplySelection(cmd.Job, revision(), cmd.Indices, cmd.Resume, cmd.Destinations)
	case "undo":
		if !cmd.Authorized {
			return nil, errors.New("Undo requires authorization for the specified run")
		}
		err = a.undoJob(cmd.Job)
	case "diagnostics":
		response, err = a.diagnostic(cmd.Job, cmd.IncludeNames)
	case "pause":
		a.mu.Lock()
		if a.scanActive || a.v.Phase == "indexing" {
			a.pauseLocked()
		} else {
			err = errors.New("No active scan to pause")
		}
		a.mu.Unlock()
	case "resume":
		v := a.snapshot()
		err = a.resumeWithFallback(v.Job, v.Partial, true)
	default:
		err = errors.New("Unknown renamer command")
	}
	return response, err
}

func toolSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func bridgeTool(action, description string, properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	tool := map[string]any{"name": "renamer_" + action, "description": description, "inputSchema": toolSchema(properties, required...)}
	switch action {
	case "status", "capabilities", "diagnostics", "scan_results", "proposals", "inspections", "report", "compare_runs", "get_settings", "get_rules":
		tool["annotations"] = map[string]bool{"readOnlyHint": true}
	}
	return tool
}

func bridgeToolList() []map[string]any {
	str := map[string]string{"type": "string"}
	integer := map[string]any{"type": "integer", "minimum": 0}
	flag := map[string]string{"type": "boolean"}
	indices := map[string]any{"type": "array", "items": integer}
	rules := toolSchema(map[string]any{"normalize_spacing": flag, "replace_underscores": flag, "normalize_uppercase": flag,
		"prefer_specific_title": flag, "spelling": map[string]any{"type": "object", "additionalProperties": str}},
		"normalize_spacing", "replace_underscores", "normalize_uppercase", "prefer_specific_title")
	return []map[string]any{
		bridgeTool("status", "Read the compact operation receipt by job ID. A completed duplicate scan can include its exact-copy approval plan.", map[string]any{"job": str}),
		bridgeTool("capabilities", "Check the STDIO bridge version and the running Windows app version separately.", nil),
		bridgeTool("diagnostics", "Read timing and recent issues for a selected or saved run. Set include_names only when the user requests per-file detail.", map[string]any{"job": str, "include_names": flag}),
		bridgeTool("pause", "Pause the active scan.", nil),
		bridgeTool("resume", "Resume the paused scan using its saved settings.", nil),
		bridgeTool("start", "Start a scan of a user-selected local folder. Select operation rename, move, or duplicates. without_ai disables local inference.", map[string]any{"root": str, "operation": map[string]any{"type": "string", "enum": []scanOperation{scanRename, scanMove, scanDuplicates}}, "without_ai": flag, "rename_folders": flag, "repair_metadata": flag, "suggest_moves": flag, "media_samples": flag, "profile": str, "authorized": flag, "include_extensions": map[string]any{"type": "array", "items": str}, "scope_mode": map[string]any{"type": "string", "enum": []string{"all", "selected", "profile"}}, "subfolders": flag, "fresh": flag, "changes": map[string]string{"type": "object"}}, "root"),
		bridgeTool("scan_results", "Read filtered Move or Duplicate Check rows by job. Pass NextOffset and Revision for the next page. Set include_names only when the user requests paths.", map[string]any{"job": str, "include_names": flag, "offset": integer, "revision": integer, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "filter": map[string]any{"type": "string", "enum": bridgeResultFilters}}, "job"),
		bridgeTool("proposals", "Read a page of proposals including paths, scan indices and draft markers. Pass NextOffset for the next page. Re-read the revision before applying.", map[string]any{"job": str, "offset": integer}),
		bridgeTool("inspections", "Read a page of per-file naming evidence summaries and timings, with paths but no excerpts or image data.", map[string]any{"job": str, "include_names": flag, "offset": integer}, "include_names"),
		bridgeTool("report", "Read compact counts, classifications, decisions and timings for the selected run.", map[string]any{"job": str, "include_names": flag}),
		bridgeTool("compare_runs", "Compare saved scans with verified identical starting copies. Returns changes in naming, classes, warnings, receipts and local AI contribution. Set include_names when requested.", map[string]any{"first": str, "second": str, "offset": integer, "include_names": flag}, "first", "second"),
		bridgeTool("get_settings", "Read the effective profile and current run settings, defaults, allowed values, and when changes take effect.", map[string]any{"profile": str, "fields": map[string]any{"type": "array", "items": str}}),
		bridgeTool("patch_settings", "Save only the supplied setting fields using dotted keys such as execution.mode. Reset restores defaults for that profile.", map[string]any{"profile": str, "revision": integer, "changes": map[string]string{"type": "object"}, "reset": flag, "authorized": flag}, "revision", "authorized"),
		bridgeTool("save_profile", "Save a named profile using the complete settings returned by get_settings. Optionally activate it.", map[string]any{"profile": str, "revision": integer, "settings": map[string]string{"type": "object"}, "activate": flag}, "profile", "revision", "settings"),
		bridgeTool("restore_profile", "Activate a saved profile for future scans.", map[string]any{"profile": str, "revision": integer}, "profile", "revision"),
		bridgeTool("get_rules", "Read structured naming options, citation formats and the current rule revision.", nil),
		bridgeTool("update_rules", "Save complete naming options at their current rule revision. New scans and explicit reassessments use the updated rules.", map[string]any{"revision": integer, "rules": rules}, "revision", "rules"),
		bridgeTool("correct", "Correct a classification or a proposed basename in an assessed, unapplied item. A classification-only correction clears the proposal until reassessment.", map[string]any{"job": str, "revision": integer, "index": integer, "classification": map[string]any{"type": "string", "enum": classifications}, "filename": str}, "job", "revision", "index"),
		bridgeTool("change_location", "Revise a file’s proposed destination inside the scan root. An empty destination keeps the current folder. This edits the proposal only.", map[string]any{"job": str, "revision": integer, "index": integer, "destination": str}, "job", "revision", "index"),
		bridgeTool("reassess", "Use cached evidence for assessed file indices, or a group keyed by classification, extension or decision. refresh_assessment requests fresh inference if needed.", map[string]any{"job": str, "revision": integer, "indices": indices, "group": map[string]any{"type": "string", "enum": []string{"classification", "extension", "decision"}}, "value": str, "refresh_assessment": flag}, "job", "revision"),
		bridgeTool("apply", "Apply authorised indices, optionally overriding Move destinations, or apply an explicitly approved exact duplicate plan_id. Retry that plan with its original revision. Use resume_saved_selection for a saved move or rename selection.", map[string]any{"job": str, "revision": integer, "indices": indices, "plan_id": str, "destinations": map[string]any{"type": "array",
			"items": toolSchema(map[string]any{"index": integer, "destination": str}, "index", "destination")}, "resume_saved_selection": flag, "authorized": flag}, "job", "revision", "authorized"),
		bridgeTool("undo", "Undo applied changes for the specified run. Set authorized only with the user's authorization.", map[string]any{"job": str, "authorized": flag}, "job", "authorized"),
	}
}

var bridgeTools = bridgeToolList()

func runMCP(home string, input io.Reader, output io.Writer) {
	in := bufio.NewScanner(input)
	in.Buffer(make([]byte, 8192), 1<<20)
	out := json.NewEncoder(output)
	for in.Scan() {
		var req struct {
			JSONRPC, Method string
			ID              json.RawMessage
			Params          json.RawMessage
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		result := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			var p struct{ ProtocolVersion string }
			_ = json.Unmarshal(req.Params, &p)
			switch p.ProtocolVersion {
			case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
			default:
				p.ProtocolVersion = "2025-11-25"
			}
			result["result"] = map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]bool{}},
				"serverInfo": map[string]string{"name": "local-file-renamer", "version": appVersion}}
		case "ping":
			result["result"] = map[string]any{}
		case "tools/list":
			result["result"] = map[string]any{"tools": bridgeTools}
		case "tools/call":
			var p struct {
				Name      string
				Arguments bridgeCommand
			}
			err := json.Unmarshal(req.Params, &p)
			var data string
			if err == nil {
				p.Arguments.Action = strings.TrimPrefix(p.Name, "renamer_")
				known := false
				for _, tool := range bridgeTools {
					if tool["name"] == p.Name {
						known = true
						break
					}
				}
				if known {
					p.Arguments.ClientVersion = appVersion
					data, err = callBridge(home, p.Arguments)
					if err == nil && p.Arguments.Action == "capabilities" {
						data, err = bridgeVersionDetails(data)
					}
				} else {
					err = errors.New("Unknown renamer tool")
				}
			}
			if err != nil {
				data = err.Error()
			}
			result["result"] = map[string]any{"isError": err != nil, "content": []map[string]string{{"type": "text", "text": data}}}
		default:
			result["error"] = map[string]any{"code": -32601, "message": "Method not found"}
		}
		if out.Encode(result) != nil {
			return
		}
	}
}

func bridgeVersionDetails(data string) (string, error) {
	var versions map[string]any
	if err := json.Unmarshal([]byte(data), &versions); err != nil || versions == nil {
		return "", errors.New("Renamer returned invalid capability details")
	}
	versions["stdio_bridge_version"] = appVersion
	if _, ok := versions["running_app_version"]; !ok {
		versions["running_app_version"] = versions["version"]
	}
	result, err := json.Marshal(versions)
	return string(result), err
}

func callBridge(home string, cmd bridgeCommand) (string, error) {
	key, err := os.ReadFile(bridgeKeyPath(home))
	if err != nil || len(key) != 48 {
		return "", errors.New("Open Local File Renamer and allow the local ChatGPT connection.")
	}
	body, err := json.Marshal(cmd)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest("POST", bridgeURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(key))
	timeout := 12 * time.Second
	if cmd.Action == "apply" {
		timeout = 5 * time.Minute
	}
	client := http.Client{Transport: loopbackTransport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("Local File Renamer did not respond. Check the job status before retrying. For exact duplicates, retry with the same plan and revision.")
	}
	defer resp.Body.Close()
	return readBridgeResponse(resp.StatusCode, resp.Body)
}

func readBridgeResponse(status int, body io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(body, bridgeResponseLimit+1))
	if err != nil {
		return "", err
	}
	if len(b) > bridgeResponseLimit {
		return "", errors.New("Renamer response exceeds the bridge limit. Request a filtered or smaller page.")
	}
	if status != http.StatusOK {
		return "", errors.New(strings.TrimSpace(string(b)))
	}
	if !json.Valid(b) {
		return "", errors.New("Renamer returned incomplete or invalid JSON")
	}
	return string(b), nil
}
