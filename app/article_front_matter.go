package main

import (
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/ledongthuc/pdf"
)

var correspondingByline = regexp.MustCompile(`(?i)^corresponding author\s*:\s*([^,;]+)`)
var printedByline = regexp.MustCompile(`(?i)^by\s+(.+)$`)
var exportNumber = regexp.MustCompile(`^[-_][0-9]+$`)

func doiDownloadFilename(name, doi string) bool {
	prefix, suffix, ok := strings.Cut(strings.ToLower(doi), "/")
	if !ok || prefix == "" || suffix == "" {
		return false
	}
	stem := strings.ToLower(strings.TrimSpace(copyLabel.ReplaceAllString(baseName(name), "")))
	for _, separator := range []string{"-", "_"} {
		expected := prefix + separator + strings.NewReplacer("/", "-", ":", "-").Replace(suffix)
		if rest, matches := strings.CutPrefix(stem, expected); matches && (rest == "" || exportNumber.MatchString(rest)) {
			return true
		}
	}
	return false
}

func readArticleFrontMatter(p pdf.Page, e *evidence) {
	rows, err := p.GetTextByRow()
	if err != nil {
		return
	}
	lines := make([]string, 0, min(len(rows), 90))
	for _, row := range rows[:min(len(rows), 90)] {
		var line strings.Builder
		for _, span := range row.Content {
			line.WriteString(span.S)
		}
		if value := clean(line.String()); value != "" {
			lines = append(lines, value)
		}
	}
	applyArticleFrontMatter(lines, e)
	if len(lines) < 4 {
		// Some PDF producers position lines with Td. GetTextByRow only
		// tracks absolute Tm movements, so use glyph positions in that case.
		applyArticleFrontMatter(layoutGlyphLines(p.Content().Text), e)
	}
}

func layoutGlyphLines(glyphs []pdf.Text) []string {
	const maximumRows = 90
	slices.SortStableFunc(glyphs, func(a, b pdf.Text) int {
		if math.Round(a.Y) > math.Round(b.Y) {
			return -1
		}
		if math.Round(a.Y) < math.Round(b.Y) {
			return 1
		}
		if a.X < b.X {
			return -1
		}
		if a.X > b.X {
			return 1
		}
		return 0
	})
	var lines []string
	for i := 0; i < len(glyphs) && len(lines) < maximumRows; {
		var line strings.Builder
		y := math.Round(glyphs[i].Y)
		right := glyphs[i].X
		for ; i < len(glyphs) && math.Round(glyphs[i].Y) == y; i++ {
			g := glyphs[i]
			if g.S == "" || g.S == "\n" {
				continue
			}
			if line.Len() > 0 && g.X-right > math.Max(1, g.FontSize*.22) {
				line.WriteByte(' ')
			}
			line.WriteString(g.S)
			right = g.X + g.W
		}
		if text := clean(line.String()); text != "" {
			lines = append(lines, text)
		}
	}
	return lines
}

func applyArticleFrontMatter(lines []string, e *evidence) {
	marker, year := -1, ""
	for i, line := range lines[:min(len(lines), 18)] {
		if articleMarker.MatchString(line) {
			marker = i
			break
		}
		if publicationDetails.MatchString(line) {
			for _, y := range validYear.FindAllString(line, -1) {
				if year != "" && year != y {
					return
				}
				year = y
			}
		}
	}
	if marker < 1 || year == "" {
		return
	}
	// Use the journal printed nearest the article heading when multiple
	// language versions of the issue header appear on the same page.
	venue := ""
	for i := marker - 1; i >= 0 && venue == ""; i-- {
		venue = publicationVenue(lines[i], evidence{})
	}
	if venue == "" {
		return
	}
	author := ""
	for _, line := range lines[marker+1:] {
		if match := correspondingByline.FindStringSubmatch(line); match != nil {
			author = clean(match[1])
			break
		}
	}
	heading := lines[marker+1:]
	for i, line := range heading[:min(len(heading), 8)] {
		if i == 0 {
			continue
		}
		byline := line
		if match := printedByline.FindStringSubmatch(line); match != nil {
			byline = match[1]
		} else if author == "" || norm(byline) != norm(author) {
			continue
		}
		title := strings.Join(heading[:i], " ")
		if len(strings.Fields(byline)) < 2 || !meaningful(title) || titleHeading.MatchString(title) || len(title) > 320 {
			return
		}
		e.Author, e.Title, e.Year, e.Venue = byline, title, year, venue
		e.ArticleFrontMatter = true
		return
	}
}

var arxivStamp = regexp.MustCompile(`(?im)^\s*arxiv\s*:\s*([0-9]{4}\.[0-9]{4,5}(?:v[1-9][0-9]*)?)\s+\[[^\]\r\n]+\][ \t]+[^\r\n]*\b(19[0-9]{2}|20[0-9]{2})\s*$`)
var affiliationMarks = regexp.MustCompile(`[\s0-9,*†‡]+$`)
var preprintAuthor = regexp.MustCompile(`^\p{Lu}[\p{L}\p{M}.'’\-]*(?: +\p{Lu}[\p{L}\p{M}.'’\-]*){1,4}$`)
var compatibilityLigatures = strings.NewReplacer("ﬀ", "ff", "ﬁ", "fi", "ﬂ", "fl", "ﬃ", "ffi", "ﬄ", "ffl")

func applyPreprintFrontMatter(p pdf.Page, firstPageText string, e *evidence) {
	stamp := arxivStamp.FindStringSubmatch(firstPageText)
	if stamp == nil {
		return
	}
	e.ArxivID, e.Year = strings.ToLower(stamp[1]), stamp[2]
	lines := layoutGlyphLines(p.Content().Text)
	var heading []string
	for _, line := range lines[:min(len(lines), 6)] {
		if titleHeading.MatchString(line) || arxivStamp.MatchString(line) || affiliationMarks.MatchString(line) && affiliationMarks.ReplaceAllString(line, "") == "" {
			continue
		}
		heading = append(heading, compatibilityLigatures.Replace(line))
	}
	for i, line := range heading[:min(len(heading), 4)] {
		if i == 0 {
			continue
		}
		author := clean(affiliationMarks.ReplaceAllString(line, ""))
		title := strings.Join(heading[:i], " ")
		if preprintAuthor.MatchString(author) && len(strings.Fields(title)) >= 4 && len(title) <= 210 && !unfinishedTitle(title) {
			e.Title, e.Author, e.ArxivFrontMatter = title, author, true
			return
		}
	}
}
