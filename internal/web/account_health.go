package web

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// UpstreamHTTPError carries the HTTP status of a failed upstream request so
// callers can distinguish rate limiting (429), authorization issues (401/403)
// and transient server errors (5xx) from one another.
type UpstreamHTTPError struct {
	Status     int
	RetryAfter int
	Body       string
}

func (e *UpstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream http %d", e.Status)
}

// IsRateLimited reports whether err represents an upstream 429 or an
// indistinguishable throttling signal (rate limit, too many requests,
// throttled).
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == 429 || httpErr.Status == 503 ||
			(httpErr.Status == 502 && strings.Contains(strings.ToLower(httpErr.Body), "limited"))
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "throttl") ||
		strings.Contains(msg, "account is limited") ||
		strings.Contains(msg, "account limited")
}

// IsAuthFailure reports whether err represents an upstream 401/403, meaning
// the account itself is unusable until re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == 401 || httpErr.Status == 403
	}
	return false
}

// RetryAfterSeconds returns the upstream Retry-After hint for a rate-limited
// error, or 0 when absent. The web layer surfaces this to clients so they can
// back off instead of hammering a throttled pool.
func RetryAfterSeconds(err error) int {
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.RetryAfter
	}
	return 0
}

// accountHealth tracks per-account failure state: rate-limited accounts are
// cooled down and skipped by the round-robin until the window expires, and
// auth-failed accounts are pinned as unusable.
type accountHealth struct {
	mu       sync.Mutex
	cooldown map[string]time.Time
	authFail map[string]bool
	calls    map[string]uint64
}

func newAccountHealth() *accountHealth {
	return &accountHealth{cooldown: map[string]time.Time{}, authFail: map[string]bool{}, calls: map[string]uint64{}}
}

func (h *accountHealth) clearExpiredCooldownLocked(accountID string) {
	until, ok := h.cooldown[accountID]
	if !ok || time.Now().Before(until) {
		return
	}
	delete(h.cooldown, accountID)
	if !h.authFail[accountID] {
		delete(h.calls, accountID)
	}
}

func (h *accountHealth) MarkCall(accountID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.clearExpiredCooldownLocked(accountID)
	h.calls[accountID]++
	h.mu.Unlock()
}

func (h *accountHealth) CallCount(accountID string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clearExpiredCooldownLocked(accountID)
	return h.calls[accountID]
}

func (h *accountHealth) RateLimited(accountID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clearExpiredCooldownLocked(accountID)
	_, ok := h.cooldown[accountID]
	return ok && !h.authFail[accountID]
}

func (h *accountHealth) CooldownUntil(accountID string) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clearExpiredCooldownLocked(accountID)
	until, ok := h.cooldown[accountID]
	return until, ok
}

// MarkFailure records the outcome of a request for one account.
// rateLimited cools the account down for window; authFailed pins it.
func (h *accountHealth) MarkFailure(accountID string, err error, window time.Duration) {
	if window <= 0 {
		window = 60 * time.Second
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if IsAuthFailure(err) {
		cooldown := window
		if cooldown > 2*time.Minute {
			cooldown = 2 * time.Minute
		}
		h.cooldown[accountID] = time.Now().Add(cooldown)
		h.authFail[accountID] = true
		return
	}
	if IsRateLimited(err) {
		delete(h.authFail, accountID)
		h.cooldown[accountID] = time.Now().Add(window)
	}
}

// MarkSuccess clears any failure state after a healthy response.
func (h *accountHealth) MarkSuccess(accountID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.cooldown, accountID)
	delete(h.authFail, accountID)
}

// Available reports whether the account may be used right now.
func (h *accountHealth) Available(accountID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clearExpiredCooldownLocked(accountID)
	if h.authFail[accountID] {
		if _, ok := h.cooldown[accountID]; ok {
			return false
		}
		delete(h.authFail, accountID)
	}
	if until, ok := h.cooldown[accountID]; ok && time.Now().Before(until) {
		return false
	}
	return true
}

// Snapshot returns a copy of the current health state for the admin UI.
func (h *accountHealth) Snapshot() map[string]map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.cooldown {
		h.clearExpiredCooldownLocked(id)
	}
	out := make(map[string]map[string]any, len(h.cooldown)+len(h.authFail))
	for id, until := range h.cooldown {
		out[id] = map[string]any{"available": time.Now().After(until), "cooldownUntil": until}
	}
	for id, failed := range h.authFail {
		if failed {
			if _, ok := out[id]; !ok {
				out[id] = map[string]any{}
			}
			out[id]["authFailed"] = true
		}
	}
	return out
}
