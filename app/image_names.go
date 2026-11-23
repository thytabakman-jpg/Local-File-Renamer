package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

var imageExtensions = []string{".jpg", ".jpeg", ".jfif", ".jpe", ".png", ".gif", ".bmp", ".tiff", ".tif", ".webp"}
var exportSuffix = regexp.MustCompile(`(?i)(?:^|[\s(\[-])(?:remove[ -]+background|background[ -]+remov(?:e[ds]?|al)|bg[ -]+removed)(?:[ -]+preview)?[)\]]?\s*(?:\(?\d+\)?)?$`)
var timestampInLabel = regexp.MustCompile(`(?:^|[^0-9])(?:19|20)[0-9]{2}[._/-][0-9]{1,2}[._/-][0-9]{1,2}(?:[^0-9]|$)|(?:^|\s)[0-9]{8,}(?:\s|$)`)
var previewSuffix = regexp.MustCompile(`(?i)(?:\s+[-–—]\s+preview|\s*\(preview\)|\s*\[preview\])(?:\s*\(?\d+\)?)?$`)
var unlabelledDesign = regexp.MustCompile(`(?i)^untitled(?: (?:design|image|project|artwork))?(?: [0-9]+)? (?:preview|copy)(?: [0-9]+)?$`)
var blankImageName = regexp.MustCompile(`(?i)^(?:untitled(?: (?:design|image|project|artwork))?|(?:a )?(?:blank (?:image|photo)|(?:white|transparent) background)|unknown|preview|no visible subject|(?:an? )?(?:image|picture|photo|design))(?:[ .()\[\]-]+\d+)*$`)

func removeImageExportSuffix(stem string) string {
	for {
		trimmed := previewSuffix.ReplaceAllString(stem, "")
		trimmed = strings.TrimRight(exportSuffix.ReplaceAllString(trimmed, ""), " -")
		if trimmed == stem {
			return stem
		}
		stem = trimmed
	}
}

func imageSubject(label string) string {
	if photoExtension(label) {
		label = baseName(label)
	}
	label = titleSpaces(label)
	label = removeImageExportSuffix(label)
	if blankImageName.MatchString(norm(label)) || unlabelledDesign.MatchString(norm(label)) || !meaningful(label) {
		return ""
	}
	return label
}

func decideImage(name string, e evidence, a assessment, modelUsed bool, rules namingRules) decision {
	stem, ext := baseName(name), extension(name)
	d := decision{Name: name, Kind: "other", Note: e.Note}
	current := imageSubject(stem)
	if modelUsed && len(e.Photo) > 0 {
		label := imageSubject(a.Label)
		if timestampInLabel.MatchString(label) {
			label = ""
		}
		if label != "" && (current == "" || a.Mismatch) {
			suffix := ""
			if match := explicitMediaVersion.FindStringIndex(current); match != nil && !hasEvidence(strings.TrimSpace(current[match[0]:]), label) {
				suffix = current[match[0]:]
			}
			d.Name = rules.makeName([]string{label + suffix}, ext)
			d.Reason = "Suggested a visible image subject for review."
			if current == "" && d.Note == "" && !a.Mismatch {
				d.Strict, d.AutoEvidence = true, true
				d.Reason = "The visible subject gives a clear replacement for a generic image name"
			}
			if current != "" {
				d.Note = joinNote(d.Note, "The image may contradict the filename. Review the original details before applying.")
			}
			return d
		}
		if label != "" && norm(label) == norm(current) && norm(stem) == norm(current) {
			d.Outcome, d.Reason = "Name verified", "The visible subject matches the filename."
			return d
		}
	}
	if current != "" && norm(current) != norm(stem) {
		d.Name = rules.makeName([]string{current}, ext)
		d.Outcome, d.Reason = "Cleanup proposed", "Removed an image export label while keeping the subject."
		return d
	}
	if current == "" {
		d.Note = joinNote(d.Note, "The file has a generic export name. A rename needs an identifiable image subject.")
	}
	d.Reason = "There is no better supported image description."
	return d
}

