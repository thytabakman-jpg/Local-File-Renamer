package main

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var titleWord = regexp.MustCompile(`[\pL\pN][\pL\pM\pN]*(?:['’][\pL\pM\pN]+)*`)
var uppercaseCourseField = regexp.MustCompile(`^[A-Z]{2,5}(?:[ -]+)?[1-9][0-9]{2,3}[A-Z]?$`)
var titleInitialisms = map[string]string{
	"ADHD": "ADHD", "AI": "AI", "APA": "APA", "ASD": "ASD", "ABA": "ABA",
	"ACT": "ACT", "AEDP": "AEDP", "CBT": "CBT", "CRPO": "CRPO", "DBT": "DBT",
	"DSM": "DSM", "EFT": "EFT", "TF": "TF", "EMDR": "EMDR", "ERP": "ERP", "ICD": "ICD",
	"IFS": "IFS", "MCC": "MCC", "MCQ": "MCQ", "OCD": "OCD", "PTSD": "PTSD",
	"REBT": "REBT", "RP": "RP", "SUD": "SUD",
	"CPU": "CPU", "CSV": "CSV", "DNA": "DNA", "DOI": "DOI", "FAQ": "FAQ",
	"GPU": "GPU", "HTML": "HTML", "HTTP": "HTTP", "HTTPS": "HTTPS", "ISBN": "ISBN",
	"JSON": "JSON", "MLA": "MLA", "MRI": "MRI", "NASA": "NASA", "OCR": "OCR",
	"PDF": "PDF", "PDFS": "PDFs", "RAM": "RAM", "RNA": "RNA", "SQL": "SQL",
	"PNAS": "PNAS", "ISSN": "ISSN", "URL": "URL", "USB": "USB", "WHO": "WHO", "XML": "XML",
}

func smallTitleWord(s string) bool {
	switch s {
	case "a", "al", "an", "and", "as", "at", "but", "by", "et", "for", "from",
		"in", "into", "nor", "of", "on", "or", "per", "the", "to", "via", "with", "yet":
		return true
	}
	return false
}

func titleSpaces(s string) string { return clean(strings.ReplaceAll(s, "_", " ")) }

func capFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToTitle(r)) + s[n:]
}

func readableTitle(s string) string {
	s = titleSpaces(s)
	if strings.ToUpper(s) != s || strings.ToLower(s) == s || uppercaseCourseField.MatchString(s) {
		return s
	}
	words := titleWord.FindAllStringIndex(s, -1)
	var out strings.Builder
	end := 0
	for i, bounds := range words {
		word := s[bounds[0]:bounds[1]]
		gap := s[end:bounds[0]]
		out.WriteString(gap)
		if exact, ok := titleInitialisms[word]; ok {
			out.WriteString(exact)
		} else if strings.IndexFunc(word, unicode.IsDigit) >= 0 {
			// Keep tokens like V2 and DSM5 intact.
			out.WriteString(word)
		} else {
			lower := strings.ToLower(word)
			if i != 0 && i != len(words)-1 && !strings.ContainsAny(gap, ":!?") && smallTitleWord(lower) {
				out.WriteString(lower)
			} else {
				if at := strings.IndexAny(lower, "'’"); at == 1 && strings.ContainsRune("odl", rune(lower[0])) {
					_, n := utf8.DecodeRuneInString(lower[at:])
					split := at + n
					lower = lower[:split] + capFirst(lower[split:])
				}
				out.WriteString(capFirst(lower))
			}
		}
		end = bounds[1]
	}
	out.WriteString(s[end:])
	return out.String()
}

func readableName(name string, folder bool) string {
	stem, ext := name, ""
	if !folder {
		stem, ext = baseName(name), extension(name)
	}
	if strings.HasPrefix(stem, ".") || !meaningful(titleSpaces(stem)) {
		return name
	}
	if !folder {
		switch strings.ToUpper(stem) {
		case "README", "LICENSE", "LICENCE", "COPYING", "AUTHORS", "CHANGELOG", "NOTICE", "MAKEFILE":
			return name
		}
	}
	parts := separator.Split(titleSpaces(stem), -1)
	for i := range parts {
		parts[i] = readableTitle(parts[i])
	}
	title := strings.Join(parts, " - ")
	if reserved.MatchString(title) {
		title = "File - " + title
	}
	return title + ext
}
