package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/greed/cadence/internal/analysis"
	"github.com/greed/cadence/internal/lavalink"
	"github.com/greed/cadence/internal/metrics"
	"github.com/greed/cadence/internal/player"
	"github.com/greed/cadence/internal/source"
)

const (
	outboundBuffer = 4096
	backlogLimit   = 2000
)

type Deps struct {
	Resolver       *source.Resolver
	Registry       *analysis.Registry
	BufferFrames   int
	PlayoutDelay   time.Duration
	UpdateInterval time.Duration
	Log            *slog.Logger
	Loudness       player.LoudnessLookup
	OnUnmeasured   func(source.Track)
	TargetLufs     float64
}

type Session struct {
	ID         string
	UserID     string
	ClientName string
	Protocol   Protocol
	manager    *Manager
	deps       Deps
	log        *slog.Logger

	mu         sync.Mutex
	out        chan []byte
	connected  bool
	resuming   bool
	timeout    time.Duration
	backlog    [][]byte
	overflowed bool
	closed     bool
	expiry     *time.Timer
	guilds     map[string]*Guild
	stop       chan struct{}
	extras     atomic.Bool
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Session) send(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		s.log.Error("unencodable message", "error", err)
		return
	}
	s.mu.Lock()
	if !s.connected {
		s.bufferLocked(raw)
		s.mu.Unlock()
		return
	}
	select {
	case s.out <- raw:
		s.mu.Unlock()
		return
	default:
	}
	s.log.Warn("client is not reading, disconnecting it so it can resume", "client", s.ClientName)
	s.bufferLocked(raw)
	remove := s.detachLocked()
	s.mu.Unlock()
	if remove {
		s.manager.remove(s.ID)
	}
}

func (s *Session) bufferLocked(raw []byte) {
	if !s.resuming {
		return
	}
	if len(s.backlog) >= backlogLimit {
		if !s.overflowed {
			s.overflowed = true
			s.log.Warn("resume backlog is full, further messages are dropped", "limit", backlogLimit)
		}
		return
	}
	s.backlog = append(s.backlog, raw)
}

var (
	ErrAlreadyAttached = errors.New("session already has a connected client")
	ErrSessionClosed   = errors.New("session expired")
)

func (s *Session) Attach() (<-chan []byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, ErrSessionClosed
	}
	if s.connected {
		return nil, false, ErrAlreadyAttached
	}
	resumed := s.expiry != nil
	if s.expiry != nil {
		if !s.expiry.Stop() {
			return nil, false, ErrSessionClosed
		}
		s.expiry = nil
	}
	s.out = make(chan []byte, outboundBuffer)
	s.connected = true
	for _, raw := range s.backlog {
		select {
		case s.out <- raw:
		default:
		}
	}
	s.backlog, s.overflowed = nil, false
	return s.out, resumed, nil
}

func (s *Session) detachLocked() bool {
	s.connected = false
	close(s.out)
	if !s.resuming {
		return true
	}
	s.expiry = time.AfterFunc(s.timeout, func() { s.manager.remove(s.ID) })
	s.log.Info("client disconnected, keeping players for resume", "timeoutSeconds", int(s.timeout.Seconds()))
	return false
}

func (s *Session) Detach(out <-chan []byte) {
	s.mu.Lock()
	if !s.connected || (<-chan []byte)(s.out) != out {
		s.mu.Unlock()
		return
	}
	remove := s.detachLocked()
	s.mu.Unlock()
	if remove {
		s.manager.remove(s.ID)
	}
}

func (s *Session) Configure(resuming bool, timeout time.Duration) {
	s.mu.Lock()
	s.resuming = resuming
	if timeout > 0 {
		s.timeout = timeout
	}
	s.mu.Unlock()
}

func (s *Session) Settings() (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resuming, s.timeout
}

func (s *Session) Guild(id string, create bool) *Guild {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := s.guilds[id]; ok || !create {
		return g
	}
	g := &Guild{ID: id, session: s, log: s.log.With("guild", id)}
	g.Player = player.New(player.Config{
		GuildID:      id,
		BufferFrames: s.deps.BufferFrames,
		PlayoutDelay: s.deps.PlayoutDelay,
		Open:         s.open,
		Resolve:      s.deps.Resolver,
		OnEvent:      func(e player.Event) { s.onEvent(g, e) },
		Log:          g.log,
		Loudness:     s.deps.Loudness,
		OnUnmeasured: s.deps.OnUnmeasured,
		TargetLufs:   s.deps.TargetLufs,
	})
	s.guilds[id] = g
	return g
}

