package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEngineProbeHelper(t *testing.T) {
	switch os.Getenv("RENAMER_ENGINE_TEST_HELPER") {
	case "success":
		fmt.Print("engine ready")
		os.Exit(0)
	case "failure":
		fmt.Fprint(os.Stderr, "engine failed")
		os.Exit(7)
	case "stall":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func TestEngineProbeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		mode string
		timeout time.Duration
	}{
		{"success", 5*time.Second},
		{"failure", 5*time.Second},
		{"stall", 100*time.Millisecond},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEngineProbeHelper$")
			cmd.Env = append(os.Environ(), "RENAMER_ENGINE_TEST_HELPER="+tc.mode)
			p := runEngineProbe(ctx, cmd, t.TempDir())
			switch tc.mode {
			case "success":
				if p.err != nil || !strings.Contains(p.output, "engine ready") { t.Fatalf("probe: %+v", p) }
			case "failure":
				if p.err == nil || p.exitCode != 7 || p.timedOut || !strings.Contains(p.diagnostic(), "engine failed") { t.Fatalf("probe: %+v", p) }
			case "stall":
				if p.err == nil || !p.timedOut || !strings.Contains(p.diagnostic(), "timed out") { t.Fatalf("probe: %+v", p) }
			}
		})
	}
}

func TestDiagnosticBufferBound(t *testing.T) {
	var b diagnosticBuffer
	p := []byte(strings.Repeat("x", 10000))
	if n, err := b.Write(p); n != len(p) || err != nil || len(b.data) != 8192 { t.Fatalf("write %d, %v, buffer %d", n, err, len(b.data)) }
}

func TestRuntimeDiagnosisRequiresEvidence(t *testing.T) {
	dir := t.TempDir()
	for _, probe := range []engineProbe{
		{},
		{err: errors.New("timeout"), timedOut: true, exitCode: 0xc0000135},
		{err: errors.New("unrelated failure"), exitCode: 7},
		{err: errors.New("missing DLL"), exitCode: 0xc0000135},
	} {
		if missing := missingMicrosoftRuntime(dir, probe); len(missing) != 0 { t.Fatalf("unsupported diagnosis: %v", missing) }
	}
	if !isMicrosoftRuntimeDLL("vcruntime140_1.dll") || !isMicrosoftRuntimeDLL("msvcp140.dll") || isMicrosoftRuntimeDLL("ggml.dll") { t.Fatal("runtime name classification") }
	if !absentFromStandardLocations("msvcp140.dll", dir) { t.Fatal("expected missing file") }
	if err := os.WriteFile(filepath.Join(dir, "msvcp140.dll"), []byte("fixture"), 0600); err != nil { t.Fatal(err) }
	if absentFromStandardLocations("msvcp140.dll", dir) { t.Fatal("present file classified missing") }
}
