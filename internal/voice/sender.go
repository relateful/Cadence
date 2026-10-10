package voice

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/codec"
)

const (
	frameInterval    = audio.FrameMs * time.Millisecond
	stallThreshold   = 3 * frameInterval
	silenceTrailer   = 5
	daveWaitLogEvery = 50
	daveHoldLimit    = 750
)

type sender struct {
	conn           *Conn
	sealer         *Sealer
	enc            codec.Encoder
	rtp            *RTP
	stop           chan struct{}
	done           chan struct{}
	bitrateChanged atomic.Bool
	speaking       bool
	silenceLeft    int
	opus           []byte
	daveBuf        []byte
	frame          audio.Frame
	once           sync.Once
}

func (c *Conn) startSender(sealer *Sealer) error {
	c.stopSender()
	enc, err := codec.NewEncoder(int(c.bitrate.Load()))
	if err != nil {
		return err
	}
	s := &sender{
		conn:    c,
		sealer:  sealer,
		enc:     enc,
		rtp:     NewRTP(c.ssrc),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		opus:    make([]byte, codec.MaxOpusBytes),
		daveBuf: make([]byte, 0, codec.MaxOpusBytes*2),
	}
	c.senderMu.Lock()
	c.sender = s
	c.senderMu.Unlock()
	go s.run()
	return nil
}

func (c *Conn) stopSender() {
	c.senderMu.Lock()
	s := c.sender
	c.sender = nil
	c.senderMu.Unlock()
	if s == nil {
		return
	}
	s.once.Do(func() { close(s.stop) })
	<-s.done
	s.enc.Close()
}

func (s *sender) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.done)
	next := time.Now()
	waits := 0
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		if s.bitrateChanged.Swap(false) {
			if err := s.enc.SetBitrate(int(s.conn.bitrate.Load())); err != nil {
				s.conn.log.Warn("bitrate change failed", "error", err)
			}
		}
		if s.conn.dave != nil && !s.conn.dave.Ready() {
			waits++
			if waits%daveWaitLogEvery == 1 {
				s.conn.log.Info("holding audio until the DAVE epoch is ready")
			}
			if waits >= daveHoldLimit {
				waits = 0
				s.conn.restartFresh("DAVE epoch never became ready")
			}
			next = s.sleep(next)
			continue
		}
		waits = 0
		s.tick()
		next = s.sleep(next)
	}
}

func (s *sender) tick() {
	info, ok := s.conn.source.NextFrame(&s.frame)
	if ok {
		if !s.speaking {
			s.setSpeaking(true)
		}
		n, err := s.enc.Encode(s.frame[:], s.opus)
		if err != nil || n <= 0 {
			s.conn.log.Warn("opus encode failed, frame dropped", "error", err)
			return
		}
		if err := s.write(s.opus[:n]); err != nil {
			s.conn.log.Warn("voice packet dropped", "error", err)
			return
		}
		s.conn.source.Sent(info, time.Now())
		s.silenceLeft = silenceTrailer
		return
	}
	if !s.speaking {
		return
	}
	if s.silenceLeft > 0 {
		s.silenceLeft--
		if err := s.write(codec.SilenceFrame); err != nil {
			s.conn.log.Debug("silence frame dropped", "error", err)
		}
		return
	}
	s.setSpeaking(false)
}

func (s *sender) setSpeaking(on bool) {
	flag := 0
	if on {
		flag = SpeakingMicrophone
	}
	if err := s.conn.send(OpSpeaking, speaking{Speaking: flag, SSRC: s.conn.ssrc}); err != nil {
		s.conn.log.Warn("speaking update failed", "error", err)
		return
	}
	s.speaking = on
}

func (s *sender) write(opus []byte) error {
	payload := opus
	if d := s.conn.dave; d != nil {
		need := d.MaxEncryptedFrameSize(len(opus))
		if cap(s.daveBuf) < need {
			s.daveBuf = make([]byte, 0, need)
		}
		n, err := d.Encrypt(s.conn.ssrc, opus, s.daveBuf[:need])
		if err != nil {
			return err
		}
		payload = s.daveBuf[:n]
	}
	packet := s.sealer.Seal(s.rtp.Next(audio.FrameSamplesPerChannel), payload)
	_, err := s.conn.udp.WriteToUDP(packet, s.conn.remote)
	return err
}

func (s *sender) sleep(next time.Time) time.Time {
	next = next.Add(frameInterval)
	now := time.Now()
	if late := now.Sub(next); late > stallThreshold {
		if s.speaking {
			s.conn.source.Stall(late)
			s.conn.log.Warn("send loop fell behind", "lateMs", late.Milliseconds())
		}
		return now
	}
	if wait := next.Sub(now); wait > 0 {
		time.Sleep(wait)
	}
	return next
}
