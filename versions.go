package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

func versionKey(name string) string {
	stem := baseName(name)
	paper := parsePaperName(stem)
	course, title := paper.course, norm(paper.title)
	if course == "" || title == "" || photoExtension(name) {
		return ""
	}
	return course + "|" + title
}

func groupVersionCopies(items []entry) map[string][]int {
	groups := make(map[string][]int)
	for i, item := range items {
		if item.Skip || item.Folder {
			continue
		}
		key := versionKey(filepath.Base(item.Path))
		if key != "" {
			groups[key] = append(groups[key], i)
		}
	}
	return groups
}

func compareVersionNumbers(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(left), len(right)); i++ {
		var x, y int
		if i < len(left) {
			x, _ = strconv.Atoi(left[i])
		}
		if i < len(right) {
			y, _ = strconv.Atoi(right[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

func (s *session) compareVersionCopies(index int, proposed string) string {
	items := s.manifest.Items
	current := items[index]
	key := versionKey(filepath.Base(current.Path))
	if key == "" {
		key = versionKey(proposed)
	}
	siblings := s.versionGroups[key]
	type copyVersion struct {
		version, path string
		modified      int64
	}
	var copies []copyVersion
	for _, i := range siblings {
		if i == index {
			continue
		}
		item := items[i]
		version, known := readVersion(baseName(filepath.Base(item.Path)))
		if !known {
			version = "?"
		}
		path, err := filepath.Rel(s.manifest.Root, item.Path)
		if err != nil {
			path = item.Path
		}
		copies = append(copies, copyVersion{version, path, item.Modified})
	}
	if len(copies) == 0 {
		return "No matching title and course in this scan"
	}
	slices.SortStableFunc(copies, func(a, b copyVersion) int {
		if cmp := compareVersionNumbers(b.version, a.version); cmp != 0 {
			return cmp
		}
		if a.modified > b.modified {
			return -1
		}
		if a.modified < b.modified {
			return 1
		}
		return strings.Compare(a.path, b.path)
	})
	modified := func(t int64) string {
		if t == 0 {
			return "modification time unavailable"
		}
		return "modified " + time.Unix(0, t).Local().Format("2006-01-02 15:04:05 -07:00")
	}
	var rows []string
	for _, copy := range copies {
		rows = append(rows, fmt.Sprintf("%s (version %s, %s)", copy.path, copy.version, modified(copy.modified)))
	}
	return fmt.Sprintf("This file: %s. Matching title and course, highest numbered first: %s", modified(current.Modified), strings.Join(rows, " | "))
}
