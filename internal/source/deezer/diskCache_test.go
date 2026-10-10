package deezer

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/greed/cadence/internal/audiocache"
)

func withDisk(t *testing.T, d *Deezer) *audiocache.Store {
	t.Helper()
	store, err := audiocache.Open(t.TempDir(), 1<<30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d.UseAudioCache(store)
	return store
}

func TestSecondPlayIsCachedAndThirdNeedsNoNetwork(t *testing.T) {
	server := &cdn{}
	plain := pattern(headBytes*2 + 333)
	d, track := newTestDeezer(t, server, plain)
	store := withDisk(t, d)
	key := cacheKey(track.Identifier, formatMP3High)
	for play := 1; play <= 2; play++ {
		got, err := readAll(t, d, track)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("play %d differs (err %v)", play, err)
		}
		if cached := store.Has(key); cached != (play == 2) {
			t.Fatalf("after play %d cached=%v", play, cached)
		}
	}
	before := len(server.seen())
	got, err := readAll(t, d, track)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("disk play differs (err %v)", err)
	}
	if after := len(server.seen()); after != before {
		t.Fatalf("disk play made %d network requests", after-before)
	}
}

func TestPreloadedPlayIsCachedToo(t *testing.T) {
	server := &cdn{}
	plain := pattern(headBytes*3 + 9)
	d, track := newTestDeezer(t, server, plain)
	d.arl = "test"
	store := withDisk(t, d)
	readAll(t, d, track)
	d.Warm(context.Background(), track)
	got, err := readAll(t, d, track)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("preloaded play differs (err %v)", err)
	}
	if !store.Has(cacheKey(track.Identifier, formatMP3High)) {
		t.Fatal("head plus range tail was not cached")
	}
}

func TestStoppedPlayIsNotCached(t *testing.T) {
	server := &cdn{}
	d, track := newTestDeezer(t, server, pattern(headBytes*2))
	store := withDisk(t, d)
	readAll(t, d, track)
	stream, err := d.Open(context.Background(), track)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(stream.Body, make([]byte, 1000))
	stream.Body.Close()
	if store.Has(cacheKey(track.Identifier, formatMP3High)) {
		t.Fatal("a half-played track was cached")
	}
	if files, _, _ := store.Stats(); files != 0 {
		t.Fatalf("%d files cached", files)
	}
}
