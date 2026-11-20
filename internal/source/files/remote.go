package files

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/source"
)

const (
	RemoteName   = "http"
	AnyHost      = "*"
	probeTimeout = 15 * time.Second
)

type Hosts []string

func NewHosts(raw []string) Hosts {
	clean := make(Hosts, 0, len(raw))
	for _, h := range raw {
		if h = strings.Trim(strings.ToLower(strings.TrimSpace(h)), "."); h != "" {
			clean = append(clean, h)
		}
	}
	return clean
}

func (hs Hosts) Allows(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	for _, h := range hs {
		if h == AnyHost || host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

type Remote struct {
	http  *httpx.Client
	hosts Hosts
}

func NewRemote(client *httpx.Client, hosts []string) *Remote {
	return &Remote{http: client, hosts: NewHosts(hosts)}
}

func (r *Remote) Name() string         { return RemoteName }
func (r *Remote) SearchPrefix() string { return "" }

func (r *Remote) MatchesURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return false
	}
	return r.hosts.Allows(u.Hostname())
}

func (r *Remote) Search(context.Context, string) ([]source.Track, error) {
	return nil, nil
}

func (r *Remote) LoadURL(ctx context.Context, raw string) (source.LoadResult, error) {
	t, err := r.Track(ctx, raw)
	if err != nil {
		return source.LoadResult{}, err
	}
	return source.Single(t), nil
}

func (r *Remote) get(ctx context.Context, raw, rangeHeader string) (*http.Response, error) {
	if !r.MatchesURL(raw) {
		return nil, source.Fail(source.SeverityCommon, RemoteName, "this host is not allowed", nil)
	}
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return nil, source.Fail(source.SeverityCommon, RemoteName, "not a valid link", nil)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	res, err := r.http.Stream(ctx, req)
	if err != nil {
		return nil, source.Classify(RemoteName, err)
	}
	if ct := strings.ToLower(res.Header.Get("Content-Type")); strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") {
		res.Body.Close()
		return nil, source.Fail(source.SeverityCommon, RemoteName, "the link is not an audio file", nil)
	}
	return res, nil
}

func totalSize(res *http.Response) int64 {
	if res.StatusCode == http.StatusPartialContent {
		if _, total, ok := strings.Cut(res.Header.Get("Content-Range"), "/"); ok {
			if n, err := strconv.ParseInt(total, 10, 64); err == nil {
				return n
			}
		}
		return -1
	}
	return res.ContentLength
}

func (r *Remote) Track(ctx context.Context, raw string) (source.Track, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	size := int64(-1)
	fetch := func(offset, n int64) ([]byte, error) {
		res, err := r.get(ctx, raw, fmt.Sprintf("bytes=%d-%d", offset, offset+n-1))
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if offset == 0 {
			size = totalSize(res)
		}
		if res.StatusCode != http.StatusPartialContent && offset > 0 {
			if _, err := io.CopyN(io.Discard, res.Body, offset); err != nil {
				return nil, source.Classify(RemoteName, err)
			}
		}
		buf, err := io.ReadAll(io.LimitReader(res.Body, n))
		if err != nil {
			return nil, source.Classify(RemoteName, err)
		}
		return buf, nil
	}
	head, err := fetch(0, headBytes)
	if err != nil {
		return source.Track{}, err
	}
	cached := func(offset, n int64) ([]byte, error) {
		if offset == 0 {
			return head, nil
		}
		return fetch(offset, n)
	}
	p, err := probe(RemoteName, cached, size)
	if err != nil {
		return source.Track{}, err
	}
	u, _ := url.Parse(raw)
	name, _ := url.PathUnescape(path.Base(u.Path))
	if name == "" || name == "/" || name == "." {
		name = u.Hostname()
	}
	return p.track(RemoteName, raw, raw, name), nil
}

func (r *Remote) Open(ctx context.Context, t source.Track) (source.Stream, error) {
	res, err := r.get(ctx, t.Identifier, "")
	if err != nil {
		return source.Stream{}, err
	}
	return source.Stream{Body: res.Body, Format: codec.FormatMP3}, nil
}

func (r *Remote) Warm(context.Context, source.Track) {}
