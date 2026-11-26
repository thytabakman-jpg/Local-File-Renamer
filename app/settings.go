package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type scanScope struct {
	Subfolders        bool     `json:"subfolders"`
	IncludeExtensions []string `json:"include_extensions"`
	ExcludeExtensions []string `json:"exclude_extensions"`
	IncludePaths      []string `json:"include_paths"`
	ExcludePaths      []string `json:"exclude_paths"`
	SelectedPaths     []string `json:"selected_paths"`
	MinBytes          int64    `json:"min_bytes"`
	MaxBytes          int64    `json:"max_bytes"`
	VerifyComparison  bool     `json:"verify_comparison"`
}

type evidenceSettings struct {
	PDFPages      int    `json:"pdf_pages"`
	ExtraPDFPages int    `json:"extra_pdf_pages"`
	TextBytes     int    `json:"text_bytes"`
	UseOCR        bool   `json:"use_ocr"`
	OCRUnresolved bool   `json:"ocr_unresolved"`
	OCREngine     string `json:"ocr_engine"`
	OCRPages      int    `json:"ocr_pages"`
	ImageEdge     int    `json:"image_edge"`
	DeepenOn      string `json:"deepen_on"`
	OnlineDates   string `json:"online_dates"`
	MediaSamples  bool   `json:"media_samples"`
}

