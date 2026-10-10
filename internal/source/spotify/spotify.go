package spotify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/source"
)

const (
	Name          = "spotify"
	SearchPrefix  = "spsearch:"
	apiBase       = "https://api.spotify.com/v1"
	tokenURL      = "https://accounts.spotify.com/api/token"
	searchLimit   = 10
	pageLimit     = 100
	playlistLimit = 1000
	tokenMargin   = time.Minute
)

var (
	linkPattern = regexp.MustCompile(`^https?://open\.spotify\.com/(?:intl-[a-z]{2}(?:-[a-z]{2})?/)?(track|album|playlist)/([A-Za-z0-9]{22})`)
	uriPattern  = regexp.MustCompile(`^spotify:(track|album|playlist):([A-Za-z0-9]{22})$`)
	shortLink   = regexp.MustCompile(`^https?://spotify\.link/`)
)

var ErrNoCredentials = errors.New("SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET are not set")

type Spotify struct {
	http         *httpx.Client
	clientID     string
	clientSecret string
	mu           sync.Mutex
	token        string
	expires      time.Time
}

func New(client *httpx.Client, clientID, clientSecret string) *Spotify {
	return &Spotify{http: client, clientID: clientID, clientSecret: clientSecret}
}

func (s *Spotify) Name() string         { return Name }
func (s *Spotify) SearchPrefix() string { return SearchPrefix }

func (s *Spotify) MatchesURL(rawURL string) bool {
	return linkPattern.MatchString(rawURL) || uriPattern.MatchString(rawURL) || shortLink.MatchString(rawURL)
}

func (s *Spotify) accessToken(ctx context.Context) (string, error) {
	if s.clientID == "" || s.clientSecret == "" {
		return "", source.Fail(source.SeverityFault, Name, "metadata unavailable", ErrNoCredentials)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expires) {
		return s.token, nil
	}
	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.clientID+":"+s.clientSecret)))
	body, err := s.http.Send(ctx, req)
	if err != nil {
		return "", source.Classify(Name, err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", err
	}
	s.token = tok.AccessToken
	s.expires = time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second - tokenMargin)
	return s.token, nil
}

func (s *Spotify) get(ctx context.Context, path string, query url.Values, out any) error {
	token, err := s.accessToken(ctx)
	if err != nil {
		return err
	}
	u := apiBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	body, err := s.http.Get(ctx, u, map[string]string{"Authorization": "Bearer " + token})
	if httpx.IsStatus(err, 401) {
		s.mu.Lock()
		s.token = ""
		s.mu.Unlock()
	}
	if err != nil {
		return source.Classify(Name, err)
	}
	return json.Unmarshal(body, out)
}

type image struct {
	URL string `json:"url"`
}

type artist struct {
	Name string `json:"name"`
}

type album struct {
	Name   string  `json:"name"`
	Images []image `json:"images"`
}

type track struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	DurationMs  int64    `json:"duration_ms"`
	IsLocal     bool     `json:"is_local"`
	Artists     []artist `json:"artists"`
	Album       *album   `json:"album"`
	ExternalIDs struct {
		ISRC string `json:"isrc"`
	} `json:"external_ids"`
	ExternalURLs struct {
		Spotify string `json:"spotify"`
	} `json:"external_urls"`
}

func (t track) toTrack(fallback *album) source.Track {
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	al := t.Album
	if al == nil {
		al = fallback
	}
	out := source.Track{
		Source:     Name,
		Identifier: t.ID,
		Title:      t.Name,
		Author:     strings.Join(names, ", "),
		DurationMs: t.DurationMs,
		ISRC:       t.ExternalIDs.ISRC,
		URI:        t.ExternalURLs.Spotify,
	}
	if out.URI == "" {
		out.URI = "https://open.spotify.com/track/" + t.ID
	}
	if al != nil {
		out.Album = al.Name
		if len(al.Images) > 0 {
			out.ArtworkURL = al.Images[0].URL
		}
	}
	return out
}

func convert(items []track, fallback *album) []source.Track {
	out := make([]source.Track, 0, len(items))
	for _, t := range items {
		if t.ID == "" || t.IsLocal {
			continue
		}
		out = append(out, t.toTrack(fallback))
	}
	return out
}

