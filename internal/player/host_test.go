package player

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/greed/cadence/internal/audio"
)

func constantClip(ms int64, value int16) *voiceClip {
	frames := make([]audio.Frame, audio.FramesFor(ms))
	for i := range frames {
		for k := range frames[i] {
			frames[i][k] = value
		}
	}
	return &voiceClip{frames: frames}
}

func hostedPlayer(t *testing.T, voiceMs int64) (*Player, *recorder, HostRequest) {
	t.Helper()
	p, rec := newTestPlayer(map[string]float64{"a": 8, "b": 8})
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	if _, err := p.LoadDeckB(track("b"), 2000); err != nil {
		t.Fatal(err)
	}
	at := int64(200)
	req := HostRequest{AtPositionMs: &at, OutFadeMs: 300, LeadMs: 400, RampMs: 200, DuckDb: 9}
	if err := p.armHost(req, constantClip(voiceMs, 6000)); err != nil {
		t.Fatal(err)
	}
	return p, rec, req
}

func TestHostHoldsTheIncomingDeckUntilItsWindowOpens(t *testing.T) {
	p, rec, _ := hostedPlayer(t, 1000)
	defer p.Destroy()
	var out audio.Frame
	for !rec.has(EventHostStarted) {
		frame(t, p, &out)
	}
	for range 20 {
		frame(t, p, &out)
	}
	if s := p.State(); !s.Mixing || !s.Hosting || s.MixIntoMs != 2000 {
		t.Fatalf("while the dj talks deck b must not move: %+v", s)
	}
}

func TestHostPlaysTheVoiceOverSilenceOnceTheOutgoingTrackHasFaded(t *testing.T) {
	p, rec, _ := hostedPlayer(t, 1000)
	defer p.Destroy()
	var out audio.Frame
	for !rec.has(EventHostStarted) {
		frame(t, p, &out)
	}
	for range 20 {
		frame(t, p, &out)
	}
	if out[0] < 1000 || out[0] != out[len(out)-1] {
		t.Fatalf("after the outgoing fade only the levelled voice should sound, got %d…%d", out[0], out[len(out)-1])
	}
}

func TestHostPromotesTheIncomingTrackFromWhereItOpened(t *testing.T) {
	p, rec, _ := hostedPlayer(t, 1000)
	defer p.Destroy()
	var out audio.Frame
	for range 120 {
		frame(t, p, &out)
	}
	ended := rec.wait(t, EventHostEnded)
	if ended.DurationMs != 1000 {
		t.Fatalf("host ended after %dms, want the voice length", ended.DurationMs)
	}
	promoted := rec.wait(t, EventMixPromoted)
	if promoted.IntoMs < 2000 || promoted.IntoMs > 2000+400+200+100 {
		t.Fatalf("deck b promoted at %dms, want just past its start: only lead + ramp may have been read", promoted.IntoMs)
	}
	rec.wait(t, EventTrackStart)
	if s := p.State(); s.Track == nil || s.Track.Identifier != "b" || s.Mixing || s.Hosting {
		t.Fatalf("state after the dj handed over %+v", s)
	}
}

func TestHostWithoutDeckBIsRefused(t *testing.T) {
	p, _ := newTestPlayer(map[string]float64{"a": 5})
	defer p.Destroy()
	if err := p.armHost(HostRequest{}, constantClip(500, 1)); !errors.Is(err, ErrNoDeckB) {
		t.Fatalf("err %v", err)
	}
}

func TestStoppingDuringTheVoiceDropsTheHost(t *testing.T) {
	p, rec, _ := hostedPlayer(t, 2000)
	defer p.Destroy()
	var out audio.Frame
	for !rec.has(EventHostStarted) {
		frame(t, p, &out)
	}
	p.Stop()
	if s := p.State(); s.Mixing || s.Hosting {
		t.Fatalf("a stop must clear the dj, got %+v", s)
	}
	if ended := rec.wait(t, EventHostEnded); ended.DurationMs >= 2000 {
		t.Fatalf("a dropped dj must report how long it actually spoke, got %dms", ended.DurationMs)
	}
}

func TestHostShapeUsesDefaultsAndCaps(t *testing.T) {
	s := HostRequest{OutFadeMs: 60_000}.shape(5000)
	if s.OutFadeMs != maxHostFadeMs || s.LeadMs != defaultLeadMs || s.RampMs != defaultRampMs || s.VoiceMs != 5000 {
		t.Fatalf("shape %+v", s)
	}
	if s.Duck != audio.DuckGain(defaultDuckDb) {
		t.Fatalf("duck %v", s.Duck)
	}
}

func TestDecodeVoiceRejectsBadInput(t *testing.T) {
	if _, err := decodeVoice("%%%not base64"); err == nil {
		t.Fatal("garbage must be rejected")
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, MaxVoiceBytes+1))
	if _, err := decodeVoice(big); !errors.Is(err, ErrVoiceTooLarge) {
		t.Fatalf("err %v, want too large", err)
	}
	if _, err := decodeVoice(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 200)))); err == nil {
		t.Fatal("bytes that are not mp3 must be rejected")
	}
}

func TestLevelBringsQuietSpeechUpWithoutClipping(t *testing.T) {
	clip := constantClip(1000, 500)
	level(clip.frames)
	if v := float64(clip.frames[0][0]); v <= 500 || v > voicePeakCeiling*32768+1 {
		t.Fatalf("levelled sample %v", v)
	}
}

func TestColdOpenTalksFirstThenStartsTheFirstSong(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"b": 8})
	defer p.Destroy()
	if _, err := p.LoadDeckB(track("b"), 1500); err != nil {
		t.Fatal(err)
	}
	if err := p.armHost(HostRequest{LeadMs: 400, RampMs: 200}, constantClip(1000, 6000)); err != nil {
		t.Fatalf("a cold open with nothing playing must be accepted: %v", err)
	}
	var out audio.Frame
	frame(t, p, &out)
	rec.wait(t, EventHostStarted)
	if out[0] < 1000 {
		t.Fatalf("the voice must be heard straight away, got %d", out[0])
	}
	if s := p.State(); !s.Hosting || s.MixIntoMs != 1500 {
		t.Fatalf("while the dj opens deck b waits at its cue: %+v", s)
	}
	for range 80 {
		frame(t, p, &out)
	}
	start := rec.wait(t, EventTrackStart)
	if start.Track == nil || start.Track.Identifier != "b" {
		t.Fatalf("track start %+v", start.Track)
	}
	if rec.has(EventMixPromoted) || rec.has(EventTrackEnd) {
		t.Fatal("a cold open has no outgoing track to end or promote from")
	}
	if s := p.State(); s.Track == nil || s.Track.Identifier != "b" || s.Hosting || s.PositionMs < 1500 {
		t.Fatalf("after the opener the first song plays from its cue: %+v", s)
	}
}

func TestColdOpenWithoutACuedSongIsRefused(t *testing.T) {
	p, _ := newTestPlayer(map[string]float64{})
	defer p.Destroy()
	if err := p.armHost(HostRequest{}, constantClip(500, 1)); !errors.Is(err, ErrNoDeckB) {
		t.Fatalf("err %v", err)
	}
}
