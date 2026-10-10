package player

import (
	"errors"
	"time"

	"github.com/greed/cadence/internal/source"
)

var adoptWindow = 5 * time.Second

var ErrNothingPlaying = errors.New("nothing is playing to queue a next track behind")

type adoption struct {
	encoded string
	inst    uint64
	until   time.Time
}

func (p *Player) SetNext(t source.Track) (uint64, error) {
	playable, err := p.resolve(t)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return 0, p.ctx.Err()
	}
	if p.active == nil && p.loading == nil {
		return 0, ErrNothingPlaying
	}
	if p.next != nil {
		p.next.Close()
	}
	p.instance++
	p.next, p.nextInst = p.newDeck(t, playable, 0), p.instance
	return p.nextInst, nil
}

func (p *Player) ClearNext() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next != nil {
		p.next.Close()
		p.next, p.nextInst = nil, 0
	}
}

func (p *Player) gaplessLocked() bool {
	d := p.active
	if p.next == nil || d == nil || !d.Ended() || d.Failure() != nil || p.pending != nil || p.fade != nil {
		return false
	}
	oldInst := p.activeInst
	p.active, p.activeInst = p.next, p.nextInst
	p.next, p.nextInst = nil, 0
	p.activeGain = 1
	d.Close()
	p.adopt = &adoption{encoded: p.active.Track.Encode(), inst: p.activeInst, until: time.Now().Add(adoptWindow)}
	p.emit(Event{Type: EventTrackEnd, Track: &d.Track, Encoded: d.Track.Encode(), Instance: oldInst, Reason: EndFinished})
	return true
}

func (p *Player) adoptLocked(t source.Track, startMs int64, paused bool) (uint64, bool) {
	a := p.adopt
	if a == nil || startMs != 0 || p.activeInst != a.inst || t.Encode() != a.encoded || time.Now().After(a.until) {
		return 0, false
	}
	p.adopt = nil
	p.paused = paused
	p.emit(trackEvent(EventTrackStart, p.active, a.inst))
	return a.inst, true
}

func (p *Player) expireAdoptionLocked() {
	a := p.adopt
	if a == nil || time.Now().Before(a.until) {
		return
	}
	p.adopt = nil
	if p.active != nil && p.activeInst == a.inst {
		p.emit(trackEvent(EventTrackStart, p.active, a.inst))
	}
}
