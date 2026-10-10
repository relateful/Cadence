package player

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/source"
)

const (
	resolveTimeout = 15 * time.Second
	MaxVolume      = 1000
	liveWindow     = 200 * time.Millisecond
)

type Resolver interface {
	Playable(ctx context.Context, t source.Track) (source.Track, error)
}

type TapFactory func(t source.Track) Tap

type Config struct {
	GuildID      string
	BufferFrames int
	PlayoutDelay time.Duration
	Open         Opener
	Resolve      Resolver
	OnEvent      func(Event)
	Log          *slog.Logger
	Loudness     LoudnessLookup
	OnUnmeasured func(source.Track)
	TargetLufs   float64
}

type sent struct {
	instance uint64
	posMs    float64
	at       time.Time
}

type pendingMix struct {
	deck       *Deck
	instance   uint64
	durationMs int
	curve      audio.FadeCurve
	atMs       int64
	profile    audio.TransitionProfile
	gain       float64
}

type CrossfadeRequest struct {
	DurationMs   int                     `json:"durationMs"`
	Curve        string                  `json:"curve"`
	AtPositionMs *int64                  `json:"atPositionMs,omitempty"`
	TempoRatio   float64                 `json:"tempoRatio"`
	GainRatio    float64                 `json:"gainRatio"`
	Profile      audio.TransitionProfile `json:"profile"`
}

var (
	ErrNoDeckB      = errors.New("no deck b is loaded")
	ErrNothingToMix = errors.New("nothing is playing on deck a")
)

type Player struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	events *dispatcher

	mu         sync.Mutex
	active     *Deck
	activeInst uint64
	incoming   *Deck
	incInst    uint64
	cued       *Deck
	cuedInst   uint64
	pending    *pendingMix
	fade       *audio.Crossfade
	profile    audio.TransitionProfile
	activeGain float64
	incGain    float64
	outDsp     *audio.DeckDsp
	inDsp      *audio.DeckDsp
	bufA, bufB audio.Frame

	instance  uint64
	paused    bool
	volume    int
	filters   Filters
	last      sent
	audible   map[uint64]bool
	analysis  TapFactory
	rampUntil time.Time
	rampFrom  float64
	rampStart time.Time

	loading     *source.Track
	loadingFrom int64

	next     *Deck
	nextInst uint64
	adopt    *adoption
	nextGen  uint64
}

func New(cfg Config) *Player {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Player{
		cfg:        cfg,
		ctx:        ctx,
		cancel:     cancel,
		volume:     100,
		activeGain: 1,
		incGain:    1,
		outDsp:     audio.NewDeckDsp(),
		inDsp:      audio.NewDeckDsp(),
		audible:    map[uint64]bool{},
	}
	p.events = newDispatcher(cfg.OnEvent)
	return p
}

func (p *Player) emit(e Event) {
	e.GuildID = p.cfg.GuildID
	p.events.push(e)
}

func trackEvent(t EventType, d *Deck, inst uint64) Event {
	tr := d.Track
	return Event{Type: t, Track: &tr, Encoded: tr.Encode(), Instance: inst}
}

func (p *Player) SetAnalysis(f TapFactory) {
	p.mu.Lock()
	p.analysis = f
	p.mu.Unlock()
}

func (p *Player) newDeck(t, playable source.Track, startMs int64) *Deck {
	d := newDeck(t, playable, p.cfg.BufferFrames, p.cfg.Log)
	p.levelDeck(d)
	d.SetFilters(p.filters)
	if p.analysis != nil && startMs == 0 {
		if tap := p.analysis(playable); tap != nil {
			d.attachTap(tap)
		}
	}
	d.start(p.ctx, p.cfg.Open, startMs)
	return d
}

func (p *Player) resolve(t source.Track) (source.Track, error) {
	ctx, cancel := context.WithTimeout(p.ctx, resolveTimeout)
	defer cancel()
	return p.cfg.Resolve.Playable(ctx, t)
}

