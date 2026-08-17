package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
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

// tokenHealth reports token expiry per account; POST refreshes every expired
// token in one sweep so operators can recover a stale pool without restart.
func (s *Server) tokenHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		results := s.tokens.RefreshAllExpired()
		refreshed, failed := 0, 0
		for _, res := range results {
			if res.Success {
				refreshed++
			} else {
				failed++
			}
		}
		jsonOut(w, map[string]any{"refreshed": refreshed, "failed": failed, "results": results})
		return
	}
	list := s.tokens.List()
	now := time.Now()
	type entry struct {
		ID        string    `json:"id"`
		Email     string    `json:"email"`
		Status    string    `json:"status"`
		ExpiresAt time.Time `json:"expires_at"`
		Expired   bool      `json:"expired"`
		ExpiresIn string    `json:"expires_in"`
	}
	out := make([]entry, 0, len(list))
	for _, a := range list {
		e := entry{ID: a.ID, Email: a.Email, Status: a.Status, ExpiresAt: a.ExpiresAt}
		if now.After(a.ExpiresAt) {
			e.Expired = true
			e.ExpiresIn = "expired"
		} else {
			e.ExpiresIn = a.ExpiresAt.Sub(now).Truncate(time.Second).String()
		}
		out = append(out, e)
	}
	jsonOut(w, map[string]any{"accounts": out, "now": now.Format(time.RFC3339)})
}

// clearCooldown resets all account cooldown state. POST /api/accounts/clear-cooldown
// is the manual recovery lever when the whole pool shows cooling-down.
func (s *Server) clearCooldown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.accountPool != nil {
		s.accountPool.ClearAllCooldowns()
	}
	jsonOut(w, map[string]any{"status": "ok"})
}

// provisionAccount adds an account via resource-owner password credentials.
// The ROPC flow only works for tenants that permit it; PKCE remains the
// recommended path.
func (s *Server) provisionAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" || body.Password == "" {
		http.Error(w, "email and password required", http.StatusBadRequest)
		return
	}
	set, err := auth.ROPC(body.Email, body.Password)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "ropc_error", err.Error())
		return
	}
	acc, err := s.tokens.Upsert(set)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "upsert_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "provisioned", "account": map[string]any{
		"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
		"status": acc.Status, "expiresAt": acc.ExpiresAt,
	}})
}