type aiSettings struct {
	Model          string `json:"model"`
	Instructions   string `json:"instructions"`
	ResponseTokens int    `json:"response_tokens"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Invoke         string `json:"invoke"`
	RecordEvidence bool   `json:"record_evidence"`
}

type resourceSettings struct {
	Threads         int    `json:"threads"`
	BatchThreads    int    `json:"batch_threads"`
	Workers         int    `json:"workers"`
	DecodeMemoryMiB int    `json:"decode_memory_mib"`
	Priority        string `json:"priority"`
}

type recoverySettings struct {
	Uncertainty     string `json:"uncertainty"`
	RequestRetries  int    `json:"request_retries"`
	ProcessRestarts int    `json:"process_restarts"`
	PauseAfter      int    `json:"pause_after"`
}

type cacheSettings struct {
	ReuseEvidence   bool `json:"reuse_evidence"`
	ReuseAssessment bool `json:"reuse_assessment"`
}

type executionSettings struct {
	Mode          string   `json:"mode"`
	AutoKinds     []string `json:"auto_kinds"`
	Metadata      bool     `json:"metadata"`
	Moves         bool     `json:"moves"`
	FolderRenames bool     `json:"folder_renames"`
}

type scanSettings struct {
	Scope     scanScope         `json:"scope"`
	Naming    namingRules       `json:"naming"`
	Evidence  evidenceSettings  `json:"evidence"`
	AI        aiSettings        `json:"ai"`
	Resources resourceSettings  `json:"resources"`
	Recovery  recoverySettings  `json:"recovery"`
	Cache     cacheSettings     `json:"cache"`
	Execution executionSettings `json:"execution"`
}

func defaultSettings() scanSettings {
	return scanSettings{
		Scope:     scanScope{Subfolders: true},
		Naming:    defaultRules,
		Evidence:  evidenceSettings{2, 4, 10000, true, false, "local_model", 2, 512, "sparse_or_ambiguous", "enabled", false},
		AI:        aiSettings{"configured", "", 300, 180, "unresolved", false},
		Resources: resourceSettings{3, 4, 1, 192, "below_normal"},
		Recovery:  recoverySettings{"propose_with_warning", 1, 1, 0},
		Cache:     cacheSettings{true, true},
		Execution: executionSettings{Mode: "review", AutoKinds: []string{"article", "academic_book", "book", "course_paper"}},
	}
}

type profiles struct {
	Revision uint64                  `json:"revision"`
	Active   string                  `json:"active"`
	Profiles map[string]scanSettings `json:"profiles"`
}

type settingSpec struct {
	Label  string   `json:"label"`
	Min    *int     `json:"min,omitempty"`
	Max    *int     `json:"max,omitempty"`
	Values []string `json:"values,omitempty"`
}

func rangeSpec(label string, min, max int) settingSpec {
	return settingSpec{Label: label, Min: &min, Max: &max}
}
func choiceSpec(label string, values ...string) settingSpec {
	return settingSpec{Label: label, Values: values}
}

var settingsSpecs = map[string]settingSpec{
	"scope.verify_comparison":      {Label: "Record file hashes for a controlled comparison"},
	"scope.subfolders":             {Label: "Include subfolders"},
	"scope.include_extensions":     {Label: "Include extensions"},
	"scope.exclude_extensions":     {Label: "Exclude extensions"},
	"scope.include_paths":          {Label: "Include paths relative to scan folder"},
	"scope.exclude_paths":          {Label: "Exclude paths relative to scan folder"},
	"scope.selected_paths":         {Label: "Scan only these relative file paths"},
	"scope.min_bytes":              rangeSpec("Minimum file size in bytes", 0, 1<<40),
	"scope.max_bytes":              rangeSpec("Maximum file size in bytes (0 means unlimited)", 0, 1<<40),
	"naming.normalize_spacing":     {Label: "Normalize spaces and separators"},
	"naming.replace_underscores":   {Label: "Replace spaces written as underscores"},
	"naming.normalize_uppercase":   {Label: "Normalize all-uppercase names"},
	"naming.prefer_specific_title": {Label: "Prefer a more specific title supported by the file"},
	"naming.spelling":              {Label: "Spelling corrections (JSON object)"},
	"naming.capitalization":        choiceSpec("Capitalization", "preserve", "readable", "sentence"),
	"naming.separator":             choiceSpec("Naming field separator", " - ", " – ", " _ "),
	"naming.max_length":            rangeSpec("Maximum filename length", 80, 255),
	"naming.author_format":         choiceSpec("Citation author format", "surname_et_al", "all_surnames", "full_names"),
	"naming.version_format":        choiceSpec("Course version format", "number", "v_prefix", "preserve_marker"),
	"naming.protected":             {Label: "Protected words and identifiers"},
	"evidence.pdf_pages":           rangeSpec("Initial PDF pages", 1, 12),
	"evidence.extra_pdf_pages":     rangeSpec("Additional PDF pages", 0, 12),
	"evidence.text_bytes":          rangeSpec("Text evidence limit in bytes", 512, 10000),
	"evidence.ocr_engine":          choiceSpec("PDF OCR engine", "local_model", "tesseract"),
	"evidence.ocr_unresolved":      {Label: "Also try OCR on unresolved PDFs with readable text"},
	"evidence.use_ocr":             {Label: "Use local OCR for scanned PDFs"},
	"evidence.ocr_pages":           rangeSpec("OCR pages", 1, 4),
	"evidence.image_edge":          rangeSpec("Maximum image edge in pixels", 256, 704),
	"evidence.deepen_on":           choiceSpec("Inspect more PDF pages", "never", "sparse", "sparse_or_ambiguous", "always"),
	"evidence.media_samples":       {Label: "Sample audio and video locally (requires media tools)"},
	"evidence.online_dates":        choiceSpec("Online search and publication date lookup", "enabled", "off"),
	"ai.model":                     choiceSpec("Local model", "configured", "qwen3.5-4b", "qwen2.5-vl-3b", "qwen3.5-9b", "off"),
	"ai.record_evidence":           {Label: "Save local model evidence for diagnostic export"},
	"ai.instructions":              {Label: "Additional local model instructions"},
	"ai.response_tokens":           rangeSpec("Maximum response tokens", 80, 512),
	"ai.timeout_seconds":           rangeSpec("Model request timeout in seconds", 10, 300),
	"ai.invoke":                    choiceSpec("When to use local AI", "unresolved", "all", "generic_names", "never"),
	"resources.threads":            rangeSpec("Model CPU threads", 1, 12),
	"resources.batch_threads":      rangeSpec("Model CPU threads for reading prompts", 1, 12),
	"resources.workers":            rangeSpec("Evidence extraction workers", 1, 4),
	"resources.decode_memory_mib":  rangeSpec("Image decoding budget per worker in MiB", 32, 512),
	"resources.priority":           choiceSpec("Model process priority", "normal", "below_normal", "idle"),
	"recovery.uncertainty":         choiceSpec("Handling uncertain names", "propose_with_warning", "flag_only", "keep"),
	"recovery.request_retries":     rangeSpec("Retries per model request", 0, 2),
	"recovery.process_restarts":    rangeSpec("Model restarts per scan after a process failure", 0, 2),
	"recovery.pause_after":         rangeSpec("Pause after consecutive AI failures (0 means continue)", 0, 20),
	"cache.reuse_evidence":         {Label: "Reuse saved evidence"},
	"cache.reuse_assessment":       {Label: "Reuse unchanged model assessments"},
	"execution.mode":               choiceSpec("File changes", "preview_only", "review", "automatic"),
	"execution.auto_kinds":         {Label: "Allowed automatic categories"},
	"execution.metadata":           {Label: "Propose metadata edits"},
	"execution.moves":              {Label: "Suggest nearby folder moves"},
	"execution.folder_renames":     {Label: "Propose folder renames"},
}

var settingsExtension = regexp.MustCompile(`^\.[A-Za-z0-9.]{1,16}$`)

func validateSettings(s scanSettings) error {
	s = withOnlineDefault(s)
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	var groups map[string]map[string]any
	if err = json.Unmarshal(data, &groups); err != nil {
		return err
	}
	for field, spec := range settingsSpecs {
		group, key, _ := strings.Cut(field, ".")
		value, _ := groups[group][key].(string)
		if len(spec.Values) != 0 && !slices.Contains(spec.Values, value) {
			return fmt.Errorf("%s has an unsupported value", spec.Label)
		}
		if spec.Min != nil {
			value, _ := groups[group][key].(float64)
			if value < float64(*spec.Min) || value > float64(*spec.Max) {
				return fmt.Errorf("%s must be between %d and %d", spec.Label, *spec.Min, *spec.Max)
			}
		}
	}
	if err := validateRules(s.Naming); err != nil {
		return err
	}
	if len(s.AI.Instructions) > 1200 {
		return errors.New("Use at most 1,200 bytes of additional model instructions")
	}
	if s.Scope.MaxBytes != 0 && s.Scope.MinBytes > s.Scope.MaxBytes {
		return errors.New("Minimum file size exceeds maximum file size")
	}
	for _, list := range [][]string{s.Scope.IncludePaths, s.Scope.ExcludePaths, s.Scope.SelectedPaths} {
		if len(list) > 500 {
			return errors.New("Use at most 500 paths per scope list")
		}
		for _, raw := range list {
			path := filepath.FromSlash(strings.ReplaceAll(raw, "\\", "/"))
			if path == "" || path == "." || filepath.IsAbs(path) || strings.Contains(raw, ":") || !filepath.IsLocal(path) || filepath.Clean(path) != path {
				return errors.New("Use clean, relative paths within the selected scan folder")
			}
		}
	}
	for _, list := range [][]string{s.Scope.IncludeExtensions, s.Scope.ExcludeExtensions} {
		if len(list) > 100 {
			return errors.New("Use at most 100 extensions per scope list")
		}
		for _, ext := range list {
			if !settingsExtension.MatchString(ext) {
				return errors.New("Enter extensions such as .pdf or .tar.gz")
			}
		}
	}
	for _, k := range s.Execution.AutoKinds {
		if !slices.Contains(classifications, k) {
			return errors.New("Automatic categories must be supported classification keys")
		}
	}
	return nil
}

func defaultProfiles() profiles {
	return profiles{Revision: 1, Active: "Default", Profiles: map[string]scanSettings{"Default": defaultSettings()}}
}

func readProfiles(home string) (profiles, error) {
	p := defaultProfiles()
	f, err := os.Open(filepath.Join(home, "settings-profiles.json"))
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	defer f.Close()
	var saved profiles
	if err := json.NewDecoder(io.LimitReader(f, 2<<20)).Decode(&saved); err != nil {
		return p, err
	}
	if saved.Revision == 0 || len(saved.Profiles) == 0 || len(saved.Profiles) > 20 {
		return p, errors.New("Invalid profiles file")
	}
	if _, ok := saved.Profiles[saved.Active]; !ok {
		return p, errors.New("Active profile is missing")
	}
	for name, settings := range saved.Profiles {
		settings = withOnlineDefault(settings)
		saved.Profiles[name] = settings
		if err := validateSettings(settings); err != nil {
			return p, err
		}
	}
	return saved, nil
}

func (a *application) activeSettings() scanSettings {
	if profile, ok := a.profiles.Profiles[a.profiles.Active]; ok {
		return profile
	}
	return defaultSettings()
}

type effectiveSettings struct {
	Revision   uint64                 `json:"revision"`
	Active     string                 `json:"active"`
	Profiles   []string               `json:"profiles"`
	Effective  scanSettings           `json:"effective"`
	Run        *scanSettings          `json:"run,omitempty"`
	Defaults   scanSettings           `json:"defaults"`
	Fields     map[string]settingSpec `json:"fields"`
	Model      string                 `json:"configured_model"`
	ModelReady bool                   `json:"model_ready"`
	Apply      string                 `json:"when_effective"`
}

func (a *application) settingsInfo(profile string) (effectiveSettings, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if profile == "" {
		profile = a.profiles.Active
	}
	settings, ok := a.profiles.Profiles[profile]
	if !ok {
		return effectiveSettings{}, errors.New("No profile with that name")
	}
	info := effectiveSettings{Revision: a.profiles.Revision, Active: a.profiles.Active, Effective: settings, Defaults: defaultSettings(), Fields: settingsSpecs,
		ModelReady: modelReadyFor(a.modelDir, settings.AI.Model), Apply: "Profile changes apply to new scans. Resume uses the saved settings. Reassessment uses current naming, evidence, AI, recovery and cache settings. Scope and execution permissions stay with the run. Changes to model resources take effect at the next model start."}
	info.Model, _, _ = modelProfileFor(settings.AI.Model)
	for name := range a.profiles.Profiles {
		info.Profiles = append(info.Profiles, name)
	}
	slices.Sort(info.Profiles)
	if a.s != nil {
		x := a.s.settings()
		info.Run = &x
	}
	return info, nil
}

var profileName = regexp.MustCompile(`^[\pL\pN][\pL\pN ._-]{0,39}$`)

func (a *application) saveProfile(revision uint64, name string, settings *scanSettings, activate bool) error {
	if !profileName.MatchString(name) || strings.TrimSpace(name) != name {
		return errors.New("Use a profile name of 1 to 40 letters, numbers, spaces or ._-")
	}
	if settings != nil {
		if err := validateSettings(*settings); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveProfileLocked(revision, name, settings, activate)
}

func (a *application) saveProfileLocked(revision uint64, name string, settings *scanSettings, activate bool) error {
	if a.v.Busy || a.v.Closing || revision != a.profiles.Revision {
		return errors.New("The app is busy or the profiles changed. Read settings again.")
	}
	next := profiles{Revision: revision + 1, Active: a.profiles.Active, Profiles: make(map[string]scanSettings, len(a.profiles.Profiles)+1)}
	for key, value := range a.profiles.Profiles {
		next.Profiles[key] = value
	}
	if settings != nil {
		next.Profiles[name] = withOnlineDefault(*settings)
	}
	if len(next.Profiles) > 20 {
		return errors.New("Use at most 20 profiles")
	}
	if _, exists := next.Profiles[name]; !exists {
		return errors.New("No profile with that name")
	}
	if activate {
		next.Active = name
	}
	if err := atomicJSON(filepath.Join(a.home, "settings-profiles.json"), next); err != nil {
		return err
	}
	a.profiles = next
	return nil
}

func (s *session) settings() scanSettings {
	if s.manifest.Settings != nil {
		return withOnlineDefault(*s.manifest.Settings)
	}
	opts := defaultSettings()
	if s.manifest.Rules != nil {
		opts.Naming = *s.manifest.Rules
	}
	if s.manifest.Auto {
		opts.Execution.Mode = "automatic"
	}
	opts.Execution.FolderRenames, opts.Execution.Metadata, opts.Execution.Moves = s.manifest.RenameFolders, s.manifest.RepairMetadata, s.manifest.SortFolders
	return opts
}

func withOnlineDefault(s scanSettings) scanSettings {
	if s.Evidence.OCREngine == "" {
		s.Evidence.OCREngine = "local_model"
	}
	if s.Evidence.OnlineDates == "" {
		s.Evidence.OnlineDates = "enabled"
	}
	return s
}

// patchSettings replaces only the named fields, leaving all other profile values intact.
func patchSettings(settings scanSettings, changes map[string]json.RawMessage) (scanSettings, error) {
	data, err := json.Marshal(settings)
	if err != nil {
		return settings, err
	}
	var tree map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &tree); err != nil {
		return settings, err
	}
	for field, value := range changes {
		if _, known := settingsSpecs[field]; !known {
			return settings, fmt.Errorf("Unknown setting %q", field)
		}
		if string(value) == "null" {
			return settings, fmt.Errorf("Supply a value for %s or use reset", field)
		}
		group, key, _ := strings.Cut(field, ".")
		tree[group][key] = value
	}
	data, err = json.Marshal(tree)
	if err != nil {
		return settings, err
	}
	var updated scanSettings
	if err := json.Unmarshal(data, &updated); err != nil {
		return settings, err
	}
	return updated, validateSettings(updated)
}

func settingValue(settings scanSettings, field string) (json.RawMessage, error) {
	data, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	var tree map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, err
	}
	group, key, _ := strings.Cut(field, ".")
	return tree[group][key], nil
}

func (a *application) patchProfile(revision uint64, name string, changes map[string]json.RawMessage, reset bool) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy || a.v.Closing {
		return a.profiles.Revision, errors.New("The app is busy")
	}
	if revision != a.profiles.Revision {
		return a.profiles.Revision, errors.New("The settings changed. Read settings again")
	}
	if name == "" {
		name = a.profiles.Active
	}
	settings, found := a.profiles.Profiles[name]
	if !found {
		return a.profiles.Revision, errors.New("No profile with that name")
	}
	if reset {
		settings = defaultSettings()
	}
	settings, err := patchSettings(settings, changes)
	if err != nil {
		return a.profiles.Revision, err
	}
	err = a.saveProfileLocked(revision, name, &settings, false)
	return a.profiles.Revision, err
}

// Section: Application naming settings access and updates.

func (a *application) getRules() ruleSet {
	a.mu.Lock()
	defer a.mu.Unlock()
	return ruleSet{Revision: a.profiles.Revision, Rules: a.activeSettings().Naming, Formats: namingFormats}
}

func (a *application) updateRules(revision uint64, r namingRules) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	settings := a.activeSettings()
	if r.Capitalization == "" {
		r.Capitalization = settings.Naming.Capitalization
	}
	if r.Separator == "" {
		r.Separator = settings.Naming.Separator
	}
	if r.MaxLength == 0 {
		r.MaxLength = settings.Naming.MaxLength
	}
	if r.AuthorFormat == "" {
		r.AuthorFormat = settings.Naming.AuthorFormat
	}
	if r.VersionFormat == "" {
		r.VersionFormat = settings.Naming.VersionFormat
	}
	settings.Naming = r
	if err := validateSettings(settings); err != nil {
		return err
	}
	return a.saveProfileLocked(revision, a.profiles.Active, &settings, true)
}
