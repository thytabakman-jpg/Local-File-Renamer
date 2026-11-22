package main

import (
	"errors"
	"fmt"
	"strings"
)

type scanStartOptions struct {
	ScopeMode  string
	Subfolders *bool
	Fresh      bool
	RequestID  string
	JobID      string
	FallbackAI bool
}

func (start scanStartOptions) configure(opts *scanSettings, include []string) error {
	switch start.ScopeMode {
	case "":
		if len(include) > 0 {
			opts.Scope.IncludeExtensions = include
		}
	case "all", "selected":
		opts.Scope = scanScope{Subfolders: opts.Scope.Subfolders, VerifyComparison: opts.Scope.VerifyComparison}
		if start.ScopeMode == "selected" {
			if len(include) == 0 {
				return errors.New("Select a file type or choose All file types")
			}
			opts.Scope.IncludeExtensions = include
		}
	case "profile":
	default:
		return errors.New("Choose All file types, Selected file types, or Use profile scope")
	}
	if start.Subfolders != nil {
		opts.Scope.Subfolders = *start.Subfolders
	}
	if start.Fresh {
		opts.Cache = cacheSettings{}
	}
	return nil
}

type inventoryCounts struct {
	Found, Included, Filtered, Folders, Unavailable, OmittedFolders int
	FileTypes                                                       []string `json:",omitempty"`
}

func (c inventoryCounts) summary() string {
	line := fmt.Sprintf("%d files found, %d included, %d filtered out, %d subfolders", c.Found, c.Included, c.Filtered, c.Folders)
	if c.Unavailable != 0 {
		line += fmt.Sprintf(", %d unavailable entries", c.Unavailable)
	}
	if c.OmittedFolders != 0 {
		line += fmt.Sprintf(", %d subfolders outside scope", c.OmittedFolders)
	}
	return line
}

func (scope scanScope) description() string {
	parts := []string{"All file types"}
	if len(scope.IncludeExtensions) > 0 {
		parts[0] = "Included extensions: " + strings.Join(scope.IncludeExtensions, ", ")
	}
	if len(scope.ExcludeExtensions) > 0 {
		parts = append(parts, "Excluded extensions: "+strings.Join(scope.ExcludeExtensions, ", "))
	}
	if scope.Subfolders {
		parts = append(parts, "Subfolders included")
	} else {
		parts = append(parts, "Subfolders excluded")
	}
	if len(scope.IncludePaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d included paths", len(scope.IncludePaths)))
	}
	if len(scope.ExcludePaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d excluded paths", len(scope.ExcludePaths)))
	}
	if len(scope.SelectedPaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d individually selected paths", len(scope.SelectedPaths)))
	}
	if scope.MinBytes > 0 {
		parts = append(parts, fmt.Sprintf("Minimum size: %d bytes", scope.MinBytes))
	}
	if scope.MaxBytes > 0 {
		parts = append(parts, fmt.Sprintf("Maximum size: %d bytes", scope.MaxBytes))
	}
	return strings.Join(parts, " · ")
}
