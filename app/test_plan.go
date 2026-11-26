package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Section: Experiment plan, variant and result contracts.

type testPlan struct {
	Success    string            `json:"success,omitempty"`
	Name       string            `json:"name"`
	Root       string            `json:"root"`
	Scope      *scanScope        `json:"scope,omitempty"`
	Variants   []testVariant     `json:"variants"`
	Expected   []testExpectation `json:"expected,omitempty"`
	KeeperRule testKeeperRule    `json:"keeper_rule,omitempty"`
	Interpret  bool              `json:"interpret,omitempty"`
}

type testVariant struct {
	BaselineJob string              `json:"baseline_job,omitempty"`
	Name        string              `json:"name"`
	Operation   scanOperation       `json:"operation"`
	From        string              `json:"from,omitempty"`
	Select      string              `json:"select,omitempty"`
	Settings    []chatSettingChange `json:"settings,omitempty"`
	Apply       bool                `json:"apply_on_copies,omitempty"`
	Criteria    testCriteria        `json:"criteria,omitzero"`
}

type testCriteria struct {
	MinCorrect   *int   `json:"min_correct,omitempty"`
	MaxWrong     *int   `json:"max_wrong,omitempty"`
	MaxDeferred  *int   `json:"max_deferred,omitempty"`
	MaxElapsedMS *int64 `json:"max_elapsed_ms,omitempty"`
}

type testExpectation struct {
	File   string  `json:"file"`
	Name   *string `json:"name,omitempty"`
	Folder *string `json:"folder,omitempty"`
	Keeper *bool   `json:"keeper,omitempty"`
}

type testKeeperRule struct {
	Mode   string `json:"mode,omitempty"`
	Folder string `json:"folder,omitempty"`
}

type testReceipt struct {
	Job       string `json:"job"`
	Status    string `json:"status"`
	Completed int    `json:"completed"`
	Variants  int    `json:"variants"`
	Headline  string `json:"headline,omitempty"`
	Report    string `json:"report,omitempty"`
}

type savedTest struct {
	testReceipt
	Plan       testPlan       `json:"plan"`
	Settings   []scanSettings `json:"settings"`
	Created    time.Time      `json:"created"`
	Version    string         `json:"version"`
	Findings   []testFinding  `json:"findings,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	InputError string         `json:"input_error,omitempty"`
	InputFiles int            `json:"input_files"`
	InputHash  string         `json:"input_sha256,omitempty"`
}

// Section: Plan identity and path validation.

var testIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,100}$`)

func cleanTestPath(value string, folder bool) (string, error) {
	if folder && value == "." {
		return ".", nil
	}
	path := filepath.FromSlash(strings.ReplaceAll(value, "\\", "/"))
	if path == "" || !filepath.IsLocal(path) || filepath.Clean(path) != path || path == "." ||
		strings.ContainsAny(path, ":\r\n\x00") {
		return "", fmt.Errorf("Use a path relative to the test folder: %q", value)
	}
	return filepath.ToSlash(path), nil
}

// Section: Experiment admission and settings validation.

