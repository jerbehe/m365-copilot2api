package web

import (
	"fmt"
	"net/http"
	"time"

	"m365-copilot2api/internal/chathub"
)

// toolResponse is one tool-call turn to serialize. It is a struct rather than a
// positional argument list because several fields are strings and bools whose
// meaning is not obvious at the call site.
type toolResponse struct {
	ID    string
	Model string
	// Stream selects SSE chunks over a single chat.completion object.
	Stream bool
	Calls  []detectedToolCall
	Result chathub.Result
	// Preamble is the assistant text that accompanied the calls. It is set
	// explicitly rather than read from Result.Text because most callers hold text
	// that must never reach the client: the tool router's raw JSON decision, or an
	// answer whose deltas were already streamed. Callers that do pass prose strip
	// the tool syntax first.
	Preamble string
	// Prompt is the flattened request text, used only for the token estimate.
	Prompt string
	// Options carries stream_options from the request. A tool turn is a normal
	// streaming response as far as include_usage is concerned.
	Options *streamOptions
}

// usage reports the turn's token estimate in the OpenAI chat.completion shape.
// The object requires usage on non-streaming responses; omitting it left clients
// unable to account for tool turns at all.
//
// EstimateTokens is used rather than the Responses estimator so both branches of
// /v1/chat/completions report on the same scale — the same request would
// otherwise yield different counts depending on whether a tool was called.
func (t toolResponse) usage() map[string]any {
	prompt := EstimateTokens(t.Prompt)
	completion := EstimateTokens(t.Preamble)
	for _, call := range t.Calls {
		completion += EstimateTokens(call.Name) + EstimateTokens(string(call.Arguments))
	}
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
}

func writeToolResponse(w http.ResponseWriter, args ...any) error {
	var t toolResponse
	switch len(args) {
	case 1:
		var ok bool
		t, ok = args[0].(toolResponse)
		if !ok {
			return fmt.Errorf("invalid tool response")
		}
	case 5:
		id, idOK := args[0].(string)
		model, modelOK := args[1].(string)
		stream, streamOK := args[2].(bool)
		calls, callsOK := args[3].([]detectedToolCall)
		result, resultOK := args[4].(chathub.Result)
		if !idOK || !modelOK || !streamOK || !callsOK || !resultOK {
			return fmt.Errorf("invalid legacy tool response")
		}
		t = toolResponse{
			ID: id, Model: model, Stream: stream, Calls: calls, Result: result,
		}
	default:
		return fmt.Errorf("invalid tool response argument count: %d", len(args))
	}
	toolCalls := toolCallMaps(t.Calls)
	msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls}
	if t.Preamble != "" {
		msg["content"] = t.Preamble
	}
	if reasoning := sanitizePublicReasoningText(t.Result.Reasoning); reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	created := time.Now().Unix()
	if t.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)
		streamed := newStreamUsage(t.Options, t.Prompt)
		emit := func(v any) {
			if err := sseDataRaw(w, flusher, mustJSON(v)); err != nil {
				return
			}
		}
		base := func(delta map[string]any, finish any) map[string]any {
			return streamed.decorate(map[string]any{"id": t.ID, "object": "chat.completion.chunk", "created": created, "model": t.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		}
		firstDelta := map[string]any{"role": "assistant", "content": nil}
		if t.Preamble != "" {
			firstDelta["content"] = t.Preamble
		}
		if reasoning := sanitizePublicReasoningText(t.Result.Reasoning); reasoning != "" {
			firstDelta["reasoning_content"] = reasoning
		}
		emit(base(firstDelta, nil))
		for i, tc := range t.Calls {
			typ := tc.Type
			if typ == "" {
				typ = "function"
			}
			emit(base(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tc.ID, "type": typ, "function": map[string]any{"name": tc.Name, "arguments": string(tc.Arguments)}}}}, nil))
		}
		emit(base(map[string]any{}, "tool_calls"))
		// The turn's completion tokens are the preamble plus the calls, which
		// t.usage already accounts for; reuse it so the streaming and
		// non-streaming totals for one turn agree.
		if streamed.enabled {
			total := t.usage()
			emit(map[string]any{"id": t.ID, "object": "chat.completion.chunk", "created": created, "model": t.Model, "choices": []any{}, "usage": total})
		}
		_ = sseSafeRaw(w, flusher, "data: [DONE]\n\n")
		return nil
	}
	jsonOut(w, map[string]any{"id": t.ID, "object": "chat.completion", "created": created, "model": t.Model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "tool_calls"}}, "usage": t.usage(), "m365": compatM365Metadata(t.Result)})
	return nil
}