func (p *Player) Play(t source.Track, startMs int64, paused bool) uint64 {
	p.mu.Lock()
	if inst, ok := p.adoptLocked(t, startMs, paused); ok {
		p.mu.Unlock()
		return inst
	}
	p.adopt = nil
	p.instance++
	inst := p.instance
	old, oldInst := p.active, p.activeInst
	wasLoading := p.takeLoadingLocked()
	p.clearMixLocked()
	p.active, p.activeInst = nil, inst
	tr := t
	p.loading, p.loadingFrom = &tr, startMs
	p.paused = paused
	p.mu.Unlock()
	if old != nil {
		old.Close()
		p.emit(Event{Type: EventTrackEnd, Track: &old.Track, Encoded: old.Track.Encode(), Instance: oldInst, Reason: EndReplaced})
	}
	if wasLoading != nil {
		p.emit(Event{Type: EventTrackEnd, Track: wasLoading, Encoded: wasLoading.Encode(), Instance: oldInst, Reason: EndReplaced})
	}
	p.emit(Event{Type: EventTrackLoading, Track: &tr, Encoded: tr.Encode(), Instance: inst})
	go p.load(t, startMs, inst)
	return inst
}

func (p *Player) load(t source.Track, startMs int64, inst uint64) {
	playable, err := p.resolve(t)
	p.mu.Lock()
	if p.activeInst != inst || p.ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	p.loading = nil
	if err != nil {
		p.activeInst = 0
		p.mu.Unlock()
		tr := t
		f := source.Classify(t.Source, err)
		p.emit(Event{Type: EventTrackException, Track: &tr, Encoded: tr.Encode(), Instance: inst, Exception: f})
		p.emit(Event{Type: EventTrackEnd, Track: &tr, Encoded: tr.Encode(), Instance: inst, Reason: EndLoadFailed})
		return
	}
	d := p.newDeck(t, playable, startMs)
	p.active = d
	p.mu.Unlock()
	p.emit(trackEvent(EventTrackStart, d, inst))
	go p.watch(d)
}

func (p *Player) watch(d *Deck) {
	<-d.done
	if d.Failure() == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == d && len(d.queue) == 0 {
		p.checkEndLocked()
	}
}

func (p *Player) clearMixLocked() {
	for _, d := range []*Deck{p.incoming, p.cued, p.next} {
		if d != nil {
			d.Close()
		}
	}
	if p.pending != nil {
		p.pending.deck.Close()
	}
	p.incoming, p.cued, p.pending, p.fade = nil, nil, nil, nil
	p.next, p.nextInst = nil, 0
	p.nextGen++
	p.incGain = 1
	p.activeGain = 1
	p.rampUntil = time.Time{}
}

func (p *Player) Stop() {
	p.mu.Lock()
	d, inst := p.active, p.activeInst
	wasLoading := p.takeLoadingLocked()
	p.clearMixLocked()
	p.active, p.activeInst = nil, 0
	p.mu.Unlock()
	if d != nil {
		d.Close()
		p.emit(Event{Type: EventTrackEnd, Track: &d.Track, Encoded: d.Track.Encode(), Instance: inst, Reason: EndStopped})
	}
	if wasLoading != nil {
		p.emit(Event{Type: EventTrackEnd, Track: wasLoading, Encoded: wasLoading.Encode(), Instance: inst, Reason: EndStopped})
	}
}

func (p *Player) takeLoadingLocked() *source.Track {
	t := p.loading
	p.loading = nil
	return t
}

func (p *Player) Pause(paused bool) {
	p.mu.Lock()
	p.paused = paused
	p.mu.Unlock()
}

func (p *Player) SetVolume(v int) {
	p.mu.Lock()
	p.volume = max(0, min(MaxVolume, v))
	p.mu.Unlock()
}

func (p *Player) SetFilters(f Filters) {
	p.mu.Lock()
	p.filters = f
	for _, d := range []*Deck{p.active, p.incoming, p.cued, p.next} {
		if d != nil {
			d.SetFilters(f)
		}
	}
	p.mu.Unlock()
}

func (p *Player) SeekTo(ms int64) error {
	p.mu.Lock()
	if p.active == nil {
		p.mu.Unlock()
		return ErrNothingToMix
	}
	if p.fade != nil && p.incoming != nil {
		p.promoteLocked()
	}
	if p.pending != nil || p.cued != nil {
		dropped := p.active.Track
		if p.pending != nil {
			p.pending.deck.Close()
			p.pending = nil
		}
		if p.cued != nil {
			p.cued.Close()
			p.cued = nil
		}
		p.emit(Event{Type: EventMixDropped, Track: &dropped, Instance: p.activeInst})
	}
	d, inst := p.active, p.activeInst
	next := p.newDeck(d.Track, d.Playable, max(0, ms))
	p.active = next
	p.last = sent{}
	p.mu.Unlock()
	d.Close()
	p.emit(Event{Type: EventPositionJump, Track: &next.Track, Instance: inst, PositionMs: ms})
	return nil
}

