package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

type evidence struct {
	Text, Title, Author, Year, Venue, Publisher, Kind, Note, Summary string
	Photo                                                            []byte
	ArticleFrontMatter, ArxivFrontMatter                             bool
	ArxivID                                                          string
}

const excerpt = 10000

var whitespace = regexp.MustCompile(`\s+`)
var validYear = regexp.MustCompile(`\b(?:1[4-9]\d{2}|20\d{2}|2100)\b`)
var xmlName = regexp.MustCompile(`(?i)\.(xml|opf|xhtml|htm|html)$`)
var tags = regexp.MustCompile(`(?is)<[^<>]+>`)

func chars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func readEvidence(path string, vision bool) evidence {
	return readEvidenceWith(path, vision, defaultSettings())
}

func readEvidenceWith(path string, vision bool, opts scanSettings) (e evidence) {
	textLimit := opts.Evidence.TextBytes
	defer func() {
		if recover() != nil {
			e.Note = "Could not parse file"
		}
	}()
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case ext == ".pdf":
		e = pdfSample(path, 1, opts.Evidence.PDFPages)
	case slices.Contains([]string{".docx", ".docm", ".pptx", ".pptm", ".xlsx", ".xlsm", ".odt", ".ods", ".odp", ".epub", ".zip"}, ext):
		e = zipEvidence(path, ext)
	case slices.Contains([]string{".tar", ".tgz", ".gz"}, ext):
		e = archiveEvidence(path, ext)
	case photoExtension(path):
		if vision {
			e.Photo, e.Note = imageEvidenceWith(path, opts.Evidence.ImageEdge, opts.Resources.DecodeMemoryMiB)
		} else {
			e.Note = "Image content was not inspected because local AI is off"
		}
	case ext == ".mp3":
		e = mp3Evidence(path)
	case slices.Contains([]string{".m4a", ".mp4", ".mov", ".m4v"}, ext):
		e = mp4Evidence(path)
	case isMedia(path):
		e.Note = "No embedded title reader for this format"
	default:
		f, err := os.Open(path)
		if err != nil {
			return evidence{Note: err.Error()}
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, int64(textLimit)))
		if err != nil {
			return evidence{Note: err.Error()}
		}
		// Preserve supported text and extensionless or uncommon UTF-8 documents.
		if !utf8.Valid(b) && len(b) == textLimit {
			b = bytes.TrimRightFunc(b, func(r rune) bool { return r == utf8.RuneError })
		}
		if utf8.Valid(b) && !bytes.ContainsRune(b, 0) && strings.IndexFunc(string(b), func(r rune) bool {
			return unicode.IsControl(r) && !unicode.IsSpace(r)
		}) == -1 {
			e.Text = string(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}))
			if xmlName.MatchString(ext) {
				e.Text = tags.ReplaceAllString(e.Text, " ")
			}
		} else {
			e.Note = "No supported content reader or usable metadata"
		}
	}
	if e.Text == "" && e.Title == "" && e.Author == "" && e.Summary == "" && len(e.Photo) == 0 && e.Note == "" {
		e.Note = "No readable text or identifying metadata"
	}
	e.Text = chars(e.Text, textLimit)
	return e
}

func pdfEvidence(path string) evidence { return pdfSample(path, 1, 2) }

func pdfSample(path string, first, last int) (e evidence) {
	defer func() {
		if recover() != nil {
			e.Note = "Could not parse PDF pages"
		}
	}()

	f, doc, err := pdf.Open(path)
	if err != nil {
		return evidence{Note: "PDF could not be opened: " + err.Error()}
	}
	defer f.Close()
	info := doc.Trailer().Key("Info")
	e = evidence{Title: info.Key("Title").Text(), Author: info.Key("Author").Text()}
	var out strings.Builder
	for i := first; i <= min(doc.NumPage(), last) && out.Len() < excerpt; i++ {
		p := doc.Page(i)
		if i == 1 {
			readArticleFrontMatter(p, &e)
		}
		fonts := map[string]*pdf.Font{}
		for _, key := range p.Fonts() {
			if fonts[key] == nil {
				font := p.Font(key)
				fonts[key] = &font
			}
		}
		text, err := p.GetPlainText(fonts)
		if err == nil {
			if i == 1 && !e.ArticleFrontMatter {
				applyPreprintFrontMatter(p, chars(text, excerpt), &e)
			}
			out.WriteString(chars(text, excerpt))
			out.WriteByte('\n')
		}
	}
	e.Text = strings.TrimSpace(out.String())
	if len(e.Text) < 35 {
		e.Note = "PDF text layer is sparse or absent (possibly a scan)"
	}
	if strings.EqualFold(e.Title, "untitled") || strings.HasSuffix(strings.ToLower(e.Title), ".docx") {
		e.Title = ""
	}
	return e
}

