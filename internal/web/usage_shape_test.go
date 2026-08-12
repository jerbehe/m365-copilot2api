package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// The chat.completion object requires created, and returns usage on
// non-streaming responses. A tool-call turn used to omit both, leaving clients
// unable to account for it at all.
func TestToolResponseIncludesCreatedAndUsage(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_1", Type: "function", Name: "read_file", Arguments: json.RawMessage(`{"path":"/etc/hostname"}`)}}
	rr := httptest.NewRecorder()
	if err := writeToolResponse(rr, toolResponse{ID: "id", Model: "m", Calls: calls, Preamble: "narration", Prompt: "read the hostname file"}); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	created, ok := d["created"].(float64)
	if !ok || created <= 0 {
		t.Fatalf("created=%#v", d["created"])
	}
	usage, ok := d["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %s", rr.Body.String())
	}
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		v, ok := usage[key].(float64)
		if !ok || v <= 0 {
			t.Fatalf("usage[%s]=%#v in %s", key, usage[key], rr.Body.String())
		}
	}
	if usage["total_tokens"] != usage["prompt_tokens"].(float64)+usage["completion_tokens"].(float64) {
		t.Fatalf("total is not the sum: %#v", usage)
	}
}

// Streaming chunks carry created too, and it must be one value across the turn
// rather than a fresh timestamp per chunk.
func TestToolResponseStreamChunksShareCreated(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_1", Type: "function", Name: "n", Arguments: json.RawMessage(`{}`)}}
	rr := httptest.NewRecorder()
	if err := writeToolResponse(rr, toolResponse{ID: "id", Model: "m", Stream: true, Calls: calls}); err != nil {
		t.Fatal(err)
	}
	seen := map[float64]bool{}
	for _, chunk := range parseChatChunks(t, rr.Body.String()) {
		created, ok := chunk["created"].(float64)
		if !ok || created <= 0 {
			t.Fatalf("chunk without created: %#v", chunk)
		}
		seen[created] = true
	}
	if len(seen) != 1 {
		t.Fatalf("created differs across chunks: %#v", seen)
	}
}

// The Responses usage object carries token details; a client reads
// cached_tokens / cache_write_tokens / reasoning_tokens from them and otherwise
// leaves those counters silently at zero.
func TestResponsesUsageIncludesTokenDetails(t *testing.T) {
	usage := estimateResponsesUsage("gpt-5.5", []oaiMsg{{Role: "user", Content: "hello"}}, nil, nil, "world").Values
	in, ok := usage["input_tokens"].(int)
	if !ok || in <= 0 {
		t.Fatalf("input_tokens=%#v", usage["input_tokens"])
	}
	out, ok := usage["output_tokens"].(int)
	if !ok || out <= 0 {
		t.Fatalf("output_tokens=%#v", usage["output_tokens"])
	}
	// total_tokens is not optional for the client: a usage object missing it
	// fails to deserialize and aborts the whole turn.
	if usage["total_tokens"] != in+out {
		t.Fatalf("total_tokens=%#v want %d", usage["total_tokens"], in+out)
	}
	inputDetails, ok := usage["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("input_tokens_details missing: %#v", usage)
	}
	for _, key := range []string{"cached_tokens", "cache_write_tokens"} {
		if _, ok := inputDetails[key].(int); !ok {
			t.Fatalf("input_tokens_details[%s]=%#v", key, inputDetails[key])
		}
	}
	outputDetails, ok := usage["output_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("output_tokens_details missing: %#v", usage)
	}
	if _, ok := outputDetails["reasoning_tokens"].(int); !ok {
		t.Fatalf("output_tokens_details[reasoning_tokens]=%#v", outputDetails["reasoning_tokens"])
	}
}

// Zeros in the details are unreported, not measured. Say so, or a caller reads
// them as "no cache hits" when the truth is "upstream never told us".
func TestUsageMetadataNamesUnreportedCounters(t *testing.T) {
	excludes, ok := localUsageMetadata(usageSourceTiktoken)["usage_excludes"].([]string)
	if !ok {
		t.Fatal("usage_excludes missing")
	}
	want := map[string]bool{"upstream_cached_tokens": false, "upstream_cache_write_tokens": false, "upstream_reasoning_tokens": false}
	for _, name := range excludes {
		want[name] = true
	}
	for name, present := range want {
		if !present {
			t.Fatalf("usage_excludes does not name %s: %v", name, excludes)
		}
	}
}

// The Anthropic usage object has its own shape (input_tokens/output_tokens
// only); the Responses details must not leak into it.
func TestAnthropicUsageKeepsItsOwnShape(t *testing.T) {
	estimate := estimateResponsesUsage("m", []oaiMsg{{Role: "user", Content: "hi"}}, nil, nil, "ok")
	rr := httptest.NewRecorder()
	writeAnthropicResultUsage(rr, "m", false, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
	}, estimate)
	var d map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	usage, ok := d["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %s", rr.Body.String())
	}
	if len(usage) != 2 || usage["input_tokens"] == nil || usage["output_tokens"] == nil {
		t.Fatalf("anthropic usage=%#v", usage)
	}
}

func parseChatChunks(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		out = append(out, chunk)
	}
	if len(out) == 0 {
		t.Fatalf("no chunks in %s", body)
	}
	return out
}
