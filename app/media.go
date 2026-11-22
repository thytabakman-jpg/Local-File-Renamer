package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

func isMedia(name string) bool {
	switch strings.ToLower(extension(name)) {
	case ".mp3", ".mp4", ".m4a", ".mov", ".m4v", ".wav", ".flac", ".ogg", ".opus", ".aac", ".avi", ".mkv", ".webm":
		return true
	}
	return false
}

func inspectMediaTitle(name string, e evidence) decision {
	d := decision{Name: name, Inspection: inspection{Metadata: e.Title != "" || e.Author != "", AI: "Skipped (media tags only)", Detail: e.Note}}
	stem := baseName(name)
	switch {
	case !meaningful(e.Title):
		d.Note = joinNote(e.Note, "Media title could not be verified")
		if !meaningful(stem) {
			d.Note = joinNote(d.Note, "Filename is too vague to identify the media")
		}
	case !meaningful(stem):
		d.Note = "Filename is vague. Embedded title: " + chars(e.Title, 220)
	case norm(stem) != norm(e.Title):
		d.Note = "Filename differs from embedded title: " + chars(e.Title, 220)
	}
	return d
}

const maxTagBytes = 1 << 20

func mp3Evidence(path string) evidence {
	f, err := os.Open(path)
	if err != nil {
		return evidence{Note: err.Error()}
	}
	defer f.Close()
	var e evidence
	var header [10]byte
	if _, err := io.ReadFull(f, header[:]); err == nil && string(header[:3]) == "ID3" {
		size := synchsafe(header[6:])
		if size > 0 {
			data := make([]byte, min(size, maxTagBytes))
			if _, err := io.ReadFull(f, data); err == nil {
				e = id3Frames(header[3], header[5], data)
			}
		}
	}
	if e.Title == "" {
		var trailer [128]byte
		if end, err := f.Seek(-int64(len(trailer)), io.SeekEnd); err == nil {
			if _, err := f.ReadAt(trailer[:], end); err == nil && string(trailer[:3]) == "TAG" {
				e.Title = tagText(trailer[3:33], 0)
				if e.Author == "" {
					e.Author = tagText(trailer[33:63], 0)
				}
			}
		}
	}
	if !meaningful(e.Title) {
		e.Note = "No usable embedded audio title"
	}
	return e
}

func synchsafe(data []byte) int {
	value := 0
	for _, b := range data {
		if b >= 128 {
			return -1
		}
		value = value<<7 | int(b)
	}
	return value
}

func id3Frames(version, flags byte, data []byte) evidence {
	var e evidence
	frameHeader := 10
	if version == 2 {
		if flags&0x40 != 0 { // Compressed ID3v2.2 tags are uncommon.
			return e
		}
		frameHeader = 6
	} else if version != 3 && version != 4 {
		return e
	}
	if version < 4 && flags&0x80 != 0 {
		data = bytes.ReplaceAll(data, []byte{0xff, 0}, []byte{0xff})
	}
	if version >= 3 && flags&0x40 != 0 {
		if len(data) < 4 {
			return e
		}
		n := 4 + int(binary.BigEndian.Uint32(data))
		if version == 4 {
			n = synchsafe(data[:4])
		}
		if n < 4 || n > len(data) {
			return e
		}
		data = data[n:]
	}
	for len(data) >= frameHeader && data[0] != 0 {
		var name string
		var size int
		var format byte
		switch version {
		case 2:
			name = string(data[:3])
			size = int(data[3])<<16 | int(data[4])<<8 | int(data[5])
		case 3:
			name = string(data[:4])
			size = int(binary.BigEndian.Uint32(data[4:8]))
			format = data[9]
		case 4:
			name = string(data[:4])
			size = synchsafe(data[4:8])
			format = data[9]
		}
		if size < 1 || size > len(data)-frameHeader {
			break
		}
		body := data[frameHeader : frameHeader+size]
		data = data[frameHeader+size:]
		if version == 3 && format&0xe0 != 0 || version == 4 && format&0x4d != 0 {
			continue // Compression, encryption, grouping, or data length prefix.
		}
		if version == 4 && (flags&0x80 != 0 || format&0x02 != 0) {
			body = bytes.ReplaceAll(body, []byte{0xff, 0}, []byte{0xff})
		}
		switch name {
		case "TIT2", "TT2":
			e.Title = tagText(body[1:], body[0])
		case "TPE1", "TP1":
			e.Author = tagText(body[1:], body[0])
		}
		if e.Title != "" && e.Author != "" {
			break
		}
	}
	return e
}

