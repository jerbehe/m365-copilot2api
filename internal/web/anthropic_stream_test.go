package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseEvents parses an SSE body into (event name, decoded data) pairs.
func sseEvents(t *testing.T, body string) []struct {
	Name string
	Data map[string]any
} {
	t.Helper()
	var out []struct {
		Name string
		Data map[string]any
	}
	name := ""
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				continue
			}
			var data map[string]any
			if err := json.Unmarshal([]byte(payload), &data); err != nil {
				t.Fatalf("bad SSE data %q: %v", payload, err)
			}
			out = append(out, struct {
				Name string
				Data map[string]any
			}{Name: name, Data: data})
		}
	}
	return out
}

// The Anthropic event order is contractual: message_start, then strictly nested
// content_block_start/delta*/stop groups, then message_delta and message_stop.
func TestAnthropicStreamWriterEventOrderAndIndexes(t *testing.T) {
	rr := httptest.NewRecorder()
	out := newAnthropicStreamWriter(rr, "claude-sonnet-4-5", 42)
	if err := out.Text("第一段"); err != nil {
		t.Fatal(err)
	}
	if err := out.Text("第二段"); err != nil {
		t.Fatal(err)
	}
	index, err := out.ToolStart("call_1", "get_weather")
	if err != nil {
		t.Fatal(err)
	}
	if err := out.ToolArgs(index, `{"city":`); err != nil {
		t.Fatal(err)
	}
	if err := out.ToolArgs(index, `"北京"}`); err != nil {
		t.Fatal(err)
	}
	if err := out.Finish(); err != nil {
		t.Fatal(err)
	}

	events := sseEvents(t, rr.Body.String())
	var names []string
	for _, e := range events {
		names = append(names, e.Name)
	}
	want := []string{
		"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", names, want)
	}

	// Two text deltas share block 0; the tool_use block gets the next index.
	textIndex, toolIndex := -1.0, -1.0
	partial := ""
	for _, e := range events {
		if e.Name == "content_block_start" {
			block, _ := e.Data["content_block"].(map[string]any)
			switch block["type"] {
			case "text":
				textIndex = e.Data["index"].(float64)
				if block["text"] != "" {
					t.Fatalf("text block start must be empty: %#v", block)
				}
			case "tool_use":
				toolIndex = e.Data["index"].(float64)
				if input, ok := block["input"].(map[string]any); !ok || len(input) != 0 {
					t.Fatalf("tool_use block start must carry an empty input: %#v", block)
				}
				if block["id"] != "call_1" || block["name"] != "get_weather" {
					t.Fatalf("tool_use block start lost id/name: %#v", block)
				}
			}
		}
		if e.Name == "content_block_delta" {
			delta, _ := e.Data["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				partial += delta["partial_json"].(string)
			}
		}
	}
	if textIndex != 0 || toolIndex != 1 {
		t.Fatalf("indexes text=%v tool=%v, want 0 and 1", textIndex, toolIndex)
	}
	if partial != `{"city":"北京"}` || !json.Valid([]byte(partial)) {
		t.Fatalf("accumulated tool input = %q", partial)
	}

	start := events[0].Data["message"].(map[string]any)
	usage := start["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 42 || usage["output_tokens"].(float64) != 0 {
		t.Fatalf("message_start usage = %#v", usage)
	}
	last := events[len(events)-2]
	if last.Name != "message_delta" {
		t.Fatalf("expected message_delta, got %s", last.Name)
	}
	delta := last.Data["delta"].(map[string]any)
	if delta["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use", delta["stop_reason"])
	}
	if last.Data["usage"].(map[string]any)["output_tokens"].(float64) <= 0 {
		t.Fatalf("message_delta must report output tokens: %#v", last.Data)
	}
}

// A text delta arriving after a tool block must open a new block rather than
// reuse the tool block's index.
func TestAnthropicStreamWriterReopensTextBlockAfterTool(t *testing.T) {
	rr := httptest.NewRecorder()
	out := newAnthropicStreamWriter(rr, "m", 1)
	_ = out.Text("before")
	index, _ := out.ToolStart("call_1", "f")
	_ = out.ToolArgs(index, "{}")
	_ = out.Text("after")
	_ = out.Finish()

	var starts []float64
	for _, e := range sseEvents(t, rr.Body.String()) {
		if e.Name == "content_block_start" {
			starts = append(starts, e.Data["index"].(float64))
		}
	}
	if len(starts) != 3 || starts[0] != 0 || starts[1] != 1 || starts[2] != 2 {
		t.Fatalf("block indexes = %v, want 0,1,2", starts)
	}
}

// Two tool calls must land in separate blocks even though both are tool_use.
func TestAnthropicStreamWriterSeparatesConsecutiveToolBlocks(t *testing.T) {
	rr := httptest.NewRecorder()
	out := newAnthropicStreamWriter(rr, "m", 1)
	first, _ := out.ToolStart("call_1", "a")
	_ = out.ToolArgs(first, `{"x":1}`)
	second, _ := out.ToolStart("call_2", "b")
	_ = out.ToolArgs(second, `{"y":2}`)
	_ = out.Finish()

	if first == second {
		t.Fatalf("both tool calls got index %d", first)
	}
	perIndex := map[float64]string{}
	for _, e := range sseEvents(t, rr.Body.String()) {
		if e.Name != "content_block_delta" {
			continue
		}
		delta := e.Data["delta"].(map[string]any)
		perIndex[e.Data["index"].(float64)] += delta["partial_json"].(string)
	}
	if perIndex[0] != `{"x":1}` || perIndex[1] != `{"y":2}` {
		t.Fatalf("per-block arguments = %#v", perIndex)
	}
}

// An answer with no content at all must still be a well-formed message, or
// strict clients reject it.
func TestAnthropicStreamWriterEmptyAnswerStillHasABlock(t *testing.T) {
	rr := httptest.NewRecorder()
	out := newAnthropicStreamWriter(rr, "m", 1)
	if err := out.Finish(); err != nil {
		t.Fatal(err)
	}
	events := sseEvents(t, rr.Body.String())
	var names []string
	for _, e := range events {
		names = append(names, e.Name)
	}
	joined := strings.Join(names, ",")
	if joined != "message_start,ping,content_block_start,content_block_stop,message_delta,message_stop" {
		t.Fatalf("event order = %s", joined)
	}
}

// An upstream failure before any block is reported as an error event; the client
// must still see a terminal frame.
func TestAnthropicStreamWriterFailedBeforeContent(t *testing.T) {
	rr := httptest.NewRecorder()
	out := newAnthropicStreamWriter(rr, "m", 1)
	if err := out.Failed("upstream is rate limiting"); err != nil {
		t.Fatal(err)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "upstream is rate limiting") {
		t.Fatalf("missing error event: %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("missing terminal frame: %s", body)
	}
}

// After text has been emitted the turn can no longer become an error, so it is
// closed normally instead of leaving the client waiting.
func TestAnthropicStreamWriterFailedAfterContentClosesTurn(t *testing.T) {
	rr := httptest.NewRecorder()
	out := newAnthropicStreamWriter(rr, "m", 1)
	_ = out.Text("partial")
	if err := out.Failed("upstream dropped"); err != nil {
		t.Fatal(err)
	}
	body := rr.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("must not inject an error after content: %s", body)
	}
	for _, want := range []string{"event: content_block_stop", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s: %s", want, body)
		}
	}
}
