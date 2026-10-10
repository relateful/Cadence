package applemusic

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/source"
)

const (
	Name          = "applemusic"
	SearchPrefix  = "amsearch:"
	apiBase       = "https://api.music.apple.com/v1"
	searchLimit   = 20
	pageLimit     = 100
	playlistLimit = 1000
	artworkSize   = "1000"
)

var errTokenMissing = errors.New("apple music developer token not found in the web bundle")

var linkPattern = regexp.MustCompile(`^https?://(?:beta\.)?music\.apple\.com/([a-z]{2})/(album|playlist|song|artist)/(?:[^/?#]+/)?([^/?#]+)(?:\?i=(\d+))?`)

type AppleMusic struct {
	http       *httpx.Client
	tokens     *tokenSource
	storefront string
}

func New(client *httpx.Client, fixedToken, storefront string) *AppleMusic {
	if storefront == "" {
		storefront = "us"
	}
	return &AppleMusic{http: client, tokens: &tokenSource{http: client, fixed: fixedToken}, storefront: storefront}
}

func (a *AppleMusic) Name() string         { return Name }
func (a *AppleMusic) SearchPrefix() string { return SearchPrefix }

func (a *AppleMusic) MatchesURL(rawURL string) bool {
	return linkPattern.MatchString(rawURL)
}

func (a *AppleMusic) get(ctx context.Context, path string, query url.Values, out any) error {
	u := apiBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := a.tokens.get(ctx, attempt > 0)
		if err != nil {
			return source.Fail(source.SeverityFault, Name, "metadata unavailable", err)
		}
		body, err := a.http.Get(ctx, u, map[string]string{"Authorization": "Bearer " + token, "Origin": origin})
		if (httpx.IsStatus(err, 401) || httpx.IsStatus(err, 403)) && attempt == 0 {
			continue
		}
		if err != nil {
			return source.Classify(Name, err)
		}
		return json.Unmarshal(body, out)
	}
	return source.Fail(source.SeveritySuspicious, Name, "token refused", nil)
}

type artwork struct {
	URL string `json:"url"`
}

type song struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		Name             string  `json:"name"`
		ArtistName       string  `json:"artistName"`
		AlbumName        string  `json:"albumName"`
		DurationInMillis int64   `json:"durationInMillis"`
		ISRC             string  `json:"isrc"`
		URL              string  `json:"url"`
		Artwork          artwork `json:"artwork"`
	} `json:"attributes"`
}

func artworkURL(a artwork) string {
	return strings.NewReplacer("{w}", artworkSize, "{h}", artworkSize).Replace(a.URL)
}

func (s song) toTrack() source.Track {
	return source.Track{
		Source:     Name,
		Identifier: s.ID,
		Title:      s.Attributes.Name,
		Author:     s.Attributes.ArtistName,
		Album:      s.Attributes.AlbumName,
		DurationMs: s.Attributes.DurationInMillis,
		ISRC:       s.Attributes.ISRC,
		ArtworkURL: artworkURL(s.Attributes.Artwork),
		URI:        s.Attributes.URL,
	}
}

func songs(items []song) []source.Track {
	out := make([]source.Track, 0, len(items))
	for _, s := range items {
		if s.Type != "" && s.Type != "songs" && s.Type != "library-songs" {
			continue
		}
		out = append(out, s.toTrack())
	}
	return out
}

func (a *AppleMusic) catalog(path string) string {
	return "/catalog/" + a.storefront + path
}

func (a *AppleMusic) Search(ctx context.Context, query string) ([]source.Track, error) {
	var res struct {
		Results struct {
			Songs struct {
				Data []song `json:"data"`
			} `json:"songs"`
		} `json:"results"`
	}
	if err := a.get(ctx, a.catalog("/search"), url.Values{"term": {query}, "types": {"songs"}, "limit": {strconv.Itoa(searchLimit)}}, &res); err != nil {
		return nil, err
	}
	return songs(res.Results.Songs.Data), nil
}

func (a *AppleMusic) Track(ctx context.Context, id string) (source.Track, error) {
	var res struct {
		Data []song `json:"data"`
	}
	if err := a.get(ctx, a.catalog("/songs/"+url.PathEscape(id)), nil, &res); err != nil {
		return source.Track{}, err
	}
	if len(res.Data) == 0 {
		return source.Track{}, source.Fail(source.SeverityCommon, Name, "not found", nil)
	}
	return res.Data[0].toTrack(), nil
}

type collection struct {
	Data []struct {
		Attributes struct {
			Name    string  `json:"name"`
			URL     string  `json:"url"`
			Artwork artwork `json:"artwork"`
		} `json:"attributes"`
		Relationships struct {
			Tracks struct {
				Data []song `json:"data"`
				Next string `json:"next"`
			} `json:"tracks"`
		} `json:"relationships"`
	} `json:"data"`
}

func (a *AppleMusic) collection(ctx context.Context, kind, id string) (source.LoadResult, error) {
	var c collection
	if err := a.get(ctx, a.catalog("/"+kind+"/"+url.PathEscape(id)), nil, &c); err != nil {
		return source.LoadResult{}, err
	}
	if len(c.Data) == 0 {
		return source.Empty(), nil
	}
	head := c.Data[0]
	tracks := songs(head.Relationships.Tracks.Data)
	next := head.Relationships.Tracks.Next
	for next != "" && len(tracks) < playlistLimit {
		var page struct {
			Data []song `json:"data"`
			Next string `json:"next"`
		}
		path, query := splitNext(next)
		query.Set("limit", strconv.Itoa(pageLimit))
		if err := a.get(ctx, path, query, &page); err != nil {
			break
		}
		tracks = append(tracks, songs(page.Data)...)
		next = page.Next
	}
	return source.LoadResult{Type: source.LoadPlaylist, Tracks: tracks, Playlist: &source.Playlist{Name: head.Attributes.Name, ArtworkURL: artworkURL(head.Attributes.Artwork), URI: head.Attributes.URL, Total: len(tracks)}}, nil
}

func splitNext(next string) (string, url.Values) {
	path, raw, _ := strings.Cut(strings.TrimPrefix(next, "/v1"), "?")
	q, _ := url.ParseQuery(raw)
	return path, q
}

func (a *AppleMusic) artistTop(ctx context.Context, id string) (source.LoadResult, error) {
	var res struct {
		Data []song `json:"data"`
	}
	if err := a.get(ctx, a.catalog("/artists/"+url.PathEscape(id)+"/view/top-songs"), url.Values{"limit": {"20"}}, &res); err != nil {
		return source.LoadResult{}, err
	}
	tracks := songs(res.Data)
	name := ""
	if len(tracks) > 0 {
		name = tracks[0].Author
	}
	return source.LoadResult{Type: source.LoadPlaylist, Tracks: tracks, Playlist: &source.Playlist{Name: name, Total: len(tracks)}}, nil
}

func (a *AppleMusic) LoadURL(ctx context.Context, rawURL string) (source.LoadResult, error) {
	m := linkPattern.FindStringSubmatch(rawURL)
	if m == nil {
		return source.Empty(), nil
	}
	kind, id, songID := m[2], m[3], m[4]
	if songID != "" || kind == "song" {
		if songID == "" {
			songID = id
		}
		t, err := a.Track(ctx, songID)
		if err != nil {
			return source.LoadResult{}, err
		}
		return source.Single(t), nil
	}
	switch kind {
	case "album":
		return a.collection(ctx, "albums", id)
	case "playlist":
		return a.collection(ctx, "playlists", id)
	default:
		return a.artistTop(ctx, id)
	}
}
