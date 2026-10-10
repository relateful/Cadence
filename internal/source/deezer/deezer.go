package deezer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/cache"
	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/metrics"
	"github.com/greed/cadence/internal/source"
)

const (
	Name         = "deezer"
	SearchPrefix = "dzsearch:"
	ISRCPrefix   = "dzisrc:"
	isrcCacheTTL = 7 * 24 * time.Hour
)

type Decryptor interface {
	Decrypt(trackID string, encrypted io.Reader) (io.Reader, error)
}

var (
	decryptorMu sync.RWMutex
	decryptor   Decryptor
)

func RegisterDecryptor(d Decryptor) {
	decryptorMu.Lock()
	decryptor = d
	decryptorMu.Unlock()
}

func registered() Decryptor {
	decryptorMu.RLock()
	defer decryptorMu.RUnlock()
	return decryptor
}

var ErrNoDecryptor = errors.New("no deezer decryptor is registered")

var (
	linkPattern  = regexp.MustCompile(`^https?://(?:www\.)?deezer\.com/(?:[a-z]{2}(?:-[a-z]{2})?/)?(track|album|playlist|artist)/(\d+)`)
	shortPattern = regexp.MustCompile(`^https?://(?:deezer\.page\.link|link\.deezer\.com)/`)
)

type Deezer struct {
	http     *httpx.Client
	arl      string
	formats  []mediaFormat
	sessions sessionStore
	urls     *cache.LRU[string, mediaLink]
	isrc     *cache.LRU[string, source.Track]
	heads    *cache.LRU[string, *head]
	flights  headStore
	disk     *diskCache
	layouts  *cache.LRU[string, mp3Layout]
	log      *slog.Logger
}

func New(client *httpx.Client, arl string, highQuality bool, log *slog.Logger) *Deezer {
	formats := []mediaFormat{{streamCipher, formatMP3High}, {streamCipher, formatMP3Low}}
	if !highQuality {
		formats = formats[1:]
	}
	return &Deezer{
		http:    client,
		arl:     arl,
		formats: formats,
		urls:    cache.New[string, mediaLink](50_000, mediaURLTTL),
		isrc:    cache.New[string, source.Track](200_000, isrcCacheTTL),
		heads:   cache.New[string, *head](headEntries, headTTL),
		flights: headStore{inflight: map[string]*headFlight{}},
		layouts: newLayoutCache(),
		log:     log.With("source", Name),
	}
}

func (d *Deezer) Name() string {
	return Name
}

func (d *Deezer) SearchPrefix() string {
	return SearchPrefix
}

func (d *Deezer) MatchesURL(rawURL string) bool {
	return linkPattern.MatchString(rawURL) || shortPattern.MatchString(rawURL)
}

func (d *Deezer) LoadURL(ctx context.Context, rawURL string) (source.LoadResult, error) {
	if shortPattern.MatchString(rawURL) {
		expanded, err := d.expand(ctx, rawURL)
		if err != nil {
			return source.LoadResult{}, err
		}
		rawURL = expanded
	}
	m := linkPattern.FindStringSubmatch(rawURL)
	if m == nil {
		return source.Empty(), nil
	}
	switch m[1] {
	case "track":
		t, err := d.Track(ctx, m[2])
		if err != nil {
			return source.LoadResult{}, err
		}
		return source.Single(t), nil
	case "album":
		return d.album(ctx, m[2])
	case "playlist":
		return d.playlist(ctx, m[2])
	default:
		return d.artistTop(ctx, m[2])
	}
}

func (d *Deezer) expand(ctx context.Context, short string) (string, error) {
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
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil || loc.String() == "" {
		return "", source.Fail(source.SeverityCommon, Name, "short link did not redirect", err)
	}
	if strings.Contains(loc.Host, "deezer.page.link") || strings.Contains(loc.Host, "link.deezer.com") {
		return d.expand(ctx, loc.String())
	}
	return loc.Scheme + "://" + loc.Host + loc.Path, nil
}

func (d *Deezer) Warm(ctx context.Context, t source.Track) {
	if d.arl == "" {
		return
	}
	if _, err := d.warmHead(ctx, t.Identifier); err != nil {
		metrics.Prefetch.Inc("failed")
		d.log.Warn("warm failed", "track", t.Identifier, "error", err)
	}
}

type decryptedBody struct {
	io.Reader
	closer io.Closer
}

func (b decryptedBody) Close() error {
	return b.closer.Close()
}

func (d *Deezer) Open(ctx context.Context, t source.Track) (source.Stream, error) {
	dec := registered()
	if dec == nil {
		return source.Stream{}, source.Fail(source.SeverityFault, Name, "stream unavailable", ErrNoDecryptor)
	}
	if file, ok := d.openDisk(t.Identifier); ok {
		return decrypted(dec, t.Identifier, file, file)
	}
	if h := d.cachedHead(ctx, t.Identifier); h != nil {
		raw, closer, total := d.openFromHead(ctx, t.Identifier, h)
		raw, closer = d.teeToDisk(t.Identifier, h.link, raw, closer, total)
		return decrypted(dec, t.Identifier, raw, closer)
	}
	metrics.Prefetch.Inc("miss")
	u, err := d.mediaURL(ctx, t.Identifier)
	if err != nil {
		return source.Stream{}, err
	}
	req, err := http.NewRequest(http.MethodGet, u.URL, nil)
	if err != nil {
		return source.Stream{}, err
	}
	res, err := d.http.Stream(ctx, req)
	if err != nil {
		d.urls.Delete(t.Identifier)
		return source.Stream{}, source.Classify(Name, err)
	}
	size := max(0, res.ContentLength)
	raw, closer := d.teeToDisk(t.Identifier, u, res.Body, res.Body, func() int64 { return size })
	return decrypted(dec, t.Identifier, raw, closer)
}

func decrypted(dec Decryptor, trackID string, raw io.Reader, closer io.Closer) (source.Stream, error) {
	plain, err := dec.Decrypt(trackID, raw)
	if err != nil {
		closer.Close()
		return source.Stream{}, source.Fail(source.SeverityFault, Name, "decrypt failed", err)
	}
	return source.Stream{Body: decryptedBody{Reader: plain, closer: closer}, Format: codec.FormatMP3}, nil
}

func (d *Deezer) Search(ctx context.Context, query string) ([]source.Track, error) {
	if strings.HasPrefix(query, ISRCPrefix) {
		t, err := d.ByISRC(ctx, strings.TrimPrefix(query, ISRCPrefix))
		if err != nil {
			return nil, err
		}
		return []source.Track{t}, nil
	}
	return d.search(ctx, query)
}
