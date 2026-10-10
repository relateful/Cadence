package api

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/greed/cadence/internal/analysis"
	"github.com/greed/cadence/internal/lavalink"
	"github.com/greed/cadence/internal/session"
	"github.com/greed/cadence/internal/source"
	"github.com/greed/cadence/internal/voice"
)

const aliasSourceV4 = "youtube"

var filtersV4 = []string{"equalizer", "timescale", "tremolo", "vibrato", "rotation", "lowPass", "karaoke"}

func (s *Server) routesV4(mux *http.ServeMux) {
	mux.HandleFunc("GET /v4/websocket", s.websocket(session.ProtocolV4))
	mux.HandleFunc("GET /version", s.auth(s.versionV4))
	mux.HandleFunc("GET /v4/info", s.auth(s.infoV4))
	mux.HandleFunc("GET /v4/stats", s.auth(s.statsV4))
	mux.HandleFunc("GET /v4/loadtracks", s.auth(s.loadTracksV4))
	mux.HandleFunc("GET /v4/decodetrack", s.auth(s.decodeTrackV4))
	mux.HandleFunc("POST /v4/decodetracks", s.auth(s.decodeTracksV4))
	mux.HandleFunc("POST /v4/prefetch", s.auth(s.prefetch))
	mux.HandleFunc("PATCH /v4/sessions/{session}", s.auth(s.updateSession))
	mux.HandleFunc("GET /v4/sessions/{session}/players", s.auth(s.listPlayersV4))
	mux.HandleFunc("GET /v4/sessions/{session}/players/{guild}", s.auth(s.getPlayerV4))
	mux.HandleFunc("PATCH /v4/sessions/{session}/players/{guild}", s.auth(s.updatePlayerV4))
	mux.HandleFunc("DELETE /v4/sessions/{session}/players/{guild}", s.auth(s.deletePlayer))
	mux.HandleFunc("POST /v4/sessions/{session}/players/{guild}/mix/deckb", s.auth(s.loadDeckB))
	mux.HandleFunc("POST /v4/sessions/{session}/players/{guild}/mix/crossfade", s.auth(s.crossfade))
	mux.HandleFunc("POST /v4/sessions/{session}/players/{guild}/mix/analyze", s.auth(s.analyzeDeckV4))
	mux.HandleFunc("DELETE /v4/sessions/{session}/players/{guild}/mix", s.auth(s.cancelMix))
	mux.HandleFunc("POST /v4/sessions/{session}/mix/analyzetrack", s.auth(s.analyzeTrack))
	mux.HandleFunc("GET /v4/mix/analysis/{source}/{identifier}", s.auth(s.getAnalysis))
}

func (s *Server) versionV4(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(Version))
}

func (s *Server) infoV4(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        lavalink.ParseVersion(Version),
		"buildTime":      s.started.UnixMilli(),
		"git":            map[string]any{"branch": "main", "commit": "", "commitTime": 0},
		"jvm":            runtime.Version(),
		"lavaplayer":     "cadence " + Version,
		"sourceManagers": append(s.resolver.Sources(), aliasSourceV4),
		"filters":        filtersV4,
		"plugins":        []any{},
		"cadence":        Version,
		"wireClock":      true,
		"mixing":         true,
		"dave":           voice.DaveAvailable(),
		"analysis":       analysis.AnalysisVersion,
	})
}

func (s *Server) statsSnapshotV4() lavalink.Stats {
	players, playing := s.counts()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return lavalink.Stats{
		Players:        players,
		PlayingPlayers: playing,
		Uptime:         time.Since(s.started).Milliseconds(),
		Memory:         lavalink.Memory{Free: mem.HeapIdle, Used: mem.HeapInuse, Allocated: mem.Sys, Reservable: mem.Sys},
		CPU:            lavalink.CPU{Cores: runtime.NumCPU()},
	}
}

func (s *Server) statsV4(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statsSnapshotV4())
}

func (s *Server) loadTracksV4(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("identifier")
	if strings.TrimSpace(id) == "" {
		fail(w, r, http.StatusBadRequest, "identifier is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), loadTimeout)
	defer cancel()
	res := s.resolver.Load(ctx, id)
	if res.Type != source.LoadError && len(res.Tracks) > 0 {
		s.resolver.Enqueue(res.Tracks[0])
	}
	writeJSON(w, http.StatusOK, lavalink.LoadResult(res))
}

func (s *Server) decodeTrackV4(w http.ResponseWriter, r *http.Request) {
	t, err := source.DecodeTrack(r.URL.Query().Get("encodedTrack"))
	if err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, lavalink.FromTrack(t))
}

func (s *Server) decodeTracksV4(w http.ResponseWriter, r *http.Request) {
	var raw []string
	if err := decode(r, &raw); err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]lavalink.Track, 0, len(raw))
	for _, e := range raw {
		t, err := source.DecodeTrack(e)
		if err != nil {
			fail(w, r, http.StatusBadRequest, err.Error())
			return
		}
		out = append(out, lavalink.FromTrack(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listPlayersV4(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		fail(w, r, http.StatusNotFound, err.Error())
		return
	}
	out := []lavalink.Player{}
	for _, g := range sess.Guilds() {
		out = append(out, sess.PlayerV4(g))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getPlayerV4(w http.ResponseWriter, r *http.Request) {
	sess, g, ok := s.guild(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sess.PlayerV4(g))
}

func (s *Server) updatePlayerV4(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, sess.PlayerV4(g))
}

func (s *Server) analyzeDeckV4(w http.ResponseWriter, r *http.Request) {
	sess, g, ok := s.guild(w, r, false)
	if !ok {
		return
	}
	g.Player.SetAnalysis(sess.AnalysisTap)
	w.WriteHeader(http.StatusNoContent)
}