func (s *Spotify) Search(ctx context.Context, query string) ([]source.Track, error) {
	var res struct {
		Tracks struct {
			Items []track `json:"items"`
		} `json:"tracks"`
	}
	if err := s.get(ctx, "/search", url.Values{"q": {query}, "type": {"track"}, "limit": {strconv.Itoa(searchLimit)}}, &res); err != nil {
		return nil, err
	}
	return convert(res.Tracks.Items, nil), nil
}

func (s *Spotify) Track(ctx context.Context, id string) (source.Track, error) {
	var t track
	if err := s.get(ctx, "/tracks/"+url.PathEscape(id), nil, &t); err != nil {
		return source.Track{}, err
	}
	return t.toTrack(nil), nil
}

func (s *Spotify) LoadURL(ctx context.Context, rawURL string) (source.LoadResult, error) {
	if shortLink.MatchString(rawURL) {
		expanded, err := expand(ctx, rawURL)
		if err != nil {
			return source.LoadResult{}, err
		}
		rawURL = expanded
	}
	m := linkPattern.FindStringSubmatch(rawURL)
	if m == nil {
		m = uriPattern.FindStringSubmatch(rawURL)
	}
	if m == nil {
		return source.Empty(), nil
	}
	switch m[1] {
	case "track":
		t, err := s.Track(ctx, m[2])
		if err != nil {
			return source.LoadResult{}, err
		}
		return source.Single(t), nil
	case "album":
		return s.album(ctx, m[2])
	default:
		return s.playlist(ctx, m[2])
	}
}

func (s *Spotify) album(ctx context.Context, id string) (source.LoadResult, error) {
	var al struct {
		album
		ExternalURLs struct {
			Spotify string `json:"spotify"`
		} `json:"external_urls"`
		Tracks struct {
			Items []track `json:"items"`
			Total int     `json:"total"`
		} `json:"tracks"`
	}
	if err := s.get(ctx, "/albums/"+url.PathEscape(id), nil, &al); err != nil {
		return source.LoadResult{}, err
	}
	tracks := convert(al.Tracks.Items, &al.album)
	art := ""
	if len(al.Images) > 0 {
		art = al.Images[0].URL
	}
	return source.LoadResult{Type: source.LoadPlaylist, Tracks: tracks, Playlist: &source.Playlist{Name: al.Name, ArtworkURL: art, URI: al.ExternalURLs.Spotify, Total: al.Tracks.Total}}, nil
}

func (s *Spotify) playlist(ctx context.Context, id string) (source.LoadResult, error) {
	var meta struct {
		Name   string  `json:"name"`
		Images []image `json:"images"`
		URLs   struct {
			Spotify string `json:"spotify"`
		} `json:"external_urls"`
	}
	if err := s.get(ctx, "/playlists/"+url.PathEscape(id), url.Values{"fields": {"name,images,external_urls"}}, &meta); err != nil {
		return source.LoadResult{}, err
	}
	var all []source.Track
	total := 0
	for offset := 0; offset < playlistLimit; offset += pageLimit {
		var page struct {
			Items []struct {
				Track *track `json:"track"`
			} `json:"items"`
			Total int    `json:"total"`
			Next  string `json:"next"`
		}
		if err := s.get(ctx, "/playlists/"+url.PathEscape(id)+"/tracks", url.Values{"offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(pageLimit)}}, &page); err != nil {
			return source.LoadResult{}, err
		}
		total = page.Total
		for _, it := range page.Items {
			if it.Track != nil {
				all = append(all, convert([]track{*it.Track}, nil)...)
			}
		}
		if page.Next == "" {
			break
		}
	}
	art := ""
	if len(meta.Images) > 0 {
		art = meta.Images[0].URL
	}
	return source.LoadResult{Type: source.LoadPlaylist, Tracks: all, Playlist: &source.Playlist{Name: meta.Name, ArtworkURL: art, URI: meta.URLs.Spotify, Total: total}}, nil
}

func expand(ctx context.Context, short string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, short, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	res.Body.Close()
	loc := res.Header.Get("Location")
	if loc == "" {
		return "", source.Fail(source.SeverityCommon, Name, "short link did not redirect", nil)
	}
	return loc, nil
}
