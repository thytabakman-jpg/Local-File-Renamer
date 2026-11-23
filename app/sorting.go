package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
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
	root         string
	folders      map[string]*folderProfile
	children     map[string][]string
	nearby       map[string][]string
	allPaths     []string
	topics       map[string]int
	topicLabels  map[string]string
	topicAliases map[string]map[string]int
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
	if meaningful(e.Title) {
		sources = append(sources, chars(e.Title, 240))
	}
	for _, sample := range []string{e.Text, e.FollowupText} {
		lines := 0
		for _, line := range textLines(chars(sample, 650)) {
			if meaningful(line) && !titleHeading.MatchString(line) {
				sources = append(sources, chars(line, 180))
				lines++
			}
			if lines == 3 {
				break
			}
		}
	}
	words := topicWords(strings.Join(sources, " "))
	words = words[:min(24, len(words))]
	if usedAI {
		for _, word := range topicWords(chars(a.Title, 240) + " " + chars(a.Label, 240)) {
			if !slices.Contains(words, word) {
				words = append(words, word)
			}
		}
	}
	return words
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

type folderMatch struct {
	Path  string
	Score int
}

type locationEvidence struct {
	Current      folderMatch
	Alternatives []folderMatch
}

type locationChoice struct {
	Path, Reason string
	Clear, New   bool
	Evidence     *locationEvidence
}

func (f *folderIndex) suggest(path string, e evidence, a assessment, usedAI bool) (string, string) {
	choice := f.location(path, e, a, usedAI)
	if choice.Path == "" && !strings.HasPrefix(choice.Reason, "File may not fit") {
		return "", ""
	}
	return choice.Path, choice.Reason
}

func (f *folderIndex) location(path string, e evidence, a assessment, usedAI bool) locationChoice {
	choice := locationChoice{Reason: "The file evidence did not identify a matching folder"}
	if f == nil {
		return choice
	}
	dir := filepath.Dir(path)
	if f.allPaths != nil {
		choice.Evidence = &locationEvidence{Current: folderMatch{Path: dir}}
	}
	if e.Note != "" && f.allPaths == nil {
		return choice
	}
	filename := baseName(filepath.Base(path))
	words := topicFromEvidence(e, a, usedAI)
	if f.allPaths != nil {
		for _, word := range topicWords(filename) {
			if !slices.Contains(words, word) {
				words = append(words, word)
			}
		}
	}
	if len(words) < 2 {
		choice.Reason = "The filename and document sample supplied fewer than two usable topic words"
		return choice
	}
	current := f.folders[dir]
	own := make(map[string]bool)
	for _, word := range topicWords(filename) {
		own[word] = true
	}
	shared := make(map[string]bool, len(words))
	for _, word := range words {
		shared[word] = true
	}
	keep := current.score(shared, own)
	var best, second folderMatch
	for _, candidate := range f.candidates(dir) {
		if samePath(candidate, dir) {
			continue
		}
		match := folderMatch{candidate, f.folders[candidate].score(shared, nil)}
		if best.Path == "" || match.Score > best.Score {
			second, best = best, match
		} else if second.Path == "" || match.Score > second.Score {
			second = match
		}
	}
	if choice.Evidence != nil {
		choice.Evidence.Current.Score = keep
		for _, match := range []folderMatch{best, second} {
			if match.Path != "" {
				choice.Evidence.Alternatives = append(choice.Evidence.Alternatives, match)
			}
		}
	}
	if best.Score >= 5 && best.Score >= keep+2 && best.Score >= second.Score+2 {
		if rel, err := filepath.Rel(f.root, best.Path); err == nil {
			choice.Path, choice.Clear = best.Path, true
			switch {
			case f.allPaths != nil:
				choice.Reason = f.matchReason(best.Path, words, "The destination")
			case rel == ".":
				choice.Reason = "Contents match the selected folder better than the current subfolder"
			default:
				choice.Reason = "File evidence matches folder " + rel
			}
			return choice
		}
	}
	if f.allPaths != nil {
		switch {
		case keep >= 5 && keep >= best.Score+2 && current != nil && slices.ContainsFunc(current.name, func(word string) bool { return shared[word] }):
			choice.Clear = true
			choice.Reason = f.matchReason(dir, words, "The current folder")
		case best.Path == "":
			choice.Reason = "There is no other existing folder in the scan scope"
		case best.Score < 5:
			choice.Reason = fmt.Sprintf("The best alternative, %s, scored %d, below the required score of 5", filepath.Base(best.Path), best.Score)
		case best.Score < keep+2:
			choice.Reason = fmt.Sprintf("%s scored %d against %d for the current folder and did not lead by 2", filepath.Base(best.Path), best.Score, keep)
		default:
			choice.Reason = fmt.Sprintf("%s and %s had similar scores (%d and %d)", filepath.Base(best.Path), filepath.Base(second.Path), best.Score, second.Score)
		}
	} else if dir != f.root && current != nil && len(current.name) > 0 && keep == 0 && len(words) >= 4 {
		choice.Reason = "File may not fit its current folder; no clear nearby destination"
	}
	return choice
}

