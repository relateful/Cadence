package source

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

type Track struct {
	Source     string `json:"source"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	Author     string `json:"author"`
	Album      string `json:"album,omitempty"`
	DurationMs int64  `json:"durationMs"`
	ISRC       string `json:"isrc,omitempty"`
	ArtworkURL string `json:"artworkUrl,omitempty"`
	URI        string `json:"uri,omitempty"`
	IsStream   bool   `json:"isStream,omitempty"`
}

func (t Track) Key() string {
	return t.Source + ":" + t.Identifier
}

func (t Track) Encode() string {
	raw, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func DecodeTrack(encoded string) (Track, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Track{}, fmt.Errorf("decode track: %w", err)
	}
	var t Track
	if err := json.Unmarshal(raw, &t); err != nil {
		return Track{}, fmt.Errorf("decode track: %w", err)
	}
	if t.Source == "" || t.Identifier == "" {
		return Track{}, fmt.Errorf("decode track: missing source or identifier")
	}
	return t, nil
}

type LoadType string

const (
	LoadTrack    LoadType = "track"
	LoadPlaylist LoadType = "playlist"
	LoadSearch   LoadType = "search"
	LoadEmpty    LoadType = "empty"
	LoadError    LoadType = "error"
)

type Playlist struct {
	Name       string `json:"name"`
	ArtworkURL string `json:"artworkUrl,omitempty"`
	URI        string `json:"uri,omitempty"`
	Total      int    `json:"total"`
}

type LoadResult struct {
	Type     LoadType  `json:"loadType"`
	Tracks   []Track   `json:"tracks,omitempty"`
	Playlist *Playlist `json:"playlist,omitempty"`
	Error    *Failure  `json:"error,omitempty"`
}

func Empty() LoadResult {
	return LoadResult{Type: LoadEmpty}
}

func Single(t Track) LoadResult {
	return LoadResult{Type: LoadTrack, Tracks: []Track{t}}
}

func Results(tracks []Track) LoadResult {
	if len(tracks) == 0 {
		return Empty()
	}
	return LoadResult{Type: LoadSearch, Tracks: tracks}
}
