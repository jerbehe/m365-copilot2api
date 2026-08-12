package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// innerSSE builds a gateway-internal OpenAI SSE body from raw chunk JSON.
func innerSSE(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func textChunk(content string) string {
	return `{"choices":[{"index":0,"delta":{"content":` + mustJSON(content) + `}}]}`
}

func toolChunk(index int, id, name, args string) string {
	call := map[string]any{"index": index}
	if id != "" {
		call["id"] = id
	}
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if args != "" {
		fn["arguments"] = args
	}
	call["function"] = fn
	return `{"choices":[{"index":0,"delta":{"tool_calls":[` + mustJSON(call) + `]}}]}`
}

type recordedEvent struct {
	Name string
	Data map[string]any
}

// replayResponsesStream drives the Responses translation with a recorded inner
// OpenAI SSE body and returns the events it emitted.
func replayResponsesStream(t *testing.T, innerBody string) []recordedEvent {
	t.Helper()
	var events []recordedEvent
	emit := func(name string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		var data map[string]any
		if err := json.Unmarshal(b, &data); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		events = append(events, recordedEvent{Name: name, Data: data})
		return nil
	}
	translateResponsesStream(emit, "gpt-5.4", oaiReq{Messages: []oaiMsg{{Role: "user", Content: "hi"}}}, func(h innerStreamHandler) (int, string, error) {
		return 200, "", dispatchInnerStream(strings.NewReader(innerBody), h, nil)
	})
	if len(events) == 0 {
		t.Fatal("translation emitted no events")
	}
	if events[0].Name != "response.created" {
		t.Fatalf("first event = %s, want response.created", events[0].Name)
	}
	return events
}

// The inner handler rejects a bad request with http.Error, i.e. a plain-text
// body. That reason must survive translation instead of being replaced by a
// generic message.
func TestDispatchInnerStreamCapturesPlainErrorBody(t *testing.T) {
	var plain strings.Builder
	err := dispatchInnerStream(strings.NewReader(": connected\n\nmessages required\n"), innerStreamHandler{
		Text: func(string) error { t.Fatal("no text expected"); return nil },
	}, &plain)
	if err != nil {
		t.Fatal(err)
	}
	if plain.String() != "messages required" {
		t.Fatalf("plain body=%q", plain.String())
	}
}

// A failure the inner handler reported as plain text must reach the client as the
// response.failed message, so the user sees why the turn failed.
func TestTranslatedResponsesFailureCarriesInnerReason(t *testing.T) {
	var events []recordedEvent
	emit := func(name string, v any) error {
		b, _ := json.Marshal(v)
		var data map[string]any
		if err := json.Unmarshal(b, &data); err != nil {
			t.Fatal(err)
		}
		events = append(events, recordedEvent{Name: name, Data: data})
		return nil
	}
	translateResponsesStream(emit, "gpt-5.4", oaiReq{}, func(innerStreamHandler) (int, string, error) {
		return 400, "messages required", nil
	})
	last := events[len(events)-1]
	if last.Name != "response.failed" {
		t.Fatalf("last event = %s", last.Name)
	}
	errObj := last.Data["response"].(map[string]any)["error"].(map[string]any)
	if errObj["message"] != "messages required" {
		t.Fatalf("error=%#v", errObj)
	}
}

func TestDispatchInnerStreamForwardsTextAndToolFragments(t *testing.T) {
	body := innerSSE(
		textChunk("第一"),
		textChunk("第二"),
		toolChunk(0, "call_1", "get_weather", `{"city":`),
		toolChunk(0, "", "", `"北京"}`),
	)
	var text, args strings.Builder
	starts := 0
	err := dispatchInnerStream(strings.NewReader(body), innerStreamHandler{
		Text: func(s string) error { text.WriteString(s); return nil },
		ToolStart: func(index int, id, name, typ string) error {
			starts++
			if index != 0 || id != "call_1" || name != "get_weather" || typ != "function" {
				t.Fatalf("ToolStart(%d,%q,%q,%q)", index, id, name, typ)
			}
			return nil
		},
		ToolArgs: func(_ int, fragment string) error { args.WriteString(fragment); return nil },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "第一第二" {
		t.Fatalf("text=%q", text.String())
	}
	// ToolStart fires exactly once even though two chunks share the index.
	if starts != 1 {
		t.Fatalf("ToolStart called %d times", starts)
	}
	if args.String() != `{"city":"北京"}` {
		t.Fatalf("args=%q", args.String())
	}
}

func TestDispatchInnerStreamSurfacesErrorFrame(t *testing.T) {
	body := innerSSE(`{"error":{"message":"upstream is rate limiting","code":"rate_limit"}}`)
	got := ""
	err := dispatchInnerStream(strings.NewReader(body), innerStreamHandler{
		Text:  func(string) error { t.Fatal("text must not be forwarded after an error"); return nil },
		Error: func(msg string) error { got = msg; return nil },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "upstream is rate limiting" {
		t.Fatalf("error message=%q", got)
	}
}

// A tool call following text must occupy the next output slot. Reusing index 0
// makes Codex overwrite the message item with the call.
func TestDispatchedResponsesIndexesAreDistinct(t *testing.T) {
	events := replayResponsesStream(t, innerSSE(
		textChunk("先说明一下。"),
		toolChunk(0, "call_1", "run", `{"cmd":"ls"}`),
	))
	textIdx, toolIdx := -1.0, -1.0
	for _, e := range events {
		if e.Name != "response.output_item.added" {
			continue
		}
		item := e.Data["item"].(map[string]any)
		switch item["type"] {
		case "message":
			textIdx = e.Data["output_index"].(float64)
		case "function_call":
			toolIdx = e.Data["output_index"].(float64)
		}
	}
	if textIdx != 0 || toolIdx != 1 {
		t.Fatalf("output_index text=%v tool=%v, want 0 and 1", textIdx, toolIdx)
	}
}

// Codex concatenates every function_call_arguments.delta for an item_id. Emitting
// the arguments once while streaming and again at completion produced
// `{"city":"北京"}{"city":"北京"}`, which fails to parse.
func TestDispatchedResponsesArgumentsAreNotDuplicated(t *testing.T) {
	events := replayResponsesStream(t, innerSSE(
		toolChunk(0, "call_1", "get_weather", `{"city":`),
		toolChunk(0, "", "", `"北京"}`),
	))
	perItem := map[string]string{}
	added := map[string]int{}
	for _, e := range events {
		switch e.Name {
		case "response.output_item.added":
			item := e.Data["item"].(map[string]any)
			id := item["id"].(string)
			added[id]++
			if item["arguments"] != "" {
				t.Fatalf("output_item.added must not carry arguments: %#v", item)
			}
		case "response.function_call_arguments.delta":
			perItem[e.Data["item_id"].(string)] += e.Data["delta"].(string)
		}
	}
	if len(perItem) != 1 {
		t.Fatalf("expected one item_id, got %#v", perItem)
	}
	for id, acc := range perItem {
		if added[id] != 1 {
			t.Fatalf("item %s announced %d times", id, added[id])
		}
		if acc != `{"city":"北京"}` || !json.Valid([]byte(acc)) {
			t.Fatalf("accumulated arguments=%q", acc)
		}
	}
	// The done event and the completed item must reuse the same id.
	var doneID, itemID string
	for _, e := range events {
		if e.Name == "response.function_call_arguments.done" {
			doneID = e.Data["item_id"].(string)
		}
		if e.Name == "response.output_item.done" {
			itemID = e.Data["item"].(map[string]any)["id"].(string)
		}
	}
	for id := range perItem {
		if doneID != id || itemID != id {
			t.Fatalf("item ids diverged: stream=%s done=%s item=%s", id, doneID, itemID)
		}
	}
}

// The final response.completed output array must contain every item the stream
// announced, in order.
func TestDispatchedResponsesCompletedOutputMatchesStream(t *testing.T) {
	events := replayResponsesStream(t, innerSSE(
		textChunk("说明"),
		toolChunk(0, "call_1", "run", `{}`),
	))
	last := events[len(events)-1]
	if last.Name != "response.completed" {
		t.Fatalf("last event = %s", last.Name)
	}
	output := last.Data["response"].(map[string]any)["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%#v", output)
	}
	if output[0].(map[string]any)["type"] != "message" || output[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("output order wrong: %#v", output)
	}
	for _, item := range output {
		if item.(map[string]any)["status"] != "completed" {
			t.Fatalf("item not completed: %#v", item)
		}
	}
}
