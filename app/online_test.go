package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testOnlineTransport func(*http.Request) (*http.Response, error)

func (f testOnlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func onlineResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func mockOnline(t *testing.T, requests func(*http.Request) string) *onlineLookup {
	t.Helper()
	w := newOnlineLookup()
	w.braveKey = ""
	w.http.Transport = testOnlineTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.Body != nil || r.URL.Scheme != "https" {
			t.Errorf("unexpected network request: %s %s", r.Method, r.URL)
		}
		return onlineResponse(200, requests(r)), nil
	})
	return w
}

func TestOnlineDOIConfirmationAndConflict(t *testing.T) {
	const doi = "10.1234/trees"
	calls := 0
	w := mockOnline(t, func(r *http.Request) string {
		calls++
		if got := r.URL.String(); got != "https://api.crossref.org/works/"+doi {
			t.Errorf("unexpected request: %s", got)
		}
		return `{"message":{"DOI":"10.1234/trees","title":["Trees and Rivers"],"published":{"date-parts":[[2009,5,1]]}}}`
	})
	e := evidence{Title: "Trees and Rivers", Text: "Trees and Rivers\nJournal of Examples\nDOI: " + doi + "."}
	v, err := w.publicationDate(context.Background(), "Trees and Rivers.pdf", e)
	if err != nil || v.Year != "2009" || v.Source.URL != "https://doi.org/"+doi {
		t.Fatal(v, err)
	}
	e.Year = "2012"
	v, err = w.publicationDate(context.Background(), "Trees and Rivers.pdf", e)
	if err != nil || v.Year != "" || !strings.Contains(v.Reason, "2012") {
		t.Fatal(v, err)
	}
	v, err = w.publicationDate(context.Background(), "Different Project.pdf", evidence{Title: "Different Project", Text: "Different Project\nRefs " + doi})
	if err != nil || v.Year != "" || !strings.Contains(v.Reason, "could not be matched") {
		t.Fatal(v, err)
	}
	if calls != 1 {
		t.Fatal("DOI result was not reused", calls)
	}
}

func TestOnlineISBNAndMultipleDates(t *testing.T) {
	w := mockOnline(t, func(r *http.Request) string {
		switch r.URL.Host {
		case "openlibrary.org":
			if got := r.URL.Query().Get("bibkeys"); got != "ISBN:0385472579" {
				t.Errorf("wrong ISBN: %s", got)
			}
			return `{"ISBN:0385472579":{"title":"Zen Speaks","publish_date":"1994","url":"https://openlibrary.org/books/OL1397864M"}}`
		case "api.crossref.org":
			return `{"message":{"DOI":"10.1234/trees","title":["Trees and Rivers"],"published-print":{"date-parts":[[2009]]},"published-online":{"date-parts":[[2008]]}}}`
		default:
			t.Errorf("unexpected provider host %s", r.URL.Host)
			return "{}"
		}
	})
	v, err := w.publicationDate(context.Background(), "Zen Speaks.pdf", evidence{Text: "Zen Speaks\nISBN-10: 0-385-47257-9"})
	if err != nil || v.Year != "1994" || v.Provider != "Open Library" {
		t.Fatal(v, err)
	}
	v, err = w.publicationDate(context.Background(), "Trees and Rivers.pdf", evidence{Title: "Trees and Rivers", Text: "DOI: 10.1234/trees"})
	if err != nil || v.Year != "" || !strings.Contains(v.Reason, "different years") {
		t.Fatal(v, err)
	}
}

func TestOnlineDateInScanAndDirectChatSearch(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "files")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	item := filepath.Join(root, "download.epub")
	writeTestZip(t, item, map[string]string{"book.opf": "<package><metadata><title>Zen Speaks</title><creator>Tsai Chih Chung</creator></metadata></package>", "chapter.xhtml": "<html><body><p>Zen Speaks</p><p>ISBN 0385472579</p></body></html>"})
	a, err := newApp(filepath.Join(base, "home"), filepath.Join(base, "engine"), filepath.Join(base, "models"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.chat.cancel(); a.s.close() })
	a.online = mockOnline(t, func(r *http.Request) string {
		switch r.URL.Host {
		case "en.wikipedia.org":
			if query := r.URL.Query().Get("srsearch"); query != "history of Toronto" {
				t.Errorf("unexpected search %s", query)
			}
			return `{"query":{"search":[{"title":"History of Toronto","snippet":"<b>Toronto</b> grew on the shore of Lake Ontario.","pageid":123}]}}`
		case "openlibrary.org":
			return `{"ISBN:0385472579":{"title":"Zen Speaks","publish_date":"1994","url":"https://openlibrary.org/books/OL1397864M"}}`
		default:
			t.Errorf("unexpected provider host %s", r.URL.Host)
			return "{}"
		}
	})
	ctx := context.Background()
	res, err := a.talk(ctx, chatRequest{Message: "Search online for history of Toronto"})
	if err != nil || len(res.Sources) != 2 || res.Sources[0].Title != "History of Toronto" || strings.Contains(res.Sources[0].Detail, "<b>") {
		t.Fatal(res, err)
	}
	if _, err = a.talk(ctx, chatRequest{Message: `Search online for C:\Users\my-notes.txt`}); err == nil {
		t.Fatal("sent a local path")
	}
	if err = a.start(root, true, false, false, false, false); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, a, "preview")
	plans, err := a.inspections(0, false)
	if err != nil || len(plans.Rows) != 1 {
		t.Fatal(plans, err)
	}
	row := plans.Rows[0]
	if !strings.Contains(row.Proposed, "1994") || row.Inspection.PublicationDate != "1994 confirmed by Open Library" || len(row.Inspection.Checklist) != 3 {
		t.Fatalf("%+v", row)
	}
	res, err = a.talk(ctx, chatRequest{Message: "Verify the publication date for proposal 0."})
	if err != nil || !strings.Contains(res.Message, "1994 confirmed by Open Library") || len(res.Sources) != 1 {
		t.Fatal(res, err)
	}
	opts := a.activeSettings()
	opts.Evidence.OnlineDates = "off"
	if err = a.saveProfile(1, "Offline", &opts, true); err != nil {
		t.Fatal(err)
	}
	if _, err = a.talk(ctx, chatRequest{Message: "Search online for history of Toronto"}); err == nil {
		t.Fatal("online off ignored")
	}
}

func TestOrderedClassification(t *testing.T) {
	tests := []struct {
		name, text, want string
		n                int
	}{
		{"Document.pdf", "Journal: Proceedings of Examples\nAbstract\nExample of a known research paper", "article", 1},
		{"Document.pdf", "University Press\nISBN 978-0-385-47257-3\nAcademic cover page", "academic_book", 2},
		{"Document.pdf", "ISBN 0-385-47257-9\nStory published as a book", "book", 3},
		{"Document.pdf", "Natural Language Processing\nCourse: CSC 123\nExample assignment submission", "course_paper", 4},
		{"Field notes.pdf", "Field Notes on Local Rivers\nSeveral observations of flowing water.", "other", 4},
	}
	for _, tc := range tests {
		d := decide(tc.name, evidence{Text: tc.text}, assessment{}, false)
		if d.Kind != tc.want || len(d.Checklist) != tc.n {
			t.Error(fmt.Sprintf("%s: %+v", tc.want, d))
		}
		if tc.want != "other" && d.Checklist[tc.n-1].Result != "Yes" {
			t.Error(d.Checklist)
		}
	}
}