func (s *Session) open(ctx context.Context, t source.Track, startMs int64) (source.Stream, error) {
	return s.deps.Resolver.Open(ctx, t, startMs)
}

func (s *Session) Guilds() []*Guild {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Guild, 0, len(s.guilds))
	for _, g := range s.guilds {
		out = append(out, g)
	}
	return out
}

func (s *Session) DestroyGuild(id string) bool {
	s.mu.Lock()
	g, ok := s.guilds[id]
	delete(s.guilds, id)
	s.mu.Unlock()
	if ok {
		g.Destroy()
	}
	return ok
}

func (s *Session) onEvent(g *Guild, e player.Event) {
	metrics.Events.Inc(string(e.Type))
	switch e.Type {
	case player.EventTrackException:
		if e.Exception != nil {
			metrics.Exceptions.Inc(e.Exception.Source + ":" + string(e.Exception.Severity))
			s.log.Warn("track failed", "guild", g.ID, "source", e.Exception.Source, "severity", e.Exception.Severity, "message", e.Exception.Message, "cause", e.Exception.Cause)
		}
	case player.EventSendStall:
		metrics.StallMs.Add(e.StallMs)
	}
	if s.Protocol == ProtocolV4 {
		s.eventV4(g, e)
		s.send(s.updateV4(g))
		return
	}
	s.send(e)
	s.send(s.Update(g))
}

type PlayerUpdate struct {
	Op      string      `json:"op"`
	GuildID string      `json:"guildId"`
	State   UpdateState `json:"state"`
}

type UpdateState struct {
	Time      int64        `json:"time"`
	Connected bool         `json:"connected"`
	Ping      int64        `json:"ping"`
	Region    string       `json:"region,omitempty"`
	Player    player.State `json:"player"`
}

func (s *Session) Update(g *Guild) PlayerUpdate {
	server, connected, ping := g.VoiceState()
	return PlayerUpdate{Op: "playerUpdate", GuildID: g.ID, State: UpdateState{
		Time:      time.Now().UnixMilli(),
		Connected: connected,
		Ping:      ping.Milliseconds(),
		Region:    server.Region(),
		Player:    g.Player.State(),
	}}
}

func (s *Session) updates() {
	t := time.NewTicker(s.deps.UpdateInterval)
	defer t.Stop()
	stats := time.NewTicker(statsInterval)
	defer stats.Stop()
	if s.Protocol == ProtocolV4 {
		s.sendStats()
	}
	for {
		select {
		case <-s.stop:
			return
		case <-stats.C:
			if s.Protocol == ProtocolV4 {
				s.sendStats()
			}
		case <-t.C:
			for _, g := range s.Guilds() {
				st := g.Player.State()
				if st.Track == nil || st.Paused {
					continue
				}
				if s.Protocol == ProtocolV4 {
					s.send(s.updateV4(g))
				} else {
					s.send(s.Update(g))
				}
			}
		}
	}
}

func (s *Session) destroy() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	close(s.stop)
	for _, g := range s.Guilds() {
		g.Destroy()
	}
}

func (s *Session) AnalysisTap(t source.Track) player.Tap {
	if s.deps.Registry.Has(t.Source, t.Identifier) {
		return nil
	}
	return analysis.NewAnalyzer(t.Source, t.Identifier, s.deps.Registry.Store)
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	deps     Deps
	stats    func() lavalink.Stats
}

func (m *Manager) SetStats(fn func() lavalink.Stats) {
	m.stats = fn
}

func NewManager(deps Deps) *Manager {
	return &Manager{sessions: map[string]*Session{}, deps: deps}
}

func (m *Manager) Create(userID, clientName string, resumeTimeout time.Duration, protocol Protocol) *Session {
	s := &Session{
		ID:         newID(),
		UserID:     userID,
		ClientName: clientName,
		Protocol:   protocol,
		manager:    m,
		deps:       m.deps,
		timeout:    resumeTimeout,
		guilds:     map[string]*Guild{},
		stop:       make(chan struct{}),
	}
	s.log = m.deps.Log.With("session", s.ID, "client", clientName)
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	go s.updates()
	return s
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) Resume(id, userID string) (*Session, bool) {
	s, ok := m.Get(id)
	if !ok || s.UserID != userID {
		return nil, false
	}
	s.mu.Lock()
	connected := s.connected
	s.mu.Unlock()
	return s, !connected
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if ok {
		s.destroy()
	}
}

func (m *Manager) Sessions() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

func (m *Manager) Shutdown() {
	for _, s := range m.Sessions() {
		m.remove(s.ID)
	}
}
