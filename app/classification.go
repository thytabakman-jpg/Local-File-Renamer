package main

import (
	"slices"
	"strings"
)

type classificationCheck struct {
	Type, Result, Reason string
}

type fileClassification struct {
	Kind, Course string
	Checks       []classificationCheck
}

func classifyFile(name string, e evidence, a assessment, paper paperName) fileClassification {
	ext := strings.ToLower(extension(name))
	published := publicationKind(ext, e, a)
	course := ""
	if published == "" {
		course = courseworkCode(name, e, paper)
	}
	readable := meaningful(e.Title) || meaningful(e.Author) || len(strings.TrimSpace(e.Text)) >= 30
	front := chars(e.Text, 5500)
	bookMarker := e.Kind == "book" || bookSignal.MatchString(front)
	applicable := slices.Contains([]string{".pdf", ".epub", ".docx", ".docm", ".odt"}, ext)
	checks := []struct {
		kind, label, positive, negative string
		matched, uncertain              bool
	}{
		{"article", "Journal article", "Identified a journal and publication markers, or a DOI with matching article evidence.", "No supporting journal publication evidence was found.", published == "article", applicable && (!readable || extractDOI(chars(front, 2400)) != "")},
		{"academic_book", "Academic book", "Identified a book plus an academic publisher or textbook marker.", "No evidence of an academic book was found.", published == "academic_book", applicable && (!readable || academicBookSignal.MatchString(front) && !bookMarker)},
		{"book", "Other book", "Identified a book from its format, ISBN or cataloguing information.", "No supporting book information was found.", published == "book", applicable && !readable},
		{"course_paper", "Course paper", "Identified a supported course code: " + course + ".", "No course code in a recognized coursework context was found.", course != "", slices.Contains([]string{".pdf", ".docx", ".docm", ".doc", ".odt", ".rtf", ".txt"}, ext) && !readable},
	}
	result := fileClassification{Course: course}
	for _, check := range checks {
		if check.matched {
			result.Checks = append(result.Checks, classificationCheck{check.label, "Yes", check.positive})
			result.Kind = check.kind
			break
		}
		if check.uncertain {
			result.Checks = append(result.Checks, classificationCheck{check.label, "Unclear", check.negative})
		} else {
			result.Checks = append(result.Checks, classificationCheck{check.label, "No", check.negative})
		}
	}
	return result
}

func checklistText(checks []classificationCheck) string {
	var fields []string
	for _, check := range checks {
		fields = append(fields, check.Type+" "+check.Result+" ("+check.Reason+")")
	}
	return strings.Join(fields, " | ")
}

// Section: Publication classification and citation evidence.

func publicationKind(ext string, e evidence, a assessment) string {
	if !slices.Contains([]string{".pdf", ".epub", ".docx", ".docm", ".odt"}, ext) {
		return ""
	}
	front := chars(e.Text, 5500)
	book := e.Kind == "book" || bookSignal.MatchString(front)
	venue := publicationVenue(front, e)
	doi := articleDOI.MatchString(chars(front, 2400))
	articleDetails := venue != "" && journalContext.MatchString(front) ||
		doi && (publicationDetails.MatchString(front) || articleMarker.MatchString(front)) ||
		articleMarker.MatchString(front) && publicationDetails.MatchString(front)
	if e.Kind != "book" && articleDetails {
		return "article"
	}
	if book {
		if academicBookSignal.MatchString(front + " " + e.Publisher) {
			return "academic_book"
		}
		return "book"
	}
	if doi && a.Kind == "article" && oneOfFirstLines(a.Title, front) {
		return "article"
	}
	return ""
}

func publicationVenue(text string, e evidence) string {
	if meaningful(e.Venue) {
		return e.Venue
	}
	if m := explicitVenue.FindStringSubmatch(text); m != nil {
		return clean(m[1])
	}
	if venue := clean(journalLine.FindString(text)); meaningful(venue) {
		return venue
	}
	if venue := clean(proceedingsVenue.FindString(text)); venue != "" {
		return venue
	}
	if strings.Contains(strings.ToLower(text), "10.1073/pnas.") {
		return "PNAS"
	}
	return ""
}

func courseworkCode(name string, e evidence, paper paperName) string {
	if !slices.Contains([]string{".pdf", ".docx", ".docm", ".doc", ".odt", ".rtf", ".txt"}, strings.ToLower(extension(name))) {
		return ""
	}
	front := chars(e.Text, 2000)
	for _, m := range courseContext.FindAllStringSubmatch(front, -1) {
		if code := getCourse(m[1], false); code != "" {
			return code
		}
	}
	if courseCoverContext.MatchString(front) {
		if code := getCourse(courseCoverCode.FindString(front), false); code != "" {
			return code
		}
	}
	if strings.HasPrefix(paper.course, "MCC ") && meaningful(paper.title) && !presentationMarker.MatchString(paper.title) {
		return paper.course
	}
	return ""
}
