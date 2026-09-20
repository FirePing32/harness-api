package server

import (
	"net/http"
)

// modelList is the GET /v1/models envelope.
type modelList struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleModels serves GET /v1/models.
//
// It reports what this server is configured to route to, rather than proxying the
// provider's catalogue. Clients call this endpoint to check connectivity and to
// populate a model picker, and both are better served by the truth about this
// deployment than by a list of models it will not use. It also means the endpoint
// keeps working when the provider is down.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	var data []modelInfo
	if m := s.upstream.DefaultModel(); m != "" {
		data = append(data, modelInfo{
			ID:      m,
			Object:  "model",
			OwnedBy: "harness-api",
		})
	}
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: data})
}
