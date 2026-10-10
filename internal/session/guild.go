package session

import (
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/greed/cadence/internal/metrics"
	"github.com/greed/cadence/internal/player"
	"github.com/greed/cadence/internal/voice"
)

type Guild struct {
	ID      string
	Player  *player.Player
	session *Session
	mu      sync.Mutex
	conn    atomic.Pointer[voice.Conn]
	server  voice.Server
	log     *slog.Logger
}

func (g *Guild) live() *voice.Conn {
	c := g.conn.Load()
	if c == nil || c.Dead() {
		return nil
	}
	return c
}

func (g *Guild) VoiceState() (voice.Server, bool, time.Duration) {
	g.mu.Lock()
	server := g.server
	g.mu.Unlock()
	conn := g.live()
	if conn == nil {
		return server, false, 0
	}
	return server, true, conn.Ping()
}

func sameServer(a, b voice.Server) bool {
	return a.SessionID == b.SessionID && a.Token == b.Token && a.Endpoint == b.Endpoint && a.ChannelID == b.ChannelID
}

func (g *Guild) UpdateVoice(s voice.Server) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if current := g.live(); current != nil && sameServer(g.server, s) {
		if s.Bitrate > 0 && s.Bitrate != g.server.Bitrate {
			current.SetBitrate(s.Bitrate)
			g.server.Bitrate = s.Bitrate
		}
		return nil
	}
	if old := g.conn.Swap(nil); old != nil {
		old.Close()
	}
	conn, err := voice.Dial(s, g.Player, g.onVoiceClose, g.log)
	if err != nil {
		return err
	}
	g.conn.Store(conn)
	g.server = s
	return nil
}

func (g *Guild) onVoiceClose(c *voice.Conn, e voice.CloseEvent) {
	metrics.VoiceClose.Inc(strconv.Itoa(e.Code))
	g.session.send(map[string]any{"op": "event", "type": "WebSocketClosedEvent", "guildId": g.ID, "code": e.Code, "reason": e.Reason, "byRemote": e.ByRemote, "terminal": e.Terminal})
	if e.Terminal {
		g.conn.CompareAndSwap(c, nil)
	}
}

func (g *Guild) Disconnect() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if conn := g.conn.Swap(nil); conn != nil {
		conn.Close()
	}
}

func (g *Guild) Destroy() {
	g.Disconnect()
	g.Player.Destroy()
}
