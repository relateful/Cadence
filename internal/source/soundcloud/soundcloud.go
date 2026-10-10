package soundcloud

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/cache"
	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/source"
)

const (
	Name           = "soundcloud"
	SearchPrefix   = "scsearch:"
	searchLimit    = 10
	idBatch        = 50
	playlistLimit  = 500
	clientIDTTL    = 6 * time.Hour
	scrapeCooldown = time.Minute
	trackCacheTTL  = 10 * time.Minute
	trackEntries   = 20_000
	maxSegments    = 2_000
	maxPlaylist    = 1 << 20
)

var (
	linkPattern     = regexp.MustCompile(`^https?://(?:www\.|m\.)?soundcloud\.com/[^/?#]+/[^?#]+`)
	shortPattern    = regexp.MustCompile(`^https?://on\.soundcloud\.com/`)
	scriptPattern   = regexp.MustCompile(`<script[^>]+src="(https?://[^"]+/assets/[^"]+\.js)"`)
	clientIDPattern = regexp.MustCompile(`client_id\s*[:=]\s*"([A-Za-z0-9]{32})"`)

	ErrNoClientID  = errors.New("could not find a soundcloud client id in the web player")
	ErrNoTranscode = errors.New("track has no full-length mp3 stream")
)

type Endpoints struct {
	API string
	Web string
}

var DefaultEndpoints = Endpoints{API: "https://api-v2.soundcloud.com", Web: "https://soundcloud.com"}

type SoundCloud struct {
	http        *httpx.Client
	ep          Endpoints
	fixedID     string
	mu          sync.Mutex
	clientID    string
	idExpires   time.Time
	failedUntil time.Time
	lastFailure error
	log         *slog.Logger
	tracks      *cache.LRU[string, apiTrack]
}

func New(client *httpx.Client, clientID string, ep Endpoints, log *slog.Logger) *SoundCloud {
	return &SoundCloud{http: client, ep: ep, fixedID: clientID, log: log.With("source", Name), tracks: cache.New[string, apiTrack](trackEntries, trackCacheTTL)}
}

func (s *SoundCloud) Name() string         { return Name }
func (s *SoundCloud) SearchPrefix() string { return SearchPrefix }

func (s *SoundCloud) MatchesURL(rawURL string) bool {
	return linkPattern.MatchString(rawURL) || shortPattern.MatchString(rawURL)
}

type transcoding struct {
	URL     string `json:"url"`
	Snipped bool   `json:"snipped"`
	Format  struct {
		Protocol string `json:"protocol"`
		MimeType string `json:"mime_type"`
	} `json:"format"`
}

type apiTrack struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Duration     int64  `json:"duration"`
	PermalinkURL string `json:"permalink_url"`
	ArtworkURL   string `json:"artwork_url"`
	Policy       string `json:"policy"`
	Streamable   *bool  `json:"streamable"`
	User         struct {
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	} `json:"user"`
	Publisher *struct {
		Artist string `json:"artist"`
		ISRC   string `json:"isrc"`
	} `json:"publisher_metadata"`
	Media struct {
		Transcodings []transcoding `json:"transcodings"`
	} `json:"media"`
}

func (t apiTrack) playable() bool {
	if t.Policy == "SNIP" || t.Policy == "BLOCK" || (t.Streamable != nil && !*t.Streamable) {
		return false
	}
	_, ok := t.bestTranscoding()
	return ok
}

func (t apiTrack) bestTranscoding() (transcoding, bool) {
	var hls *transcoding
	for i, tc := range t.Media.Transcodings {
		if tc.Snipped || !strings.HasPrefix(tc.Format.MimeType, "audio/mpeg") {
			continue
		}
		switch tc.Format.Protocol {
		case "progressive":
			return tc, true
		case "hls":
			if hls == nil {
				hls = &t.Media.Transcodings[i]
			}
		}
	}
	if hls != nil {
		return *hls, true
	}
	return transcoding{}, false
}

func (t apiTrack) toTrack() source.Track {
	author := t.User.Username
	isrc := ""
	if t.Publisher != nil {
		if t.Publisher.Artist != "" {
			author = t.Publisher.Artist
		}
		isrc = t.Publisher.ISRC
	}
	art := t.ArtworkURL
	if art == "" {
		art = t.User.AvatarURL
	}
	return source.Track{
		Source:     Name,
		Identifier: strconv.FormatInt(t.ID, 10),
		Title:      t.Title,
		Author:     author,
		DurationMs: t.Duration,
		ISRC:       source.NormalizeISRC(isrc),
		ArtworkURL: strings.Replace(art, "-large.", "-t500x500.", 1),
		URI:        t.PermalinkURL,
	}
}