func (f *folderIndex) matchReason(path string, words []string, subject string) string {
	profile := f.folders[path]
	var matching []string
	for _, word := range words {
		if slices.Contains(profile.name, word) || profile.titles[word] >= 2 {
			matching = append(matching, word)
		}
	}
	return fmt.Sprintf("%s %s matches topic words from the document or filename: %s", subject, filepath.Base(path), strings.Join(matching, ", "))
}

func (f *folderIndex) enableMoveScan(items []entry) {
	f.allPaths = make([]string, 0, len(f.folders))
	for path := range f.folders {
		f.allPaths = append(f.allPaths, path)
	}
	sort.Strings(f.allPaths)
	f.topics = make(map[string]int)
	f.topicLabels = make(map[string]string)
	f.topicAliases = make(map[string]map[string]int)
	var names []string
	for _, item := range items {
		if !item.Folder && !item.Skip {
			name := baseName(filepath.Base(item.Path))
			names = append(names, name)
			for _, topic := range leadingTopics(name) {
				f.topics[topic]++
				if f.topicLabels[topic] == "" {
					f.topicLabels[topic] = displayTopic(name, topic)
				}
			}
		}
	}
	for _, name := range names {
		leading := leadingTopics(name)
		if len(leading) == 0 || f.topics[leading[0]] < 2 {
			continue
		}
		words := strings.Fields(norm(name))
		for i := 0; i+1 < min(len(words), 4); i++ {
			alias := strings.Join(words[i:i+2], " ")
			if len(topicWords(alias)) < 2 {
				continue
			}
			if f.topicAliases[alias] == nil {
				f.topicAliases[alias] = make(map[string]int)
			}
			f.topicAliases[alias][leading[0]]++
		}
	}
}

var folderLabelWord = regexp.MustCompile(`[\p{L}\p{N}]+`)

func displayTopic(title, topic string) string {
	words := folderLabelWord.FindAllStringIndex(title, len(strings.Fields(topic)))
	if len(words) == 0 {
		return topic
	}
	prefix := strings.TrimSpace(title[words[0][0]:words[len(words)-1][1]])
	if norm(prefix) == topic && validProposedName(prefix, "", true) {
		return readableTitle(prefix)
	}
	return readableTitle(strings.ToUpper(topic))
}

func leadingTopics(title string) []string {
	words := strings.Fields(norm(title))
	if len(words) < 2 || len(topicWords(words[0])) != 1 || strings.IndexFunc(words[1], unicode.IsLetter) < 0 {
		return nil
	}
	return []string{strings.Join(words[:2], " "), words[0]}
}

func (f *folderIndex) proposedLocation(path string, e evidence, a assessment, usedAI bool) locationChoice {
	choice := f.location(path, e, a, usedAI)
	if choice.Clear || f == nil || f.topics == nil {
		return choice
	}
	if len(f.allPaths) == 1 {
		hasSharedTopic := false
		for _, count := range f.topics {
			if count >= 2 {
				hasSharedTopic = true
				break
			}
		}
		if !hasSharedTopic {
			choice.Clear, choice.Reason = true, "The scan has no other folder and no group of filenames supporting a new folder"
			return choice
		}
	}
	filename := baseName(filepath.Base(path))
	titles := []string{e.Title, filename}
	if usedAI {
		titles = append(titles, a.Title, a.Label)
	}
	for _, title := range titles {
		if !meaningful(title) {
			continue
		}
		for _, prefix := range leadingTopics(title) {
			topic, support := f.preferredTopic(prefix)
			if support < 2 {
				continue
			}
			if norm(filepath.Base(filepath.Dir(path))) == topic {
				return choice
			}
			name := f.topicLabels[topic]
			if !validProposedName(name, "", true) {
				continue
			}
			for _, existing := range f.allPaths {
				if norm(filepath.Base(existing)) == topic && existing != f.root {
					return choice
				}
			}
			choice.Path, choice.Clear, choice.New = filepath.Join(f.root, name), true, true
			choice.Reason = fmt.Sprintf("The phrase %q matches %d filenames in the %s group", prefix, support, name)
			return choice
		}
	}
	return choice
}

func (f *folderIndex) preferredTopic(prefix string) (string, int) {
	topic, support := prefix, f.topics[prefix]
	var winner string
	best, second := 0, 0
	for candidate, count := range f.topicAliases[prefix] {
		if count > best {
			winner, best, second = candidate, count, best
		} else if count > second {
			second = count
		}
	}
	// A shared filename phrase can join a smaller proposal to a more
	// established one when the same phrase clearly favours that group.
	if best >= 2 && best >= second+2 && best >= support+2 {
		return winner, best
	}
	return topic, support
}
