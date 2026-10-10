package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/greed/cadence/internal/analysis"
	"github.com/greed/cadence/internal/metrics"
	"github.com/greed/cadence/internal/session"
	"github.com/greed/cadence/internal/source"
	"github.com/greed/cadence/internal/voice"
)

const (
	Version     = "1.0.0"
	maxBodySize = 1 << 20
	loadTimeout = 20 * time.Second
	maxPrefetch = 10
)

type Server struct {
	password      string
	sessions      *session.Manager
	resolver      *source.Resolver
	registry      *analysis.Registry
	offline       *analysis.Offline
	resumeTimeout time.Duration
	bitrate       int
	started       time.Time
	log           *slog.Logger
}

type Deps struct {
	Password      string
	Sessions      *session.Manager
	Resolver      *source.Resolver
	Registry      *analysis.Registry
	Offline       *analysis.Offline
	ResumeTimeout time.Duration
	Bitrate       int
	Log           *slog.Logger
}

func New(d Deps) *Server {
	return &Server{password: d.Password, sessions: d.Sessions, resolver: d.Resolver, registry: d.Registry, offline: d.Offline, resumeTimeout: d.ResumeTimeout, bitrate: d.Bitrate, started: time.Now(), log: d.Log.With("component", "api")}
}

type apiError struct {
	Timestamp int64  `json:"timestamp"`
	Status    int    `json:"status"`
	Error     string `json:"error"`
	Message   string `json:"message"`
	Path      string `json:"path"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	writeJSON(w, status, apiError{Timestamp: time.Now().UnixMilli(), Status: status, Error: http.StatusText(status), Message: msg, Path: r.URL.Path})
}

func jsonBytes(v any) ([]byte, error) {
	return json.Marshal(v)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodySize))
	return dec.Decode(v)
}

func (s *Server) authorized(r *http.Request) bool {
	got := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.password)) == 1
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			fail(w, r, http.StatusUnauthorized, "missing or wrong Authorization header")
			return
		}
		next(w, r)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/websocket", s.websocket)
	mux.HandleFunc("GET /v1/info", s.auth(s.info))
	mux.HandleFunc("GET /v1/stats", s.auth(s.stats))
	mux.HandleFunc("GET /v1/clock", s.auth(s.clock))
	mux.HandleFunc("GET /metrics", s.auth(s.metrics))
	mux.HandleFunc("GET /v1/loadtracks", s.auth(s.loadTracks))
	mux.HandleFunc("GET /v1/decodetrack", s.auth(s.decodeTrack))
	mux.HandleFunc("POST /v1/decodetracks", s.auth(s.decodeTracks))
	mux.HandleFunc("POST /v1/prefetch", s.auth(s.prefetch))
	mux.HandleFunc("GET /v1/mix/analysis/{source}/{identifier}", s.auth(s.getAnalysis))
	mux.HandleFunc("POST /v1/mix/analyze", s.auth(s.analyzeTrack))
	mux.HandleFunc("PATCH /v1/sessions/{session}", s.auth(s.updateSession))
	mux.HandleFunc("GET /v1/sessions/{session}/players", s.auth(s.listPlayers))
	mux.HandleFunc("GET /v1/sessions/{session}/players/{guild}", s.auth(s.getPlayer))
	mux.HandleFunc("PATCH /v1/sessions/{session}/players/{guild}", s.auth(s.updatePlayer))
	mux.HandleFunc("DELETE /v1/sessions/{session}/players/{guild}", s.auth(s.deletePlayer))
	mux.HandleFunc("POST /v1/sessions/{session}/players/{guild}/mix/deckb", s.auth(s.loadDeckB))
	mux.HandleFunc("POST /v1/sessions/{session}/players/{guild}/mix/crossfade", s.auth(s.crossfade))
	mux.HandleFunc("DELETE /v1/sessions/{session}/players/{guild}/mix", s.auth(s.cancelMix))
	return s.recoverer(mux)
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("handler panicked", "path", r.URL.Path, "panic", v)
				fail(w, r, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   Version,
		"sources":   s.resolver.Sources(),
		"dave":      voice.DaveAvailable(),
		"filters":   []string{"equalizer", "timescale", "tremolo", "vibrato", "rotation", "lowPass", "karaoke"},
		"mixing":    true,
		"analysis":  analysis.AnalysisVersion,
		"wireClock": true,
	})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	players, playing := s.counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"players":        players,
		"playingPlayers": playing,
		"sessions":       len(s.sessions.Sessions()),
		"uptime":         time.Since(s.started).Milliseconds(),
		"analysis":       s.offline.Stats(),
	})
}

func (s *Server) counts() (int, int) {
	players, playing := 0, 0
	for _, sess := range s.sessions.Sessions() {
		for _, g := range sess.Guilds() {
			players++
			if st := g.Player.State(); st.Sounding {
				playing++
			}
		}
	}
	return players, playing
}

func (s *Server) clock(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int64{"now": time.Now().UnixMilli()})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	players, playing := s.counts()
	st := s.offline.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	metrics.Write(w, []metrics.Gauge{
		{Name: "cadence_players", Help: "Players held.", Value: float64(players)},
		{Name: "cadence_playing_players", Help: "Players currently sending audio.", Value: float64(playing)},
		{Name: "cadence_sessions", Help: "Client sessions.", Value: float64(len(s.sessions.Sessions()))},
		{Name: "cadence_analysis_queued", Help: "Offline analyses waiting.", Value: float64(st.Queued)},
		{Name: "cadence_analysis_dropped_total", Help: "Offline analyses dropped because the queue was full.", Value: float64(st.Dropped)},
	})
}

func (s *Server) loadTracks(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, encodeResult(res))
}

func (s *Server) prefetch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tracks []string `json:"tracks"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Tracks) == 0 || len(body.Tracks) > maxPrefetch {
		fail(w, r, http.StatusBadRequest, fmt.Sprintf("tracks must hold 1 to %d encoded tracks", maxPrefetch))
		return
	}
	tracks := make([]source.Track, 0, len(body.Tracks))
	for _, e := range body.Tracks {
		t, err := source.DecodeTrack(e)
		if err != nil {
			fail(w, r, http.StatusBadRequest, err.Error())
			return
		}
		tracks = append(tracks, t)
	}
	queued, dropped := 0, 0
	for _, t := range tracks {
		if s.resolver.Enqueue(t) {
			queued++
		} else {
			dropped++
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": queued, "dropped": dropped})
}

type encodedTrack struct {
	Encoded string       `json:"encoded"`
	Info    source.Track `json:"info"`
}

func encoded(t source.Track) encodedTrack {
	return encodedTrack{Encoded: t.Encode(), Info: t}
}

func encodeResult(res source.LoadResult) map[string]any {
	tracks := make([]encodedTrack, 0, len(res.Tracks))
	for _, t := range res.Tracks {
		tracks = append(tracks, encoded(t))
	}
	out := map[string]any{"loadType": res.Type}
	switch res.Type {
	case source.LoadTrack:
		out["data"] = tracks[0]
	case source.LoadPlaylist:
		out["data"] = map[string]any{"info": res.Playlist, "tracks": tracks}
	case source.LoadSearch:
		out["data"] = tracks
	case source.LoadError:
		out["data"] = res.Error
	}
	return out
}

func (s *Server) decodeTrack(w http.ResponseWriter, r *http.Request) {
	t, err := source.DecodeTrack(r.URL.Query().Get("encodedTrack"))
	if err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, encoded(t))
}

