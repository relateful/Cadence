package deezer

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/source"
)

type cdn struct {
	mu          sync.Mutex
	data        []byte
	ignoreRange bool
	denyTail    bool
	ranges      []string
}

func (c *cdn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	rng := r.Header.Get("Range")
	c.ranges = append(c.ranges, rng)
	c.mu.Unlock()
	if c.denyTail && rng != "" && rng[:8] != "bytes=0-" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if c.ignoreRange {
		r.Header.Del("Range")
	}
	http.ServeContent(w, r, "track.mp3", time.Time{}, bytes.NewReader(c.data))
}

func (c *cdn) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ranges...)
}

func newTestDeezer(t *testing.T, server *cdn, plain []byte) (*Deezer, source.Track) {
	t.Helper()
	dec, err := NewStripeDecryptor(testMaster)
	if err != nil {
		t.Fatal(err)
	}
	RegisterDecryptor(dec)
	track := source.Track{Source: Name, Identifier: "3135553"}
	server.data = stripeEncrypt(t, dec, track.Identifier, plain)
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := httpx.New(Name, "", log)
	if err != nil {
		t.Fatal(err)
	}
	d := New(client, nil, RotationFailover, true, log)
	d.urls.Set(track.Identifier, mediaLink{URL: srv.URL + "/track.mp3", Format: formatMP3High})
	return d, track
}

func pattern(size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(i*7 + i/2048)
	}
	return out
}

func readAll(t *testing.T, d *Deezer, track source.Track) ([]byte, error) {
	t.Helper()
	stream, err := d.Open(context.Background(), track)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	return io.ReadAll(stream.Body)
}

func TestWarmedOpenServesHeadThenRangeTail(t *testing.T) {
	server := &cdn{}
	plain := pattern(headBytes*3 + 777)
	d, track := newTestDeezer(t, server, plain)
	d.accounts = newAccountPool([]string{"test"}, RotationFailover)
	d.Warm(context.Background(), track)
	got, err := readAll(t, d, track)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("stream differs from source (err %v, %d of %d bytes)", err, len(got), len(plain))
	}
	ranges := server.seen()
	if len(ranges) != 2 || ranges[0] != "bytes=0-196607" || ranges[1] != "bytes=196608-" {
		t.Fatalf("requests %q", ranges)
	}
}

func TestWarmedOpenSurvivesServerIgnoringRange(t *testing.T) {
	server := &cdn{ignoreRange: true}
	plain := pattern(headBytes*2 + 5)
	d, track := newTestDeezer(t, server, plain)
	d.accounts = newAccountPool([]string{"test"}, RotationFailover)
	d.Warm(context.Background(), track)
	got, err := readAll(t, d, track)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("stream differs from source (err %v)", err)
	}
}

func TestShortTrackIsServedEntirelyFromHead(t *testing.T) {
	server := &cdn{}
	plain := pattern(headBytes / 3)
	d, track := newTestDeezer(t, server, plain)
	d.accounts = newAccountPool([]string{"test"}, RotationFailover)
	d.Warm(context.Background(), track)
	got, err := readAll(t, d, track)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("stream differs from source (err %v)", err)
	}
	if n := len(server.seen()); n != 1 {
		t.Fatalf("%d requests, want only the warm", n)
	}
}

func TestColdOpenStreamsWholeFile(t *testing.T) {
	server := &cdn{}
	plain := pattern(headBytes + 1234)
	d, track := newTestDeezer(t, server, plain)
	got, err := readAll(t, d, track)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("stream differs from source (err %v)", err)
	}
	if ranges := server.seen(); len(ranges) != 1 || ranges[0] != "" {
		t.Fatalf("requests %q", ranges)
	}
}

func TestRefusedTailIsAnErrorNotSilence(t *testing.T) {
	server := &cdn{denyTail: true}
	plain := pattern(headBytes * 2)
	d, track := newTestDeezer(t, server, plain)
	d.accounts = newAccountPool([]string{"test"}, RotationFailover)
	d.Warm(context.Background(), track)
	d.accounts = newAccountPool(nil, RotationFailover)
	got, err := readAll(t, d, track)
	if err == nil {
		t.Fatal("refused tail ended the stream cleanly")
	}
	if !bytes.Equal(got, plain[:headBytes]) {
		t.Fatalf("head was not served before the failure (%d bytes)", len(got))
	}
}

func TestOpenJoinsWarmInFlight(t *testing.T) {
	server := &cdn{}
	plain := pattern(headBytes * 2)
	d, track := newTestDeezer(t, server, plain)
	d.accounts = newAccountPool([]string{"test"}, RotationFailover)
	f := &headFlight{done: make(chan struct{})}
	d.flights.mu.Lock()
	d.flights.inflight[track.Identifier] = f
	d.flights.mu.Unlock()
	go func() {
		h, err := d.fetchHead(context.Background(), track.Identifier)
		f.head, f.err = h, err
		close(f.done)
	}()
	got, err := readAll(t, d, track)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("stream differs from source (err %v)", err)
	}
	ranges := server.seen()
	if len(ranges) != 2 || ranges[0] != "bytes=0-196607" {
		t.Fatalf("open did not reuse the in-flight head: %q", ranges)
	}
}
