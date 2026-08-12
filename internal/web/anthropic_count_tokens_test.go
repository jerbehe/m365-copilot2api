package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Claude Code calls count_tokens before every turn to decide whether to compact
// the conversation. A 404 makes it fall back to a local guess, so the endpoint
// must exist and return a positive count.
func TestCountTokensReportsInputTokens(t *testing.T) {
	s := &Server{}
	body := `{"model":"claude-sonnet-4-5","system":"be concise","messages":[{"role":"user","content":"解释一下数据库索引"}]}`
	r := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.countAnthropicTokens(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	tokens, ok := out["input_tokens"].(float64)
	if !ok || tokens <= 0 {
		t.Fatalf("input_tokens=%#v", out)
	}
}

// A longer conversation must count as more tokens, or compaction triggers at the
// wrong time.
func TestCountTokensGrowsWithConversation(t *testing.T) {
	s := &Server{}
	count := func(body string) float64 {
		r := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.countAnthropicTokens(w, r)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("bad response %s: %v", w.Body.String(), err)
		}
		return out["input_tokens"].(float64)
	}
	short := count(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	long := count(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello there"},{"role":"user","content":"please explain database indexes in detail"}]}`)
	if long <= short {
		t.Fatalf("long=%v must exceed short=%v", long, short)
	}
}

func TestCountTokensRejectsBadRequests(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/v1/messages/count_tokens", nil)
	w := httptest.NewRecorder()
	s.countAnthropicTokens(w, r)
	if w.Code != 405 {
		t.Fatalf("GET status=%d", w.Code)
	}

	r = httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader("not json"))
	w = httptest.NewRecorder()
	s.countAnthropicTokens(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_request_error") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// The non-streaming Anthropic response must report the estimated token counts
// rather than hard-coded zeros, which Claude Code reads as an empty context.
func TestAnthropicResultReportsEstimatedUsage(t *testing.T) {
	src := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "hello"}}}}
	estimate := estimateResponsesUsage("claude-sonnet-4-5", []oaiMsg{{Role: "user", Content: "hi"}}, nil, nil, "hello")
	w := httptest.NewRecorder()
	writeAnthropicResultUsage(w, "claude-sonnet-4-5", false, src, estimate)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"].(float64) <= 0 || usage["output_tokens"].(float64) <= 0 {
		t.Fatalf("usage=%#v", usage)
	}
}
