package files

import (
	"path"
	"strings"

	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/source"
)

const (
	headBytes     = 64 << 10
	audioProbe    = 16 << 10
	maxTagSkip    = 8 << 20
	unknownArtist = "Unknown artist"
)

type fetchFunc func(offset, n int64) ([]byte, error)

type probed struct {
	tags       tags
	durationMs int64
	sized      bool
}

func probe(src string, fetch fetchFunc, size int64) (probed, error) {
	head, err := fetch(0, headBytes)
	if err != nil {
		return probed{}, err
	}
	out := probed{tags: readTags(head), sized: size > 0}
	tagEnd := codec.ID3Size(head)
	audio := head[min(tagEnd, len(head)):]
	if len(audio) < audioProbe && len(head) == headBytes {
		if tagEnd > maxTagSkip || (out.sized && int64(tagEnd) >= size) {
			return probed{}, source.Fail(source.SeverityCommon, src, "not an mp3 file cadence can play", nil)
		}
		if audio, err = fetch(int64(tagEnd), audioProbe); err != nil {
			return probed{}, err
		}
	}
	remaining := int64(0)
	if out.sized {
		remaining = size - int64(tagEnd)
	}
	info, ok := codec.ProbeMP3(audio, remaining)
	if !ok {
		return probed{}, source.Fail(source.SeverityCommon, src, "not an mp3 file cadence can play", nil)
	}
	out.durationMs = info.DurationMs
	return out, nil
}

func (p probed) track(src, identifier, uri, name string) source.Track {
	title := p.tags.title
	if title == "" {
		title = strings.TrimSuffix(name, path.Ext(name))
	}
	author := p.tags.artist
	if author == "" {
		author = unknownArtist
	}
	return source.Track{
		Source:     src,
		Identifier: identifier,
		Title:      title,
		Author:     author,
		Album:      p.tags.album,
		DurationMs: p.durationMs,
		URI:        uri,
		IsStream:   !p.sized && p.durationMs == 0,
	}
}

func mp3Name(name string) bool {
	return strings.EqualFold(path.Ext(name), ".mp3")
}
