package session

import (
	"time"

	"github.com/greed/cadence/internal/lavalink"
	"github.com/greed/cadence/internal/player"
)

type Protocol int

const (
	ProtocolV1 Protocol = iota
	ProtocolV4
)

const statsInterval = time.Minute

func (s *Session) SetCadenceEvents(on bool) {
	s.extras.Store(on)
}

func (s *Session) CadenceEvents() bool {
	return s.extras.Load()
}

func (s *Session) stateV4(g *Guild) lavalink.PlayerState {
	_, connected, ping := g.VoiceState()
	pingMs := int64(-1)
	if connected {
		pingMs = ping.Milliseconds()
	}
	return lavalink.PlayerState{Time: time.Now().UnixMilli(), Position: g.Player.State().PositionMs, Connected: connected, Ping: pingMs}
}

func (s *Session) PlayerV4(g *Guild) lavalink.Player {
	server, _, _ := g.VoiceState()
	st := g.Player.State()
	out := lavalink.FromPlayer(g.ID, st, s.stateV4(g), lavalink.VoiceState{Token: server.Token, Endpoint: server.Endpoint, SessionID: server.SessionID, ChannelID: server.ChannelID})
	if out.Track != nil {
		out.Track.UserData = g.UserData(out.Track.Encoded)
	}
	return out
}

func (s *Session) updateV4(g *Guild) map[string]any {
	return lavalink.PlayerUpdate(g.ID, s.stateV4(g))
}

func (s *Session) eventV4(g *Guild, e player.Event) {
	if !lavalink.IsStandardEvent(e.Type) && !s.extras.Load() {
		return
	}
	msg := lavalink.Event(e)
	if t, ok := msg["track"].(*lavalink.Track); ok {
		t.UserData = g.UserData(t.Encoded)
	}
	s.send(msg)
}

func (s *Session) sendStats() {
	if fn := s.manager.stats; fn != nil {
		st := fn()
		s.send(struct {
			Op string `json:"op"`
			lavalink.Stats
		}{"stats", st})
	}
}

func (g *Guild) SetUserData(encoded string, data map[string]any) {
	g.mu.Lock()
	g.userEncoded, g.userData = encoded, data
	g.mu.Unlock()
}

func (g *Guild) UserData(encoded string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	if encoded == g.userEncoded && g.userData != nil {
		return g.userData
	}
	return map[string]any{}
}