func (a *application) validateTestPlan(plan testPlan, baseline scanSettings) (testPlan, []scanSettings, error) {
	plan.Name = strings.TrimSpace(plan.Name)
	if plan.Name == "" || len(plan.Name) > 160 || len(plan.Variants) == 0 || len(plan.Variants) > 16 {
		return plan, nil, errors.New("Give the test a name and between one and sixteen variants")
	}
	if !filepath.IsAbs(plan.Root) {
		return plan, nil, errors.New("Choose an absolute path for the test folder")
	}
	plan.Root = filepath.Clean(plan.Root)
	for _, location := range []string{a.home, a.backend, a.modelDir} {
		if location != "" && (within(plan.Root, location) || within(location, plan.Root)) {
			return plan, nil, errors.New("Choose a test folder separate from the app and its data")
		}
	}
	if err := validateRoot(plan.Root); err != nil {
		return plan, nil, err
	}

	if plan.Scope != nil {
		baseline.Scope = *plan.Scope
	}
	baseline.Scope.VerifyComparison = false
	baseline.Cache = cacheSettings{}
	// Public citation lookups are opt-in for individual test variants.
	baseline.Evidence.OnlineDates = "off"
	if err := validateSettings(baseline); err != nil {
		return plan, nil, err
	}
	plan.Scope = &baseline.Scope

	names := make(map[string]int)
	settings := make([]scanSettings, 0, len(plan.Variants))
	for i, v := range plan.Variants {
		v.Name = strings.TrimSpace(v.Name)
		key := strings.ToLower(v.Name)
		if _, exists := names[key]; exists || v.Name == "" || len(v.Name) > 80 || v.Operation == "" || !v.Operation.valid() {
			return plan, nil, fmt.Errorf("Variant %d needs a unique name and a supported workflow", i+1)
		}
		v.From = strings.TrimSpace(v.From)
		if v.Select == "" {
			v.Select = "all"
		}
		if !slices.Contains([]string{"all", "unresolved", "wrong", "changed"}, v.Select) {
			return plan, nil, fmt.Errorf("%s has an unsupported file selection", v.Name)
		}
		if c := v.Criteria; c.MinCorrect != nil && *c.MinCorrect < 0 || c.MaxWrong != nil && *c.MaxWrong < 0 ||
			c.MaxDeferred != nil && *c.MaxDeferred < 0 || c.MaxElapsedMS != nil && *c.MaxElapsedMS < 0 {
			return plan, nil, fmt.Errorf("%s: use nonnegative success thresholds", v.Name)
		}
		opts := baseline
		if v.From != "" {
			prior, exists := names[strings.ToLower(v.From)]
			if !exists {
				return plan, nil, fmt.Errorf("%s must refer to an earlier variant", v.Name)
			}
			v.From = plan.Variants[prior].Name
			opts = settings[prior]
			// Workflows can inherit the analysis settings without inheriting
			// another workflow's execution rules.
			opts.Execution = baseline.Execution
			opts.Scope = baseline.Scope
			if v.Operation == scanDuplicates && v.Select != "all" {
				return plan, nil, fmt.Errorf("%s must compare the entire scoped set for duplicates", v.Name)
			}
		} else if v.Select != "all" {
			return plan, nil, fmt.Errorf("%s needs an earlier variant to select files", v.Name)
		}
		for _, change := range v.Settings {
			if strings.HasPrefix(change.Key, "scope.") || change.Key == "execution.mode" {
				return plan, nil, fmt.Errorf("%s: use the common scope and the variant's apply-on-copies option", v.Name)
			}
		}
		if v.BaselineJob != "" {
			if v.Operation != scanRename || !testIDPattern.MatchString(v.BaselineJob) || v.Apply || v.From != "" ||
				v.Select != "all" || len(v.Settings) != 0 {
				return plan, nil, fmt.Errorf("%s: a saved baseline must be a complete Rename scan with comparison hashes", v.Name)
			}
			saved, err := openSessionReadOnly(filepath.Join(a.home, "runs", v.BaselineJob))
			if err != nil {
				return plan, nil, fmt.Errorf("%s: the saved baseline run is unavailable", v.Name)
			}
			opts = saved.settings()
			saved.close()
		}
		if err := applyChatSettings(&opts, v.Settings); err != nil {
			return plan, nil, fmt.Errorf("%s: %w", v.Name, err)
		}
		if v.BaselineJob == "" {
			opts.Execution.Mode = "preview_only"
		}
		if v.Apply {
			opts.Execution.Mode = "review"
		}
		switch v.Operation {
		case scanRename:
			opts.Execution.Moves = false
		case scanMove, scanDuplicates:
			opts.Execution.FolderRenames, opts.Execution.Metadata = false, false
			opts.Execution.Moves = v.Operation == scanMove
			opts.Evidence.OnlineDates = "off"
		}
		if err := validateSettings(opts); err != nil {
			return plan, nil, fmt.Errorf("%s: %w", v.Name, err)
		}
		settings = append(settings, opts)
		names[key] = i
		plan.Variants[i] = v
	}

	files := make(map[string]bool)
	for i, expected := range plan.Expected {
		path, err := cleanTestPath(expected.File, false)
		if err != nil {
			return plan, nil, err
		}
		key := testPathKey(path)
		if files[key] || expected.Name == nil && expected.Folder == nil && expected.Keeper == nil {
			return plan, nil, fmt.Errorf("Give %s one reference entry with an expected name, folder or keeper decision", path)
		}
		files[key] = true
		expected.File = path
		if expected.Name != nil {
			if _, err = cleanTestPath(*expected.Name, false); err != nil || strings.ContainsAny(*expected.Name, "/\\") {
				return plan, nil, fmt.Errorf("Give %s an expected basename including its extension", path)
			}
		}
		if expected.Folder != nil {
			folder, err := cleanTestPath(*expected.Folder, true)
			if err != nil {
				return plan, nil, err
			}
			expected.Folder = &folder
		}
		plan.Expected[i] = expected
	}
	switch plan.KeeperRule.Mode {
	case "", "newest", "shortest_name", "no_copy_suffix":
	case "prefer_folder":
		folder, err := cleanTestPath(plan.KeeperRule.Folder, true)
		if err != nil {
			return plan, nil, err
		}
		plan.KeeperRule.Folder = folder
	default:
		return plan, nil, errors.New("Choose one keeper rule: prefer_folder, newest, shortest_name or no_copy_suffix")
	}
	return plan, settings, nil
}

// Section: Experiment persistence.

func (a *application) saveTestPlan(plan testPlan) (savedTest, error) {
	a.mu.Lock()
	baseline, closing := a.activeSettings(), a.v.Closing
	a.mu.Unlock()
	if closing {
		return savedTest{}, errors.New("The app is closing")
	}
	plan, settings, err := a.validateTestPlan(plan, baseline)
	if err != nil {
		return savedTest{}, err
	}
	return a.saveNewTest(plan, settings)
}

func (a *application) saveNewTest(plan testPlan, settings []scanSettings) (savedTest, error) {
	dir := filepath.Join(a.home, "tests")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return savedTest{}, err
	}
	created := time.Now().UTC()
	dir, err := os.MkdirTemp(dir, created.Format("20060102T150405.000000000")+"-")
	if err != nil {
		return savedTest{}, err
	}
	job := savedTest{
		testReceipt: testReceipt{Job: filepath.Base(dir), Status: "draft", Variants: len(plan.Variants)},
		Plan:        plan, Settings: settings, Created: created, Version: appVersion,
	}
	if err := a.writeTest(job); err != nil {
		_ = os.Remove(dir)
		return savedTest{}, err
	}
	return job, nil
}

func (a *application) writeTest(job savedTest) error {
	return atomicJSON(filepath.Join(a.home, "tests", job.Job, "test.json"), job)
}

func (a *application) readTest(id string) (savedTest, error) {
	if !testIDPattern.MatchString(id) {
		return savedTest{}, errors.New("Specify a saved test job ID")
	}
	data, err := os.ReadFile(filepath.Join(a.home, "tests", id, "test.json"))
	if err != nil {
		return savedTest{}, errors.New("No saved test with that ID")
	}
	var job savedTest
	if err := json.Unmarshal(data, &job); err != nil || job.Job != id || len(job.Settings) != len(job.Plan.Variants) {
		return savedTest{}, errors.New("This test record could not be read")
	}
	return job, nil
}

