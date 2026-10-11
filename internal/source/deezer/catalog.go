package deezer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/greed/cadence/internal/source"
)

const (
	apiBase       = "https://api.deezer.com/2.0"
	searchLimit   = 20
	playlistPage  = 100
	playlistLimit = 1000
	errNoData     = 800
)

var englishHeaders = map[string]string{"Accept-Language": "en-US,en;q=0.9"}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    int    `json:"code"`
}

type apiArtist struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type apiAlbum struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	CoverXL string `json:"cover_xl"`
}

type apiTrack struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	Duration int64     `json:"duration"`
	ISRC     string    `json:"isrc"`
	Link     string    `json:"link"`
	Readable *bool     `json:"readable"`
	Artist   apiArtist `json:"artist"`
	Album    apiAlbum  `json:"album"`
}

type trackPage struct {
	Data  []apiTrack `json:"data"`
	Total int        `json:"total"`
	Next  string     `json:"next"`
	Error *apiError  `json:"error"`
}

type collection struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Name      string    `json:"name"`
	Link      string    `json:"link"`
	CoverXL   string    `json:"cover_xl"`
	PictureXL string    `json:"picture_xl"`
	NbTracks  int       `json:"nb_tracks"`
	Tracks    trackPage `json:"tracks"`
	Error     *apiError `json:"error"`
}

func (t apiTrack) toTrack(fallbackArtwork string) source.Track {
	art := t.Album.CoverXL
	if art == "" {
		art = fallbackArtwork
	}
	link := t.Link
	if link == "" {
		link = "https://www.deezer.com/track/" + strconv.FormatInt(t.ID, 10)
	}
	return source.Track{
		Source:     Name,
		Identifier: strconv.FormatInt(t.ID, 10),
		Title:      t.Title,
		Author:     t.Artist.Name,
		Album:      t.Album.Title,
		DurationMs: t.Duration * 1000,
		ISRC:       t.ISRC,
		ArtworkURL: art,
		URI:        link,
	}
}

func (d *Deezer) api(ctx context.Context, path string, query url.Values, out any) error {
	u := apiBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	body, err := d.http.Get(ctx, u, englishHeaders)
	if err != nil {
		return err
	}
	var probe struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error != nil {
		if probe.Error.Code == errNoData {
			return source.Fail(source.SeverityCommon, Name, "not found", nil)
		}
		return source.Fail(source.SeveritySuspicious, Name, "api error", fmt.Errorf("%s (%d): %s", probe.Error.Type, probe.Error.Code, probe.Error.Message))
	}
	return json.Unmarshal(body, out)
}

func playableOnly(tracks []apiTrack, art string) []source.Track {
	out := make([]source.Track, 0, len(tracks))
	for _, t := range tracks {
		if t.Readable != nil && !*t.Readable {
			continue
		}
		out = append(out, t.toTrack(art))
	}
	return out
}

func (d *Deezer) search(ctx context.Context, query string) ([]source.Track, error) {
	var page trackPage
	if err := d.api(ctx, "/search", url.Values{"q": {query}, "limit": {strconv.Itoa(searchLimit)}}, &page); err != nil {
		return nil, err
	}
	return playableOnly(page.Data, ""), nil
}

func (d *Deezer) Track(ctx context.Context, identifier string) (source.Track, error) {
	var t apiTrack
	if err := d.api(ctx, "/track/"+url.PathEscape(identifier), nil, &t); err != nil {
		return source.Track{}, err
	}
	return t.toTrack(""), nil
}

func (d *Deezer) ByISRC(ctx context.Context, isrc string) (source.Track, error) {
	if hit, ok := d.isrc.Get(isrc); ok {
		return hit, nil
	}
	t, err := d.Track(ctx, "isrc:"+isrc)
	if err != nil {
		return source.Track{}, err
	}
	d.isrc.Set(isrc, t)
	return t, nil
}

func (d *Deezer) album(ctx context.Context, id string) (source.LoadResult, error) {
	var c collection
	if err := d.api(ctx, "/album/"+url.PathEscape(id), nil, &c); err != nil {
		return source.LoadResult{}, err
	}
	tracks := playableOnly(c.Tracks.Data, c.CoverXL)
	for i := range tracks {
		if tracks[i].Album == "" {
			tracks[i].Album = c.Title
		}
	}
	return source.LoadResult{Type: source.LoadPlaylist, Tracks: tracks, Playlist: &source.Playlist{Name: c.Title, ArtworkURL: c.CoverXL, URI: c.Link, Total: len(tracks)}}, nil
}

func (d *Deezer) playlist(ctx context.Context, id string) (source.LoadResult, error) {
	var c collection
	if err := d.api(ctx, "/playlist/"+url.PathEscape(id), url.Values{"limit": {"0"}}, &c); err != nil {
		return source.LoadResult{}, err
	}
	var all []source.Track
	for index := 0; index < playlistLimit; index += playlistPage {
		var page trackPage
		if err := d.api(ctx, "/playlist/"+url.PathEscape(id)+"/tracks", url.Values{"index": {strconv.Itoa(index)}, "limit": {strconv.Itoa(playlistPage)}}, &page); err != nil {
			return source.LoadResult{}, err
		}
		all = append(all, playableOnly(page.Data, "")...)
		if page.Next == "" || len(page.Data) == 0 {
			break
		}
	}
	return source.LoadResult{Type: source.LoadPlaylist, Tracks: all, Playlist: &source.Playlist{Name: c.Title, ArtworkURL: c.PictureXL, URI: c.Link, Total: c.NbTracks}}, nil
}

func (d *Deezer) artistTop(ctx context.Context, id string) (source.LoadResult, error) {
	var c collection
	if err := d.api(ctx, "/artist/"+url.PathEscape(id), nil, &c); err != nil {
		return source.LoadResult{}, err
	}
	var page trackPage
	if err := d.api(ctx, "/artist/"+url.PathEscape(id)+"/top", url.Values{"limit": {"50"}}, &page); err != nil {
		return source.LoadResult{}, err
	}
	name := c.Name
	return source.LoadResult{Type: source.LoadPlaylist, Tracks: playableOnly(page.Data, ""), Playlist: &source.Playlist{Name: name, ArtworkURL: c.PictureXL, URI: c.Link, Total: len(page.Data)}}, nil
}
