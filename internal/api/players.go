package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/greed/cadence/internal/player"
	"github.com/greed/cadence/internal/session"
	"github.com/greed/cadence/internal/source"
	"github.com/greed/cadence/internal/voice"
)

func (s *Server) guild(w http.ResponseWriter, r *http.Request, create bool) (*session.Session, *session.Guild, bool) {
	sess, err := s.session(r)
	if err != nil {
		fail(w, r, http.StatusNotFound, err.Error())
		return nil, nil, false
	}
	g := sess.Guild(r.PathValue("guild"), create)
	if g == nil {
		fail(w, r, http.StatusNotFound, "no player for this guild")
		return nil, nil, false
	}
	return sess, g, true
}

func (s *Server) listPlayers(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		fail(w, r, http.StatusNotFound, err.Error())
		return
	}
	out := []session.PlayerUpdate{}
	for _, g := range sess.Guilds() {
		out = append(out, sess.Update(g))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getPlayer(w http.ResponseWriter, r *http.Request) {
	sess, g, ok := s.guild(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sess.Update(g))
}

type trackField struct {
	Encoded  *string        `json:"encoded"`
	UserData map[string]any `json:"userData"`
	set      bool
}

func (t *trackField) UnmarshalJSON(raw []byte) error {
	t.set = true
	type plain struct {
		Encoded  *string        `json:"encoded"`
		UserData map[string]any `json:"userData"`
	}
	var p plain
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	t.Encoded, t.UserData = p.Encoded, p.UserData
	return nil
}

type voiceField struct {
	Token     string `json:"token"`
	Endpoint  string `json:"endpoint"`
	SessionID string `json:"sessionId"`
	ChannelID string `json:"channelId"`
	Bitrate   int    `json:"bitrate"`
}

type playerPatch struct {
	Track    *trackField     `json:"track"`
	Position *int64          `json:"position"`
	EndTime  *int64          `json:"endTime"`
	Volume   *int            `json:"volume"`
	Paused   *bool           `json:"paused"`
	Filters  *player.Filters `json:"filters"`
	Voice    *voiceField     `json:"voice"`
	Analyze  *bool           `json:"analyze"`
	Bitrate  *int            `json:"bitrate"`
	Next     *trackField     `json:"nextTrack"`
}

func (s *Server) updatePlayer(w http.ResponseWriter, r *http.Request) {
	sess, g, ok := s.guild(w, r, true)
	if !ok {
		return
	}
	var p playerPatch
	if err := decode(r, &p); err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if status, err := s.applyPatch(sess, g, p, r.URL.Query().Get("noReplace") == "true"); err != nil {
		fail(w, r, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess.Update(g))
}

func (s *Server) applyPatch(sess *session.Session, g *session.Guild, p playerPatch, noReplace bool) (int, error) {
	if p.Bitrate != nil {
		g.SetBitrate(*p.Bitrate)
	}
	if p.Analyze != nil {
		if *p.Analyze {
			g.Player.SetAnalysis(sess.AnalysisTap)
		} else {
			g.Player.SetAnalysis(nil)
		}
	}
	if p.Filters != nil {
		g.Player.SetFilters(*p.Filters)
	}
	if p.Volume != nil {
		g.Player.SetVolume(*p.Volume)
	}
	if p.Voice != nil {
		bitrate := p.Voice.Bitrate
		if bitrate <= 0 {
			bitrate = g.PreferredBitrate()
		}
		if bitrate <= 0 {
			bitrate = s.bitrate
		}
		server := voice.Server{GuildID: g.ID, UserID: sess.UserID, ChannelID: p.Voice.ChannelID, SessionID: p.Voice.SessionID, Token: p.Voice.Token, Endpoint: p.Voice.Endpoint, Bitrate: bitrate}
		if err := g.UpdateVoice(server); err != nil {
			return http.StatusBadRequest, err
		}
	}
	switch {
	case p.Track != nil && p.Track.set && p.Track.Encoded == nil:
		g.Player.Stop()
	case p.Track != nil && p.Track.Encoded != nil:
		t, err := source.DecodeTrack(*p.Track.Encoded)
		if err != nil {
			return http.StatusBadRequest, err
		}
		if noReplace && g.Player.State().Track != nil {
			break
		}
		var start int64
		if p.Position != nil {
			start = max(0, *p.Position)
		}
		paused := p.Paused != nil && *p.Paused
		g.SetUserData(t.Encode(), p.Track.UserData)
		g.Player.Play(t, start, paused)
		p.Position, p.Paused = nil, nil
	}
	if p.Next != nil {
		if status, err := s.applyNext(g, p.Next); err != nil {
			return status, err
		}
	}
	if p.Paused != nil {
		g.Player.Pause(*p.Paused)
	}
	if p.Position != nil {
		if err := g.Player.SeekTo(*p.Position); err != nil && !errors.Is(err, player.ErrNothingToMix) {
			return http.StatusConflict, err
		}
	}
	return http.StatusOK, nil
}

func (s *Server) deletePlayer(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		fail(w, r, http.StatusNotFound, err.Error())
		return
	}
	sess.DestroyGuild(r.PathValue("guild"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) loadDeckB(w http.ResponseWriter, r *http.Request) {
	_, g, ok := s.guild(w, r, false)
	if !ok {
		return
	}
	var body struct {
		EncodedTrack string `json:"encodedTrack"`
		Position     int64  `json:"position"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	t, err := source.DecodeTrack(body.EncodedTrack)
	if err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), loadTimeout)
	defer cancel()
	if _, err := s.resolver.Playable(ctx, t); err != nil {
		fail(w, r, http.StatusUnprocessableEntity, source.Classify(t.Source, err).Error())
		return
	}
	inst, err := g.Player.LoadDeckB(t, body.Position)
	if err != nil {
		fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint64{"instance": inst})
}

func (s *Server) crossfade(w http.ResponseWriter, r *http.Request) {
	_, g, ok := s.guild(w, r, false)
	if !ok {
		return
	}
	var req player.CrossfadeRequest
	if err := decode(r, &req); err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	switch err := g.Player.StartCrossfade(req); {
	case errors.Is(err, player.ErrNoDeckB), errors.Is(err, player.ErrNothingToMix):
		fail(w, r, http.StatusConflict, err.Error())
	case err != nil:
		fail(w, r, http.StatusBadRequest, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) cancelMix(w http.ResponseWriter, r *http.Request) {
	_, g, ok := s.guild(w, r, false)
	if !ok {
		return
	}
	g.Player.CancelPending()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) applyNext(g *session.Guild, next *trackField) (int, error) {
	if next.Encoded == nil {
		g.Player.ClearNext()
		return http.StatusOK, nil
	}
	t, err := source.DecodeTrack(*next.Encoded)
	if err != nil {
		return http.StatusBadRequest, err
	}
	go func() {
		if _, err := g.Player.SetNext(t); errors.Is(err, player.ErrNextSuperseded) {
			s.log.Debug("next track preload superseded", "guild", g.ID, "track", t.Key())
		} else if err != nil {
			s.log.Warn("next track could not be preloaded", "guild", g.ID, "track", t.Key(), "error", err)
		}
	}()
	return http.StatusOK, nil
}