func (r namingRules) normalizeGeneratedName(current, candidate string) string {
	if strings.EqualFold(current, candidate) && strings.ToUpper(baseName(current)) != baseName(current) {
		return current
	}
	stem, ext := baseName(candidate), extension(candidate)
	parts := strings.Split(stem, r.Separator)
	for i, part := range parts {
		if codeWithUnderscore.MatchString(part) {
			continue
		}
		if r.Capitalization == "sentence" {
			parts[i] = r.renderPart(part)
		} else {
			parts[i] = readableTitle(part)
		}
		// Retain the creator's mixed-case wording and spacing in parts that
		// were already readable. The normal naming rules handle any cleanup.
		if strings.ToUpper(part) != part {
			parts[i] = part
		}
	}
	normalized := strings.Join(parts, r.Separator) + ext
	if !r.protect(current, normalized) {
		return current
	}
	return normalized
}

var explicitMediaVersion = regexp.MustCompile(`(?i)(?:\s*[-–—])?\s+(?:version\s+|v)([1-9]\d*(?:\.\d+)*)$`)

func imageVersionGroups(items []entry) (map[int]string, map[int]bool) {
	type group struct {
		title              string
		originals, exports []int
	}
	groups := map[string]*group{}
	for i, item := range items {
		if item.Skip || item.Folder || !photoExtension(item.Path) {
			continue
		}
		stem := titleSpaces(baseName(filepath.Base(item.Path)))
		title := imageSubject(stem)
		if title == "" {
			continue
		}
		key := filepath.Dir(item.Path) + "\x00" + norm(title)
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
		}
		if exportSuffix.MatchString(previewSuffix.ReplaceAllString(stem, "")) {
			g.exports = append(g.exports, i)
		} else if stem == title && !explicitMediaVersion.MatchString(stem) {
			g.title = title
			g.originals = append(g.originals, i)
		}
	}
	versions, uncertain := map[int]string{}, map[int]bool{}
	for _, g := range groups {
		if len(g.exports) == 0 || len(g.originals) == 0 {
			continue
		}
		if len(g.exports) == 1 && len(g.originals) == 1 {
			versions[g.originals[0]], versions[g.exports[0]] = g.title+"\x001", g.title+"\x002"
		} else {
			for _, index := range g.exports {
				uncertain[index] = true
			}
		}
	}
	return versions, uncertain
}

func (s *session) versionImage(index int, d decision) decision {
	name := filepath.Base(s.manifest.Items[index].Path)
	if s.uncertainImageVersions[index] {
		d.Name, d.Outcome, d.AutoEvidence = name, "Needs review", false
		d.Note = joinNote(d.Note, "Several images share an export title. The version order needs review.")
		return d
	}
	title, version, paired := strings.Cut(s.imageVersions[index], "\x00")
	if !paired || d.Outcome == "Analysis failed" {
		return d
	}
	if d.Name != "" && norm(imageSubject(d.Name)) != norm(title) {
		return d
	}
	rules := s.namingRules()
	proposed := title + rules.Separator + "Version " + version + extension(name)
	if windowsNameLength(proposed) > rules.MaxLength || !rules.protect(name, proposed) {
		return d
	}
	d.Name, d.Outcome, d.AutoEvidence = proposed, "Rename proposed", false
	d.Reason = "Paired an original with a same-title background-removal export"
	d.Note = joinNote(d.Note, "Review both files to confirm the version relationship.")
	return d
}

func identifyImageSpecies(name string, d decision, a assessment, rules namingRules) decision {
	species := strings.TrimSpace(a.Species)
	if imageSubject(species) != species || timestampInLabel.MatchString(species) {
		return d
	}
	current := imageSubject(baseName(name))
	if hasEvidence(species, current) || current != "" && d.Name == name && !a.Mismatch {
		return d
	}
	label := imageSubject(d.Name)
	if label == "" || hasEvidence(species, label) {
		label = species
	} else {
		label = species + rules.Separator + label
	}
	proposed := rules.makeName([]string{label}, extension(name))
	if proposed != "" && rules.protect(name, proposed) {
		d.Name, d.Outcome = proposed, "Rename proposed"
		d.Reason = "Used the species name visible in the image"
	}
	return d
}