func tagText(data []byte, encoding byte) string {
	var result string
	switch encoding {
	case 0:
		runes := make([]rune, len(data))
		for i, b := range data {
			runes[i] = rune(b)
		}
		result = string(runes)
	case 1, 2:
		var order binary.ByteOrder = binary.BigEndian
		if len(data) >= 2 {
			switch binary.BigEndian.Uint16(data) {
			case 0xfffe:
				order = binary.LittleEndian
				data = data[2:]
			case 0xfeff:
				data = data[2:]
			}
		}
		words := make([]uint16, len(data)/2)
		for i := range words {
			words[i] = order.Uint16(data[2*i:])
		}
		result = string(utf16.Decode(words))
	case 3:
		if !utf8.Valid(data) {
			return ""
		}
		result = string(data)
	}
	if head, _, ok := strings.Cut(result, "\x00"); ok {
		result = head
	}
	return clean(chars(result, 750))
}

type mp4Box struct {
	kind      string
	body, end int64
}

type mediaReader struct {
	file      io.ReaderAt
	remaining int
}

func (r *mediaReader) boxes(start, end int64, visit func(mp4Box) bool) {
	for offset := start; end-offset >= 8 && r.remaining > 0; {
		r.remaining--
		var header [16]byte
		if _, err := r.file.ReadAt(header[:8], offset); err != nil {
			break
		}
		length := int64(binary.BigEndian.Uint32(header[:4]))
		head := int64(8)
		switch length {
		case 0:
			length = end - offset
		case 1:
			if end-offset < 16 {
				return
			}
			if _, err := r.file.ReadAt(header[8:], offset+8); err != nil {
				return
			}
			length = int64(binary.BigEndian.Uint64(header[8:]))
			head = 16
		}
		if length < head || length > end-offset {
			break
		}
		box := mp4Box{kind: string(header[4:8]), body: offset + head, end: offset + length}
		if !visit(box) {
			break
		}
		offset += length
	}
}

func (r *mediaReader) titles(start, end int64, depth int, e *evidence) {
	if depth > 6 {
		return
	}
	r.boxes(start, end, func(b mp4Box) bool {
		switch b.kind {
		case "moov", "udta":
			r.titles(b.body, b.end, depth+1, e)
		case "meta":
			r.metadata(b.body+4, b.end, e)
		}
		return e.Title == "" || e.Author == ""
	})
}

func (r *mediaReader) metadata(start, end int64, e *evidence) {
	keys := map[uint32]string{}
	var listing mp4Box
	r.boxes(start, end, func(b mp4Box) bool {
		switch b.kind {
		case "ilst":
			listing = b
		case "keys":
			if b.end-b.body < 8 {
				return true
			}
			var idx uint32
			r.boxes(b.body+8, b.end, func(k mp4Box) bool {
				idx++
				if k.kind == "mdta" && k.end-k.body <= 256 {
					value := make([]byte, k.end-k.body)
					if _, err := r.file.ReadAt(value, k.body); err == nil {
						keys[idx] = string(value)
					}
				}
				return idx < 512
			})
		}
		return true
	})
	r.boxes(listing.body, listing.end, func(b mp4Box) bool {
		key := b.kind
		if indexed, ok := keys[binary.BigEndian.Uint32([]byte(key))]; ok {
			key = indexed
		}
		switch key {
		case "\xa9nam", "com.apple.quicktime.title", "title":
			e.Title = r.text(b)
		case "\xa9ART", "com.apple.quicktime.artist", "artist":
			e.Author = r.text(b)
		}
		return e.Title == "" || e.Author == ""
	})
}

func (r *mediaReader) text(b mp4Box) string {
	var result string
	r.boxes(b.body, b.end, func(d mp4Box) bool {
		if d.kind != "data" || d.end-d.body <= 8 || d.end-d.body > 4096 {
			return true
		}
		data := make([]byte, d.end-d.body)
		if _, err := r.file.ReadAt(data, d.body); err != nil {
			return false
		}
		switch binary.BigEndian.Uint32(data[:4]) & 0xffffff {
		case 1:
			result = tagText(data[8:], 3)
		case 2:
			result = tagText(data[8:], 2)
		}
		return result == ""
	})
	return result
}

func mp4Evidence(path string) evidence {
	f, err := os.Open(path)
	if err != nil {
		return evidence{Note: err.Error()}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return evidence{Note: err.Error()}
	}
	reader := mediaReader{file: f, remaining: 8192}
	var e evidence
	reader.titles(0, info.Size(), 0, &e)
	if !meaningful(e.Title) {
		e.Note = "No usable embedded media title"
	}
	return e
}
