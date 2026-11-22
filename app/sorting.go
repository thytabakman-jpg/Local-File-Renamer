package main

import (
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const folderDistance = 2
const maxFolderSamples = 40

type folderProfile struct {
	name    []string
	titles  map[string]int
	samples int
}

type folderIndex struct {
	root     string
	folders  map[string]*folderProfile
	children map[string][]string
	nearby   map[string][]string
	allPaths []string
	topics   map[string]int
}

func topicWords(s string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, word := range strings.Fields(norm(s)) {
		if len([]rune(word)) < 3 || strings.IndexFunc(word, unicode.IsLetter) == -1 || seen[word] {
			continue
		}
		switch word {
		case "and", "the", "for", "from", "with", "about", "folder", "folders", "files", "downloads",
			"download", "document", "documents", "misc", "other", "untitled", "unknown", "draft", "final",
			"version", "file", "general", "archive", "backup", "copy", "notes":
			continue
		}
		seen[word] = true
		out = append(out, word)
	}
	return out
}

func indexFolders(root string, items []entry) *folderIndex {
	f := &folderIndex{root: root, folders: make(map[string]*folderProfile), children: make(map[string][]string), nearby: make(map[string][]string)}
	f.folders[root] = &folderProfile{name: topicWords(filepath.Base(root)), titles: make(map[string]int)}
	for _, item := range items {
		if !item.Folder || item.Skip || !within(root, item.Path) || item.Path == root {
			continue
		}
		f.folders[item.Path] = &folderProfile{name: topicWords(filepath.Base(item.Path)), titles: make(map[string]int)}
		parent := filepath.Dir(item.Path)
		f.children[parent] = append(f.children[parent], item.Path)
	}
	for _, item := range items {
		if item.Folder || item.Skip {
			continue
		}
		profile := f.folders[filepath.Dir(item.Path)]
		if profile == nil || profile.samples >= maxFolderSamples {
			continue
		}
		name := baseName(filepath.Base(item.Path))
		if meaningful(name) {
			profile.samples++
			for _, token := range topicWords(name) {
				profile.titles[token]++
			}
		}
	}
	return f
}

func (f *folderIndex) candidates(dir string) []string {
	if f.allPaths != nil {
		return f.allPaths
	}
	if paths, ok := f.nearby[dir]; ok {
		return paths
	}
	seen := map[string]bool{dir: true}
	front := []string{dir}
	var candidates []string
	for level := 0; level < folderDistance && len(front) > 0; level++ {
		var next []string
		add := func(path string) {
			if !seen[path] && f.folders[path] != nil {
				seen[path] = true
				next = append(next, path)
			}
		}
		for _, path := range front {
			if path != f.root {
				add(filepath.Dir(path))
			}
			for _, child := range f.children[path] {
				add(child)
			}
		}
		candidates = append(candidates, next...)
		front = next
	}
	sort.Strings(candidates)
	f.nearby[dir] = candidates
	return candidates
}

func topicFromEvidence(e evidence, a assessment, usedAI bool) []string {
	var sources []string
	for _, title := range []string{e.Title, a.Title, a.Label} {
		if meaningful(title) {
			sources = append(sources, chars(title, 240))
		}
	}
	if len(sources) == 0 || !usedAI {
		for _, line := range textLines(chars(e.Text, 650)) {
			if meaningful(line) && !titleHeading.MatchString(line) {
				sources = append(sources, chars(line, 180))
			}
			if len(sources) >= 3 {
				break
			}
		}
	}
	words := topicWords(strings.Join(sources, " "))
	return words[:min(24, len(words))]
}

func (p *folderProfile) score(words map[string]bool, excluded map[string]bool) int {
	if p == nil {
		return 0
	}
	match := 0
	for _, word := range p.name {
		if words[word] {
			match++
		}
	}
	score := 0
	if match > 0 && match == len(p.name) {
		score = 4 + min(3, match)
	} else if match >= 2 {
		score = 2 + min(3, match)
	} else if match == 1 {
		score = 1
	}
	sampleMatches := 0
	for word := range words {
		frequency := p.titles[word]
		if excluded[word] {
			frequency--
		}
		if frequency >= 2 {
			sampleMatches++
		}
	}
	if sampleMatches >= 2 {
		score += 2 + min(3, sampleMatches)
	}
	return score
}

func (f *folderIndex) suggest(path string, e evidence, a assessment, usedAI bool) (string, string) {
	if f == nil || e.Note != "" {
		return "", ""
	}
	words := topicFromEvidence(e, a, usedAI)
	if f.allPaths != nil && len(words) < 2 {
		words = topicWords(baseName(filepath.Base(path)))
	}
	if len(words) < 2 {
		return "", ""
	}
	dir := filepath.Dir(path)
	current := f.folders[dir]
	own := make(map[string]bool)
	for _, word := range topicWords(baseName(filepath.Base(path))) {
		own[word] = true
	}
	shared := make(map[string]bool, len(words))
	for _, word := range words {
		shared[word] = true
	}
	keep := current.score(shared, own)
	var best string
	bestScore, second := 0, 0
	for _, candidate := range f.candidates(dir) {
		if samePath(candidate, dir) {
			continue
		}
		score := f.folders[candidate].score(shared, nil)
		if score > bestScore {
			best, bestScore, second = candidate, score, bestScore
		} else if score > second {
			second = score
		}
	}
	if bestScore >= 5 && bestScore >= keep+2 && bestScore >= second+2 {
		if rel, err := filepath.Rel(f.root, best); err == nil {
			if rel == "." {
				return best, "Contents match the selected folder better than the current subfolder"
			}
			return best, "File evidence matches folder " + rel
		}
	}
	if dir != f.root && current != nil && len(current.name) > 0 && keep == 0 && len(words) >= 4 {
		return "", "File may not fit its current folder; no clear nearby destination"
	}
	return "", ""
}

func (f *folderIndex) enableMoveScan(items []entry) {
	f.allPaths = make([]string, 0, len(f.folders))
	for path := range f.folders {
		f.allPaths = append(f.allPaths, path)
	}
	sort.Strings(f.allPaths)
	f.topics = make(map[string]int)
	for _, item := range items {
		if item.Folder || item.Skip {
			continue
		}
		for _, topic := range leadingTopics(baseName(filepath.Base(item.Path))) {
			f.topics[topic]++
		}
	}
}

func leadingTopics(title string) []string {
	words := strings.Fields(norm(title))
	if len(words) < 2 || len(topicWords(words[0])) != 1 || strings.IndexFunc(words[1], unicode.IsLetter) < 0 {
		return nil
	}
	return []string{strings.Join(words[:2], " "), words[0]}
}

func (f *folderIndex) suggestNewFolder(path string, e evidence, a assessment, usedAI bool) string {
	if f == nil || f.topics == nil {
		return ""
	}
	titles := []string{e.Title, baseName(filepath.Base(path))}
	if usedAI {
		titles = append([]string{a.Title, a.Label}, titles...)
	}
	for _, title := range titles {
		if !meaningful(title) {
			continue
		}
		for _, topic := range leadingTopics(title) {
			if f.topics[topic] < 2 {
				continue
			}
			if profile := f.folders[filepath.Dir(path)]; profile != nil && norm(filepath.Base(filepath.Dir(path))) == topic {
				return ""
			}
			name := readableTitle(strings.ToUpper(topic))
			if !validProposedName(name, "", true) {
				continue
			}
			dest := filepath.Join(f.root, name)
			for _, existing := range f.allPaths {
				if norm(filepath.Base(existing)) == topic && existing != f.root {
					return ""
				}
			}
			return dest
		}
	}
	return ""
}
