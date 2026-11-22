package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

const pdfDateDifference = "The complete PDF bytes match apart from document creation or modification timestamps."
const textLineEndingDifference = "The complete text matches after converting Windows line endings to LF."

// The supported PDF subset has a single cross-reference section and a flat
// document Info dictionary of string literals. Leave other encodings alone.
var pdfInfoObject = regexp.MustCompile(`(?s)^([0-9]+)[\t\r\n ]+([0-9]+)[\t\r\n ]+obj[\t\r\n ]*<<((?:[\t\r\n ]*/[A-Za-z][A-Za-z0-9]*[\t\r\n ]*\((?:\\.|[^\\()])*\))*)[\t\r\n ]*>>[\t\r\n ]*endobj\b`)
var pdfInfoEntry = regexp.MustCompile(`(?s)/([A-Za-z][A-Za-z0-9]*)[\t\r\n ]*\(((?:\\.|[^\\()])*)\)`)
var pdfTimestamp = regexp.MustCompile(`^D:[0-9]{14}(?:Z|[+\-][0-9]{2}'[0-9]{2}'?)?$`)

func normalizeDuplicatePDF(ctx context.Context, data []byte) (normalized []byte) {
	defer func() {
		if recover() != nil {
			normalized = nil
		}
	}()
	if !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.HasSuffix(bytes.TrimSpace(data), []byte("%%EOF")) ||
		bytes.Count(data, []byte("startxref")) != 1 || bytes.Count(data, []byte("%%EOF")) != 1 ||
		(!bytes.Contains(data, []byte("/CreationDate")) && !bytes.Contains(data, []byte("/ModDate"))) || ctx.Err() != nil {
		return nil
	}
	conf := &model.Configuration{ValidationMode: model.ValidationStrict, Reader15: true}
	pdf, err := api.ReadAndValidate(bytes.NewReader(data), conf)
	if err != nil || pdf.Info == nil || pdf.Encrypt != nil || pdf.SignatureExist || len(pdf.Signatures) != 0 ||
		pdf.URSignature != nil || pdf.Read.UsingXRefStreams || pdf.Read.UsingObjectStreams || pdf.Read.RepairOffset != 0 {
		return nil
	}
	catalog, err := pdf.Catalog()
	if err != nil || catalog["Perms"] != nil {
		return nil
	}
	entry, ok := pdf.FindTableEntryForIndRef(pdf.Info)
	if !ok || entry.Free || entry.Compressed || entry.RefCount != 0 || entry.Offset == nil || *entry.Offset < 0 || *entry.Offset >= int64(len(data)) {
		return nil
	}
	info, ok := entry.Object.(types.Dict)
	if !ok {
		return nil
	}
	offset := int(*entry.Offset)
	object := data[offset:]
	match := pdfInfoObject.FindSubmatchIndex(object)
	if match == nil || string(object[match[2]:match[3]]) != fmt.Sprint(pdf.Info.ObjectNumber.Value()) ||
		string(object[match[4]:match[5]]) != fmt.Sprint(pdf.Info.GenerationNumber.Value()) {
		return nil
	}
	fields := object[match[6]:match[7]]
	for _, other := range pdf.Table {
		if stream, ok := other.Object.(types.StreamDict); ok {
			if stream.StreamLength == nil || (int64(offset) < stream.StreamOffset+*stream.StreamLength && int64(offset+match[1]) > stream.StreamOffset) {
				return nil
			}
		}
	}
	seen := make(map[string]bool, len(info))
	for _, field := range pdfInfoEntry.FindAllSubmatchIndex(fields, -1) {
		key := string(fields[field[2]:field[3]])
		value := fields[field[4]:field[5]]
		if seen[key] || info[key] != types.StringLiteral(value) {
			return nil
		}
		seen[key] = true
		if (key == "CreationDate" || key == "ModDate") && pdfTimestamp.Match(value) {
			if normalized == nil {
				normalized = bytes.Clone(data)
			}
			// Leave all offsets, punctuation, streams, and other objects intact.
			start := offset + match[6] + field[4]
			clear(normalized[start : start+len(value)])
		}
	}
	if len(seen) != len(info) || ctx.Err() != nil {
		return nil
	}
	return normalized
}

func equivalenceFinding(key string) string {
	if strings.HasPrefix(key, ".pdf:") {
		return pdfDateDifference
	}
	return textLineEndingDifference
}

func duplicateEquivalence(ctx context.Context, root *os.Root, relative string, st stamp, digest string) string {
	ext := strings.ToLower(extension(relative))
	if st.Size <= 0 || (ext == ".pdf" && st.Size > 16<<20) || (ext != ".pdf" && st.Size > 1<<20) {
		return ""
	}
	if ext != ".pdf" && ext != ".txt" && ext != ".md" {
		return ""
	}
	f, err := root.Open(relative)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, st.Size+1))
	if err != nil || int64(len(data)) != st.Size || ctx.Err() != nil {
		return ""
	}
	original := sha256.Sum256(data)
	if hex.EncodeToString(original[:]) != digest {
		return ""
	}
	if ext == ".pdf" {
		data = normalizeDuplicatePDF(ctx, data)
	} else {
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return ""
		}
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	}
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return ext + ":" + hex.EncodeToString(sum[:])
}
