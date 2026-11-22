package main

import (
	"context"
	"net/url"
	"regexp"
	"strings"
)

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
