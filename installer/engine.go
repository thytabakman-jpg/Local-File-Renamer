package main

import (
	"context"
	"debug/pe"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type engineProbe struct {
	err      error
	exitCode uint32
	timedOut bool
	output   string
}

func probeEngine(exe string, timeout time.Duration, configure func(*exec.Cmd)) engineProbe {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	if configure != nil {
		configure(cmd)
	}
	return runEngineProbe(ctx, cmd, filepath.Dir(exe))
}

type diagnosticBuffer struct{ data []byte }

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	const limit = 8192
	if remaining := limit - len(b.data); remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		b.data = append(b.data, p[:remaining]...)
	}
	return len(p), nil
}

func runEngineProbe(ctx context.Context, cmd *exec.Cmd, dir string) engineProbe {
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	var output diagnosticBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	p := engineProbe{err: err, timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), output: strings.TrimSpace(string(output.data))}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		p.exitCode = uint32(exit.ExitCode())
	}
	return p
}

func (p engineProbe) diagnostic() string {
	if p.timedOut {
		return "engine startup check timed out"
	}
	if p.err == nil {
		return ""
	}
	if p.output == "" {
		return fmt.Sprintf("%v (exit code 0x%08x)", p.err, p.exitCode)
	}
	return fmt.Sprintf("%v (exit code 0x%08x): %s", p.err, p.exitCode, p.output)
}

// Only diagnose a Microsoft runtime candidate when Windows reports a missing
// DLL and inspection finds a relevant import absent from standard locations.
func missingMicrosoftRuntime(engineDir string, p engineProbe) []string {
	if p.err == nil || p.timedOut || p.exitCode != 0xc0000135 {
		return nil
	}
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return nil
	}
	entries, err := os.ReadDir(engineDir)
	if err != nil {
		return nil
	}
	found := map[string]bool{}
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if entry.IsDir() || (!strings.HasSuffix(name, ".dll") && !strings.HasSuffix(name, ".exe")) {
			continue
		}
		binary, err := pe.Open(filepath.Join(engineDir, entry.Name()))
		if err != nil {
			continue
		}
		symbols, err := binary.ImportedSymbols()
		binary.Close()
		if err != nil {
			continue
		}
		for _, symbol := range symbols {
			_, library, ok := strings.Cut(symbol, ":")
			library = strings.ToLower(library)
			if !ok || !isMicrosoftRuntimeDLL(library) {
				continue
			}
			if absentFromStandardLocations(library, engineDir, filepath.Join(systemRoot, "System32")) {
				found[library] = true
			}
		}
	}
	var missing []string
	for library := range found {
		missing = append(missing, library)
	}
	sort.Strings(missing)
	return missing
}

func isMicrosoftRuntimeDLL(name string) bool {
	return filepath.Base(name) == name && strings.HasSuffix(name, ".dll") &&
		(strings.HasPrefix(name, "vcruntime140") || strings.HasPrefix(name, "msvcp140") || strings.HasPrefix(name, "concrt140"))
}

func absentFromStandardLocations(name string, dirs ...string) bool {
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			return false
		}
	}
	return true
}