func xmlFields(f *zip.File, fields map[string]string, body bool) string {
	if f.UncompressedSize64 > 32<<20 {
		return ""
	}
	stream, err := f.Open()
	if err != nil {
		return ""
	}
	defer stream.Close()
	dec := xml.NewDecoder(io.LimitReader(stream, 2<<20))
	dec.Strict = false
	var out strings.Builder
	var stack []string
	for out.Len() < excerpt {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch x := tok.(type) {
		case xml.StartElement:
			stack = append(stack, x.Name.Local)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if body && slices.Contains([]string{"p", "row", "h", "si", "li"}, x.Name.Local) {
				out.WriteByte('\n')
			}
		case xml.CharData:
			if len(stack) == 0 {
				continue
			}
			s := strings.TrimSpace(string(x))
			if s == "" {
				continue
			}
			key := strings.ToLower(stack[len(stack)-1])
			if fields != nil && slices.Contains([]string{"title", "creator", "publisher", "date", "subject", "description"}, key) {
				if fields[key] != "" && key == "creator" {
					fields[key] += "; " + s
				} else if fields[key] == "" {
					fields[key] = s
				}
			}
			if body {
				out.WriteString(chars(s, excerpt))
				out.WriteByte(' ')
			}
		}
	}
	return out.String()
}

func zipEvidence(path, ext string) evidence {
	z, err := zip.OpenReader(path)
	if err != nil {
		return evidence{Note: "Archive or document could not be opened"}
	}
	defer z.Close()
	fields := map[string]string{}
	var out strings.Builder
	for i, f := range z.File {
		if i >= 3000 {
			break
		}
		name := strings.ToLower(f.Name)
		switch {
		case ext == ".zip":
			if out.Len() < excerpt && !f.FileInfo().IsDir() {
				fmt.Fprintln(&out, chars(f.Name, 260))
			}
		case name == "docprops/core.xml" || name == "meta.xml" || strings.HasSuffix(name, ".opf"):
			xmlFields(f, fields, false)
		case name == "word/document.xml", name == "content.xml", name == "xl/sharedstrings.xml", name == "xl/workbook.xml",
			strings.HasPrefix(name, "xl/worksheets/sheet") && strings.HasSuffix(name, ".xml"),
			strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml"),
			ext == ".epub" && xmlName.MatchString(name) && !strings.HasSuffix(name, ".xml"):
			if out.Len() < excerpt {
				out.WriteString(xmlFields(f, nil, true))
				out.WriteByte('\n')
			}
		}
	}
	e := evidence{Text: out.String(), Title: fields["title"], Author: fields["creator"], Publisher: fields["publisher"]}
	if ext == ".epub" {
		e.Kind = "book"
		e.Year = validYear.FindString(fields["date"])
	}
	e.Summary = strings.TrimSpace(fields["subject"] + "\n" + fields["description"])
	return e
}

func archiveEvidence(path, ext string) evidence {
	f, err := os.Open(path)
	if err != nil {
		return evidence{Note: err.Error()}
	}
	defer f.Close()
	var stream io.Reader = f
	if ext == ".gz" || ext == ".tgz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return evidence{Note: "Compressed file could not be read"}
		}
		defer gz.Close()
		if ext == ".gz" && !strings.HasSuffix(strings.ToLower(path), ".tar.gz") {
			return evidence{Title: filepath.Base(gz.Name)}
		}
		stream = gz
	}
	entries := tar.NewReader(io.LimitReader(stream, 32<<20))
	var out strings.Builder
	for i := 0; i < 100 && out.Len() < excerpt; i++ {
		h, err := entries.Next()
		if err != nil {
			break
		}
		if h.Typeflag == tar.TypeReg {
			fmt.Fprintln(&out, chars(h.Name, 260))
		}
	}
	return evidence{Text: out.String()}
}

func imageEvidence(path string) ([]byte, string) { return imageEvidenceWith(path, visionEdge, 192) }

func imageEvidenceWith(path string, edge, budget int) ([]byte, string) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err.Error()
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > int64(budget)*(1<<20)/8 {
		return nil, "Image could not be decoded within memory limits"
	}
	if _, err = f.Seek(0, 0); err != nil {
		return nil, err.Error()
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err.Error()
	}
	return encodeVision(img, edge)
}

func resizeJPEG(data []byte, edge int) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	result, note := encodeVision(img, edge)
	if note != "" {
		return nil, fmt.Errorf("%s", note)
	}
	return result, nil
}

func encodeVision(img image.Image, edge int) ([]byte, string) {
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	if longest := max(width, height); longest > edge {
		width = max(1, width*edge/longest)
		height = max(1, height*edge/longest)
	}
	resized := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(resized, resized.Bounds(), img, img.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: 83}); err != nil {
		return nil, err.Error()
	}
	return buf.Bytes(), ""
}

func textLines(s string) []string {
	var lines []string
	scan := bufio.NewScanner(strings.NewReader(s))
	for scan.Scan() {
		if line := strings.TrimSpace(scan.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
