package peercache

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/greed/cadence/internal/audiocache"
)

const token = "s3cret"

func newStore(t *testing.T) *audiocache.Store {
	t.Helper()
	store, err := audiocache.Open(t.TempDir(), 1<<30, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func write(t *testing.T, store *audiocache.Store, key string, body []byte) {
	t.Helper()
	w, err := store.Create(key, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}

func peerServer(t *testing.T, store *audiocache.Store) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/cache/index", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		keys := store.Keys()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "[")
		for i, k := range keys {
			if i > 0 {
				io.WriteString(w, ",")
			}
			io.WriteString(w, strconv.Quote(k))
		}
		io.WriteString(w, "]")
	})
	mux.HandleFunc("GET /v1/cache/file", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f, size, ok := store.Get(r.URL.Query().Get("key"))
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		io.Copy(w, f)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDisabledWithoutPeers(t *testing.T) {
	if New(nil, token, newStore(t), slog.New(slog.DiscardHandler)).Enabled() {
		t.Fatal("no peers should be disabled")
	}
	if New([]string{"http://x"}, token, nil, slog.New(slog.DiscardHandler)).Enabled() {
		t.Fatal("no local store should be disabled")
	}
}

func TestGossipAndLazyPull(t *testing.T) {
	remote := newStore(t)
	body := []byte("encrypted-audio-bytes")
	write(t, remote, "12345-mp3_128", body)
	srv := peerServer(t, remote)

	local := newStore(t)
	pool := New([]string{srv.URL}, token, local, slog.New(slog.DiscardHandler))
	pool.refresh(context.Background())

	if _, ok := pool.index["12345-mp3_128"]; !ok {
		t.Fatal("gossip did not learn the peer's key")
	}

	pool.MaybePull([]string{"12345-mp3_128", "12345-mp3_64"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !local.Has("12345-mp3_128") {
		time.Sleep(10 * time.Millisecond)
	}
	f, size, ok := local.Get("12345-mp3_128")
	if !ok {
		t.Fatal("track was not pulled into the local cache")
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if size != int64(len(body)) || string(got) != string(body) {
		t.Fatalf("pulled bytes differ: %q", got)
	}
}

func TestMaybePullSkipsWhenAlreadyLocal(t *testing.T) {
	remote := newStore(t)
	write(t, remote, "99-mp3_128", []byte("remote"))
	srv := peerServer(t, remote)

	local := newStore(t)
	write(t, local, "99-mp3_128", []byte("local-copy"))
	pool := New([]string{srv.URL}, token, local, slog.New(slog.DiscardHandler))
	pool.refresh(context.Background())
	pool.MaybePull([]string{"99-mp3_128"})
	time.Sleep(100 * time.Millisecond)
	f, _, ok := local.Get("99-mp3_128")
	if !ok {
		t.Fatal("local copy vanished")
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "local-copy" {
		t.Fatalf("local copy was overwritten: %q", got)
	}
}

func TestGossipMissingPeerIsHarmless(t *testing.T) {
	pool := New([]string{"http://127.0.0.1:1"}, token, newStore(t), slog.New(slog.DiscardHandler))
	pool.refresh(context.Background())
	if len(pool.index) != 0 {
		t.Fatal("unreachable peer should leave an empty index")
	}
	pool.MaybePull([]string{"nope-mp3_128"})
}
