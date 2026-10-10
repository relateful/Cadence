package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/disgoorg/godave"
	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/player"
)

const (
	connectTimeout = 10 * time.Second
	maxReconnects  = 10
	maxBackoff     = 16 * time.Second
	readLimit      = 1 << 20
)

func reconnectDelay(attempt int) time.Duration {
	return min(maxBackoff, time.Duration(1<<min(attempt-1, 5))*time.Second)
}

type Server struct {
	GuildID   string `json:"guildId"`
	UserID    string `json:"userId"`
	ChannelID string `json:"channelId"`
	SessionID string `json:"sessionId"`
	Token     string `json:"token"`
	Endpoint  string `json:"endpoint"`
	Bitrate   int    `json:"bitrate,omitempty"`
}

func (s Server) Valid() bool {
	return s.SessionID != "" && s.Token != "" && s.Endpoint != "" && s.GuildID != "" && s.UserID != "" && s.ChannelID != ""
}

func (s Server) Region() string {
	host := strings.TrimPrefix(strings.Split(s.Endpoint, ":")[0], "wss://")
	label := strings.TrimPrefix(strings.Split(host, ".")[0], "c-")
	end := 0
	for end < len(label) && label[end] >= 'a' && label[end] <= 'z' {
		end++
	}
	return label[:end]
}

type FrameSource interface {
	NextFrame(out *audio.Frame) (player.FrameInfo, bool)
	Sent(info player.FrameInfo, at time.Time)
	Stall(gap time.Duration)
}

type CloseEvent struct {
	Code     int    `json:"code"`
	Reason   string `json:"reason"`
	ByRemote bool   `json:"byRemote"`
	Terminal bool   `json:"terminal"`
}

type Conn struct {
	server  Server
	source  FrameSource
	onClose func(*Conn, CloseEvent)
	log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	ws          *websocket.Conn
	wsMu        sync.Mutex
	seq         atomic.Int64
	dave        godave.Session
	udp         *net.UDPConn
	remote      *net.UDPAddr
	ssrc        uint32
	resumable   atomic.Bool
	healthy     atomic.Bool
	dead        atomic.Bool
	awaitingAck atomic.Bool
	ping        atomic.Int64
	sentAt      atomic.Int64
	bitrate     atomic.Int64
	sender      *sender
	senderMu    sync.Mutex
	daveVersion atomic.Int32
	transMu     sync.Mutex
	pending     map[uint16]uint16
}

func (c *Conn) holdAudio() bool {
	return c.dave != nil && c.daveVersion.Load() > 0 && !c.dave.Ready()
}

func (c *Conn) setDaveVersion(v uint16, why string) {
	if old := c.daveVersion.Swap(int32(v)); old != int32(v) {
		c.log.Info("dave protocol version changed", "from", old, "to", v, "via", why)
	}
}

