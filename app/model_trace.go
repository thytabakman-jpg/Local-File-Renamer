package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type captureKey struct{}

type completionTiming struct {
	Prompt     float64 `json:"prompt_ms"`
	Generation float64 `json:"predicted_ms"`
}

type completionCapture struct {
	RecordEvidence         bool `json:"-"`
	PromptMS, GenerationMS int64
	Attempts               []completionAttempt `json:",omitempty"`
}

type completionAttempt struct {
	System, Text  string
	ImageAttached bool            `json:",omitempty"`
	Response      json.RawMessage `json:",omitempty"`
	Error         string          `json:",omitempty"`
}

func (c *completionCapture) add(timing completionTiming, system, text string, image bool, content string, err error) {
	c.PromptMS += int64(timing.Prompt + 0.5)
	c.GenerationMS += int64(timing.Generation + 0.5)
	if !c.RecordEvidence {
		return
	}
	row := completionAttempt{System: system, Text: text, ImageAttached: image}
	if json.Valid([]byte(content)) {
		row.Response = json.RawMessage(content)
	}
	if err != nil {
		row.Error = err.Error()
	}
	c.Attempts = append(c.Attempts, row)
}

func modelTracePath(run string, index int) string {
	return filepath.Join(run, "model-evidence", fmt.Sprintf("%d.json", index))
}

func (a *application) modelTrace(job string, index int) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if job == "" {
		return nil, errors.New("Choose a scan result")
	}
	s, owned, err := a.bridgeRunLocked(job, true)
	if err != nil {
		return nil, err
	}
	if owned {
		defer s.close()
	}
	if index < 0 || index >= len(s.plans) || !s.plans[index].Inspection.ModelTrace {
		return nil, errors.New("No model evidence was saved for this assessment")
	}
	return os.ReadFile(modelTracePath(s.dir, index))
}
