package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func chunksOf(t *testing.T, body string) []map[string]any {
	t.Helper()
	return parseChatChunks(t, body)
}

// Opting out must change nothing: no usage key at all, not even a null one.
func TestStreamUsageDisabledEmitsNoUsageKey(t *testing.T) {
	u := newStreamUsage(nil, "prompt text")
	chunk := u.decorate(map[string]any{"id": "x"})
	if _, present := chunk["usage"]; present {
		t.Fatalf("opted-out chunk carries usage: %#v", chunk)
	}
	u2 := newStreamUsage(&streamOptions{IncludeUsage: false}, "prompt text")
	if _, present := u2.decorate(map[string]any{})["usage"]; present {
		t.Fatal("include_usage=false must not add usage")
	}
	rr := httptest.NewRecorder()
	u.writeFinal(context.Background(), rr, nil, "id", "m", 1)
	if rr.Body.Len() != 0 {
		t.Fatalf("opted-out stream emitted a usage chunk: %s", rr.Body.String())
	}
}

// Opting in makes the field always present: null on ordinary chunks, populated on
// the extra final chunk.
func TestStreamUsageEnabledNullsOrdinaryChunks(t *testing.T) {
	u := newStreamUsage(&streamOptions{IncludeUsage: true}, "prompt text")
	chunk := u.decorate(map[string]any{"id": "x"})
	v, present := chunk["usage"]
	if !present || v != nil {
		t.Fatalf("ordinary chunk usage=%#v, want explicit null", v)
	}
}

func TestStreamUsageFinalChunkShape(t *testing.T) {
	u := newStreamUsage(&streamOptions{IncludeUsage: true}, "some prompt text here")
	u.addCompletion("answer text")
	rr := httptest.NewRecorder()
	u.writeFinal(context.Background(), rr, nil, "id", "m", 42)
	chunks := chunksOf(t, rr.Body.String())
	if len(chunks) != 1 {
		t.Fatalf("want one usage chunk, got %d", len(chunks))
	}
	final := chunks[0]
	// choices must be empty: a client accumulating deltas by index would
	// otherwise append this chunk's content to the answer.
	choices, ok := final["choices"].([]any)
	if !ok || len(choices) != 0 {
		t.Fatalf("choices=%#v, want empty array", final["choices"])
	}
	if final["created"].(float64) != 42 {
		t.Fatalf("created=%#v, want the turn's timestamp", final["created"])
	}
	usage, ok := final["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %#v", final)
	}
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if v, ok := usage[key].(float64); !ok || v <= 0 {
			t.Fatalf("usage[%s]=%#v", key, usage[key])
		}
	}
	if usage["total_tokens"] != usage["prompt_tokens"].(float64)+usage["completion_tokens"].(float64) {
		t.Fatalf("total is not the sum: %#v", usage)
	}
}

// The usage chunk follows the terminal finish_reason chunk. Emitting it earlier
// would make a client stop reading before the answer is complete.
func TestStreamFinishOrdersUsageAfterFinishReason(t *testing.T) {
	u := newStreamUsage(&streamOptions{IncludeUsage: true}, "prompt")
	u.addCompletion("answer")
	rr := httptest.NewRecorder()
	writeStreamFinish(context.Background(), rr, nil, "id", "m", u)
	chunks := chunksOf(t, rr.Body.String())
	if len(chunks) != 2 {
		t.Fatalf("want finish + usage chunk, got %d: %s", len(chunks), rr.Body.String())
	}
	finish := chunks[0]["choices"].([]any)[0].(map[string]any)
	if finish["finish_reason"] != "stop" {
		t.Fatalf("first chunk must carry finish_reason: %#v", chunks[0])
	}
	if _, ok := chunks[1]["usage"].(map[string]any); !ok {
		t.Fatalf("second chunk must carry usage: %#v", chunks[1])
	}
	// The finish chunk itself is an ordinary chunk and so carries a null usage.
	if v, present := chunks[0]["usage"]; !present || v != nil {
		t.Fatalf("finish chunk usage=%#v, want null", v)
	}
}

func TestStreamFinishWithoutOptInEmitsOnlyFinish(t *testing.T) {
	rr := httptest.NewRecorder()
	writeStreamFinish(context.Background(), rr, nil, "id", "m", streamUsage{})
	chunks := chunksOf(t, rr.Body.String())
	if len(chunks) != 1 {
		t.Fatalf("opted-out finish emitted %d chunks: %s", len(chunks), rr.Body.String())
	}
	if _, present := chunks[0]["usage"]; present {
		t.Fatal("opted-out finish chunk carries usage")
	}
}

// A tool turn is an ordinary streaming response as far as include_usage goes.
func TestToolResponseStreamHonorsIncludeUsage(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_1", Type: "function", Name: "read_file", Arguments: json.RawMessage(`{"path":"/etc/hostname"}`)}}
	rr := httptest.NewRecorder()
	if err := writeToolResponse(rr, toolResponse{
		ID: "id", Model: "m", Stream: true, Calls: calls,
		Preamble: "narration", Prompt: "read the hostname file",
		Options: &streamOptions{IncludeUsage: true},
	}); err != nil {
		t.Fatal(err)
	}
	chunks := chunksOf(t, rr.Body.String())
	final := chunks[len(chunks)-1]
	usage, ok := final["usage"].(map[string]any)
	if !ok {
		t.Fatalf("last chunk has no usage: %s", rr.Body.String())
	}
	if choices, _ := final["choices"].([]any); len(choices) != 0 {
		t.Fatalf("usage chunk must have empty choices: %#v", final)
	}
	if usage["total_tokens"].(float64) <= 0 {
		t.Fatalf("usage=%#v", usage)
	}
	// Every earlier chunk carries an explicit null.
	for i, chunk := range chunks[:len(chunks)-1] {
		v, present := chunk["usage"]
		if !present || v != nil {
			t.Fatalf("chunk %d usage=%#v, want null", i, v)
		}
	}
	rr2 := httptest.NewRecorder()
	if err := writeToolResponse(rr2, toolResponse{ID: "id", Model: "m", Stream: true, Calls: calls, Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	for i, chunk := range chunksOf(t, rr2.Body.String()) {
		if _, present := chunk["usage"]; present {
			t.Fatalf("opted-out tool chunk %d carries usage: %#v", i, chunk)
		}
	}
}

// The streaming total must match what the same turn reports non-streaming,
// otherwise one request is billed differently depending on transport.
func TestToolResponseStreamTotalMatchesNonStream(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_1", Type: "function", Name: "read_file", Arguments: json.RawMessage(`{"path":"/etc/hostname"}`)}}
	req := toolResponse{ID: "id", Model: "m", Calls: calls, Preamble: "narration", Prompt: "read the hostname file"}

	plain := httptest.NewRecorder()
	if err := writeToolResponse(plain, req); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(plain.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := body["usage"].(map[string]any)

	streaming := req
	streaming.Stream = true
	streaming.Options = &streamOptions{IncludeUsage: true}
	rr := httptest.NewRecorder()
	if err := writeToolResponse(rr, streaming); err != nil {
		t.Fatal(err)
	}
	chunks := chunksOf(t, rr.Body.String())
	got := chunks[len(chunks)-1]["usage"].(map[string]any)
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if got[key] != want[key] {
			t.Fatalf("%s: streaming=%v non-streaming=%v", key, got[key], want[key])
		}
	}
}