func Dial(server Server, src FrameSource, onClose func(*Conn, CloseEvent), log *slog.Logger) (*Conn, error) {
	if !server.Valid() {
		return nil, errors.New("incomplete voice server state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{server: server, source: src, onClose: onClose, log: log.With("region", server.Region()), ctx: ctx, cancel: cancel}
	c.bitrate.Store(int64(codec.ClampBitrate(server.Bitrate)))
	c.seq.Store(-1)
	c.wg.Add(1)
	go c.run()
	return c, nil
}

func (c *Conn) Server() Server {
	return c.server
}

func (c *Conn) Dead() bool {
	return c.dead.Load()
}

func (c *Conn) restartFresh(reason string) {
	c.log.Warn("restarting the voice session from scratch", "reason", reason)
	c.resumable.Store(false)
	c.wsMu.Lock()
	if c.ws != nil {
		c.ws.Close(websocket.StatusGoingAway, reason)
	}
	c.wsMu.Unlock()
}

func (c *Conn) Ping() time.Duration {
	return time.Duration(c.ping.Load())
}

func (c *Conn) SetBitrate(bps int) {
	c.bitrate.Store(int64(codec.ClampBitrate(bps)))
	c.senderMu.Lock()
	if c.sender != nil {
		c.sender.bitrateChanged.Store(true)
	}
	c.senderMu.Unlock()
}

func (c *Conn) Close() {
	c.cancel()
	c.wsMu.Lock()
	if c.ws != nil {
		c.ws.Close(websocket.StatusNormalClosure, "")
	}
	c.wsMu.Unlock()
	c.wg.Wait()
	c.teardown()
}

func (c *Conn) teardown() {
	c.stopSender()
	if c.udp != nil {
		c.udp.Close()
		c.udp = nil
	}
	if c.dave != nil {
		c.dave.Close()
		c.dave = nil
	}
}

var terminalCodes = map[int]string{
	4001: "unknown opcode",
	4002: "failed to decode payload",
	4003: "not authenticated",
	4004: "authentication failed",
	4005: "already authenticated",
	4006: "session no longer valid",
	4011: "server not found",
	4012: "unknown protocol",
	4014: "disconnected",
	4016: "unknown encryption mode",
	4020: "bad request",
	4021: "rate limited",
	4022: "call terminated",
}

func (c *Conn) run() {
	defer c.wg.Done()
	attempts := 0
	for c.ctx.Err() == nil {
		code, reason, err := c.session(c.resumable.Load())
		if c.ctx.Err() != nil {
			return
		}
		if c.healthy.Swap(false) {
			attempts = 0
		}
		if _, terminal := terminalCodes[code]; terminal || code == int(websocket.StatusNormalClosure) {
			c.log.Warn("voice gateway closed for good", "code", code, "reason", reason)
			c.dead.Store(true)
			c.teardown()
			c.onClose(c, CloseEvent{Code: code, Reason: reason, ByRemote: true, Terminal: true})
			return
		}
		attempts++
		if attempts > maxReconnects {
			c.log.Error("voice gateway gave up reconnecting", "attempts", maxReconnects, "code", code, "error", err)
			c.dead.Store(true)
			c.teardown()
			c.onClose(c, CloseEvent{Code: code, Reason: fmt.Sprintf("gave up after %d reconnects: %v", maxReconnects, err), ByRemote: true, Terminal: true})
			return
		}
		c.log.Warn("voice gateway dropped, reconnecting", "code", code, "reason", reason, "error", err, "attempt", attempts, "resume", c.resumable.Load())
		c.onClose(c, CloseEvent{Code: code, Reason: reason, ByRemote: true})
		if !c.resumable.Load() {
			c.teardown()
		}
		select {
		case <-time.After(reconnectDelay(attempts)):
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Conn) endpoint() string {
	host := strings.TrimPrefix(c.server.Endpoint, "wss://")
	return fmt.Sprintf("wss://%s/?v=%d", host, GatewayVersion)
}

func (c *Conn) send(op Opcode, d any) error {
	raw, err := json.Marshal(outbound{Op: op, D: d})
	if err != nil {
		return err
	}
	return c.write(websocket.MessageText, raw)
}

func (c *Conn) write(typ websocket.MessageType, raw []byte) error {
	c.wsMu.Lock()
	ws := c.ws
	c.wsMu.Unlock()
	if ws == nil {
		return errors.New("voice gateway not connected")
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	return ws.Write(ctx, typ, raw)
}

func closeCode(err error) (int, string) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return int(ce.Code), ce.Reason
	}
	return 0, ""
}

func (c *Conn) session(resuming bool) (int, string, error) {
	dialCtx, cancel := context.WithTimeout(c.ctx, connectTimeout)
	ws, _, err := websocket.Dial(dialCtx, c.endpoint(), nil)
	cancel()
	if err != nil {
		return 0, "", err
	}
	ws.SetReadLimit(readLimit)
	c.wsMu.Lock()
	c.ws = ws
	c.wsMu.Unlock()
	defer func() {
		c.wsMu.Lock()
		c.ws = nil
		c.wsMu.Unlock()
		ws.CloseNow()
	}()

	hbCtx, stopHeartbeat := context.WithCancel(c.ctx)
	defer stopHeartbeat()

	for {
		typ, raw, err := ws.Read(c.ctx)
		if err != nil {
			code, reason := closeCode(err)
			return code, reason, err
		}
		if typ == websocket.MessageBinary {
			c.binary(raw)
			continue
		}
		var m inbound
		if err := json.Unmarshal(raw, &m); err != nil {
			c.log.Warn("undecodable voice message", "error", err)
			continue
		}
		if s, ok := m.sequence(); ok {
			c.seq.Store(int64(s))
		}
		if err := c.handle(m, hbCtx, resuming); err != nil {
			ws.Close(websocket.StatusInternalError, "setup failed")
			return 0, err.Error(), err
		}
	}
}

func (c *Conn) heartbeat(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.awaitingAck.Load() {
				c.log.Warn("voice heartbeat was not acknowledged, reconnecting")
				c.wsMu.Lock()
				if c.ws != nil {
					c.ws.Close(websocket.StatusGoingAway, "heartbeat timeout")
				}
				c.wsMu.Unlock()
				return
			}
			now := time.Now()
			c.sentAt.Store(now.UnixNano())
			c.awaitingAck.Store(true)
			if err := c.send(OpHeartbeat, heartbeat{T: now.UnixMilli(), SeqAck: int(c.seq.Load())}); err != nil {
				c.log.Warn("heartbeat failed", "error", err)
			}
		}
	}
}

func (c *Conn) handle(m inbound, hbCtx context.Context, resuming bool) error {
	switch m.Op {
	case OpHello:
		var h hello
		if err := json.Unmarshal(m.D, &h); err != nil {
			return err
		}
		c.awaitingAck.Store(false)
		go c.heartbeat(hbCtx, time.Duration(h.HeartbeatInterval*float64(time.Millisecond)))
		if resuming {
			return c.send(OpResume, resume{ServerID: c.server.GuildID, SessionID: c.server.SessionID, Token: c.server.Token, SeqAck: int(c.seq.Load())})
		}
		c.stopSender()
		if c.dave != nil {
			c.dave.Close()
			c.dave = nil
		}
		dave, err := newDave(c.log, c.server.UserID, daveCallbacks{c})
		if err != nil {
			return err
		}
		channelID, err := strconv.ParseUint(c.server.ChannelID, 10, 64)
		if err != nil {
			dave.Close()
			return fmt.Errorf("voice channel id %q: %w", c.server.ChannelID, err)
		}
		dave.SetChannelID(godave.ChannelID(channelID))
		c.dave = dave
		return c.send(OpIdentify, identify{ServerID: c.server.GuildID, UserID: c.server.UserID, SessionID: c.server.SessionID, Token: c.server.Token, MaxDaveProtocolVersion: dave.MaxSupportedProtocolVersion()})
	case OpReady:
		var r ready
		if err := json.Unmarshal(m.D, &r); err != nil {
			return err
		}
		return c.ready(r)
	case OpSessionDescription:
		var sd sessionDescription
		if err := json.Unmarshal(m.D, &sd); err != nil {
			return err
		}
		sealer, err := NewSealer(sd.Mode, sd.SecretKey)
		if err != nil {
			return err
		}
		c.dave.OnSelectProtocolAck(uint16(sd.DaveProtocolVersion))
		c.setDaveVersion(uint16(sd.DaveProtocolVersion), "session description")
		c.resumable.Store(true)
		c.healthy.Store(true)
		c.log.Info("voice session ready", "mode", sd.Mode, "dave", sd.DaveProtocolVersion)
		return c.startSender(sealer)
	case OpHeartbeatAck:
		c.awaitingAck.Store(false)
		if sent := c.sentAt.Load(); sent > 0 {
			c.ping.Store(time.Now().UnixNano() - sent)
		}
	case OpResumed:
		c.healthy.Store(true)
		c.log.Info("voice session resumed")
	case OpClientsConnect:
		var cc clientsConnect
		if json.Unmarshal(m.D, &cc) == nil && c.dave != nil {
			for _, id := range cc.UserIDs {
				c.dave.AddUser(godave.UserID(id))
			}
		}
	case OpClientDisconnect:
		var cd clientDisconnect
		if json.Unmarshal(m.D, &cd) == nil && c.dave != nil {
			c.dave.RemoveUser(godave.UserID(cd.UserID))
		}
	case OpDavePrepareTransition:
		var t transition
		if json.Unmarshal(m.D, &t) == nil && c.dave != nil {
			c.dave.OnDavePrepareTransition(t.TransitionID, t.ProtocolVersion)
			c.transMu.Lock()
			if c.pending == nil {
				c.pending = map[uint16]uint16{}
			}
			if len(c.pending) < 64 {
				c.pending[t.TransitionID] = t.ProtocolVersion
			}
			c.transMu.Unlock()
		}
	case OpDaveExecuteTransition:
		var t transition
		if json.Unmarshal(m.D, &t) == nil && c.dave != nil {
			c.dave.OnDaveExecuteTransition(t.TransitionID)
			c.transMu.Lock()
			v, ok := c.pending[t.TransitionID]
			delete(c.pending, t.TransitionID)
			c.transMu.Unlock()
			if ok {
				c.setDaveVersion(v, "transition")
			}
		}
	case OpDavePrepareEpoch:
		var e prepareEpoch
		if json.Unmarshal(m.D, &e) == nil && c.dave != nil {
			c.dave.OnDavePrepareEpoch(e.Epoch, e.ProtocolVersion)
		}
	}
	return nil
}

func (c *Conn) binary(raw []byte) {
	f, err := ParseBinary(raw)
	if err != nil {
		c.log.Warn("bad binary voice frame", "error", err)
		return
	}
	c.seq.Store(int64(f.Seq))
	if c.dave == nil {
		return
	}
	switch f.Op {
	case OpDaveExternalSender:
		c.dave.OnDaveMLSExternalSenderPackage(f.Payload)
	case OpDaveProposals:
		c.dave.OnDaveMLSProposals(f.Payload)
	case OpDaveAnnounceCommit:
		c.dave.OnDaveMLSPrepareCommitTransition(f.TransitionID, f.Payload)
	case OpDaveWelcome:
		c.dave.OnDaveMLSWelcome(f.TransitionID, f.Payload)
	default:
		c.log.Debug("unhandled binary voice op", "op", f.Op)
	}
}

func (c *Conn) ready(r ready) error {
	c.stopSender()
	mode, err := ChooseMode(r.Modes)
	if err != nil {
		return err
	}
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(r.IP, fmt.Sprint(r.Port)))
	if err != nil {
		return err
	}
	if c.udp != nil {
		c.udp.Close()
	}
	udp, err := net.ListenUDP("udp", nil)
	if err != nil {
		return err
	}
	found, err := discover(udp, addr, r.SSRC)
	if err != nil {
		udp.Close()
		return err
	}
	if !found.From.IP.Equal(addr.IP) || found.From.Port != addr.Port || found.Attempts > 1 {
		c.log.Info("ip discovery needed help", "server", addr.String(), "repliedFrom", found.From.String(), "attempts", found.Attempts)
	}
	c.udp, c.remote, c.ssrc = udp, addr, r.SSRC
	go drainUDP(udp)
	c.dave.AssignSsrcToCodec(r.SSRC, godave.CodecOpus)
	return c.send(OpSelectProtocol, selectProtocol{Protocol: "udp", Data: selectProtocolData{Address: found.IP, Port: found.Port, Mode: mode}})
}

type daveCallbacks struct {
	c *Conn
}

func (d daveCallbacks) SendMLSKeyPackage(kp []byte) error {
	return d.c.write(websocket.MessageBinary, EncodeBinary(OpDaveKeyPackage, kp))
}

func (d daveCallbacks) SendMLSCommitWelcome(cw []byte) error {
	return d.c.write(websocket.MessageBinary, EncodeBinary(OpDaveCommitWelcome, cw))
}

func (d daveCallbacks) SendReadyForTransition(id uint16) error {
	return d.c.send(OpDaveTransitionReady, map[string]uint16{"transition_id": id})
}

func (d daveCallbacks) SendInvalidCommitWelcome(id uint16) error {
	return d.c.send(OpDaveInvalidCommitWelcome, map[string]uint16{"transition_id": id})
}
