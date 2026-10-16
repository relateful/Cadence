package api

import (
	"io"
	"net/http"
	"strconv"
)

func (s *Server) cacheIndex(w http.ResponseWriter, r *http.Request) {
	if s.audioCache == nil {
		writeJSON(w, http.StatusOK, []string{})
		return
	}
	writeJSON(w, http.StatusOK, s.audioCache.Keys())
}

func (s *Server) cacheFile(w http.ResponseWriter, r *http.Request) {
	if s.audioCache == nil {
		fail(w, r, http.StatusNotFound, "no audio cache on this node")
		return
	}
	key := r.URL.Query().Get("key")
	f, size, ok := s.audioCache.Get(key)
	if !ok {
		fail(w, r, http.StatusNotFound, "not cached")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, f)
}
