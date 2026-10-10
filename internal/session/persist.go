package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/greed/cadence/internal/player"
	"github.com/greed/cadence/internal/source"
	"github.com/greed/cadence/internal/voice"
)

const stateVersion = 1

type savedPlayer struct {
	GuildID    string         `json:"guildId"`
	Track      string         `json:"track,omitempty"`
	PositionMs int64          `json:"positionMs"`
	Paused     bool           `json:"paused"`
	Volume     int            `json:"volume"`
	Filters    player.Filters `json:"filters"`
	Voice      voice.Server   `json:"voice"`
	Bitrate    int            `json:"bitrate,omitempty"`
	UserData   map[string]any `json:"userData,omitempty"`
}

type savedSession struct {
	ID            string        `json:"id"`
	UserID        string        `json:"userId"`
	ClientName    string        `json:"clientName"`
	Protocol      Protocol      `json:"protocol"`
	TimeoutMs     int64         `json:"timeoutMs"`
	CadenceEvents bool          `json:"cadenceEvents"`
	Players       []savedPlayer `json:"players"`
}

type savedState struct {
	Version  int            `json:"version"`
	SavedAt  int64          `json:"savedAt"`
	Sessions []savedSession `json:"sessions"`
}

func (m *Manager) snapshot() savedState {
	st := savedState{Version: stateVersion, SavedAt: time.Now().UnixMilli()}
	for _, s := range m.Sessions() {
		resuming, timeout := s.Settings()
		if !resuming {
			continue
		}
		ss := savedSession{ID: s.ID, UserID: s.UserID, ClientName: s.ClientName, Protocol: s.Protocol, TimeoutMs: timeout.Milliseconds(), CadenceEvents: s.CadenceEvents()}
		for _, g := range s.Guilds() {
			server, _, _ := g.VoiceState()
			ps := g.Player.State()
			sp := savedPlayer{GuildID: g.ID, PositionMs: ps.PositionMs, Paused: ps.Paused, Volume: ps.Volume, Filters: ps.Filters, Voice: server, Bitrate: g.PreferredBitrate()}
			if ps.Track != nil {
				sp.Track = ps.Track.Encode()
				sp.UserData = g.UserData(sp.Track)
			}
			ss.Players = append(ss.Players, sp)
		}
		st.Sessions = append(st.Sessions, ss)
	}
	return st
}

func (m *Manager) Save(path string) (int, error) {
	st := m.snapshot()
	m.quiet.Store(true)
	raw, err := json.Marshal(st)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return len(st.Sessions), nil
}

type RestoreReport struct {
	Sessions, Players, Skipped int
	Age                        time.Duration
}

func (m *Manager) Restore(path string) (RestoreReport, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return RestoreReport{}, nil
	}
	if err != nil {
		return RestoreReport{}, err
	}
	if err := os.Remove(path); err != nil {
		return RestoreReport{}, fmt.Errorf("could not consume the state file, refusing to restore twice: %w", err)
	}
	var st savedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return RestoreReport{}, fmt.Errorf("state file: %w", err)
	}
	if st.Version != stateVersion {
		return RestoreReport{}, fmt.Errorf("state file version %d, want %d", st.Version, stateVersion)
	}
	rep := RestoreReport{Age: time.Since(time.UnixMilli(st.SavedAt))}
	for _, ss := range st.Sessions {
		timeout := time.Duration(ss.TimeoutMs) * time.Millisecond
		if timeout <= 0 || rep.Age >= timeout {
			rep.Skipped++
			continue
		}
		s := m.restoreSession(ss, timeout-rep.Age)
		rep.Sessions++
		for _, sp := range ss.Players {
			if m.restorePlayer(s, sp) {
				rep.Players++
			}
		}
	}
	return rep, nil
}

func (m *Manager) restoreSession(ss savedSession, remaining time.Duration) *Session {
	s := m.newSession(ss.ID, ss.UserID, ss.ClientName, remaining, ss.Protocol)
	s.extras.Store(ss.CadenceEvents)
	s.mu.Lock()
	s.resuming = true
	s.timeout = time.Duration(ss.TimeoutMs) * time.Millisecond
	s.expiry = time.AfterFunc(remaining, func() { m.remove(s.ID) })
	s.mu.Unlock()
	return s
}

func (m *Manager) restorePlayer(s *Session, sp savedPlayer) bool {
	g := s.Guild(sp.GuildID, true)
	g.Player.SetVolume(sp.Volume)
	g.Player.SetFilters(sp.Filters)
	if sp.Bitrate > 0 {
		g.SetBitrate(sp.Bitrate)
	}
	if sp.Voice.Valid() {
		if err := g.UpdateVoice(sp.Voice); err != nil {
			s.log.Warn("restored player could not rejoin voice", "guild", sp.GuildID, "error", err)
		}
	}
	if sp.Track == "" {
		return true
	}
	t, err := source.DecodeTrack(sp.Track)
	if err != nil {
		s.log.Warn("restored player had an unreadable track", "guild", sp.GuildID, "error", err)
		return false
	}
	g.SetUserData(sp.Track, sp.UserData)
	g.Player.Play(t, max(0, sp.PositionMs), sp.Paused)
	return true
}
