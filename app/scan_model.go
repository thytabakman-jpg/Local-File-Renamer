package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type scanModel struct {
	app                  *application
	session              *session
	model                *localModel
	startupMS, ocrLoadMS int64
	restarts             int
	settings             scanSettings
	vision               bool
	holdsSlot            bool
	unavailable          [2]error
	capture              completionCapture
}

func (m *scanModel) call(ctx context.Context, vision bool, run func(*localModel) error) error {
	if !m.holdsSlot {
		select {
		case m.app.chat.slot <- struct{}{}:
			m.holdsSlot = true
			m.app.chat.closeModel()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	slot := 0
	if vision {
		slot = 1
	}
	if m.unavailable[slot] != nil {
		return m.unavailable[slot]
	}
	if vision && m.model != nil && !m.vision {
		m.close()
	}
	for {
		if m.model == nil {
			started := time.Now()
			var err error
			m.model, err = startModelWith(ctx, m.app.backend, m.app.modelDir, filepath.Join(m.session.dir, "model.log"), m.settings, vision)
			m.startupMS += elapsedMS(started)
			m.vision = vision
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					m.unavailable[slot] = err
				}
				return err
			}
		}
		err := run(m.model)
		if !hasModelFailure(err, modelProcess) {
			return err
		}
		m.close()
		if m.restarts >= m.settings.Recovery.ProcessRestarts || ctx.Err() != nil {
			m.unavailable[slot] = err
			return err
		}
		m.restarts++
	}
}

func (m *scanModel) close() {
	m.model.Close()
	m.model = nil
}
func (m *scanModel) end() {
	m.close()
	if m.holdsSlot {
		<-m.app.chat.slot
		m.holdsSlot = false
	}
}
func (m *scanModel) handlers(partial bool) (assessFile, transcribePage) {
	if partial || m.settings.AI.Model == "off" || m.settings.AI.Invoke == "never" {
		return nil, nil
	}
	return func(ctx context.Context, name string, e evidence) (a assessment, err error) {
			err = m.call(ctx, len(e.Photo) != 0, func(model *localModel) error {
				assess := model.assess
				if m.session.manifest.Operation == scanMove {
					assess = model.assessTopic
				}
				m.capture.RecordEvidence = m.settings.AI.RecordEvidence
				ctx = context.WithValue(ctx, captureKey{}, &m.capture)
				a, err = assess(ctx, name, e)
				return err
			})
			return a, err
		}, func(ctx context.Context, image []byte) (text string, err error) {
			before := m.startupMS
			err = m.call(ctx, true, func(model *localModel) error { text, err = model.transcribe(ctx, image); return err })
			m.ocrLoadMS += m.startupMS - before
			return text, err
		}
}
func (m *scanModel) timings(i *inspection, partial bool, index int) {
	if !partial {
		i.Model, _, _ = modelProfileFor(m.settings.AI.Model)
	}
	i.StartupMS = m.startupMS
	i.OCRMS = max(0, i.OCRMS-m.ocrLoadMS)
	i.ModelMS = max(0, i.ModelMS-(m.startupMS-m.ocrLoadMS))
	i.PromptMS, i.GenerationMS = m.capture.PromptMS, m.capture.GenerationMS
	if len(m.capture.Attempts) != 0 {
		path := modelTracePath(m.session.dir, index)
		err := os.MkdirAll(filepath.Dir(path), 0700)
		if err == nil {
			err = atomicJSON(path, m.capture)
		}
		i.ModelTrace = err == nil
		if err != nil {
			i.Detail = joinNote(i.Detail, "Could not save the local model evidence")
		}
	}
	m.capture = completionCapture{}
	m.startupMS, m.ocrLoadMS = 0, 0
}
