package soundcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/source"
)

const testClientID = "abcdefghijklmnopqrstuvwxyz123456"

type fake struct {
	srv          *httptest.Server
	rejectFirst  atomic.Bool
	scrapes      atomic.Int32
	audio, seg0  string
	seg1         string
	searchResult []map[string]any
}

func track(id int64, title, policy string, transcodings ...map[string]any) map[string]any {
	return map[string]any{
		"id": id, "kind": "track", "title": title, "duration": 200000, "policy": policy,
		"permalink_url":      fmt.Sprintf("https://soundcloud.com/artist/t%d", id),
		"artwork_url":        "https://i1.sndcdn.com/artworks-x-large.jpg",
		"user":               map[string]any{"username": "uploader"},
		"publisher_metadata": map[string]any{"artist": "Real Artist", "isrc": "us-abc-12-34567"},
		"media":              map[string]any{"transcodings": transcodings},
	}
}

func tc(base, protocol, path string, snipped bool) map[string]any {
	return map[string]any{"url": base + path, "snipped": snipped, "format": map[string]any{"protocol": protocol, "mime_type": "audio/mpeg"}}
}

func newFake(t *testing.T) (*fake, *SoundCloud) {
	t.Helper()
	f := &fake{audio: "PROGRESSIVE-MP3-BYTES", seg0: "HLS-SEGMENT-ONE|", seg1: "HLS-SEGMENT-TWO"}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	base := f.srv.URL
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		f.scrapes.Add(1)
		fmt.Fprintf(w, `<html><script crossorigin src="%s/assets/0-vendor.js"></script><script crossorigin src="%s/assets/49-app.js"></script></html>`, base, base)
	})
	mux.HandleFunc("/assets/0-vendor.js", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "var nothing=1;") })
	mux.HandleFunc("/assets/49-app.js", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `e.exports={client_id:"%s",x:1}`, testClientID)
	})
	api := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("client_id") != testClientID {
				http.Error(w, "bad client", http.StatusUnauthorized)
				return
			}
			if f.rejectFirst.CompareAndSwap(true, false) {
				http.Error(w, "rotated", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	progressive := track(1, "Song One", "ALLOW", tc(base, "hls", "/media/1/hls", false), tc(base, "progressive", "/media/1/progressive", false))
	hlsOnly := track(2, "Song Two", "MONETIZE", tc(base, "progressive", "/media/2/snip", true), tc(base, "hls", "/media/2/hls", false))
	preview := track(3, "Preview Only", "SNIP", tc(base, "progressive", "/media/3/progressive", false))
	f.searchResult = []map[string]any{preview, progressive, hlsOnly}
	byID := map[string]map[string]any{"1": progressive, "2": hlsOnly, "3": preview}
	mux.HandleFunc("/search/tracks", api(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"collection": f.searchResult})
	}))
	mux.HandleFunc("/tracks/", api(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(byID[strings.TrimPrefix(r.URL.Path, "/tracks/")])
	}))
	mux.HandleFunc("/tracks", api(func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
			out = append(out, byID[id])
		}
		json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("/resolve", api(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("url"), "/sets/") {
			json.NewEncoder(w).Encode(map[string]any{"kind": "playlist", "title": "My Set", "permalink_url": "https://soundcloud.com/a/sets/s",
				"tracks": []map[string]any{progressive, {"id": 2}, {"id": 3}}})
			return
		}
		out := map[string]any{}
		for k, v := range progressive {
			out[k] = v
		}
		json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("/media/1/progressive", api(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"url": base + "/cf/audio.mp3"})
	}))
	mux.HandleFunc("/media/2/hls", api(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"url": base + "/cf/list/playlist.m3u8"})
	}))
	mux.HandleFunc("/cf/audio.mp3", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, f.audio) })
	mux.HandleFunc("/cf/list/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10.0,\nseg0.mp3\n#EXTINF:10.0,\n"+base+"/cf/list/seg1.mp3\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/cf/list/seg0.mp3", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, f.seg0) })
	mux.HandleFunc("/cf/list/seg1.mp3", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, f.seg1) })
	client, err := httpx.New(Name, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return f, New(client, "", Endpoints{API: base, Web: base})
}

func TestSearchScrapesClientIDAndDropsPreviews(t *testing.T) {
	_, sc := newFake(t)
	got, err := sc.Search(context.Background(), "song")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Identifier != "1" || got[1].Identifier != "2" {
		t.Fatalf("results %+v", got)
	}
	if got[0].Author != "Real Artist" || got[0].ISRC != "USABC1234567" || !strings.Contains(got[0].ArtworkURL, "t500x500") {
		t.Fatalf("mapping %+v", got[0])
	}
}

func TestRotatedClientIDIsRescraped(t *testing.T) {
	f, sc := newFake(t)
	sc.Search(context.Background(), "song")
	f.rejectFirst.Store(true)
	if _, err := sc.Search(context.Background(), "song"); err != nil {
		t.Fatalf("search after rotation: %v", err)
	}
	if f.scrapes.Load() != 2 {
		t.Fatalf("scraped %d times, want 2", f.scrapes.Load())
	}
}

func read(t *testing.T, sc *SoundCloud, id string) string {
	t.Helper()
	s, err := sc.Open(context.Background(), source.Track{Source: Name, Identifier: id})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Body.Close()
	b, err := io.ReadAll(s.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenPrefersProgressiveAndJoinsHLSSegments(t *testing.T) {
	f, sc := newFake(t)
	if got := read(t, sc, "1"); got != f.audio {
		t.Fatalf("progressive %q", got)
	}
	if got := read(t, sc, "2"); got != f.seg0+f.seg1 {
		t.Fatalf("hls %q", got)
	}
	if _, err := sc.Open(context.Background(), source.Track{Source: Name, Identifier: "3"}); err == nil {
		t.Fatal("a preview-only track opened")
	}
}

func TestLinksAndSetsResolve(t *testing.T) {
	_, sc := newFake(t)
	if !sc.MatchesURL("https://soundcloud.com/artist/song?si=abc") || sc.MatchesURL("https://soundcloud.com/artist") {
		t.Fatal("url matching")
	}
	res, err := sc.LoadURL(context.Background(), "https://soundcloud.com/artist/t1?si=x")
	if err != nil || res.Type != source.LoadTrack || res.Tracks[0].Identifier != "1" {
		t.Fatalf("track %+v %v", res, err)
	}
	set, err := sc.LoadURL(context.Background(), "https://soundcloud.com/a/sets/s")
	if err != nil || set.Type != source.LoadPlaylist || len(set.Tracks) != 2 || set.Playlist.Name != "My Set" {
		t.Fatalf("set %+v %v", set, err)
	}
}
