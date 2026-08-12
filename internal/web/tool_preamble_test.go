package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// narratedToolCall is one completion that both narrates and calls a tool, plus a
// reasoning transcript. Every protocol must deliver all three.
func narratedToolCall() map[string]any {
	return map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"role":              "assistant",
		"content":           "PREAMBLE I will read the file.",
		"reasoning_content": "THINKING about which file",
		"tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
			"function": map[string]any{"name": "read_file", "arguments": `{"path":"/etc/hostname"}`}}},
	}}}}
}

func itemTypes(t *testing.T, body []byte) []string {
	var d map[string]any
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	out, _ := d["output"].([]any)
	types := make([]string, 0, len(out))
	for _, raw := range out {
		item, _ := raw.(map[string]any)
		typ, _ := item["type"].(string)
		types = append(types, typ)
	}
	return types
}

func TestResponsesResultKeepsPreambleWithToolCall(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "m", false, narratedToolCall())
	types := itemTypes(t, rr.Body.Bytes())
	want := []string{"reasoning", "message", "function_call"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("output types=%v want=%v body=%s", types, want, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "PREAMBLE") {
		t.Fatalf("preamble dropped: %s", rr.Body.String())
	}
}

// A pure tool-call turn must not gain an empty message item, and a pure text turn
// must still produce exactly one message.
func TestResponsesOutputItemsOmitEmptyText(t *testing.T) {
	onlyCall := map[string]any{"tool_calls": []any{map[string]any{"id": "c", "type": "function",
		"function": map[string]any{"name": "n", "arguments": "{}"}}}}
	if got := len(responsesOutputItems(onlyCall)); got != 1 {
		t.Fatalf("tool-only turn produced %d items", got)
	}
	onlyText := map[string]any{"content": "hello"}
	items := responsesOutputItems(onlyText)
	if len(items) != 1 || items[0].(map[string]any)["type"] != "message" {
		t.Fatalf("text-only turn produced %#v", items)
	}
	// An empty turn still needs one item: an empty output array is a protocol
	// violation for the client.
	if got := len(responsesOutputItems(map[string]any{})); got != 1 {
		t.Fatalf("empty turn produced %d items", got)
	}
}

func TestResponsesStreamReplayEmitsReasoningAndPreamble(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "m", true, narratedToolCall())
	body := rr.Body.String()
	for _, want := range []string{
		`"type":"reasoning"`,
		"event: response.reasoning_summary_text.delta",
		"event: response.reasoning_summary_text.done",
		"THINKING about which file",
		"PREAMBLE",
		`"type":"function_call"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
}

// Every item needs a distinct output_index, and reasoning must be added before
// the message: the client attaches reasoning deltas to the most recent item.
func TestResponsesLiveStreamOrdersReasoningFirst(t *testing.T) {
	var frames []string
	emit := func(name string, v any) error { frames = append(frames, name+" "+mustJSON(v)); return nil }
	translateResponsesStream(emit, "m", oaiReq{}, func(h innerStreamHandler) (int, string, error) {
		if h.Reasoning == nil {
			t.Fatal("Responses adapter must request reasoning deltas")
		}
		if err := h.Reasoning("THINKING"); err != nil {
			return 0, "", err
		}
		if err := h.Text("PREAMBLE"); err != nil {
			return 0, "", err
		}
		if err := h.ToolStart(0, "call_1", "read_file", "function"); err != nil {
			return 0, "", err
		}
		if err := h.ToolArgs(0, `{"path":"/etc/hostname"}`); err != nil {
			return 0, "", err
		}
		return 200, "", nil
	})
	joined := strings.Join(frames, "\n")
	for _, want := range []string{"THINKING", "PREAMBLE", `"type":"reasoning"`, `"type":"message"`, `"type":"function_call"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	indexes := map[string]int{}
	for _, f := range frames {
		if !strings.HasPrefix(f, "response.output_item.added ") {
			continue
		}
		var ev struct {
			OutputIndex int `json:"output_index"`
			Item        struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(f, "response.output_item.added ")), &ev); err != nil {
			t.Fatal(err)
		}
		indexes[ev.Item.Type] = ev.OutputIndex
	}
	if indexes["reasoning"] != 0 || indexes["message"] != 1 || indexes["function_call"] != 2 {
		t.Fatalf("output indexes=%#v", indexes)
	}
}

func TestToolResponseCarriesPreamble(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_1", Type: "function", Name: "read_file", Arguments: json.RawMessage(`{"path":"/etc/hostname"}`)}}
	rr := httptest.NewRecorder()
	if err := writeToolResponse(rr, toolResponse{ID: "id", Model: "m", Calls: calls, Preamble: "PREAMBLE"}); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	msg := d["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "PREAMBLE" {
		t.Fatalf("content=%#v", msg["content"])
	}
	if len(msg["tool_calls"].([]any)) != 1 {
		t.Fatalf("tool call lost: %s", rr.Body.String())
	}
	// Callers whose text is not prose pass "", which must stay null rather than
	// becoming an empty string the client would render as a blank message.
	rr2 := httptest.NewRecorder()
	if err := writeToolResponse(rr2, toolResponse{ID: "id", Model: "m", Calls: calls}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr2.Body.String(), `"content":null`) {
		t.Fatalf("empty preamble must stay null: %s", rr2.Body.String())
	}
}

func TestToolResponseStreamsPreambleBeforeCall(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_1", Type: "function", Name: "read_file", Arguments: json.RawMessage(`{}`)}}
	rr := httptest.NewRecorder()
	if err := writeToolResponse(rr, toolResponse{ID: "id", Model: "m", Stream: true, Calls: calls, Preamble: "PREAMBLE"}); err != nil {
		t.Fatal(err)
	}
	body := rr.Body.String()
	preamble := strings.Index(body, "PREAMBLE")
	call := strings.Index(body, "read_file")
	if preamble < 0 || call < 0 || preamble > call {
		t.Fatalf("preamble must precede the call: %s", body)
	}
}

// The Anthropic path used to drop text whenever a tool call was present.
func TestAnthropicResultKeepsTextWithToolUse(t *testing.T) {
	rr := httptest.NewRecorder()
	writeAnthropicResult(rr, "m", false, narratedToolCall())
	var d map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	types := []string{}
	for _, raw := range d["content"].([]any) {
		block, _ := raw.(map[string]any)
		typ, _ := block["type"].(string)
		types = append(types, typ)
	}
	want := "thinking,text,tool_use"
	if strings.Join(types, ",") != want {
		t.Fatalf("blocks=%v want=%s body=%s", types, want, rr.Body.String())
	}
	if d["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason=%v", d["stop_reason"])
	}
}

// A tool-only turn must not gain an empty text block.
func TestAnthropicContentBlocksSkipEmptyText(t *testing.T) {
	if blocks := anthropicContentBlocks(nil); len(blocks) != 0 {
		t.Fatalf("nil content produced %#v", blocks)
	}
	if blocks := anthropicContentBlocks(""); len(blocks) != 0 {
		t.Fatalf("empty string produced %#v", blocks)
	}
	if blocks := anthropicContentBlocks("hi"); len(blocks) != 1 {
		t.Fatalf("text produced %#v", blocks)
	}
}