func (p *Player) LoadDeckB(t source.Track, startMs int64) (uint64, error) {
	playable, err := p.resolve(t)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return 0, p.ctx.Err()
	}
	if p.cued != nil {
		p.cued.Close()
	}
	p.instance++
	p.cued, p.cuedInst = p.newDeck(t, playable, max(0, startMs)), p.instance
	return p.cuedInst, nil
}

func (p *Player) StartCrossfade(req CrossfadeRequest) error {
	curve, err := audio.ParseCurve(req.Curve)
	if err != nil {
		return err
	}
	if req.DurationMs <= 0 {
		return errors.New("durationMs must be positive")
	}
	p.mu.Lock()
	if p.cued == nil {
		p.mu.Unlock()
		return ErrNoDeckB
	}
	if p.active == nil {
		p.mu.Unlock()
		return ErrNothingToMix
	}
	deck, inst := p.cued, p.cuedInst
	p.cued = nil
	tempo := req.TempoRatio
	if tempo <= 0 {
		tempo = 1
	}
	deck.SetTempoRatio(tempo)
	gain := req.GainRatio
	if gain <= 0 {
		gain = 1
	}
	if req.AtPositionMs != nil {
		if p.pending != nil {
			p.pending.deck.Close()
		}
		p.pending = &pendingMix{deck: deck, instance: inst, durationMs: req.DurationMs, curve: curve, atMs: *req.AtPositionMs, profile: req.Profile, gain: gain}
		p.mu.Unlock()
		p.emit(Event{Type: EventMixArmed, Track: &deck.Track, Instance: inst, PositionMs: *req.AtPositionMs, DurationMs: int64(req.DurationMs)})
		return nil
	}
	p.beginFadeLocked(deck, inst, req.DurationMs, curve, req.Profile, gain)
	p.mu.Unlock()
	return nil
}

func (p *Player) beginFadeLocked(deck *Deck, inst uint64, durationMs int, curve audio.FadeCurve, profile audio.TransitionProfile, gain float64) {
	p.pending = nil
	p.profile = profile
	if deck.measured {
		gain = 1
	}
	p.incGain = gain
	p.outDsp.Reset()
	p.inDsp.Reset()
	p.incoming, p.incInst = deck, inst
	p.fade = audio.NewCrossfade(durationMs, curve)
	into := deck.Track
	p.emit(Event{Type: EventMixStarted, Track: p.activeTrackLocked(), Instance: p.activeInst, Into: &into, IntoMs: deck.PositionMs(), DurationMs: int64(p.fade.DurationMs())})
}

func (p *Player) activeTrackLocked() *source.Track {
	if p.active == nil {
		return nil
	}
	t := p.active.Track
	return &t
}

func (p *Player) CancelPending() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending != nil {
		p.pending.deck.Close()
		p.pending = nil
	}
	if p.cued != nil {
		p.cued.Close()
		p.cued = nil
	}
}

type FrameInfo struct {
	Instance   uint64
	PositionMs float64
	Mixing     bool
}

func (p *Player) releasePendingLocked() {
	w := p.pending
	if w == nil || p.active == nil {
		return
	}
	if p.active.Ended() {
		p.pending = nil
		w.deck.Close()
		p.emit(Event{Type: EventMixDropped, Track: &w.deck.Track, Instance: w.instance})
		return
	}
	if p.active.PositionMs() >= w.atMs {
		p.beginFadeLocked(w.deck, w.instance, w.durationMs, w.curve, w.profile, w.gain)
	}
}

func (p *Player) gainLocked() float64 {
	return float64(p.volume) / 100
}