func (s *SoundCloud) id(ctx context.Context, rejected string) (string, error) {
	if s.fixedID != "" {
		return s.fixedID, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientID != "" && s.clientID != rejected && time.Now().Before(s.idExpires) {
		return s.clientID, nil
	}
	if time.Now().Before(s.failedUntil) {
		return "", s.lastFailure
	}
	id, err := s.scrape(ctx)
	if err != nil {
		s.failedUntil, s.lastFailure = time.Now().Add(scrapeCooldown), err
		s.log.Warn("could not find a client id, pausing scrapes", "cooldownSeconds", int(scrapeCooldown.Seconds()), "error", err)
		return "", err
	}
	s.clientID, s.idExpires, s.failedUntil = id, time.Now().Add(clientIDTTL), time.Time{}
	return id, nil
}

func (s *SoundCloud) scrape(ctx context.Context) (string, error) {
	page, err := s.http.Get(ctx, s.ep.Web+"/", nil)
	if err != nil {
		return "", source.Classify(Name, err)
	}
	scripts := scriptPattern.FindAllStringSubmatch(string(page), -1)
	for i := len(scripts) - 1; i >= 0; i-- {
		js, err := s.http.Get(ctx, scripts[i][1], nil)
		if err != nil {
			s.log.Debug("web player script unavailable", "script", scripts[i][1], "error", err)
			continue
		}
		if m := clientIDPattern.FindSubmatch(js); m != nil {
			return string(m[1]), nil
		}
	}
	return "", source.Fail(source.SeveritySuspicious, Name, "client id unavailable", ErrNoClientID)
}

func (s *SoundCloud) api(ctx context.Context, path string, q url.Values, out any) error {
	rejected := ""
	for attempt := 0; attempt < 2; attempt++ {
		id, err := s.id(ctx, rejected)
		if err != nil {
			return err
		}
		rejected = id
		if q == nil {
			q = url.Values{}
		}
		q.Set("client_id", id)
		body, err := s.http.Get(ctx, s.ep.API+path+"?"+q.Encode(), nil)
		if err != nil {
			if attempt == 0 && s.fixedID == "" && (httpx.IsStatus(err, http.StatusUnauthorized) || httpx.IsStatus(err, http.StatusForbidden)) {
				continue
			}
			return source.Classify(Name, err)
		}
		return json.Unmarshal(body, out)
	}
	return source.Fail(source.SeveritySuspicious, Name, "client id rejected twice", nil)
}

func (s *SoundCloud) Search(ctx context.Context, query string) ([]source.Track, error) {
	var res struct {
		Collection []apiTrack `json:"collection"`
	}
	if err := s.api(ctx, "/search/tracks", url.Values{"q": {query}, "limit": {strconv.Itoa(searchLimit)}}, &res); err != nil {
		return nil, err
	}
	return s.keep(res.Collection), nil
}

func (s *SoundCloud) keep(in []apiTrack) []source.Track {
	out := make([]source.Track, 0, len(in))
	for _, t := range in {
		if t.Kind != "" && t.Kind != "track" {
			continue
		}
		if !t.playable() {
			continue
		}
		s.tracks.Set(strconv.FormatInt(t.ID, 10), t)
		out = append(out, t.toTrack())
	}
	return out
}

func (s *SoundCloud) apiTrack(ctx context.Context, id string) (apiTrack, error) {
	if t, ok := s.tracks.Get(id); ok {
		return t, nil
	}
	var t apiTrack
	if err := s.api(ctx, "/tracks/"+url.PathEscape(id), nil, &t); err != nil {
		return apiTrack{}, err
	}
	s.tracks.Set(id, t)
	return t, nil
}

func (s *SoundCloud) Track(ctx context.Context, id string) (source.Track, error) {
	t, err := s.apiTrack(ctx, id)
	if err != nil {
		return source.Track{}, err
	}
	return t.toTrack(), nil
}

func (s *SoundCloud) LoadURL(ctx context.Context, rawURL string) (source.LoadResult, error) {
	var res struct {
		apiTrack
		Kind       string     `json:"kind"`
		Tracks     []apiTrack `json:"tracks"`
		ArtworkURL string     `json:"artwork_url"`
	}
	clean := strings.SplitN(rawURL, "?", 2)[0]
	if err := s.api(ctx, "/resolve", url.Values{"url": {clean}}, &res); err != nil {
		return source.LoadResult{}, err
	}
	switch res.Kind {
	case "track":
		res.apiTrack.Kind = "track"
		kept := s.keep([]apiTrack{res.apiTrack})
		if len(kept) == 0 {
			return source.LoadResult{Type: source.LoadError, Error: source.Fail(source.SeverityCommon, Name, "this track is a preview or cannot be streamed", nil)}, nil
		}
		return source.Single(kept[0]), nil
	case "playlist":
		full, err := s.fill(ctx, res.Tracks)
		if err != nil {
			return source.LoadResult{}, err
		}
		tracks := s.keep(full)
		if len(tracks) == 0 {
			return source.Empty(), nil
		}
		return source.LoadResult{Type: source.LoadPlaylist, Tracks: tracks, Playlist: &source.Playlist{Name: res.Title, ArtworkURL: res.ArtworkURL, URI: res.PermalinkURL, Total: len(res.Tracks)}}, nil
	}
	return source.Empty(), nil
}

func (s *SoundCloud) fill(ctx context.Context, in []apiTrack) ([]apiTrack, error) {
	if len(in) > playlistLimit {
		in = in[:playlistLimit]
	}
	var missing []string
	for _, t := range in {
		if t.Title == "" {
			missing = append(missing, strconv.FormatInt(t.ID, 10))
		}
	}
	got := map[int64]apiTrack{}
	for i := 0; i < len(missing); i += idBatch {
		var batch []apiTrack
		if err := s.api(ctx, "/tracks", url.Values{"ids": {strings.Join(missing[i:min(len(missing), i+idBatch)], ",")}}, &batch); err != nil {
			return nil, err
		}
		for _, t := range batch {
			got[t.ID] = t
		}
	}
	out := make([]apiTrack, 0, len(in))
	for _, t := range in {
		if t.Title == "" {
			full, ok := got[t.ID]
			if !ok {
				continue
			}
			t = full
		}
		t.Kind = "track"
		out = append(out, t)
	}
	return out, nil
}

func (s *SoundCloud) streamURL(ctx context.Context, id string) (string, string, error) {
	t, err := s.apiTrack(ctx, id)
	if err != nil {
		return "", "", err
	}
	tc, ok := t.bestTranscoding()
	if !ok || !t.playable() {
		return "", "", source.Fail(source.SeverityCommon, Name, "stream unavailable", ErrNoTranscode)
	}
	u, err := url.Parse(tc.URL)
	if err != nil {
		return "", "", err
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := s.api(ctx, u.Path, u.Query(), &out); err != nil {
		return "", "", err
	}
	if out.URL == "" {
		return "", "", source.Fail(source.SeverityCommon, Name, "no stream url returned", nil)
	}
	return out.URL, tc.Format.Protocol, nil
}

func (s *SoundCloud) Warm(ctx context.Context, t source.Track) {
	s.apiTrack(ctx, t.Identifier)
}

func (s *SoundCloud) Open(ctx context.Context, t source.Track) (source.Stream, error) {
	u, protocol, err := s.streamURL(ctx, t.Identifier)
	if err != nil {
		return source.Stream{}, err
	}
	if protocol == "progressive" {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return source.Stream{}, err
		}
		res, err := s.http.Stream(ctx, req)
		if err != nil {
			return source.Stream{}, source.Classify(Name, err)
		}
		return source.Stream{Body: res.Body, Format: codec.FormatMP3}, nil
	}
	segments, err := s.playlist(ctx, u)
	if err != nil {
		return source.Stream{}, err
	}
	return source.Stream{Body: &hlsReader{ctx: ctx, http: s.http, segments: segments}, Format: codec.FormatMP3}, nil
}

func (s *SoundCloud) playlist(ctx context.Context, playlistURL string) ([]string, error) {
	body, err := s.http.Get(ctx, playlistURL, nil)
	if err != nil {
		return nil, source.Classify(Name, err)
	}
	if len(body) > maxPlaylist {
		return nil, source.Fail(source.SeverityCommon, Name, "hls playlist too large", nil)
	}
	base, err := url.Parse(playlistURL)
	if err != nil {
		return nil, err
	}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ref, err := base.Parse(line)
		if err != nil {
			return nil, err
		}
		out = append(out, ref.String())
		if len(out) > maxSegments {
			return nil, source.Fail(source.SeverityCommon, Name, "hls playlist has too many segments", nil)
		}
	}
	if len(out) == 0 {
		return nil, source.Fail(source.SeverityCommon, Name, "hls playlist has no segments", nil)
	}
	return out, nil
}

type hlsReader struct {
	ctx      context.Context
	http     *httpx.Client
	segments []string
	next     int
	cur      io.ReadCloser
	closed   bool
}

func (h *hlsReader) Read(p []byte) (int, error) {
	for {
		if h.closed {
			return 0, io.ErrClosedPipe
		}
		if h.cur == nil {
			if h.next >= len(h.segments) {
				return 0, io.EOF
			}
			req, err := http.NewRequest(http.MethodGet, h.segments[h.next], nil)
			if err != nil {
				return 0, err
			}
			res, err := h.http.Stream(h.ctx, req)
			if err != nil {
				return 0, fmt.Errorf("hls segment %d of %d: %w", h.next+1, len(h.segments), err)
			}
			h.cur = res.Body
			h.next++
		}
		n, err := h.cur.Read(p)
		if errors.Is(err, io.EOF) {
			h.cur.Close()
			h.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (h *hlsReader) Close() error {
	h.closed = true
	if h.cur != nil {
		return h.cur.Close()
	}
	return nil
}
