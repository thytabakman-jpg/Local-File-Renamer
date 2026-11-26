package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

type assessment struct {
	Kind               string   `json:"kind"`
	Title              string   `json:"title"`
	Authors            []string `json:"authors"`
	Year               string   `json:"year"`
	Venue              string   `json:"venue"`
	Publisher          string   `json:"publisher"`
	Course             string   `json:"course"`
	Label              string   `json:"label"`
	Species            string   `json:"species,omitempty"`
	SpeciesVisibleText bool     `json:"species_visible_text,omitempty"`
	Keep               bool     `json:"keep"`
	Mismatch           bool     `json:"mismatch"`
}
type decision struct {
	NewFolder                     bool
	Name, Note, Folder            string
	Kind, Outcome, Reason         string
	Inspection                    inspection
	Checklist                     []classificationCheck
	Metadata                      *metadataEdit
	Unknown, Strict, AutoEvidence bool
	ReplaceDownloadID             bool
}

var coursePattern = regexp.MustCompile(`(?i)\b([A-Z]{2,5})[ _-]*([1-9][0-9]{2,3}[A-Z]?)\b`)
var courseFilename = regexp.MustCompile(`(?i)(?:^| - )([A-Z]{2,5})[ _-]*([1-9][0-9]{2,3}[A-Z]?)(?:$| - )`)
var separator = regexp.MustCompile(`\s+-\s+`)
var statusSuffix = regexp.MustCompile(`(?i)(?:\s+|^|\s*[-_([]\s*)(?:final|done|complete|completed|finished)(?:[)\]]|\s)*$`)

const pendingVersion = "Version Pending"
const versionPendingNote = "Version ? (unresolved)"