func (p *Player) NextFrame(out *audio.Frame) (FrameInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused || p.active == nil {
		return FrameInfo{}, false
	}
	p.applyRampLocked()
	p.expireAdoptionLocked()
	p.releasePendingLocked()
	vol := p.gainLocked()
	if p.fade != nil && p.incoming != nil {
		posA, aOK := p.active.Read(&p.bufA)
		_, bOK := p.incoming.Read(&p.bufB)
		p.fade.Advance()
		prog := p.fade.Progress()
		if aOK && p.profile.SwapsBass() {
			p.outDsp.ApplyBassGain(&p.bufA, p.profile.OutgoingBassGain(prog))
		}
		if aOK && p.profile.SweepsOut() {
			p.outDsp.ApplyHighPass(&p.bufA, p.profile.OutgoingSweepHz(prog))
		}
		if bOK && p.profile.SwapsBass() {
			p.inDsp.ApplyBassGain(&p.bufB, p.profile.IncomingBassGain(prog))
		}
		if bOK && p.profile.SweepsIn() {
			p.inDsp.ApplyHighPass(&p.bufB, p.profile.IncomingSweepHz(prog))
		}
		a, b := &p.bufA, &p.bufB
		if !aOK {
			a = &audio.Silence
		}
		if !bOK {
			b = &audio.Silence
		}
		audio.Mix(a, p.fade.OutGain*p.activeGain*p.active.norm*vol, b, p.fade.InGain*p.incGain*p.incoming.norm*vol, out)
		info := FrameInfo{Instance: p.activeInst, PositionMs: posA, Mixing: true}
		if p.fade.Finished() {
			p.promoteLocked()
		}
		return info, true
	}
	pos, ok := p.active.Read(&p.bufA)
	if !ok && p.gaplessLocked() {
		pos, ok = p.active.Read(&p.bufA)
	}
	if !ok {
		p.checkEndLocked()
		return FrameInfo{}, false
	}
	audio.Scale(&p.bufA, p.activeGain*p.active.norm*vol, out)
	return FrameInfo{Instance: p.activeInst, PositionMs: pos}, true
}

func (p *Player) checkEndLocked() {
	d, inst := p.active, p.activeInst
	if f := d.Failure(); f != nil {
		p.active, p.activeInst = nil, 0
		p.clearMixLocked()
		d.Close()
		p.emit(Event{Type: EventTrackException, Track: &d.Track, Encoded: d.Track.Encode(), Instance: inst, Exception: f})
		p.emit(Event{Type: EventTrackEnd, Track: &d.Track, Encoded: d.Track.Encode(), Instance: inst, Reason: EndLoadFailed})
		return
	}
	if d.Ended() {
		p.active, p.activeInst = nil, 0
		if p.pending != nil {
			p.pending.deck.Close()
			p.emit(Event{Type: EventMixDropped, Track: &p.pending.deck.Track, Instance: p.pending.instance})
			p.pending = nil
		}
		d.Close()
		p.emit(Event{Type: EventTrackEnd, Track: &d.Track, Encoded: d.Track.Encode(), Instance: inst, Reason: EndFinished})
		return
	}
	if d.Stuck() {
		p.emit(Event{Type: EventTrackStuck, Track: &d.Track, Encoded: d.Track.Encode(), Instance: inst, ThresholdMs: stuckAfter.Milliseconds()})
	}
}

const tempoRampDuration = 11 * time.Second

func (p *Player) promoteLocked() {
	old, oldInst := p.active, p.activeInst
	p.active, p.activeInst = p.incoming, p.incInst
	p.incoming, p.fade = nil, nil
	p.activeGain, p.incGain = p.incGain, 1
	p.rampUntil = time.Time{}
	if r := p.active.TempoRatio(); r != 1 {
		p.rampFrom, p.rampStart, p.rampUntil = r, time.Now(), time.Now().Add(tempoRampDuration)
	}
	old.Close()
	into := p.active.Track
	p.emit(Event{Type: EventMixPromoted, Track: &old.Track, Instance: oldInst, Into: &into, IntoMs: p.active.PositionMs()})
	p.emit(Event{Type: EventTrackEnd, Track: &old.Track, Encoded: old.Track.Encode(), Instance: oldInst, Reason: EndReplaced})
	p.emit(trackEvent(EventTrackStart, p.active, p.activeInst))
}

func (p *Player) applyRampLocked() {
	if p.rampUntil.IsZero() || p.active == nil {
		return
	}
	now := time.Now()
	if !now.Before(p.rampUntil) {
		p.active.SetTempoRatio(1)
		p.rampUntil = time.Time{}
		return
	}
	prog := float64(now.Sub(p.rampStart)) / float64(p.rampUntil.Sub(p.rampStart))
	p.active.SetTempoRatio(p.rampFrom + (1-p.rampFrom)*prog)
}

