package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// Section: Metadata contracts and format constants.

const dcNamespace = "http://purl.org/dc/elements/1.1/"
const maxMetadataFileSize = 128 << 20

type metadataEdit struct {
	Title, Author, OldTitle, OldAuthor string
}

// Section: Metadata evidence and suggested edits.

var labelledTitle = regexp.MustCompile(`(?im)^\s*title:[ \t]*([^\n]+)`)
var labelledAuthor = regexp.MustCompile(`(?im)^\s*(?:author(?:s)?|written by):[ \t]*([^\n]+)`)

func oneOfFirstLines(value, text string) bool {
	if !meaningful(value) || len(value) > 500 {
		return false
	}
	n := 0
	for _, line := range strings.Split(chars(text, 2800), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if n >= 12 {
			break
		}
		if norm(line) == norm(value) {
			return true
		}
		n++
	}
	return false
}

func titleInFirstLines(title, text string) bool {
	if !meaningful(title) || len(title) > 500 {
		return false
	}
	lines := textLines(chars(text, 2800))
	lines = lines[:min(len(lines), 12)]
	expected := norm(title)
	for i := range lines {
		var joined string
		for _, line := range lines[i:min(len(lines), i+4)] {
			if joined != "" && titleHeading.MatchString(strings.TrimSpace(line)) {
				break
			}
			joined = strings.TrimSpace(joined + " " + norm(line))
			if joined == expected {
				return true
			}
			if !strings.HasPrefix(expected, joined+" ") {
				break
			}
		}
	}
	return false
}

func suggestMetadata(name string, e evidence, a assessment) *metadataEdit {
	switch strings.ToLower(extension(name)) {
	case ".pdf", ".docx", ".docm":
	default:
		return nil
	}
	lines := strings.SplitN(strings.TrimSpace(chars(e.Text, 2800)), "\n", 13)
	front := strings.Join(lines[:min(12, len(lines))], "\n")
	title := clean(a.Title)
	if !oneOfFirstLines(title, front) {
		title = ""
		if m := labelledTitle.FindStringSubmatch(front); m != nil {
			title = clean(m[1])
		}
	}
	if !meaningful(title) || titleHeading.MatchString(title) || strings.Contains(strings.ToLower(title), "author:") || norm(title) == norm(e.Title) {
		title = ""
	}
	var author string
	if m := labelledAuthor.FindStringSubmatch(front); m != nil {
		author = clean(m[1])
	} else if len(a.Authors) == 1 && oneOfFirstLines(a.Authors[0], front) {
		// A multi-author model response can be truncated. Keep the full byline when explicitly labelled.
		author = clean(a.Authors[0])
	}
	if !meaningful(author) || norm(author) == norm(e.Author) {
		author = ""
	}
	if title == "" && author == "" {
		return nil
	}
	return &metadataEdit{Title: title, Author: author, OldTitle: e.Title, OldAuthor: e.Author}
}

// Section: Format dispatch and metadata rewriting.

func rewriteMetadata(original, extensionHint string, stage *os.File, edit *metadataEdit) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("the metadata writer could not read this document safely")
		}
	}()
	switch strings.ToLower(extensionHint) {
	case ".pdf":
		err = rewritePDF(original, stage, edit)
	case ".docx", ".docm":
		err = rewriteOffice(original, stage, edit)
	default:
		err = errors.New("no metadata writer for this format")
	}
	if err != nil {
		return err
	}
	// The reader used by the app must see the expected fields in the rewritten file.
	var updated evidence
	if strings.EqualFold(extensionHint, ".pdf") {
		updated = pdfEvidence(stage.Name())
	} else {
		updated = zipEvidence(stage.Name(), strings.ToLower(extensionHint))
	}
	if edit.Title != "" && clean(updated.Title) != edit.Title || edit.Author != "" && clean(updated.Author) != edit.Author {
		return errors.New("the rewritten metadata could not be verified")
	}
	return stage.Sync()
}

// Section: PDF metadata rewriting.

