package api

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/greed/cadence/internal/session"
)

const (
	pingInterval = 30 * time.Second
	writeTimeout = 10 * time.Second
)

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		fail(w, r, http.StatusUnauthorized, "missing or wrong Authorization header")
		return
	}
	userID := r.Header.Get("User-Id")
	if userID == "" {
		fail(w, r, http.StatusBadRequest, "User-Id header is required")
		return
	}
	client := r.Header.Get("Client-Name")
	var (
		sess    *session.Session
		out     <-chan []byte
		resumed bool
	)
	if candidate, ok := s.sessions.Resume(r.Header.Get("Session-Id"), userID); ok {
		if ch, wasResumed, err := candidate.Attach(); err == nil {
			sess, out, resumed = candidate, ch, wasResumed
		}
	}
	if sess == nil {
		sess = s.sessions.Create(userID, client, s.resumeTimeout)
		ch, _, err := sess.Attach()
		if err != nil {
			fail(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		out = ch
	}
	defer sess.Detach(out)
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ready, _ := jsonBytes(map[string]any{"op": "ready", "resumed": resumed, "sessionId": sess.ID})
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if err := write(ctx, conn, ready); err != nil {
		return
	}
	s.log.Info("client connected", "session", sess.ID, "client", client, "resumed", resumed)
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return
		case raw, ok := <-out:
			if !ok {
				return
			}
			if err := write(ctx, conn, raw); err != nil {
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}

func write(ctx context.Context, conn *websocket.Conn, raw []byte) error {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, raw)
}
