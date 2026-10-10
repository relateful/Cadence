package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/greed/cadence/internal/source"
)

func dialV4(t *testing.T, ts *httptest.Server) (*websocket.Conn, string) {
	t.Helper()
	h := http.Header{"Authorization": {password}, "User-Id": {"123"}, "Client-Name": {"lavalink-client/2.10.3"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v4/websocket", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	ready := readOp(t, conn, "ready")
	return conn, ready["sessionId"].(string)
}

func readOp(t *testing.T, conn *websocket.Conn, op string) map[string]any {
	t.Helper()
	msgs := readUntil(t, conn, func(m map[string]any) bool { return m["op"] == op })
	return msgs[len(msgs)-1]
}

func readUntil(t *testing.T, conn *websocket.Conn, done func(map[string]any) bool) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var seen []map[string]any
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for message: %v (seen %v)", err, seen)
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		seen = append(seen, m)
		if done(m) {
			return seen
		}
	}
}

func TestV4InfoAndVersionMatchLavalink(t *testing.T) {
	ts, _ := newTestServer(t)
	_, info := call(t, "GET", ts.URL+"/v4/info", nil)
	v := info["version"].(map[string]any)
	if v["semver"] != Version || v["major"].(float64) != 1 {
		t.Fatalf("version %v", v)
	}
	if info["wireClock"] != true || len(info["sourceManagers"].([]any)) == 0 || info["plugins"] == nil {
		t.Fatalf("info %v", info)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/version", nil)
	req.Header.Set("Authorization", password)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != Version {
		t.Fatalf("version body %q", body)
	}
}

func TestV4LoadTracksUsesLavalinkTrackShape(t *testing.T) {
	ts, _ := newTestServer(t)
	_, out := call(t, "GET", ts.URL+"/v4/loadtracks?identifier=stsearch:hello", nil)
	if out["loadType"] != "search" {
		t.Fatalf("%v", out)
	}
	tr := out["data"].([]any)[0].(map[string]any)
	info := tr["info"].(map[string]any)
	if info["length"].(float64) != 60000 || info["sourceName"] != "stub" || info["isSeekable"] != true || info["title"] != "hello" {
		t.Fatalf("info %v", info)
	}
	if _, ok := tr["pluginInfo"].(map[string]any); !ok {
		t.Fatalf("pluginInfo missing: %v", tr)
	}
	_, dec := call(t, "GET", ts.URL+"/v4/decodetrack?encodedTrack="+tr["encoded"].(string), nil)
	if dec["info"].(map[string]any)["length"].(float64) != 60000 {
		t.Fatalf("decode %v", dec)
	}
}

func TestV4SessionSendsStatsAndLavalinkPlayers(t *testing.T) {
	ts, _ := newTestServer(t)
	conn, sid := dialV4(t, ts)
	defer conn.CloseNow()
	stats := readOp(t, conn, "stats")
	if _, ok := stats["memory"].(map[string]any); !ok || stats["cpu"] == nil {
		t.Fatalf("stats %v", stats)
	}
	base := ts.URL + "/v4/sessions/" + sid + "/players/555"
	enc := source.Track{Source: "stub", Identifier: "1", Title: "x", DurationMs: 1000}.Encode()
	res, p := call(t, "PATCH", base, map[string]any{"track": map[string]any{"encoded": enc, "userData": map[string]any{"requester": "42"}}, "volume": 80})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("patch %d %v", res.StatusCode, p)
	}
	if p["guildId"] != "555" || p["volume"].(float64) != 80 {
		t.Fatalf("player %v", p)
	}
	st := p["state"].(map[string]any)
	if st["connected"] != false || st["ping"].(float64) != -1 {
		t.Fatalf("state %v", st)
	}
	track := p["track"].(map[string]any)
	if track["userData"].(map[string]any)["requester"] != "42" {
		t.Fatalf("userData not echoed: %v", track)
	}
	msgs := readUntil(t, conn, func(m map[string]any) bool { return m["type"] == "TrackEndEvent" })
	for _, m := range msgs {
		if m["type"] == "TrackLoadingEvent" {
			t.Fatal("cadence-only event sent to a client that did not opt in")
		}
	}
	end := msgs[len(msgs)-1]
	if end["reason"] != "loadFailed" || end["track"].(map[string]any)["info"].(map[string]any)["sourceName"] != "stub" {
		t.Fatalf("end %v", end)
	}
	_, list := callList(t, ts.URL+"/v4/sessions/"+sid+"/players")
	if len(list) != 1 {
		t.Fatalf("players %v", list)
	}
}

func TestV4CadenceEventsAreOptIn(t *testing.T) {
	ts, _ := newTestServer(t)
	conn, sid := dialV4(t, ts)
	defer conn.CloseNow()
	if res, _ := call(t, "PATCH", ts.URL+"/v4/sessions/"+sid, map[string]any{"cadenceEvents": true}); res.StatusCode != http.StatusOK {
		t.Fatalf("session patch %d", res.StatusCode)
	}
	enc := source.Track{Source: "stub", Identifier: "1", Title: "x"}.Encode()
	call(t, "PATCH", ts.URL+"/v4/sessions/"+sid+"/players/9", map[string]any{"track": map[string]any{"encoded": enc}})
	msgs := readUntil(t, conn, func(m map[string]any) bool { return m["type"] == "TrackLoadingEvent" })
	if msgs[len(msgs)-1]["instance"] == nil {
		t.Fatalf("loading event without instance: %v", msgs[len(msgs)-1])
	}
}

func TestV4DoesNotResumeAV1Session(t *testing.T) {
	ts, _ := newTestServer(t)
	conn, ready := dial(t, ts, "")
	sid := ready["sessionId"].(string)
	call(t, "PATCH", ts.URL+"/v1/sessions/"+sid, map[string]any{"resuming": true, "timeout": 60})
	conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(100 * time.Millisecond)
	h := http.Header{"Authorization": {password}, "User-Id": {"123"}, "Session-Id": {sid}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c2, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v4/websocket", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.CloseNow()
	r := readOp(t, c2, "ready")
	if r["resumed"] == true || r["sessionId"] == sid {
		t.Fatalf("v4 client resumed a v1 session: %v", r)
	}
}

func callList(t *testing.T, url string) (*http.Response, []any) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", password)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out []any
	json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestPlayerBitrateIsRememberedAndClamped(t *testing.T) {
	ts, sessions := newTestServer(t)
	conn, sid := dialV4(t, ts)
	defer conn.CloseNow()
	base := ts.URL + "/v4/sessions/" + sid + "/players/42"
	if res, _ := call(t, "PATCH", base, map[string]any{"bitrate": 128000}); res.StatusCode != http.StatusOK {
		t.Fatalf("patch %d", res.StatusCode)
	}
	sess, _ := sessions.Get(sid)
	g := sess.Guild("42", false)
	if g.PreferredBitrate() != 128000 {
		t.Fatalf("bitrate %d", g.PreferredBitrate())
	}
	call(t, "PATCH", base, map[string]any{"bitrate": 9_000_000})
	if g.PreferredBitrate() != 384000 {
		t.Fatalf("not clamped: %d", g.PreferredBitrate())
	}
}
