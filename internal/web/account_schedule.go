package web

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) scheduleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := s.tokens.SetScheduleEnabled(strings.TrimSpace(body.ID), body.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonOut(w, map[string]any{"ok": true, "id": body.ID, "enabled": body.Enabled})
}