func rewritePDF(path string, out *os.File, edit *metadataEdit) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	api.DisableConfigDir()
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	ctx, err := api.ReadAndValidate(f, conf)
	if err != nil {
		return err
	}
	catalog, err := ctx.Catalog()
	if err != nil {
		return err
	}
	switch {
	case ctx.Encrypt != nil:
		return errors.New("encrypted PDFs are left unchanged")
	case ctx.SignatureExist || len(ctx.Signatures) > 0 || ctx.URSignature != nil || catalog["Perms"] != nil:
		return errors.New("signed or certified PDFs are left unchanged")
	case catalog["Metadata"] != nil:
		return errors.New("this PDF also has XMP metadata and needs a compatible editor")
	case *ctx.HeaderVersion < model.V14 || ctx.XRefTable.Version() >= model.V20:
		return errors.New("metadata repair supports PDF versions 1.4 through 1.7")
	}
	info := types.NewDict()
	if ctx.Info != nil {
		info, err = ctx.DereferenceDict(*ctx.Info)
		if err != nil || info == nil {
			return errors.New("PDF document properties could not be read")
		}
	} else {
		ctx.Info, err = ctx.IndRefForNewObject(info)
		if err != nil {
			return err
		}
	}
	for key, value := range map[string]string{"Title": edit.Title, "Author": edit.Author} {
		if value != "" {
			s, err := types.EscapedUTF16String(value)
			if err != nil {
				return err
			}
			info.Update(key, types.StringLiteral(*s))
		}
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err = io.Copy(out, f); err != nil {
		return err
	}
	// Append one properties object and a cross-reference update. Page bytes are preserved.
	if _, err = out.WriteString("\n"); err != nil {
		return err
	}
	ctx.WriteXRefStream = ctx.Read.UsingXRefStreams
	ctx.Write.Increment = true
	ctx.Write.Offset = ctx.Read.FileSize + 1
	ctx.Write.IncrementWithObjNr(ctx.Info.ObjectNumber.Value())
	return api.WriteIncr(ctx, out, conf)
}

// Section: Office package metadata rewriting.

func rewriteOffice(path string, out io.Writer, edit *metadataEdit) error {
	z, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer z.Close()
	var core *zip.File
	for _, f := range z.File {
		name := strings.ToLower(f.Name)
		if strings.HasPrefix(name, "_xmlsignatures/") || strings.Contains(name, "vbaprojectsignature") {
			return errors.New("signed Office files are left unchanged")
		}
		if name == "docprops/core.xml" {
			if core != nil || f.UncompressedSize64 > 1<<20 {
				return errors.New("unexpected Office core properties")
			}
			core = f
		}
	}
	if core == nil {
		return errors.New("Office file has no core properties part")
	}
	rc, err := core.Open()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	rc.Close()
	if err != nil {
		return err
	}
	data, err = editOfficeProperties(data, edit)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(out)
	for _, f := range z.File {
		if f != core {
			if err = writer.Copy(f); err != nil {
				return err
			}
			continue
		}
		h := f.FileHeader
		h.CRC32, h.CompressedSize, h.UncompressedSize, h.CompressedSize64, h.UncompressedSize64 = 0, 0, 0, 0, 0
		w, err := writer.CreateHeader(&h)
		if err != nil {
			return err
		}
		if _, err = w.Write(data); err != nil {
			return err
		}
	}
	return writer.Close()
}

func editOfficeProperties(data []byte, edit *metadataEdit) ([]byte, error) {
	changes := map[string]string{"title": edit.Title, "creator": edit.Author}
	type splice struct {
		start, end int64
		value      string
	}
	var updates []splice
	var field string
	var start int64
	depth := 0
	d := xml.NewDecoder(bytes.NewReader(data))
	for {
		before := d.InputOffset()
		token, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch tag := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 && tag.Name.Local != "coreProperties" {
				return nil, errors.New("unrecognised Office properties document")
			}
			if depth == 2 && tag.Name.Space == dcNamespace && changes[tag.Name.Local] != "" {
				field, start = tag.Name.Local, before
			}
		case xml.EndElement:
			if depth == 2 && field != "" {
				updates = append(updates, splice{start, d.InputOffset(), dcProperty(field, changes[field])})
				delete(changes, field)
				field = ""
			}
			if depth == 1 {
				if before == d.InputOffset() {
					return nil, errors.New("empty Office properties document")
				}
				for _, name := range []string{"title", "creator"} {
					if changes[name] != "" {
						updates = append(updates, splice{before, before, dcProperty(name, changes[name])})
					}
				}
			}
			depth--
		}
	}
	sort.SliceStable(updates, func(i, j int) bool { return updates[i].start < updates[j].start })
	var out bytes.Buffer
	var next int64
	for _, u := range updates {
		out.Write(data[next:u.start])
		out.WriteString(u.value)
		next = u.end
	}
	out.Write(data[next:])
	return out.Bytes(), nil
}

func dcProperty(name, value string) string {
	var text bytes.Buffer
	_ = xml.EscapeText(&text, []byte(value))
	return fmt.Sprintf("<dc:%s xmlns:dc=%q>%s</dc:%s>", name, dcNamespace, text.String(), name)
}

// Section: Scan metadata proposal finalization.

func finalizeScanMetadata(s *session, name string, e evidence, a assessment, d decision) decision {
	if s.manifest.RepairMetadata {
		d.Metadata = suggestMetadata(name, e, a)
		if d.Metadata != nil && d.Metadata.Title != "" {
			d.Metadata.Title = readableTitle(d.Metadata.Title)
			if d.Metadata.Title == d.Metadata.OldTitle {
				d.Metadata.Title = ""
			}
			if d.Metadata.Title == "" && d.Metadata.Author == "" {
				d.Metadata = nil
			}
		}
	}
	return d
}