func (p *Player) Sent(info FrameInfo, at time.Time) {
	p.mu.Lock()
	p.last = sent{instance: info.Instance, posMs: info.PositionMs, at: at}
	first := !p.audible[info.Instance]
	if first {
		p.audible = map[uint64]bool{info.Instance: true}
	}
	var audible *Event
	if first && p.active != nil && p.activeInst == info.Instance {
		tr := p.activeTrackLocked()
		ev := Event{Type: EventTrackAudible, Track: tr, Encoded: tr.Encode(), Instance: info.Instance, PositionMs: int64(info.PositionMs), SentAt: at.UnixMilli(), AudibleAt: at.Add(p.cfg.PlayoutDelay).UnixMilli()}
		if p.adopt != nil && p.adopt.inst == info.Instance {
			p.adopt.audible = &ev
		} else {
			audible = &ev
		}
	}
	p.mu.Unlock()
	if audible != nil {
		p.emit(*audible)
	}
}

func (p *Player) Stall(gap time.Duration) {
	p.mu.Lock()
	inst := p.activeInst
	tr := p.activeTrackLocked()
	p.mu.Unlock()
	p.emit(Event{Type: EventSendStall, Track: tr, Instance: inst, StallMs: gap.Milliseconds()})
}

type State struct {
	Track           *source.Track `json:"track,omitempty"`
	Playable        *source.Track `json:"playable,omitempty"`
	Instance        uint64        `json:"instance,omitempty"`
	Paused          bool          `json:"paused"`
	Volume          int           `json:"volume"`
	Filters         Filters       `json:"filters"`
	PositionMs      int64         `json:"positionMs"`
	AnchorMs        float64       `json:"anchorMs"`
	AnchorSentAt    int64         `json:"anchorSentAt"`
	AnchorAudibleAt int64         `json:"anchorAudibleAt"`
	Rate            float64       `json:"rate"`
	Sounding        bool          `json:"sounding"`
	Mixing          bool          `json:"mixing"`
	MixInto         *source.Track `json:"mixInto,omitempty"`
	MixIntoMs       int64         `json:"mixIntoPositionMs,omitempty"`
	MixProgress     float64       `json:"mixProgress,omitempty"`
	DeckBLoaded     bool          `json:"deckBLoaded"`
	MixArmedAtMs    *int64        `json:"mixArmedAtMs,omitempty"`
	NextTrack       *source.Track `json:"nextTrack,omitempty"`
}

func (p *Player) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := State{Paused: p.paused, Volume: p.volume, Filters: p.filters, Instance: p.activeInst, DeckBLoaded: p.cued != nil}
	rate := p.filters.PositionRate()
	if p.active != nil {
		rate *= p.active.TempoRatio()
		t, pl := p.active.Track, p.active.Playable
		s.Track, s.Playable = &t, &pl
		s.PositionMs = p.active.PositionMs()
	} else if p.loading != nil {
		t := *p.loading
		s.Track, s.PositionMs = &t, p.loadingFrom
	}
	s.Rate = rate
	if p.last.instance == p.activeInst && !p.last.at.IsZero() {
		s.AnchorMs = p.last.posMs
		s.AnchorSentAt = p.last.at.UnixMilli()
		s.AnchorAudibleAt = p.last.at.Add(p.cfg.PlayoutDelay).UnixMilli()
		s.Sounding = !p.paused && time.Since(p.last.at) < liveWindow
		pos := p.last.posMs
		if s.Sounding {
			pos += float64(time.Since(p.last.at).Milliseconds()) * rate
		}
		s.PositionMs = int64(pos)
	}
	if p.fade != nil && p.incoming != nil {
		into := p.incoming.Track
		s.Mixing, s.MixInto, s.MixIntoMs, s.MixProgress = true, &into, p.incoming.PositionMs(), p.fade.Progress()
	}
	if p.pending != nil {
		at := p.pending.atMs
		s.MixArmedAtMs = &at
	}
	if p.next != nil {
		nt := p.next.Track
		s.NextTrack = &nt
	}
	return s
}

func (p *Player) Destroy() {
	p.mu.Lock()
	d, inst := p.active, p.activeInst
	wasLoading := p.takeLoadingLocked()
	p.clearMixLocked()
	p.active = nil
	p.mu.Unlock()
	if d != nil {
		d.Close()
		p.emit(Event{Type: EventTrackEnd, Track: &d.Track, Encoded: d.Track.Encode(), Instance: inst, Reason: EndCleanup})
	}
	if wasLoading != nil {
		p.emit(Event{Type: EventTrackEnd, Track: wasLoading, Encoded: wasLoading.Encode(), Instance: inst, Reason: EndCleanup})
	}
	p.cancel()
	p.events.close()
}