var takeVersion = regexp.MustCompile(`(?i)^take[ _-]+(one|two|three|four|five|six|seven|eight|nine|ten|[1-9]\d*)$`)
var markedVersion = regexp.MustCompile(`(?i)^(?:v(?:ersion)?|draft)[ _.\-]*([1-9]\d*(?:\.\d+)*)$`)
var plainVersion = regexp.MustCompile(`^[1-9]\d{0,2}(?:\.\d+)*$`)
var copyLabel = regexp.MustCompile(`(?i)\s*(?:\[(?:copy|duplicate)\s*\d*\]|\(\d+\))\s*$`)
var bracketedStatus = regexp.MustCompile(`(?i)\s*[\[(](?:final|done|complete|completed|finished)[\])]\s*$`)
var bracketedVersion = regexp.MustCompile(`(?i)\s*[\[(]((?:v(?:ersion)?|draft)[ _.\-]*[1-9]\d*(?:\.\d+)*)[\])]\s*$`)
var bracketedTake = regexp.MustCompile(`(?i)\s*[\[(](take[ _-]+(?:one|two|three|four|five|six|seven|eight|nine|ten|[1-9]\d*))[\])]\s*$`)
var generic = regexp.MustCompile(`(?i)^(?:(?:img|dsc|pxl|photo|image|vid|screen[ _-]?(?:shot|capture)|scan|download|untitled|document|file|book|article|clip|audio|video|export|temp|misc|stuff|new folder|folder|copy)(?:[ _()\[\].-]*\d{0,24})*|\d[\d_. -]*|[\da-f]{16,})$`)
var defaultMeta = regexp.MustCompile(`(?i)^(untitled|microsoft word|word document|anonymous|unknown|document|image|untitled presentation|book|main|n/a)$`)
var forbidden = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
var reserved = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|COM[1-9¹²³]|LPT[1-9¹²³])(?:\.|$)`)
var articleDOI = regexp.MustCompile(`(?i)\b10\.\d{4,9}/\S+`)
var journalLine = regexp.MustCompile(`(?im)^[^\n.]{0,45}\b(?:journal of|transactions on|proceedings of)\b[^\n.]{3,100}$`)
var journalContext = regexp.MustCompile(`(?i)\b(?:ISSN|volume\s+\d|vol\.?\s+\d|published|copyright|abstract|research article)\b|©`)
var articleMarker = regexp.MustCompile(`(?im)^\s*(?:research article|original article|review article|brief report)\s*$`)
var publicationDetails = regexp.MustCompile(`(?i)\b(?:ISSN|vol(?:ume)?\.?\s*\d+|issue\s*\d+|no\.\s*\d+|pages?\s+\d+[–-]\d+|published online|first published)\b`)
var explicitVenue = regexp.MustCompile(`(?im)^\s*(?:journal|publication|published in)\s*:\s*([^\n]{3,140})$`)
var proceedingsVenue = regexp.MustCompile(`(?im)^\s*(?:PNAS|Nature|Science|Cell|PLOS\s+[\pL ]{2,45}|Proceedings of the National Academy of Sciences)\s*$`)
var courseContext = regexp.MustCompile(`(?im)^\s*(?:course(?: code| number)?|class|section|assignment)\s*[:\-]\s*([^\n]{3,130})$`)
var courseCoverCode = regexp.MustCompile(`(?m)^\s*[A-Z]{2,5}[ -]+[1-9][0-9]{2,3}[A-Z]?\s*(?:[:–-][^\n]+)?$`)
var courseCoverContext = regexp.MustCompile(`(?i)\b(?:submitted (?:to|for)|instructor|professor|semester|syllabus|rubric|course assignment|university)\b`)
var presentationMarker = regexp.MustCompile(`(?i)\b(?:slides|slide deck|slideshow|presentation)\b`)
var academicBookSignal = regexp.MustCompile(`(?i)\b(?:university press|textbook|academic press)\b`)
var bookSignal = regexp.MustCompile(`(?i)\b(?:ISBN(?:-1[03])?\s*:?\s*(?:97[89][ -]?)?(?:\d[ -]?){9}[\dX]|library of congress cataloging(?:-in-publication)?)\b`)
var titleHeading = regexp.MustCompile(`(?i)^(?:abstract|contents|table of contents|chapter\s+\d|copyright|all rights reserved|untitled|doi\b|ISBN\b)`)
var yearLine = regexp.MustCompile(`(?im)^.{0,38}(?:copyright|©|published|publication date|\bvolume\b).{0,95}$`)
var punctuation = regexp.MustCompile(`[^\pL\pN]+`)

func norm(s string) string {
	return strings.TrimSpace(punctuation.ReplaceAllString(strings.ToLower(s), " "))
}
func hasEvidence(value, haystack string) bool {
	v := norm(value)
	return v != "" && strings.Contains(" "+norm(haystack)+" ", " "+v+" ")
}
func clean(s string) string { return strings.TrimSpace(whitespace.ReplaceAllString(s, " ")) }
func meaningful(s string) bool {
	s = clean(s)
	return s != "" && !defaultMeta.MatchString(s) && !generic.MatchString(s) && strings.IndexFunc(s, unicode.IsLetter) >= 0
}
func stripStatus(s string) string {
	for {
		last := strings.TrimSpace(s)
		s = strings.TrimSpace(strings.TrimRight(statusSuffix.ReplaceAllString(last, ""), "-_ "))
		if s == last {
			return s
		}
	}
}
func extension(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".tar.gz") {
		return name[len(name)-7:]
	}
	ext := filepath.Ext(name)
	if len(ext) == len(name) {
		return ""
	}
	return ext
}
func baseName(name string) string                { return strings.TrimSuffix(name, extension(name)) }
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

func getCourse(s string, filename bool) string {
	p := coursePattern
	if filename {
		p = courseFilename
	}
	for _, m := range p.FindAllStringSubmatch(s, -1) {
		prefix := strings.ToUpper(m[1])
		if !slices.Contains([]string{"VOL", "ISBN", "HTTP", "PAGE", "FIG", "YEAR", "TABLE", "NO", "ISSN", "COPY"}, prefix) {
			return prefix + " " + strings.ToUpper(m[2])
		}
	}
	return ""
}

type paperName struct {
	title, course, version string
	conflict               bool
}

func versionToken(part string) string {
	if plainVersion.MatchString(part) {
		return part
	}
	if m := markedVersion.FindStringSubmatch(part); m != nil {
		return m[1]
	}
	if m := takeVersion.FindStringSubmatch(part); m != nil {
		words := []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
		if i := slices.Index(words, strings.ToLower(m[1])); i >= 0 {
			return fmt.Sprint(i + 1)
		}
		return m[1]
	}
	return ""
}

func parsePaperName(stem string) paperName {
	stem = titleSpaces(alternateSeparator.Replace(stem))
	var p paperName
	var title, numbers []string
	parts := separator.Split(stem, -1)
	for i, part := range parts {
		part = strings.TrimSpace(bracketedStatus.ReplaceAllString(strings.TrimSpace(part), ""))
		// The first segment is the title unless it is an explicit course or version field.
		// Keep words such as Final or Version 2 inside the title.
		marked := bracketedVersion.FindStringSubmatchIndex(part)
		if marked == nil {
			marked = bracketedTake.FindStringSubmatchIndex(part)
		}
		if m := marked; m != nil {
			numbers = append(numbers, versionToken(part[m[2]:m[3]]))
			part = strings.TrimSpace(part[:m[0]])
		}
		field := part
		if i > 0 || getCourse(stripStatus(part), true) != "" {
			field = copyLabel.ReplaceAllString(field, "")
		}
		fieldTrimmed := strings.Trim(strings.TrimSpace(field), "[]() ")
		versionField := fieldTrimmed
		if i > 0 {
			versionField = stripStatus(fieldTrimmed)
		}
		if n := versionToken(versionField); n != "" {
			numbers = append(numbers, n)
			continue
		}
		statusFree := stripStatus(field)
		if course := getCourse(statusFree, true); course != "" {
			if p.course == "" {
				p.course = course
			}
			continue
		}
		if i > 0 && (strings.EqualFold(fieldTrimmed, pendingVersion) || stripStatus(fieldTrimmed) == "") {
			continue
		}
		if part != "" {
			title = append(title, part)
		}
	}
	p.title = strings.Join(title, " - ")
	for _, number := range numbers {
		if number == "" {
			continue
		}
		if p.version == "" {
			p.version = number
		} else if p.version != number {
			p.conflict = true
		}
	}
	if p.conflict {
		p.version = ""
	}
	return p
}

func readVersion(stem string) (string, bool) {
	p := parsePaperName(stem)
	return p.version, p.version != ""
}

func citationParts(stem string) (author, year, title, venue string) {
	parts := separator.Split(titleSpaces(alternateSeparator.Replace(stem)), -1)
	if len(parts) < 3 || (validYear.FindString(parts[1]) != parts[1] && parts[1] != "Unknown year") || parts[1] == "" || len(parts[0]) > 100 || !meaningful(parts[0]) {
		return
	}
	author, year, title = parts[0], parts[1], strings.Join(parts[2:], " - ")
	if len(parts) >= 4 {
		title, venue = strings.Join(parts[2:len(parts)-1], " - "), parts[len(parts)-1]
	}
	for _, field := range []*string{&author, &year, &title, &venue} {
		if strings.HasPrefix(*field, "Unknown ") {
			*field = ""
		}
	}
	return
}
func familyName(author string) string {
	a := strings.TrimSpace(author)
	if head, _, found := strings.Cut(a, ","); found {
		return strings.TrimSpace(head)
	}
	tokens := strings.Fields(a)
	if len(tokens) < 2 {
		return a
	}
	start := len(tokens) - 1
	if slices.Contains([]string{"jr.", "sr.", "jr", "sr", "ii", "iii", "iv"}, strings.ToLower(tokens[start])) && start > 1 {
		start--
	}
	for start > 0 && slices.Contains([]string{"de", "del", "della", "van", "von", "der", "da", "dos", "di", "du", "la", "le", "al", "bin"}, strings.ToLower(tokens[start-1])) {
		start--
	}
	return strings.Join(tokens[start:], " ")
}
func authorLabel(authors []string, authorFormat string) string {
	var names []string
	for _, author := range authors {
		a := familyName(author)
		if authorFormat == "full_names" {
			a = author
		}
		if meaningful(a) {
			names = append(names, a)
		}
	}
	if len(names) >= 3 && authorFormat == "surname_et_al" {
		return names[0] + " et al"
	}
	return strings.Join(names, " and ")
}
func heuristicTitle(text string) string {
	lines := textLines(text)
	for i, line := range lines {
		line = strings.TrimSpace(strings.TrimLeft(line, "# "))
		if arxivStamp.MatchString(line) {
			continue
		}
		if len(line) >= 12 && len(line) <= 120 && meaningful(line) && !strings.ContainsAny(line, "{}<>\t") &&
			!strings.HasSuffix(line, ".") && !strings.Contains(strings.ToLower(line), "https://") {
			if unfinishedTitle(line) {
				if i+1 < len(lines) {
					next := strings.TrimSpace(lines[i+1])
					if validTitleContinuation(next) && len(line)+1+len(next) <= 120 && !unfinishedTitle(next) {
						return line + " " + next
					}
				}
				return ""
			}
			return line
		}
		if len(line) > 120 {
			return ""
		}
	}
	return ""
}

func unfinishedTitle(title string) bool {
	words := strings.Fields(strings.ToLower(title))
	if len(words) == 0 {
		return false
	}
	return slices.Contains([]string{"a", "an", "and", "at", "for", "from", "in", "of", "on", "or", "the", "to", "with"}, words[len(words)-1])
}

func validTitleContinuation(line string) bool {
	lower := strings.ToLower(line)
	return meaningful(line) && !strings.ContainsAny(line, "@{}<>\t") && !strings.HasSuffix(line, ".") &&
		!titleHeading.MatchString(line) && !strings.HasPrefix(lower, "by ") &&
		!strings.HasPrefix(lower, "author") && !strings.HasPrefix(lower, "https://")
}

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

func decide(name string, e evidence, a assessment, modelUsed bool) decision {
	return decideWithRules(name, e, a, modelUsed, defaultRules, "")
}

func decideWithRules(name string, e evidence, a assessment, modelUsed bool, rules namingRules, override string) (result decision) {
	stem, ext := baseName(name), extension(name)
	evidenceText := e.Text + "\n" + e.Title + "\n" + e.Author + "\n" + e.Year + "\n" + e.Publisher + "\n" + e.Venue
	verified := func(s string) string {
		if hasEvidence(s, evidenceText) {
			return clean(s)
		}
		return ""
	}
	paper := parsePaperName(stem)
	classification := classifyFile(name, e, a, paper)
	defer func() {
		result.Checklist = classification.Checks
		if override != "" && classification.Kind != override {
			result.Checklist = append(result.Checklist, classificationCheck{override, "Override", "Used the operator’s selected classification."})
		}
	}()
	kind, course := classification.Kind, classification.Course
	if override == "course_paper" && course == "" {
		course = courseworkCode(name, e, paper)
	}
	if (override == "" || override == "article") && strings.EqualFold(ext, ".pdf") && e.ArxivID != "" {
		if !e.ArxivFrontMatter {
			return decision{Name: name, Kind: "article", Unknown: true, Outcome: "Needs review",
				Note: "Found an arXiv identifier, but the title and byline could not be verified"}
		}
		classification.Checks = append(classification.Checks, classificationCheck{"Preprint", "Yes", "Found the arXiv version stamp and the title and byline on the first page."})
		author := authorLabel([]string{e.Author}, rules.AuthorFormat)
		fromID := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(baseName(name)), "arxiv"))
		d := decision{Name: rules.makeName([]string{author, e.Year, e.Title, "arXiv " + e.ArxivID}, ext),
			Kind: "article", Strict: true, AutoEvidence: true, ReplaceDownloadID: fromID == e.ArxivID,
			Reason: "Verified the first-page title, author and arXiv version date"}
		return preservePublicationName(name, d)
	}
	switch override {
	case "article", "academic_book", "book", "course_paper":
		kind = override
	case "other", "presentation":
		d := decideGeneral(name, e, a, modelUsed, rules)
		d.Kind = override
		return d
	}

	if kind == "course_paper" {
		return decideCoursePaper(name, stem, ext, kind, course, e, a, paper, rules, verified)
	}
	if slices.Contains([]string{"article", "book", "academic_book"}, kind) {
		return decidePublication(name, stem, ext, kind, e, a, rules, evidenceText, verified)
	}
	return decideGeneral(name, e, a, modelUsed, rules)
}


func decideCoursePaper(name, stem, ext, kind, course string, e evidence, a assessment, paper paperName, rules namingRules, verified func(string) string) decision {
	version := paper.version
	if version == "" {
		version = pendingVersion
	}
	title := paper.title
	if t := verified(a.Title); meaningful(t) && !titleHeading.MatchString(t) && (!meaningful(title) || rules.PreferSpecificTitle && betterTitle(title, t)) {
		title = t
	}
	if meaningful(e.Title) && !meaningful(title) {
		title = e.Title
	}
	if !meaningful(title) {
		title = heuristicTitle(e.Text)
	}
	var issues []string
	if !meaningful(title) {
		return decision{Name: name, Note: "Missing course paper title"}
	}
	if course == "" {
		issues = append(issues, "Missing course code")
	}
	if version == pendingVersion {
		issues = append(issues, versionPendingNote)
	}
	if paper.conflict {
		issues = append(issues, "Conflicting version labels")
	}
	if course == "" {
		return decision{Name: name, Unknown: true, Kind: kind, Note: strings.Join(issues, "; "), Reason: "The course code is unresolved"}
	}
	titleSupported := oneOfFirstLines(title, e.Text) || hasEvidence(title, e.Title) || hasEvidence(title, chars(e.Text, 1800))
	autoEvidence := len(issues) == 0 && titleSupported && paper.version != ""
	if version != pendingVersion {
		if rules.VersionFormat == "v_prefix" {
			version = "V" + version
		}
		if rules.VersionFormat == "preserve_marker" {
			for _, field := range separator.Split(titleSpaces(alternateSeparator.Replace(stem)), -1) {
				field = clean(field)
				if versionToken(field) == version {
					version = field
					break
				}
			}
		}
	}
	return decision{Name: rules.makeName([]string{title, course, version}, ext), Note: strings.Join(issues, "; "), Strict: true, AutoEvidence: autoEvidence, Kind: kind, Reason: "Checked course and version fields against explicit coursework evidence"}
}

func decidePublication(name, stem, ext, kind string, e evidence, a assessment, rules namingRules, evidenceText string, verified func(string) string) decision {
	oldAuthor, oldYear, oldTitle, oldVenue := citationParts(stem)
	title := verified(a.Title)
	if e.ArticleFrontMatter && kind == "article" {
		title = e.Title
	}
	if title == "" {
		if match := labelledTitle.FindStringSubmatch(chars(e.Text, 1600)); match != nil {
			title = clean(match[1])
		}
	}
	if !meaningful(title) && meaningful(e.Title) {
		title = e.Title
	}
	if !meaningful(title) {
		title = matchingFrontTitle(stem, e.Text)
	}
	titleSupported := meaningful(title)
	if !meaningful(title) {
		title = oldTitle
	}
	if !meaningful(title) && meaningful(stem) && oldTitle == "" && !strings.HasPrefix(strings.ToLower(stem), "unknown ") {
		title = stem
	}
	if !meaningful(title) || titleHeading.MatchString(title) {
		title = ""
	}
	authors := a.Authors
	if e.ArticleFrontMatter && kind == "article" {
		authors = []string{e.Author}
	}
	if len(authors) == 0 {
		raw := e.Author
		if !meaningful(raw) {
			raw = ""
			if m := labelledAuthor.FindStringSubmatch(chars(e.Text, 2200)); m != nil {
				raw = m[1]
			}
		}
		if raw != "" {
			authors = strings.Split(raw, ";")
		}
	}
	var found []string
	for _, author := range authors {
		if hasEvidence(author, evidenceText) {
			found = append(found, author)
		}
	}
	author := authorLabel(found, rules.AuthorFormat)
	authorSupported := author != ""
	if oldAuthor != "" && (author == "" || strings.HasSuffix(oldAuthor, " et al") && !strings.HasSuffix(author, " et al")) {
		author = oldAuthor
	}
	year := validYear.FindString(e.Year)
	if year == "" {
		year = validYear.FindString(yearLine.FindString(chars(e.Text, 3500)))
	}
	if year == "" && validYear.FindString(a.Year) == a.Year && hasEvidence(a.Year, chars(e.Text, 2400)) {
		year = a.Year
	}
	yearSupported := year != ""
	if year == "" {
		year = oldYear
	}
	venue := publicationVenue(chars(e.Text, 4000), e)
	if kind == "article" && venue == "" && (articleDOI.MatchString(chars(e.Text, 2400)) || publicationDetails.MatchString(chars(e.Text, 3000))) {
		venue = verified(a.Venue)
	}
	if kind == "academic_book" {
		venue = e.Publisher
		if venue == "" {
			venue = verified(a.Publisher)
		}
	}
	venueSupported := kind == "book" || venue != ""
	if venue == "" {
		venue = oldVenue
	}
	var issues []string
	fill := func(value *string, field, label string) {
		if *value == "" {
			issues = append(issues, "Missing "+field)
			*value = "Unknown " + label
		}
	}
	fill(&author, "author", "author")
	fill(&year, "publication year", "year")
	fill(&title, "title", "title")
	switch kind {
	case "article":
		fill(&venue, "journal", "journal")
	case "academic_book":
		fill(&venue, "publisher", "publisher")
	default:
		venue = ""
	}
	complete := titleSupported && authorSupported && yearSupported && venueSupported && len(issues) == 0
	if len(issues) == 0 && !complete {
		issues = append(issues, "Some existing citation fields could not be verified in the file")
	}
	d := decision{Name: rules.makeName([]string{author, year, title, venue}, ext), Note: strings.Join(issues, "; "), Unknown: len(issues) > 0, Strict: true, AutoEvidence: complete, Kind: kind, Reason: "Checked citation fields against publication evidence"}
	d.ReplaceDownloadID = kind == "article" && complete && doiDownloadFilename(name, extractDOI(e.Text))
	return preservePublicationName(name, d)
}

func matchingFrontTitle(stem, text string) string {
	for _, line := range textLines(chars(text, 2200)) {
		line = clean(strings.TrimLeft(line, "# "))
		if meaningful(line) && norm(stem) == norm(line) {
			return line
		}
	}
	return ""
}

var vagueTitle = regexp.MustCompile(`(?i)^(?:(?:about (?:me|us)|my (?:notes|paper)|notes|draft|research notes|meeting notes|project|lecture|document)(?:[ _.-]+[0-9]+)?)$`)
var genericQualifier = regexp.MustCompile(`(?i)\b(?:notes|photo|image|document|slides|final|research|draft|version|report|summary|paper)\b`)

func betterTitle(current, candidate string) bool {
	old, new := norm(current), norm(candidate)
	if old == new || old == "" || new == "" {
		return false
	}
	if vagueTitle.MatchString(current) && len(strings.Fields(new)) >= 3 {
		return true
	}
	if hasEvidence(old, new) && len(strings.Fields(new)) >= len(strings.Fields(old))+2 {
		extra := strings.TrimSpace(strings.Replace(new, old, "", 1))
		return meaningful(extra) && !genericQualifier.MatchString(extra)
	}
	first, second := strings.Fields(old), strings.Fields(new)
	if len(first) != len(second) || len(first) < 2 {
		return false
	}
	changes := 0
	for i, word := range first {
		if word != second[i] {
			if !singleEdit(word, second[i]) {
				return false
			}
			changes++
		}
	}
	return changes == 1
}

func singleEdit(a, b string) bool {
	x, y := []rune(a), []rune(b)
	if len(x) > len(y) {
		x, y = y, x
	}
	if len(x) < 4 || len(y)-len(x) > 1 {
		return false
	}
	for i := 0; i < len(x); i++ {
		if x[i] == y[i] {
			continue
		}
		if len(x) < len(y) {
			return string(x[i:]) == string(y[i+1:])
		}
		return string(x[i+1:]) == string(y[i+1:]) ||
			i+1 < len(x) && x[i] == y[i+1] && x[i+1] == y[i] && string(x[i+2:]) == string(y[i+2:])
	}
	return len(x) != len(y)
}

var meaningfulSuffix = regexp.MustCompile(`(?i)\s+(\((?:v(?:ersion)?|draft)[ _.-]*\d+(?:\.\d+)*\)|(?:v(?:ersion)?|draft)[ _.-]*\d+(?:\.\d+)*|\d[\d.]*|[A-Z]+[-_]\d+)$`)

func decideGeneral(name string, e evidence, a assessment, modelUsed bool, rules namingRules) decision {
	if photoExtension(name) {
		return decideImage(name, e, a, modelUsed, rules)
	}
	stem, ext := baseName(name), extension(name)
	d := decision{Name: name, Kind: "other", Note: e.Note}
	if presentationMarker.MatchString(stem) || slices.Contains([]string{".pptx", ".pptm", ".odp"}, strings.ToLower(ext)) {
		d.Kind = "presentation"
	}
	current, suffix := titleSpaces(stem), ""
	if m := meaningfulSuffix.FindStringIndex(current); m != nil && (meaningful(stem) || bracketedVersion.MatchString(stem)) {
		current, suffix = strings.TrimSpace(current[:m[0]]), current[m[0]:]
	}
	frontTitle := matchingFrontTitle(current, e.Text)
	title := clean(e.Title)
	if arxivStamp.MatchString(title) || !meaningful(title) || !oneOfFirstLines(title, e.Text) && strings.HasSuffix(strings.ToLower(title), strings.ToLower(ext)) {
		title = ""
	}
	if first := heuristicTitle(e.Text); !meaningful(title) || unfinishedTitle(title) && strings.HasPrefix(first, title+" ") || betterTitle(title, first) {
		title = first
	}
	if strings.EqualFold(ext, ".pdf") && unfinishedTitle(title) {
		title = ""
		d.Note = joinNote(d.Note, "The extracted PDF title may be incomplete")
	}
	if !arxivStamp.MatchString(a.Title) && !unfinishedTitle(a.Title) && titleInFirstLines(a.Title, e.Text) {
		extendsOpening := !oneOfFirstLines(a.Title, e.Text) && oneOfFirstLines(title, e.Text) &&
			strings.HasPrefix(norm(a.Title), norm(title)+" ")
		if !meaningful(title) || betterTitle(title, a.Title) || extendsOpening {
			title = a.Title
		}
	}

	if frontTitle != "" || meaningful(title) && norm(current) == norm(title) {
		d.Outcome = "Name verified"
		d.Reason = "The filename matches a title in the file"
		return d
	}
	if meaningful(title) && (!meaningful(current) || rules.PreferSpecificTitle && betterTitle(current, title)) && !titleHeading.MatchString(title) {
		d.Name = rules.makeName([]string{title + suffix}, ext)
		d.Reason = "Used a more specific or corrected title supported by the file"
		if !modelUsed && e.Title == "" && !meaningful(current) {
			d.Note = joinNote(d.Note, "Best-effort name from extracted text")
		}
		return d
	}
	if !meaningful(current) && meaningful(a.Label) && hasEvidence(a.Label, e.Text+" "+e.Summary) {
		d.Name = rules.makeName([]string{a.Label + suffix}, ext)
		d.Reason = "Used the subject found in the file evidence"
		return d
	}
	if strings.EqualFold(ext, ".pdf") && title == "" {
		for _, line := range textLines(e.Text) {
			if len(line) >= 12 && unfinishedTitle(line) {
				d.Note = joinNote(d.Note, "The extracted PDF title may be incomplete")
				break
			}
		}
	}
	switch {
	case meaningful(a.Title) && !titleInFirstLines(a.Title, e.Text):
		d.Reason = "The proposed title did not match the extracted opening lines"
	case meaningful(title) && meaningful(current):
		d.Reason = "The supported title did not establish a more specific or corrected name"
	case !meaningful(current) && meaningful(a.Label):
		d.Reason = "The proposed subject could not be verified against the extracted evidence"
	default:
		d.Reason = "The available evidence did not establish a better name or verify the existing one"
	}
	return d
}

func photoExtension(name string) bool {
	return slices.Contains(imageExtensions, strings.ToLower(extension(name)))
}
