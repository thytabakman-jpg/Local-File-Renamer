package main

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

// Section: Naming configuration and defaults.

type namingRules struct {
	NormalizeSpacing    bool              `json:"normalize_spacing"`
	ReplaceUnderscores  bool              `json:"replace_underscores"`
	NormalizeUppercase  bool              `json:"normalize_uppercase"`
	PreferSpecificTitle bool              `json:"prefer_specific_title"`
	Spelling            map[string]string `json:"spelling"`
	Capitalization      string            `json:"capitalization"`
	Separator           string            `json:"separator"`
	MaxLength           int               `json:"max_length"`
	AuthorFormat        string            `json:"author_format"`
	VersionFormat       string            `json:"version_format"`
	Protected           []string          `json:"protected"`
}

type ruleSet struct {
	Revision uint64            `json:"revision"`
	Rules    namingRules       `json:"rules"`
	Formats  map[string]string `json:"formats"`
}

var defaultRules = namingRules{NormalizeSpacing: true, ReplaceUnderscores: true, NormalizeUppercase: true, PreferSpecificTitle: true, Capitalization: "readable", Separator: " - ", MaxLength: 240, AuthorFormat: "surname_et_al", VersionFormat: "number"}
var namingFormats = map[string]string{
	"article":       "Author(s) - Year - Article Title - Journal",
	"academic_book": "Author(s) - Year - Book Title - Publisher",
	"book":          "Author(s) - Year - Book Title",
	"course_paper":  "Paper Title - Course Code - Version",
}

var spacedDash = regexp.MustCompile(`(?:\s+-\s*|\s*-\s+)`)
var spellingWord = regexp.MustCompile(`^[\pL\pM]+$`)

// Section: Rule validation.

func validateRules(r namingRules) error {
	if len(r.Protected) > 100 {
		return errors.New("Use at most 100 protected terms")
	}
	for _, term := range r.Protected {
		if !meaningful(term) || len(term) > 80 || forbidden.MatchString(term) {
			return errors.New("Protected terms must be meaningful filename text")
		}
	}
	if len(r.Spelling) > 100 {
		return errors.New("Use at most 100 spelling corrections")
	}
	for from, to := range r.Spelling {
		if len(from) > 60 || len(to) > 60 || strings.ToLower(from) != from || !spellingWord.MatchString(from) || !spellingWord.MatchString(to) {
			return errors.New("Spelling corrections require lowercase single-word keys and single-word replacements")
		}
	}
	return nil
}

// Section: Name normalization and protection.

var codeWithUnderscore = regexp.MustCompile(`\b[A-Z]{2,12}_\d{2,}[A-Z]?\b`)
var alternateSeparator = strings.NewReplacer(" – ", " - ", " _ ", " - ")

func (r namingRules) protect(current, candidate string) bool {
	for _, term := range r.Protected {
		if strings.Contains(current, term) && !strings.Contains(candidate, term) {
			return false
		}
	}
	for _, term := range codeWithUnderscore.FindAllString(current, -1) {
		if !strings.Contains(candidate, term) {
			return false
		}
	}
	return true
}

func (r namingRules) renderPart(part string) string {
	if r.Capitalization == "preserve" {
		return part
	}
	if r.Capitalization == "sentence" && strings.ToUpper(part) == part && !uppercaseCourseField.MatchString(part) {
		words := 0
		return titleWord.ReplaceAllStringFunc(part, func(word string) string {
			words++
			if exact, ok := titleInitialisms[word]; ok {
				return exact
			}
			if strings.IndexAny(word, "0123456789") >= 0 {
				return word
			}
			if words == 1 {
				return capFirst(strings.ToLower(word))
			}
			return strings.ToLower(word)
		})
	}
	return readableTitle(part)
}

