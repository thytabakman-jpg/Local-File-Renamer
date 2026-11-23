package main

import (
	"regexp"
	"slices"
	"strings"
)

var placeholder = regexp.MustCompile(`(?i)\bunknown (?:author|year|title|journal|publisher)\b|\bversion pending\b`)
var comparableVersion = regexp.MustCompile(`(?i)\b(?:version|v|draft)[ ._-]*(\d+(?:\.\d+)*)\b`)

func usefulFilename(name string) bool {
	stem := clean(placeholder.ReplaceAllString(baseName(name), " "))
	stem = strings.Trim(stem, " -–_")
	return meaningful(titleSpaces(stem))
}

func removedDetails(old, proposed string) []string {
	if !usefulFilename(old) {
		return nil
	}
	details := func(name string) []string {
		stem := placeholder.ReplaceAllString(baseName(name), " ")
		stem = comparableVersion.ReplaceAllString(stem, "$1")
		return strings.Fields(norm(stem))
	}
	after := details(proposed)
	var missing []string
	for _, word := range details(old) {
		if !smallTitleWord(word) && !slices.Contains(after, word) && !slices.Contains(missing, word) {
			missing = append(missing, word)
		}
	}
	return missing
}

func preservePublicationName(name string, d decision) decision {
	if !usefulFilename(name) || d.Name == name || d.ReplaceDownloadID {
		return d
	}
	if d.Unknown || len(removedDetails(name, d.Name)) > 0 {
		d.Name, d.Strict, d.AutoEvidence, d.Outcome = name, false, false, "Insufficient evidence"
		d.Note = joinNote(d.Note, "Kept the existing filename to preserve its citation or descriptive details")
	}
	return d
}

func evidenceBenefit(before, after decision) string {
	switch {
	case after.Kind != "" && before.Kind != after.Kind:
		return "Corrected classification"
	case before.Unknown && (!after.Unknown || before.Note != after.Note) ||
		before.Name != after.Name && after.Name != "" && after.Outcome != "Name verified":
		return "Supplied missing information"
	case before.Outcome != "Name verified" && after.Outcome == "Name verified":
		return "Verified name"
	default:
		return "No useful change"
	}
}

func (s *session) canAutoApply(plan record) bool {
	return s.manifest.Auto && plan.Auto && plan.AutoValidated && !s.autoCompleted[plan.Index] && !s.autoHeld[plan.Index] &&
		(s.manifest.Settings == nil || slices.Contains(s.settings().Execution.AutoKinds, plan.Kind))
}
