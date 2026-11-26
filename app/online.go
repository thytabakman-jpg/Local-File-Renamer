package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Section: Provider configuration and network client.

const crossrefEndpoint = "https://api.crossref.org/works"
const wikipediaEndpoint = "https://en.wikipedia.org/w/api.php"
const braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

type onlineSource struct {
	Title, URL, Detail string
}

type onlineLookup struct {
	http      *http.Client
	braveKey  string
	mu        sync.Mutex
	citations map[string]cachedCitation
}
type cachedCitation struct {
	value   publicationRecord
	expires time.Time
}

func newOnlineLookup() *onlineLookup {
	return &onlineLookup{
		http: &http.Client{
			Timeout:       6 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		braveKey:  strings.TrimSpace(os.Getenv("RENAMER_BRAVE_SEARCH_KEY")),
		citations: make(map[string]cachedCitation),
	}
}

func (w *onlineLookup) get(ctx context.Context, endpoint, provider string, params url.Values, target any) error {
	if len(params) != 0 {
		endpoint += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "LocalFileRenamer/"+appVersion+" (interactive file and publication lookup)")
	if provider == "Brave Search" {
		req.Header.Set("X-Subscription-Token", w.braveKey)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s could not be reached", provider)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound && provider == "Crossref" {
			return errNoCitation
		}
		return fmt.Errorf("%s returned HTTP %d", provider, resp.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
		return fmt.Errorf("%s returned an unreadable response", provider)
	}
	return nil
}

// Section: Publication record parsing.

type publicationRecord struct {
	DOI       string
	Title     []string
	Authors   []struct{ Family string } `json:"author"`
	Published publicationDate
	Print     publicationDate `json:"published-print"`
	Online    publicationDate `json:"published-online"`
	Issued    publicationDate
}
type publicationDate struct {
	Parts [][]int `json:"date-parts"`
}

func (d publicationDate) year() string {
	if len(d.Parts) == 0 || len(d.Parts[0]) == 0 {
		return ""
	}
	year := strconv.Itoa(d.Parts[0][0])
	if validYear.FindString(year) != year {
		return ""
	}
	return year
}
func (p publicationRecord) title() string {
	if len(p.Title) > 0 {
		return plainOnlineText(p.Title[0])
	}
	return ""
}
func (p publicationRecord) year() (year, detail string) {
	printYear, onlineYear := p.Print.year(), p.Online.year()
	if printYear != "" && onlineYear != "" && printYear != onlineYear {
		return "", "Crossref lists different years for online (" + onlineYear + ") and print (" + printYear + ") publication"
	}
	for _, date := range []publicationDate{p.Published, p.Print, p.Online, p.Issued} {
		if year = date.year(); year != "" {
			return year, ""
		}
	}
	return "", "Crossref has no publication year for this record"
}
func (p publicationRecord) source() onlineSource {
	year, note := p.year()
	if year != "" {
		note = "Publication year: " + year
	}
	title := p.title()
	if title == "" {
		title = p.DOI
	}
	return onlineSource{Title: title, URL: "https://doi.org/" + doiPath(p.DOI), Detail: note}
}

// Section: Citation lookup and source normalization.

var errNoCitation = errors.New("No matching Crossref publication record")
var errNoBook = errors.New("No matching ISBN edition in Open Library")
var onlineTags = regexp.MustCompile(`<[^>]+>`)

func plainOnlineText(value string) string {
	return clean(html.UnescapeString(onlineTags.ReplaceAllString(value, "")))
}

var safeDOI = regexp.MustCompile(`(?i)^10\.\d{4,9}/[^\s<>"\\]+$`)

func doiPath(doi string) string {
	parts := strings.Split(doi, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
func extractDOI(text string) string {
	candidate := strings.TrimRight(articleDOI.FindString(text), ".,;>]")
	for strings.Count(candidate, ")") > strings.Count(candidate, "(") {
		candidate = strings.TrimSuffix(candidate, ")")
	}
	if len(candidate) <= 200 && safeDOI.MatchString(candidate) {
		return candidate
	}
	return ""
}

func (w *onlineLookup) byDOI(ctx context.Context, doi string) (publicationRecord, error) {
	if !safeDOI.MatchString(doi) || len(doi) > 200 {
		return publicationRecord{}, errNoCitation
	}
	key := strings.ToLower(doi)
	w.mu.Lock()
	old, found := w.citations[key]
	w.mu.Unlock()
	if found && time.Now().Before(old.expires) {
		return old.value, nil
	}
	var response struct{ Message publicationRecord }
	err := w.get(ctx, crossrefEndpoint+"/"+doiPath(doi), "Crossref", nil, &response)
	if err != nil {
		return publicationRecord{}, err
	}
	if !strings.EqualFold(response.Message.DOI, doi) {
		return publicationRecord{}, errNoCitation
	}
	w.mu.Lock()
	if len(w.citations) >= 200 {
		clear(w.citations)
	}
	w.citations[key] = cachedCitation{response.Message, time.Now().Add(time.Hour)}
	w.mu.Unlock()
	return response.Message, nil
}

func (w *onlineLookup) byTitle(ctx context.Context, title, author string) (publicationRecord, error) {
	var response struct {
		Message struct{ Items []publicationRecord }
	}
	params := url.Values{"query.bibliographic": {title + " " + author}, "rows": {"5"}}
	if err := w.get(ctx, crossrefEndpoint, "Crossref", params, &response); err != nil {
		return publicationRecord{}, err
	}
	var found publicationRecord
	for _, item := range response.Message.Items {
		if !safeDOI.MatchString(item.DOI) || norm(title) != norm(item.title()) ||
			!slices.ContainsFunc(item.Authors, func(a struct{ Family string }) bool { return norm(a.Family) == norm(author) }) {
			continue
		}
		if found.DOI != "" && !strings.EqualFold(found.DOI, item.DOI) {
			return publicationRecord{}, errors.New("Several Crossref records matched the title and author")
		}
		found = item
	}
	if found.DOI == "" {
		return found, errNoCitation
	}
	return found, nil
}

// Section: Publication date verification.

type dateCheck struct {
	Year, LocalYear, Reason, Provider string
	Source                            onlineSource
}

func localPublicationYear(e evidence) string {
	if year := validYear.FindString(e.Year); year != "" {
		return year
	}
	return validYear.FindString(yearLine.FindString(chars(e.Text, 3500)))
}

func (w *onlineLookup) publicationDate(ctx context.Context, name string, e evidence) (dateCheck, error) {
	result := dateCheck{LocalYear: localPublicationYear(e), Provider: "Crossref"}
	front := chars(e.Text, 2800)
	title := matchingFrontTitle(baseName(name), front)
	if meaningful(e.Title) {
		title = e.Title
	}
	if match := labelledTitle.FindStringSubmatch(front); title == "" && match != nil {
		title = clean(match[1])
	}
	if title == "" {
		_, _, title, _ = citationParts(baseName(name))
	}
	doi := extractDOI(front)
	if isbn := extractISBN(front); isbn != "" && (doi == "" || e.Kind == "book") {
		book, err := w.bookByISBN(ctx, isbn)
		if err != nil {
			return result, err
		}
		result.Source, result.Provider = book.source(isbn), "Open Library"
		fullTitle := clean(book.Title + " " + book.Subtitle)
		if norm(title) != norm(book.Title) && norm(title) != norm(fullTitle) && !hasEvidence(book.Title, front) {
			result.Reason = "The ISBN edition title could not be matched to this file."
			return result, nil
		}
		years := validYear.FindAllString(book.Date, -1)
		if len(years) != 1 {
			result.Reason = "Open Library did not give a single publication year for this ISBN edition."
			return result, nil
		}
		return confirmDateResult(result, years[0]), nil
	}
	author := e.Author
	if match := labelledAuthor.FindStringSubmatch(front); !meaningful(author) && match != nil {
		author = clean(match[1])
	}
	if author, _, _ = strings.Cut(author, ";"); meaningful(author) {
		author = familyName(author)
	} else {
		author = ""
	}
	if doi == "" && (!meaningful(title) || len(title) > 240 || !meaningful(author) || len(author) > 90) {
		result.Reason = "No DOI or reliable local title and author for an online match."
		return result, nil
	}
	var record publicationRecord
	var err error
	if doi != "" {
		record, err = w.byDOI(ctx, doi)
	} else {
		record, err = w.byTitle(ctx, title, author)
	}
	if err != nil {
		return result, err
	}
	recordTitle := record.title()
	result.Source = record.source()
	if recordTitle == "" || !(meaningful(title) && norm(recordTitle) == norm(title) || hasEvidence(recordTitle, front)) {
		result.Reason = "The Crossref title could not be matched to this file."
		return result, nil
	}
	year, note := record.year()
	if year == "" {
		result.Reason = note
		return result, nil
	}
	return confirmDateResult(result, year), nil
}

func confirmDateResult(result dateCheck, year string) dateCheck {
	if result.LocalYear != "" && result.LocalYear != year {
		result.Reason = "The document lists " + result.LocalYear + " and " + result.Provider + " lists " + year + ". The year remains unresolved."
		return result
	}
	result.Year = year
	result.Reason = "Publication year confirmed by " + result.Provider + "."
	return result
}

// Section: ISBN and book edition verification.

const openLibraryEndpoint = "https://openlibrary.org/api/books"

var isbnMarker = regexp.MustCompile(`(?i)\bISBN(?:-1[03])?\s*:?\s*((?:97[89][ -]?)?(?:\d[ -]?){9}[0-9X])\b`)

func extractISBN(text string) string {
	for _, m := range isbnMarker.FindAllStringSubmatch(text, -1) {
		number := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(m[1]))
		sum := 0
		switch len(number) {
		case 10:
			for i, c := range number {
				digit := int(c - '0')
				if c == 'X' {
					digit = 10
				}
				sum += digit * (10 - i)
			}
			if sum%11 == 0 {
				return number
			}
		case 13:
			for i, c := range number {
				sum += int(c-'0') * (1 + 2*(i%2))
			}
			if sum%10 == 0 {
				return number
			}
		}
	}
	return ""
}

type bookRecord struct {
	Title, Subtitle, URL string
	Date                 string `json:"publish_date"`
}

func (w *onlineLookup) bookByISBN(ctx context.Context, isbn string) (bookRecord, error) {
	key := "ISBN:" + isbn
	response := make(map[string]bookRecord)
	params := url.Values{"bibkeys": {key}, "jscmd": {"data"}, "format": {"json"}}
	if err := w.get(ctx, openLibraryEndpoint, "Open Library", params, &response); err != nil {
		return bookRecord{}, err
	}
	book, ok := response[key]
	if !ok || !meaningful(book.Title) {
		return bookRecord{}, errNoBook
	}
	return book, nil
}

func (b bookRecord) source(isbn string) onlineSource {
	link := "https://openlibrary.org/isbn/" + isbn
	if u, err := url.Parse(b.URL); err == nil && u.Host == "openlibrary.org" && u.User == nil && strings.HasPrefix(u.Path, "/books/") {
		u.Scheme = "https"
		link = u.String()
	}
	return onlineSource{Title: b.Title, Detail: b.Date, URL: link}
}
// Section: General web search and provider fallback.

func (w *onlineLookup) search(ctx context.Context, query string) (message string, sources []onlineSource, failure error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 400 {
		return "", nil, errors.New("Enter a search query of up to 400 bytes")
	}
	if doi := extractDOI(query); doi != "" {
		record, err := w.byDOI(ctx, doi)
		if err != nil {
			return "", nil, err
		}
		return "Crossref publication record", []onlineSource{record.source()}, nil
	}
	if isbn := extractISBN(query); isbn != "" {
		book, err := w.bookByISBN(ctx, isbn)
		if err != nil {
			return "", nil, err
		}
		return "Open Library ISBN edition", []onlineSource{book.source(isbn)}, nil
	}
	if w.braveKey != "" {
		return w.brave(ctx, query)
	}
	params := url.Values{"action": {"query"}, "list": {"search"}, "srsearch": {query}, "srlimit": {"5"}, "format": {"json"}}
	var response struct {
		Query struct {
			Search []struct {
				Title, Snippet string
				PageID         int
			}
		}
	}
	err := w.get(ctx, wikipediaEndpoint, "Wikipedia", params, &response)
	for _, item := range response.Query.Search {
		if item.PageID > 0 {
			sources = append(sources, onlineSource{Title: plainOnlineText(item.Title), Detail: plainOnlineText(item.Snippet), URL: "https://en.wikipedia.org/?curid=" + strconv.Itoa(item.PageID)})
		}
	}
	sources = append(sources, onlineSource{Title: "Search the wider web in your browser", URL: "https://duckduckgo.com/?" + url.Values{"q": {query}}.Encode()})
	if err != nil && ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	if err != nil {
		return err.Error() + ". You can still open the browser search below.", sources, nil
	}
	return "Wikipedia matches. The last link opens a broader web search.", sources, nil
}

func (w *onlineLookup) brave(ctx context.Context, query string) (string, []onlineSource, error) {
	var response struct {
		Web struct {
			Results []struct{ Title, Description, URL string }
		}
	}
	params := url.Values{"q": {query}, "count": {"5"}, "country": {"CA"}, "search_lang": {"en"}}
	if err := w.get(ctx, braveEndpoint, "Brave Search", params, &response); err != nil {
		return "", nil, err
	}
	var sources []onlineSource
	for _, item := range response.Web.Results {
		u, err := url.Parse(item.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			continue
		}
		sources = append(sources, onlineSource{Title: plainOnlineText(item.Title), URL: u.String(), Detail: plainOnlineText(item.Description)})
	}
	if len(sources) == 0 {
		return "Brave Search found no usable results.", nil, nil
	}
	return "Web results from Brave Search.", sources, nil
}

// Section: Scan publication-date verification.

func verifyScanPublicationDate(ctx context.Context, s *session, opts scanSettings, name string, e evidence, a assessment, used bool, rules namingRules, override string, d decision, check inspection) (decision, evidence, inspection) {
	if s.online != nil && opts.Evidence.OnlineDates != "off" && localPublicationYear(e) == "" &&
		(d.Kind == "article" || d.Kind == "book" || d.Kind == "academic_book" || extractDOI(chars(e.Text, 2200)) != "") {
		started := time.Now()
		confirmed, err := s.online.publicationDate(ctx, name, e)
		check.OnlineMS = elapsedMS(started)
		if err != nil {
			check.PublicationDate = err.Error()
		} else {
			check.PublicationDate, check.PublicationSource = confirmed.Reason, confirmed.Source.URL
			if confirmed.Year != "" {
				e.Year = confirmed.Year
				d = decideWithRules(name, e, a, used, rules, override)
				d.AutoEvidence = false
				check.PublicationDate = confirmed.Year + " confirmed by " + confirmed.Provider
			}
		}
	}
	return d, e, check
}