func (r namingRules) cleanName(name string, folder bool) string {
	stem, ext := name, ""
	if !folder {
		stem, ext = baseName(name), extension(name)
	}
	if strings.HasPrefix(stem, ".") || !meaningful(titleSpaces(stem)) || !folder && photoExtension(name) && imageSubject(stem) == "" {
		return name
	}
	if !folder {
		switch strings.ToUpper(stem) {
		case "README", "LICENSE", "LICENCE", "COPYING", "AUTHORS", "CHANGELOG", "NOTICE", "MAKEFILE":
			return name
		}
	}
	if r.NormalizeSpacing || r.Separator == " _ " {
		stem = alternateSeparator.Replace(stem)
	}
	if r.ReplaceUnderscores && strings.Contains(stem, "_") {
		// Keep compact codes such as AB_123 when the stem is an identifier.
		if strings.ContainsAny(stem, " ") || len(strings.Split(stem, "_")) > 2 || strings.IndexFunc(stem, func(c rune) bool { return c >= '0' && c <= '9' }) == -1 {
			var out strings.Builder
			end := 0
			for _, bounds := range codeWithUnderscore.FindAllStringIndex(stem, -1) {
				out.WriteString(strings.ReplaceAll(stem[end:bounds[0]], "_", " "))
				out.WriteString(stem[bounds[0]:bounds[1]])
				end = bounds[1]
			}
			out.WriteString(strings.ReplaceAll(stem[end:], "_", " "))
			stem = out.String()
		}
	}
	if r.NormalizeSpacing {
		stem = spacedDash.ReplaceAllString(clean(alternateSeparator.Replace(stem)), " - ")
	}
	if r.NormalizeUppercase {
		parts := separator.Split(stem, -1)
		for i, part := range parts {
			if !strings.Contains(part, "_") {
				parts[i] = r.renderPart(part)
			}
		}
		stem = strings.Join(parts, " - ")
	}
	if len(r.Spelling) > 0 {
		stem = titleWord.ReplaceAllStringFunc(stem, func(word string) string {
			replacement := r.Spelling[strings.ToLower(word)]
			if replacement == "" {
				return word
			}
			if word == strings.ToUpper(word) {
				return strings.ToUpper(replacement)
			}
			if word != strings.ToLower(word) {
				return capFirst(replacement)
			}
			return replacement
		})
	}
	if reserved.MatchString(stem) {
		stem = "File - " + stem
	}
	if r.Separator != " - " {
		stem = strings.ReplaceAll(stem, " - ", r.Separator)
	}
	if !r.protect(name, stem+ext) {
		return name
	}
	return stem + ext
}

// Section: Windows name validation.

func windowsNameLength(name string) int { return len(utf16.Encode([]rune(name))) }

func validProposedName(name, original string, folder bool) bool {
	return name != "" && name != "." && name != ".." && name == filepath.Base(name) &&
		name == strings.TrimSpace(name) && !strings.HasSuffix(name, ".") && !forbidden.MatchString(name) &&
		!reserved.MatchString(name) && windowsNameLength(name) <= 255 &&
		(folder || extension(name) == extension(original))
}

// Section: Filename construction.

func makeName(parts []string, ext string) string { return defaultRules.makeName(parts, ext) }

func (r namingRules) makeName(parts []string, ext string) string {
	var safe []string
	for _, part := range parts {
		part = r.renderPart(strings.Trim(clean(forbidden.ReplaceAllString(part, " ")), " ."))
		if part != "" {
			safe = append(safe, part)
		}
	}
	if len(safe) == 0 {
		return ""
	}
	// Keep the leading citation fields and journal/publisher even when the title is long.
	for windowsNameLength(strings.Join(safe, r.Separator)+ext) > r.MaxLength {
		longest := 0
		for i, part := range safe {
			if len([]rune(part)) > len([]rune(safe[longest])) {
				longest = i
			}
		}
		runes := []rune(safe[longest])
		if len(runes) < 8 {
			break
		}
		safe[longest] = strings.TrimRight(string(runes[:len(runes)-1]), " .")
	}
	title := strings.Join(safe, r.Separator)
	if reserved.MatchString(title) {
		title = "File - " + title
	}
	return title + ext
}
