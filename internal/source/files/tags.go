package files

import (
	"encoding/binary"
	"strings"
	"unicode/utf16"

	"github.com/greed/cadence/internal/codec"
)

type tags struct {
	title  string
	artist string
	album  string
}

var frameIDs = map[string]string{
	"TIT2": "title", "TT2": "title",
	"TPE1": "artist", "TP1": "artist",
	"TALB": "album", "TAL": "album",
}

func syncsafe(b []byte) int {
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 | int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

func readTags(p []byte) tags {
	var out tags
	end := min(len(p), codec.ID3Size(p))
	if end <= 10 {
		return out
	}
	version, flags := p[3], p[5]
	if flags&0x80 != 0 || version < 2 || version > 4 {
		return out
	}
	at := 10
	if flags&0x40 != 0 && version >= 3 && at+4 <= end {
		ext := int(binary.BigEndian.Uint32(p[at:]))
		if version == 4 {
			ext = syncsafe(p[at:])
		} else {
			ext += 4
		}
		at += ext
	}
	idLen, headLen := 4, 10
	if version == 2 {
		idLen, headLen = 3, 6
	}
	for at+headLen <= end {
		id := string(p[at : at+idLen])
		if id[0] == 0 {
			break
		}
		var size int
		switch version {
		case 2:
			size = int(p[at+3])<<16 | int(p[at+4])<<8 | int(p[at+5])
		case 3:
			size = int(binary.BigEndian.Uint32(p[at+4:]))
		default:
			size = syncsafe(p[at+4:])
		}
		body := at + headLen
		if size <= 0 || body+size > end {
			break
		}
		switch frameIDs[id] {
		case "title":
			out.title = text(p[body : body+size])
		case "artist":
			out.artist = text(p[body : body+size])
		case "album":
			out.album = text(p[body : body+size])
		}
		at = body + size
	}
	return out
}

func text(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	enc, b := b[0], b[1:]
	var s string
	switch enc {
	case 0:
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		s = string(r)
	case 1, 2:
		s = utf16Text(b, enc == 2)
	default:
		s = string(b)
	}
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.ToValidUTF8(s, ""))
}

func utf16Text(b []byte, bigEndian bool) string {
	if len(b) >= 2 {
		switch {
		case b[0] == 0xFF && b[1] == 0xFE:
			bigEndian, b = false, b[2:]
		case b[0] == 0xFE && b[1] == 0xFF:
			bigEndian, b = true, b[2:]
		}
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u := binary.LittleEndian.Uint16(b[i:])
		if bigEndian {
			u = binary.BigEndian.Uint16(b[i:])
		}
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return string(utf16.Decode(units))
}