func (s *Server) decodeTracks(w http.ResponseWriter, r *http.Request) {
	var raw []string
	if err := decode(r, &raw); err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]encodedTrack, 0, len(raw))
	for _, e := range raw {
		t, err := source.DecodeTrack(e)
		if err != nil {
			fail(w, r, http.StatusBadRequest, err.Error())
			return
		}
		out = append(out, encoded(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getAnalysis(w http.ResponseWriter, r *http.Request) {
	fp, ok := s.registry.Get(r.PathValue("source"), r.PathValue("identifier"))
	if !ok {
		fail(w, r, http.StatusNotFound, "no fingerprint for this track yet")
		return
	}
	writeJSON(w, http.StatusOK, fp)
}

func (s *Server) analyzeTrack(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EncodedTrack string `json:"encodedTrack"`
		Force        bool   `json:"force"`
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
	queued := s.offline.Enqueue(t, body.Force)
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": queued})
}

var errNoSession = errors.New("unknown session")

func (s *Server) session(r *http.Request) (*session.Session, error) {
	sess, ok := s.sessions.Get(r.PathValue("session"))
	if !ok {
		return nil, errNoSession
	}
	return sess, nil
}

func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		fail(w, r, http.StatusNotFound, err.Error())
		return
	}
	var body struct {
		Resuming *bool `json:"resuming"`
		Timeout  *int  `json:"timeout"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	resuming, timeout := sess.Settings()
	if body.Resuming != nil {
		resuming = *body.Resuming
	}
	if body.Timeout != nil && *body.Timeout > 0 {
		timeout = time.Duration(*body.Timeout) * time.Second
	}
	sess.Configure(resuming, timeout)
	writeJSON(w, http.StatusOK, map[string]any{"resuming": resuming, "timeout": int(timeout.Seconds())})
}
