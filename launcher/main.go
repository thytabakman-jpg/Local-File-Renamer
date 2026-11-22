package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"localrenamer/internal/mcplaunch"
)

func main() {
	if err := launch(); err != nil {
		var child *exec.ExitError
		if errors.As(err, &child) {
			os.Exit(child.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "Local File Renamer MCP:", err)
		os.Exit(1)
	}
}

func launch() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	app, err := mcplaunch.Current(filepath.Dir(self))
	if err != nil {
		return err
	}
	cmd := exec.Command(app, "--mcp")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
