package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greed/cadence/internal/source"
)

type slowAudio struct{}

func (slowAudio) Name() string           { return "slow" }
func (slowAudio) SearchPrefix() string   { return "" }
func (slowAudio) MatchesURL(string) bool { return false }
func (slowAudio) LoadURL(context.Context, string) (source.LoadResult, error) {
	return source.Empty(), nil
}
func (slowAudio) Track(context.Context, string) (source.Track, error) {
	return source.Track{}, errors.New("no")
}
func (slowAudio) Search(context.Context, string) ([]source.Track, error) { return nil, nil }
func (slowAudio) Warm(context.Context, source.Track)                     {}
func (slowAudio) Open(ctx context.Context, _ source.Track) (source.Stream, error) {
	<-ctx.Done()
	return source.Stream{}, ctx.Err()
}

func testManager() *Manager {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewManager(Deps{Resolver: source.NewResolver(slowAudio{}, nil, log), BufferFrames: 100, UpdateInterval: time.Hour, Log: log})
}

func TestSaveAndRestoreKeepsTheSessionAndItsPlayers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	m := testManager()
	s := m.Create("bot", "greed", time.Minute, ProtocolV4)
	s.Configure(true, time.Minute)
	s.SetCadenceEvents(true)
	g := s.Guild("42", true)
	g.Player.SetVolume(77)
	g.SetBitrate(128000)
	tr := source.Track{Source: "slow", Identifier: "t1", Title: "Song"}
	g.SetUserData(tr.Encode(), map[string]any{"requester": "9"})
	g.Player.Play(tr, 61_000, false)
	if n, err := m.Save(path); err != nil || n != 1 {
		t.Fatalf("save %d %v", n, err)
	}
	m.Shutdown()

	m2 := testManager()
	defer m2.Shutdown()
	rep, err := m2.Restore(path)
	if err != nil || rep.Sessions != 1 || rep.Players != 1 {
		t.Fatalf("restore %+v %v", rep, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("state file was not consumed")
	}
	got, resumable := m2.Resume(s.ID, "bot")
	if !resumable || got.Protocol != ProtocolV4 || !got.CadenceEvents() {
		t.Fatalf("session not resumable as before: %v %+v", resumable, got)
	}
	if _, wasResumed, err := got.Attach(); err != nil || !wasResumed {
		t.Fatalf("attach resumed=%v err=%v", wasResumed, err)
	}
	g2 := got.Guild("42", false)
	st := g2.Player.State()
	if g2 == nil || st.Volume != 77 || g2.PreferredBitrate() != 128000 {
		t.Fatalf("player settings lost: %+v bitrate %d", st, g2.PreferredBitrate())
	}
	if st.Track == nil || st.Track.Identifier != "t1" || st.PositionMs != 61_000 {
		t.Fatalf("track not resumed at its position: %+v", st)
	}
	if g2.UserData(tr.Encode())["requester"] != "9" {
		t.Fatal("userData lost")
	}
}

func TestExpiredOrNonResumingSessionsAreNotRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	m := testManager()
	m.Create("bot", "greed", time.Minute, ProtocolV4)
	if n, _ := m.Save(path); n != 0 {
		t.Fatalf("a non-resuming session was saved")
	}
	m.Shutdown()
	old, _ := json.Marshal(savedState{Version: stateVersion, SavedAt: time.Now().Add(-time.Hour).UnixMilli(), Sessions: []savedSession{{ID: "old", UserID: "bot", TimeoutMs: 60_000}}})
	os.WriteFile(path, old, 0o600)
	m2 := testManager()
	defer m2.Shutdown()
	rep, err := m2.Restore(path)
	if err != nil || rep.Sessions != 0 || rep.Skipped != 1 {
		t.Fatalf("expired restore %+v %v", rep, err)
	}
	if _, ok := m2.Get("old"); ok {
		t.Fatal("expired session restored")
	}
}
