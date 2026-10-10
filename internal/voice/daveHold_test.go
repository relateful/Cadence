package voice

import (
	"io"
	"log/slog"
	"testing"

	"github.com/disgoorg/godave"
)

type readiness struct {
	godave.Session
	ready bool
}

func (r readiness) Ready() bool { return r.ready }

func connWith(ready bool, version uint16) *Conn {
	c := &Conn{dave: readiness{ready: ready}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.setDaveVersion(version, "test")
	return c
}

func TestPassthroughCallsAreNotHeld(t *testing.T) {
	if connWith(false, 0).holdAudio() {
		t.Fatal("a call Discord runs without E2EE (protocol 0) must not hold audio")
	}
}

func TestEncryptedCallsHoldUntilReady(t *testing.T) {
	if !connWith(false, 1).holdAudio() {
		t.Fatal("an E2EE call must hold audio until the epoch is ready")
	}
	if connWith(true, 1).holdAudio() {
		t.Fatal("a ready E2EE call must not hold audio")
	}
}

func TestNoDaveSessionNeverHolds(t *testing.T) {
	c := &Conn{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if c.holdAudio() {
		t.Fatal("a connection without a DAVE session must not hold audio")
	}
}
