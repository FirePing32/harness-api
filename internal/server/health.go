package server

import (
	"encoding/json"
	"net/http"
)

// handleHealthz reports process liveness. It deliberately checks nothing beyond
// "this process is serving", so that a flaky upstream cannot cause a restart loop.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports whether the server is configured well enough to serve
// traffic. Unlike healthz, a missing upstream configuration makes this fail —
// accepting requests we cannot fulfil is worse than being marked unready.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Upstream.BaseURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not ready",
			"reason": "upstream.base_url is not configured",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
