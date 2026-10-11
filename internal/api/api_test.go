package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/greed/cadence/internal/analysis"
	"github.com/greed/cadence/internal/session"
	"github.com/greed/cadence/internal/source"
)

type stubAudio struct{}

func (stubAudio) Name() string           { return "stub" }
func (stubAudio) SearchPrefix() string   { return "stsearch:" }
func (stubAudio) MatchesURL(string) bool { return false }
func (stubAudio) LoadURL(context.Context, string) (source.LoadResult, error) {
	return source.Empty(), nil
}
func (stubAudio) Track(context.Context, string) (source.Track, error) {
	return source.Track{}, errors.New("no")
}
func (stubAudio) Search(_ context.Context, q string) ([]source.Track, error) {
	return []source.Track{{Source: "stub", Identifier: "1", Title: q, DurationMs: 60000}}, nil
}
func (stubAudio) Open(context.Context, source.Track) (source.Stream, error) {
	return source.Stream{}, errors.New("no audio in tests")
}
func (stubAudio) Warm(context.Context, source.Track) {}

const password = "pw"

func newTestServer(t *testing.T) (*httptest.Server, *session.Manager) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	resolver := source.NewResolver(stubAudio{}, []string{"stub"}, log)
	registry := analysis.NewRegistry()
	sessions := session.NewManager(session.Deps{Resolver: resolver, Registry: registry, BufferFrames: 100, UpdateInterval: time.Second, Log: log})
	srv := New(Deps{Password: password, Sessions: sessions, Resolver: resolver, Registry: registry, Offline: analysis.NewOffline(registry, nil, 1, 1, log), ResumeTimeout: time.Minute, Bitrate: 96000, Log: log})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		sessions.Shutdown()
	})
	return ts, sessions
}

func call(t *testing.T, method, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, url, reader)
	req.Header.Set("Authorization", password)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func dial(t *testing.T, ts *httptest.Server, sessionID string) (*websocket.Conn, map[string]any) {
	t.Helper()
	h := http.Header{"Authorization": {password}, "User-Id": {"123"}, "Client-Name": {"test"}}
	if sessionID != "" {
		h.Set("Session-Id", sessionID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/websocket", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ready map[string]any
	json.Unmarshal(raw, &ready)
	return conn, ready
}

func TestAuthIsRequired(t *testing.T) {
	ts, _ := newTestServer(t)
	res, err := http.Get(ts.URL + "/v1/info")
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %v %v", res.StatusCode, err)
	}
	if res, _ := call(t, "GET", ts.URL+"/v1/info", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("authorized status %d", res.StatusCode)
	}
}

func TestLoadAndDecode(t *testing.T) {
	ts, _ := newTestServer(t)
	_, out := call(t, "GET", ts.URL+"/v1/loadtracks?identifier=stsearch:hello", nil)
	if out["loadType"] != "search" {
		t.Fatalf("%v", out)
	}
	enc := out["data"].([]any)[0].(map[string]any)["encoded"].(string)
	_, dec := call(t, "GET", ts.URL+"/v1/decodetrack?encodedTrack="+enc, nil)
	if dec["info"].(map[string]any)["title"] != "hello" {
		t.Fatalf("%v", dec)
	}
}

func TestPlayerLifecycleAndMixConflicts(t *testing.T) {
	ts, _ := newTestServer(t)
	conn, ready := dial(t, ts, "")
	defer conn.CloseNow()
	sid := ready["sessionId"].(string)
	base := ts.URL + "/v1/sessions/" + sid + "/players/555"
	enc := source.Track{Source: "stub", Identifier: "1", Title: "x"}.Encode()
	res, state := call(t, "PATCH", base, map[string]any{"track": map[string]any{"encoded": enc}, "volume": 80})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("patch %d %v", res.StatusCode, state)
	}
	player := state["state"].(map[string]any)["player"].(map[string]any)
	if player["volume"].(float64) != 80 {
		t.Fatalf("volume %v", player)
	}
	if res, _ := call(t, "POST", base+"/mix/crossfade", map[string]any{"durationMs": 1000}); res.StatusCode != http.StatusConflict {
		t.Fatalf("crossfade without deck b: %d", res.StatusCode)
	}
	if res, _ := call(t, "POST", base+"/mix/crossfade", map[string]any{"durationMs": 0}); res.StatusCode != http.StatusConflict && res.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero duration: %d", res.StatusCode)
	}
	if res, _ := call(t, "POST", base+"/mix/host", map[string]any{"audio": "bm90IGFuIG1wMw=="}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("dj voice with a clip that is not mp3: %d", res.StatusCode)
	}
	if res, _ := call(t, "POST", base+"/mix/host", map[string]any{"audio": "%%%"}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("dj voice with a clip that is not base64: %d", res.StatusCode)
	}
	if res, _ := call(t, "DELETE", base, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete %d", res.StatusCode)
	}
	if res, _ := call(t, "GET", base, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted player still present: %d", res.StatusCode)
	}
}

func TestResumeKeepsPlayers(t *testing.T) {
	ts, _ := newTestServer(t)
	conn, ready := dial(t, ts, "")
	sid := ready["sessionId"].(string)
	call(t, "PATCH", ts.URL+"/v1/sessions/"+sid, map[string]any{"resuming": true, "timeout": 60})
	call(t, "PATCH", ts.URL+"/v1/sessions/"+sid+"/players/777", map[string]any{"volume": 50})
	conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(100 * time.Millisecond)
	conn2, ready2 := dial(t, ts, sid)
	defer conn2.CloseNow()
	if ready2["resumed"] != true || ready2["sessionId"] != sid {
		t.Fatalf("not resumed: %v", ready2)
	}
	if res, _ := call(t, "GET", ts.URL+"/v1/sessions/"+sid+"/players/777", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("player lost across resume: %d", res.StatusCode)
	}
}

func TestNonResumingSessionIsDestroyed(t *testing.T) {
	ts, sessions := newTestServer(t)
	conn, ready := dial(t, ts, "")
	sid := ready["sessionId"].(string)
	conn.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := sessions.Get(sid); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session survived a disconnect without resuming enabled")
}
