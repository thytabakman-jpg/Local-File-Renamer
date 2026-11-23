package main

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (a *application) previewFile(job string, index int) (string, stamp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if s == nil || job == "" || job != a.v.Job || index < 0 || index >= len(s.manifest.Items) {
		return "", stamp{}, errors.New("Select a file in the current scan")
	}
	item := s.manifest.Items[index]
	if item.Skip || item.Folder {
		return "", stamp{}, errors.New("This item has no file preview")
	}
	path := s.currentPath(item.Path, index)
	if err := safeParent(s.manifest.Root, path); err != nil {
		return "", stamp{}, err
	}
	st, err := fingerprint(path)
	if err != nil || st.Dir {
		return "", stamp{}, errors.New("The file is no longer available")
	}
	return path, st, nil
}

type previewParagraph struct {
	Text  string
	Style string
}

func wordPreview(path string) ([]previewParagraph, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	body, err := z.Open("word/document.xml")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	d := xml.NewDecoder(io.LimitReader(body, 2<<20))
	var paragraphs []previewParagraph
	var inParagraph, inText bool
	var line strings.Builder
	var style string
	for len(paragraphs) < 120 {
		token, err := d.Token()
		if err != nil {
			if len(paragraphs) == 0 && err != io.EOF {
				return nil, err
			}
			break
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				line.Reset()
				style, inParagraph = "", true
			case "pStyle":
				for _, attr := range t.Attr {
					if attr.Name.Local == "val" {
						switch value := strings.ToLower(attr.Value); {
						case value == "title":
							style = "title"
						case strings.HasPrefix(value, "heading"):
							style = "heading"
						}
					}
				}
			case "t":
				inText = inParagraph
			case "tab", "br":
				if inParagraph {
					line.WriteByte(' ')
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				inParagraph = false
				if line.Len() > 0 {
					paragraphs = append(paragraphs, previewParagraph{Text: clean(line.String()), Style: style})
				}
			}
		case xml.CharData:
			if inText && line.Len() < 2500 {
				line.WriteString(chars(string(t), 2500-line.Len()))
			}
		}
	}
	return paragraphs, nil
}

func (a *application) serveFilePreview(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	index, err := strconv.Atoi(query.Get("index"))
	if err != nil {
		http.Error(w, "Choose a file from the scan", http.StatusBadRequest)
		return
	}
	path, before, err := a.previewFile(query.Get("job"), index)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ext := strings.ToLower(extension(path))
	page, _ := strconv.Atoi(query.Get("page"))
	if page < 0 || page > 10000 {
		http.Error(w, "Page outside the preview range", http.StatusBadRequest)
		return
	}
	var jpeg []byte
	var paragraphs []previewParagraph
	var label string
	switch {
	case photoExtension(path):
		var note string
		edge := 1100
		if query.Get("thumbnail") == "1" {
			edge = 96
		}
		jpeg, note = imageEvidenceWith(path, edge, 128)
		if len(jpeg) == 0 {
			err = errors.New(note)
		}
	case ext == ".pdf":
		jpeg, err = renderSamplePage(r.Context(), path, page)
		if err == nil && len(jpeg) == 0 {
			http.Error(w, "No more PDF pages", http.StatusNotFound)
			return
		}
	case ext == ".docx" || ext == ".docm":
		paragraphs, err = wordPreview(path)
		label = "Word document preview · simplified layout"
	case ext == ".txt" || ext == ".md" || ext == ".odt":
		paragraphs = textPreview(readEvidenceWith(path, false, defaultSettings()).Text)
		label = "Text preview"
	case isVideo(path):
		jpeg, err = mediaPreviewFrame(r.Context(), path)
	default:
		err = errors.New("No preview available for this format")
	}
	if err != nil || len(jpeg) == 0 && len(paragraphs) == 0 {
		message := "A preview is unavailable for this file"
		if err != nil {
			message += ": " + err.Error()
		}
		http.Error(w, message, http.StatusUnprocessableEntity)
		return
	}
	after, statErr := fingerprint(path)
	if statErr != nil || before != after || r.Context().Err() != nil {
		http.Error(w, "File changed while preparing the preview", http.StatusConflict)
		return
	}
	if len(jpeg) > 0 {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpeg)
		return
	}
	reply(w, struct {
		Label      string
		Paragraphs []previewParagraph
	}{label, paragraphs})
}

func textPreview(content string) []previewParagraph {
	var paragraphs []previewParagraph
	for _, line := range strings.Split(content, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paragraphs = append(paragraphs, previewParagraph{Text: line})
		}
	}
	return paragraphs
}

func mediaPreviewFrame(ctx context.Context, path string) ([]byte, error) {
	return videoFrame(ctx, path, 0, 850)
}
