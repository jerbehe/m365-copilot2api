package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

func openAIChoice(v map[string]any) (map[string]any, string) {
	choices, _ := v["choices"].([]any)
	if len(choices) == 0 {
		return nil, ""
	}
	c, _ := choices[0].(map[string]any)
	m, _ := c["message"].(map[string]any)
	finish, _ := c["finish_reason"].(string)
	return m, finish
}

// writeAnthropicResult renders a non-streaming Anthropic message, or replays a
// completed result as an SSE stream for callers that already hold the full text.
// Live streaming uses streamAnthropicMessages instead.
func writeAnthropicResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	writeAnthropicResultUsage(w, model, stream, src, responsesUsageEstimate{})
}

func writeAnthropicResultUsage(w http.ResponseWriter, model string, stream bool, src map[string]any, estimate responsesUsageEstimate) {
	id := "msg_" + uuid.NewString()
	msg, finish := openAIChoice(src)
	inputTokens, outputTokens := 0, 0
	usageSource := "unavailable_from_chathub"
	if estimate.Values != nil {
		inputTokens, _ = estimate.Values["input_tokens"].(int)
		outputTokens, _ = estimate.Values["output_tokens"].(int)
		usageSource = estimate.Source
	}
	blocks := []any{}
	stop := "end_turn"
	if reasoning, _ := msg["reasoning_content"].(string); reasoning != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": reasoning, "signature": ""})
	}
	// Text comes before the calls, and both are emitted: a turn that narrates and
	// then calls a tool must deliver the narration too, or the client shows a bare
	// call and the model's stated intent is lost.
	blocks = append(blocks, anthropicContentBlocks(msg["content"])...)
	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		stop = "tool_use"
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			var input any = map[string]any{}
			if a, ok := fn["arguments"].(string); ok {
				_ = json.Unmarshal([]byte(a), &input)
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc["id"], "name": fn["name"], "input": input})
		}
	} else if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	_ = finish
	out := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens}, "m365": localUsageMetadata(usageSource)}
	if !stream {
		jsonOut(w, out)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	f, _ := w.(http.Flusher)
	aborted := false
	emit := func(n string, v any) {
		if aborted {
			return
		}
		if err := sseWriteFrame(w, f, n, v); err != nil {
			aborted = true
		}
	}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0}}})
	emit("ping", map[string]any{"type": "ping"})
	for i, b := range blocks {
		m, _ := b.(map[string]any)
		blockType, _ := m["type"].(string)
		// content_block_start must never carry the block's payload: clients append
		// the deltas to it, so a pre-filled start duplicates the whole block.
		var startBlock any
		switch blockType {
		case "tool_use":
			startBlock = map[string]any{"type": "tool_use", "id": m["id"], "name": m["name"], "input": map[string]any{}}
		case "thinking":
			startBlock = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
		case "image":
			startBlock = map[string]any{"type": "image", "source": m["source"]}
		default:
			startBlock = map[string]any{"type": "text", "text": ""}
		}
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": startBlock})
		switch blockType {
		case "text":
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": m["text"]}})
		case "tool_use":
			partial, _ := json.Marshal(m["input"])
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(partial)}})
		case "thinking":
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": m["thinking"]}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": outputTokens}})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

// anthropicContentBlocks converts an internal message content field into
// Anthropic content blocks. Empty text yields no block, so a pure tool-call turn
// does not gain a stray empty text block.
func anthropicContentBlocks(content any) []any {
	var blocks []any
	switch content := content.(type) {
	case nil:
		return nil
	case string:
		if content != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": content})
		}
	case []any:
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			switch part["type"] {
			case "text":
				if t, _ := part["text"].(string); t != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": t})
				}
			case "image_url":
				img, _ := part["image_url"].(map[string]any)
				if u, _ := img["url"].(string); u != "" {
					blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}})
				}
			}
		}
	default:
		if text := fmt.Sprint(content); text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
	}
	return blocks
}

// sseWriteFrame writes one SSE frame and flushes; a write error (client gone,
// deadline exceeded) aborts the stream instead of leaving the handler blocked.
func sseWriteFrame(w http.ResponseWriter, f http.Flusher, name string, value any) error {
	b, _ := json.Marshal(value)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseDataRaw writes a raw "data: ..." frame with the same write deadline.
func sseDataRaw(w http.ResponseWriter, f http.Flusher, data string) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseSafeRaw writes a pre-formatted frame (e.g. ": connected" or "[DONE]").
func sseSafeRaw(w http.ResponseWriter, f http.Flusher, payload string) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}
