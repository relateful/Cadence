package deezer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	url      string
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
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", headBytes-1))
	res, err := d.http.Stream(ctx, req)
	if err != nil {
		if httpx.IsStatus(err, http.StatusRequestedRangeNotSatisfiable) {
			return &head{url: u, complete: true}, nil
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
		return &head{url: u, data: buf[:n], complete: true}, nil
	default:
		return nil, source.Classify(Name, err)
	}
	return &head{url: u, data: buf}, nil
}

type tailStream struct {
	ready  chan struct{}
	cancel context.CancelFunc
	mu     sync.Mutex
	body   io.ReadCloser
	err    error
	closed bool
}

func (d *Deezer) openTail(ctx context.Context, trackID string, h *head) *tailStream {
	ctx, cancel := context.WithCancel(ctx)
	t := &tailStream{ready: make(chan struct{}), cancel: cancel}
	go func() {
		body, err := d.fetchTail(ctx, trackID, h)
		t.mu.Lock()
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

func (d *Deezer) fetchTail(ctx context.Context, trackID string, h *head) (io.ReadCloser, error) {
	offset := int64(len(h.data))
	body, err := d.rangeFrom(ctx, h.url, offset)
	if err == nil {
		return body, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !httpx.IsStatus(err, http.StatusForbidden) && !httpx.IsStatus(err, http.StatusGone) && !httpx.IsStatus(err, http.StatusNotFound) {
		return nil, source.Classify(Name, err)
	}
	d.heads.Delete(trackID)
	d.urls.Delete(trackID)
	fresh, uerr := d.mediaURL(ctx, trackID)
	if uerr != nil {
		return nil, uerr
	}
	body, err = d.rangeFrom(ctx, fresh, offset)
	if err != nil {
		return nil, source.Classify(Name, err)
	}
	return body, nil
}

func (d *Deezer) rangeFrom(ctx context.Context, u string, offset int64) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	res, err := d.http.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusPartialContent {
		return res.Body, nil
	}
	if _, err := io.CopyN(io.Discard, res.Body, offset); err != nil {
		res.Body.Close()
		return nil, fmt.Errorf("skip cached head: %w", err)
	}
	return res.Body, nil
}

func (t *tailStream) Read(p []byte) (int, error) {
	<-t.ready
	if t.err != nil {
		return 0, t.err
	}
	return t.body.Read(p)
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

func (d *Deezer) openFromHead(ctx context.Context, trackID string, h *head) (io.Reader, io.Closer) {
	metrics.Prefetch.Inc("hit")
	cached := bytes.NewReader(h.data)
	if h.complete {
		return cached, noClose{}
	}
	tail := d.openTail(ctx, trackID, h)
	return io.MultiReader(cached, tail), tail
}

type noClose struct{}

func (noClose) Close() error {
	return nil
}
