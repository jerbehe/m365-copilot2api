package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// reasoningOutputItem builds a Responses `reasoning` item. summary carries the
// chain-of-thought text; the client renders it from that array, and reads
// content/encrypted_content too, so both are always present to keep the item
// decodable regardless of which fields the client treats as required.
func reasoningOutputItem(id, summary string) map[string]any {
	summaries := []any{}
	if summary != "" {
		summaries = append(summaries, map[string]any{"type": "summary_text", "text": summary})
	}
	return map[string]any{
		"type":              "reasoning",
		"id":                id,
		"summary":           summaries,
		"content":           []any{},
		"encrypted_content": nil,
	}
}

// emitReasoningSummary emits the deltas for one reasoning item.
//
// Both the delta and the done event are sent because the client consumes exactly
// one of them depending on its concurrent-reasoning-summaries setting, and
// ignores the other. Sending both is correct under either setting; sending one
// makes reasoning invisible under the other.
func emitReasoningSummary(emit func(string, any), outputIndex int, itemID, summary string) {
	if summary == "" {
		return
	}
	emit("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": outputIndex, "item_id": itemID, "summary_index": 0, "delta": summary})
	emit("response.reasoning_summary_text.done", map[string]any{"type": "response.reasoning_summary_text.done", "output_index": outputIndex, "item_id": itemID, "summary_index": 0, "text": summary})
}

// writeResponsesResult projects an internal OpenAI-style result into the
// Responses events and completion shape consumed by Codex.
func writeResponsesResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	id := firstNonEmpty(mapString(src, "m365_response_id"), "resp_"+uuid.NewString())
	msg, _ := openAIChoice(src)
	text, _ := msg["content"].(string)
	output := responsesOutputItems(msg)
	usage, _ := src["usage"].(map[string]any)
	usageSource, _ := src["m365_usage_source"].(string)
	if usage == nil {
		estimate := estimateResponsesUsage(model, nil, nil, nil, text)
		usage = estimate.Values
		usageSource = estimate.Source
	}
	if usageSource == "" {
		usageSource = usageSourceHeuristic
	}
	resp := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": output, "usage": usage, "m365": localUsageMetadata(usageSource)}
	if !stream {
		jsonOut(w, resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	aborted := false
	emit := func(name string, v any) {
		if aborted {
			return
		}
		if err := sseWriteFrame(w, f, name, v); err != nil {
			aborted = true
		}
	}
	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}})
	for i, item := range output {
		m, _ := item.(map[string]any)
		addedItem := item
		if m["type"] == "function_call" {
			// Arguments arrive in function_call_arguments.delta. Including them
			// here too would make conforming clients append duplicate JSON.
			added := make(map[string]any, len(m))
			for k, v := range m {
				added[k] = v
			}
			added["arguments"] = ""
			added["status"] = "in_progress"
			addedItem = added
		}
		emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i, "item": addedItem})
		if m["type"] == "message" {
			content, _ := m["content"].([]any)
			if len(content) > 0 {
				c, _ := content[0].(map[string]any)
				emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": i, "content_index": 0, "delta": c["text"]})
			}
		} else if m["type"] == "reasoning" {
			itemID, _ := m["id"].(string)
			emitReasoningSummary(emit, i, itemID, reasoningItemSummary(m))
		} else if m["type"] == "function_call" {
			args, _ := m["arguments"].(string)
			emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": m["id"], "delta": args})
			emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": m["id"], "arguments": args})
		} else if m["type"] == "custom_tool_call" {
			input, _ := m["input"].(string)
			emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": i, "item_id": m["id"], "delta": input})
			emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": i, "item_id": m["id"], "input": input})
		}
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
	}
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

// Message phases classify assistant text within one turn. Clients render
// commentary as progress and only treat a final answer as the turn's result.
//
// The field is optional in the protocol, and its absence means "phase unknown",
// which conforming clients must handle by assuming a final answer. Leaving it out
// therefore made every mid-turn preamble render as another finished answer,
// interleaved with the real one.
const (
	messagePhaseCommentary  = "commentary"
	messagePhaseFinalAnswer = "final_answer"
)

// assistantMessageItem builds a Responses message output item. phase says whether
// this text is mid-turn narration or the turn's answer.
func assistantMessageItem(id, text, phase string) map[string]any {
	return map[string]any{
		"type":    "message",
		"id":      id,
		"role":    "assistant",
		"status":  "completed",
		"phase":   phase,
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
}

// responsesOutputItems projects one internal completion into Responses output
// items. A turn that both narrates and calls a tool produces both: the message
// first, then the calls. Emitting only the calls loses the model's stated intent,
// which clients render alongside the call detail. Reasoning, when present, comes
// first of all — clients attach later deltas to the most recently added item.
func responsesOutputItems(msg map[string]any) []any {
	var output []any
	if reasoning, _ := msg["reasoning_content"].(string); strings.TrimSpace(reasoning) != "" {
		output = append(output, reasoningOutputItem("rs_"+uuid.NewString(), reasoning))
	}
	calls, _ := msg["tool_calls"].([]any)
	if text, _ := msg["content"].(string); strings.TrimSpace(text) != "" {
		// Text accompanied by a call is a preamble: the turn continues once the
		// call returns, so it is commentary rather than the answer.
		phase := messagePhaseFinalAnswer
		if len(calls) > 0 {
			phase = messagePhaseCommentary
		}
		output = append(output, assistantMessageItem("msg_"+uuid.NewString(), text, phase))
	}
	for _, raw := range calls {
		tc, _ := raw.(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		if tc["type"] == "custom" {
			output = append(output, map[string]any{"type": "custom_tool_call", "id": "ctc_" + uuid.NewString(), "call_id": tc["id"], "name": fn["name"], "input": customToolInput(fn["arguments"]), "status": "completed"})
			continue
		}
		output = append(output, map[string]any{"type": "function_call", "id": "fc_" + uuid.NewString(), "call_id": tc["id"], "name": fn["name"], "arguments": fn["arguments"], "status": "completed"})
	}
	if len(output) == 0 {
		// A turn with neither text nor a call still needs one item: clients treat a
		// completed response with an empty output array as a protocol violation.
		output = append(output, assistantMessageItem("msg_"+uuid.NewString(), "", messagePhaseFinalAnswer))
	}
	return output
}

// reasoningItemSummary reads the summary text back out of a reasoning item.
func reasoningItemSummary(item map[string]any) string {
	entries, _ := item["summary"].([]any)
	for _, raw := range entries {
		if entry, ok := raw.(map[string]any); ok {
			if text, _ := entry["text"].(string); text != "" {
				return text
			}
		}
	}
	return ""
}

func customToolInput(arguments any) string {
	if s, ok := arguments.(string); ok {
		var v struct {
			Input string `json:"input"`
		}
		if json.Unmarshal([]byte(s), &v) == nil {
			return v.Input
		}
	}
	return ""
}
