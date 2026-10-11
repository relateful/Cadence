package player

import (
	"sync"

	"github.com/greed/cadence/internal/source"
)

type EventType string

const (
	EventTrackLoading   EventType = "TrackLoadingEvent"
	EventTrackStart     EventType = "TrackStartEvent"
	EventTrackAudible   EventType = "TrackAudibleEvent"
	EventTrackEnd       EventType = "TrackEndEvent"
	EventTrackException EventType = "TrackExceptionEvent"
	EventTrackStuck     EventType = "TrackStuckEvent"
	EventMixArmed       EventType = "MixArmedEvent"
	EventMixStarted     EventType = "MixStartedEvent"
	EventMixPromoted    EventType = "MixPromotedEvent"
	EventMixDropped     EventType = "MixDroppedEvent"
	EventHostStarted    EventType = "HostStartedEvent"
	EventHostEnded      EventType = "HostEndedEvent"
	EventSendStall      EventType = "SendStallEvent"
	EventPositionJump   EventType = "PositionEvent"
)

type EndReason string

const (
	EndFinished   EndReason = "finished"
	EndLoadFailed EndReason = "loadFailed"
	EndStopped    EndReason = "stopped"
	EndReplaced   EndReason = "replaced"
	EndCleanup    EndReason = "cleanup"
)

type Event struct {
	Op          string          `json:"op"`
	Type        EventType       `json:"type"`
	GuildID     string          `json:"guildId"`
	Track       *source.Track   `json:"track,omitempty"`
	Encoded     string          `json:"encoded,omitempty"`
	Instance    uint64          `json:"instance,omitempty"`
	Reason      EndReason       `json:"reason,omitempty"`
	Exception   *source.Failure `json:"exception,omitempty"`
	PositionMs  int64           `json:"positionMs,omitempty"`
	SentAt      int64           `json:"sentAt,omitempty"`
	AudibleAt   int64           `json:"audibleAt,omitempty"`
	DurationMs  int64           `json:"durationMs,omitempty"`
	StallMs     int64           `json:"stallMs,omitempty"`
	ThresholdMs int64           `json:"thresholdMs,omitempty"`
	Into        *source.Track   `json:"into,omitempty"`
	IntoMs      int64           `json:"intoPositionMs,omitempty"`
}

type dispatcher struct {
	mu      sync.Mutex
	queue   []Event
	wake    chan struct{}
	handler func(Event)
	closed  bool
}

func newDispatcher(handler func(Event)) *dispatcher {
	d := &dispatcher{wake: make(chan struct{}, 1), handler: handler}
	go d.run()
	return d
}

func (d *dispatcher) push(e Event) {
	e.Op = "event"
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.queue = append(d.queue, e)
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *dispatcher) run() {
	for range d.wake {
		d.mu.Lock()
		batch := d.queue
		d.queue = nil
		closed := d.closed
		d.mu.Unlock()
		for _, e := range batch {
			d.handler(e)
		}
		if closed {
			return
		}
	}
}

func (d *dispatcher) close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}
