package deezer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/metrics"
	"github.com/greed/cadence/internal/source"
)

const (
	headBytes   = 96 * stripeChunk
	headEntries = 256
	headTTL     = mediaURLTTL - time.Minute
	headTimeout = 15 * time.Second
)

type head struct {
	link     mediaLink
	data     []byte
	complete bool
}

type headFlight struct {
	done chan struct{}
	head *head
	err  error
}

type headStore struct {
	mu       sync.Mutex
	inflight map[string]*headFlight
}

func (d *Deezer) cachedHead(ctx context.Context, trackID string) *head {
	if h, ok := d.heads.Get(trackID); ok {
		return h
	}
	d.flights.mu.Lock()
	f := d.flights.inflight[trackID]
	d.flights.mu.Unlock()
	if f == nil {
		return nil
	}
	select {
	case <-f.done:
		return f.head
	case <-ctx.Done():
		return nil
	}
}

func (d *Deezer) warmHead(ctx context.Context, trackID string) (*head, error) {
	if h, ok := d.heads.Get(trackID); ok {
		return h, nil
	}
	d.flights.mu.Lock()
	if f, ok := d.flights.inflight[trackID]; ok {
		d.flights.mu.Unlock()
		select {
		case <-f.done:
			return f.head, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &headFlight{done: make(chan struct{})}
	d.flights.inflight[trackID] = f
	d.flights.mu.Unlock()

	f.head, f.err = d.fetchHead(ctx, trackID)
	if f.err == nil {
		d.heads.Set(trackID, f.head)
	}
	d.flights.mu.Lock()
	delete(d.flights.inflight, trackID)
	d.flights.mu.Unlock()
	close(f.done)
	return f.head, f.err
}

func (d *Deezer) fetchHead(ctx context.Context, trackID string) (*head, error) {
	ctx, cancel := context.WithTimeout(ctx, headTimeout)
	defer cancel()
	u, err := d.mediaURL(ctx, trackID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, u.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", headBytes-1))
	res, err := d.http.Stream(ctx, req)
	if err != nil {
		if httpx.IsStatus(err, http.StatusRequestedRangeNotSatisfiable) {
			return &head{link: u, complete: true}, nil
		}
		d.urls.Delete(trackID)
		return nil, source.Classify(Name, err)
	}
	defer res.Body.Close()
	buf := make([]byte, headBytes)
	n, err := io.ReadFull(res.Body, buf)
	switch {
	case err == nil:
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return &head{link: u, data: buf[:n], complete: true}, nil
	default:
		return nil, source.Classify(Name, err)
	}
	return &head{link: u, data: buf}, nil
}

type tailStream struct {
	ready  chan struct{}
	cancel context.CancelFunc
	mu     sync.Mutex
	body   io.ReadCloser
	total  int64
	err    error
	closed bool
}

func (d *Deezer) openTail(ctx context.Context, trackID string, h *head) *tailStream {
	ctx, cancel := context.WithCancel(ctx)
	t := &tailStream{ready: make(chan struct{}), cancel: cancel}
	go func() {
		body, total, err := d.fetchTail(ctx, trackID, h)
		t.mu.Lock()
		t.total = total
		if t.closed && body != nil {
			body.Close()
			body = nil
		}
		t.body, t.err = body, err
		t.mu.Unlock()
		close(t.ready)
	}()
	return t
}

func (d *Deezer) fetchTail(ctx context.Context, trackID string, h *head) (io.ReadCloser, int64, error) {
	offset := int64(len(h.data))
	body, total, err := d.rangeFrom(ctx, h.link.URL, offset)
	if err == nil {
		return body, total, nil
	}
	if ctx.Err() != nil {
		return nil, 0, ctx.Err()
	}
	if !httpx.IsStatus(err, http.StatusForbidden) && !httpx.IsStatus(err, http.StatusGone) && !httpx.IsStatus(err, http.StatusNotFound) {
		return nil, 0, source.Classify(Name, err)
	}
	d.heads.Delete(trackID)
	d.urls.Delete(trackID)
	fresh, uerr := d.mediaURL(ctx, trackID)
	if uerr != nil {
		return nil, 0, uerr
	}
	if fresh.Format != h.link.Format {
		return nil, 0, source.Fail(source.SeverityCommon, Name, "stream format changed mid-track", fmt.Errorf("cached start is %s, refreshed url is %s", h.link.Format, fresh.Format))
	}
	body, total, err = d.rangeFrom(ctx, fresh.URL, offset)
	if err != nil {
		return nil, 0, source.Classify(Name, err)
	}
	return body, total, nil
}

func (d *Deezer) rangeFrom(ctx context.Context, u string, offset int64) (io.ReadCloser, int64, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	res, err := d.http.Stream(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	if res.StatusCode == http.StatusPartialContent {
		return res.Body, rangeTotal(res.Header.Get("Content-Range")), nil
	}
	metrics.RangeIgnored.Add(1)
	d.log.Warn("cdn ignored a range request, re-downloading the skipped bytes", "offset", offset, "status", res.StatusCode)
	if _, err := io.CopyN(io.Discard, res.Body, offset); err != nil {
		res.Body.Close()
		return nil, 0, fmt.Errorf("skip cached head: %w", err)
	}
	return res.Body, max(0, res.ContentLength), nil
}

func rangeTotal(header string) int64 {
	_, total, ok := strings.Cut(header, "/")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (t *tailStream) Read(p []byte) (int, error) {
	<-t.ready
	if t.err != nil {
		return 0, t.err
	}
	return t.body.Read(p)
}

func (t *tailStream) Total() int64 {
	select {
	case <-t.ready:
		return t.total
	default:
		return 0
	}
}

func (t *tailStream) Close() error {
	t.cancel()
	t.mu.Lock()
	t.closed = true
	body := t.body
	t.mu.Unlock()
	if body != nil {
		return body.Close()
	}
	return nil
}

func (d *Deezer) openFromHead(ctx context.Context, trackID string, h *head) (io.Reader, io.Closer, func() int64) {
	metrics.Prefetch.Inc("hit")
	cached := bytes.NewReader(h.data)
	if h.complete {
		size := int64(len(h.data))
		return cached, noClose{}, func() int64 { return size }
	}
	tail := d.openTail(ctx, trackID, h)
	return io.MultiReader(cached, tail), tail, tail.Total
}

type noClose struct{}

func (noClose) Close() error {
	return nil
}
