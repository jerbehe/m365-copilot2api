package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func newServerWithKey(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_API_KEYS", dir+"/api-keys.json")
	s := &Server{apiKeys: openAPIKeys()}
	_, raw, err := s.apiKeys.create("test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return s, raw
}

// TestValidAPIKeyAcceptsAnyOfferedHeader covers the Claude Code shape: the CLI
// sends its own stale x-api-key alongside the configured ANTHROPIC_AUTH_TOKEN in
// Authorization. Checking only the first header rejected valid requests.
func TestValidAPIKeyAcceptsAnyOfferedHeader(t *testing.T) {
	s, raw := newServerWithKey(t)
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"x-api-key only", map[string]string{"X-API-Key": raw}, true},
		{"bearer only", map[string]string{"Authorization": "Bearer " + raw}, true},
		{"stale x-api-key plus valid bearer", map[string]string{
			"X-API-Key":     "sk-stale-from-another-provider",
			"Authorization": "Bearer " + raw,
		}, true},
		{"valid x-api-key plus junk bearer", map[string]string{
			"X-API-Key":     raw,
			"Authorization": "Bearer nope",
		}, true},
		{"both invalid", map[string]string{
			"X-API-Key":     "sk-bad",
			"Authorization": "Bearer also-bad",
		}, false},
		{"no credentials", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}"))
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if _, ok := s.matchAPIKey(r); ok != tc.want {
				t.Fatalf("matchAPIKey = %t, want %t", ok, tc.want)
			}
		})
	}
}

// TestExtractAPIKeyNeverLeaksFullCredential guards the usage log: an untruncated
// key in usage.jsonl is a credential leak, and the x-api-key path used to skip
// truncation entirely.
func TestExtractAPIKeyNeverLeaksFullCredential(t *testing.T) {
	const key = "m365_0123456789abcdef0123456789abcdef"
	for _, header := range []string{"X-API-Key", "Authorization"} {
		t.Run(header, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}"))
			if header == "Authorization" {
				r.Header.Set(header, "Bearer "+key)
			} else {
				r.Header.Set(header, key)
			}
			got := extractAPIKey(r)
			if got == key || !strings.HasSuffix(got, "...") {
				t.Fatalf("extractAPIKey = %q, want truncated prefix", got)
			}
			if got != key[:8]+"..." {
				t.Fatalf("extractAPIKey = %q, want %q", got, key[:8]+"...")
			}
		})
	}
}

// TestAPIKeyTenantIsolatesKeysSharingAPrefix ensures /v1/responses history is not
// shared between distinct keys whose first eight characters match.
func TestAPIKeyTenantIsolatesKeysSharingAPrefix(t *testing.T) {
	first := httptest.NewRequest("POST", "/v1/responses", strings.NewReader("{}"))
	first.Header.Set("Authorization", "Bearer m365_aaaa_one")
	second := httptest.NewRequest("POST", "/v1/responses", strings.NewReader("{}"))
	second.Header.Set("Authorization", "Bearer m365_aaaa_two")

	if extractAPIKey(first) != extractAPIKey(second) {
		t.Fatal("precondition: the two keys should share a truncated prefix")
	}
	if apiKeyTenant(first) == apiKeyTenant(second) {
		t.Fatal("apiKeyTenant collided for distinct keys sharing a prefix")
	}
}
