package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/google/uuid"
)

type pipeResponseWriter struct {
	h      http.Header
	w      *io.PipeWriter
	status int
}

func (p *pipeResponseWriter) Header() http.Header { return p.h }
func (p *pipeResponseWriter) WriteHeader(n int) {
	if p.status == 0 {
		p.status = n
	}
}
func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = 200
	}
	return p.w.Write(b)
}
func (p *pipeResponseWriter) Flush() {}

// streamResponsesAdapter converts the internal OpenAI SSE incrementally instead
// of buffering the entire completion in httptest.ResponseRecorder.
func (s *Server) streamResponsesAdapter(w http.ResponseWriter, r *http.Request, o oaiReq, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	emit := func(name string, v any) error {
		return writeSSE(r, w, flusher, name, v)
	}
	translateResponsesStream(emit, model, o, func(h innerStreamHandler) (int, string, error) {
		return s.pipeOpenAIStream(r, o, h)
	})
}

// translateResponsesStream emits the Responses event sequence for one inner
// OpenAI stream. run supplies the fragments; it is a parameter so the translation
// can be exercised against a recorded stream in tests.
//
// The invariants Codex depends on:
//   - response.created is first and either response.completed or response.failed
//     is last, so the client is never left waiting
//   - every item gets one output_item.added with an id that all later events reuse
//   - output_index is a single monotonic space shared by the message and the calls
//   - arguments are streamed exactly once as function_call_arguments.delta, so
//     output_item.added carries an empty arguments string
func translateResponsesStream(emit func(string, any) error, model string, o oaiReq, run func(innerStreamHandler) (int, string, error)) {
	id := "resp_" + uuid.NewString()
	created := time.Now().Unix()
	_ = emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}})

	var text strings.Builder
	messageID := "msg_" + uuid.NewString()
	contentID := "txt_" + uuid.NewString()
	textStarted := false
	textIndex := 0
	type tcState struct {
		ID, Name, Args, Type string
		ItemID               string
		// OutputIndex is this item's position in the response output array. It is
		// assigned when the item is announced and must stay stable across every
		// later event, or clients cannot correlate the deltas.
		OutputIndex int
	}
	calls := map[int]*tcState{}
	var order []int
	// nextOutputIndex hands out response output slots. Text and tool items share
	// one index space, so a tool call after a text block must not reuse index 0.
	nextOutputIndex := 0
	var upstreamErr string

	status, plainErr, streamErr := run(innerStreamHandler{
		Text: func(content string) error {
			text.WriteString(content)
			if !textStarted {
				textStarted = true
				textIndex = nextOutputIndex
				nextOutputIndex++
				if err := emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": textIndex, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}}); err != nil {
					return err
				}
			}
			return emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": textIndex, "content_index": 0, "item_id": messageID, "delta": content})
		},
		ToolStart: func(index int, callID, name, typ string) error {
			prefix := "fc_"
			item := map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": "", "status": "in_progress"}
			if typ == "custom" {
				prefix = "ctc_"
				item = map[string]any{"type": "custom_tool_call", "call_id": callID, "name": name, "input": "", "status": "in_progress"}
			}
			st := &tcState{ID: callID, Name: name, Type: typ, ItemID: prefix + uuid.NewString(), OutputIndex: nextOutputIndex}
			nextOutputIndex++
			calls[index] = st
			order = append(order, index)
			item["id"] = st.ItemID
			return emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": st.OutputIndex, "item": item})
		},
		ToolArgs: func(index int, fragment string) error {
			st := calls[index]
			if st == nil {
				return nil
			}
			st.Args += fragment
			if st.Type == "custom" {
				// A custom tool's input is a raw script bridged through
				// {"input":"..."}; it only becomes a string once the JSON is whole.
				return nil
			}
			return emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": st.OutputIndex, "item_id": st.ItemID, "delta": fragment})
		},
		Error: func(message string) error {
			upstreamErr = firstNonEmpty(message, "upstream request failed")
			return nil
		},
	})
	if streamErr != nil {
		// The client is gone or a write failed; nothing further can be delivered.
		return
	}
	failed := func(code any, message string) {
		_ = emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": code, "message": message},
			},
		})
	}
	if upstreamErr != "" || status >= http.StatusBadRequest {
		code := any(status)
		message := firstNonEmpty(upstreamErr, plainErr, "inner chat request failed")
		if status < http.StatusBadRequest {
			code = "upstream_error"
		}
		failed(code, message)
		return
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" {
		// Never leave a Responses stream after response.created without a
		// terminal event: clients otherwise render this as a successful blank
		// answer and may reuse an incomplete response on the next turn.
		failed("empty_upstream_response", "ChatHub returned no text or tool call")
		return
	}
	output := []any{}
	if textStarted {
		item := map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": text.String(), "annotations": []any{}}}}
		output = append(output, item)
		_ = emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": textIndex, "content_index": 0, "item_id": messageID, "text": text.String()})
		_ = emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": textIndex, "item": item})
	}
	for _, index := range order {
		st := calls[index]
		if st == nil {
			continue
		}
		if st.Type == "custom" {
			input := customToolInput(st.Args)
			item := map[string]any{"type": "custom_tool_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "input": input, "status": "completed"}
			output = append(output, item)
			_ = emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": st.OutputIndex, "item_id": st.ItemID, "delta": input})
			_ = emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": st.OutputIndex, "item_id": st.ItemID, "input": input})
			_ = emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": st.OutputIndex, "item": item})
			continue
		}
		// output_item.added and every arguments delta were already emitted for
		// st.ItemID while streaming. Re-announcing the item under a fresh id makes
		// conforming clients concatenate the argument JSON twice, so only the
		// terminal events are emitted here.
		item := map[string]any{"type": "function_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "arguments": st.Args, "status": "completed"}
		output = append(output, item)
		_ = emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": st.OutputIndex, "item_id": st.ItemID, "arguments": st.Args})
		_ = emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": st.OutputIndex, "item": item})
	}
	usageOutput := text.String()
	for _, index := range order {
		if st := calls[index]; st != nil {
			usageOutput += st.Name + st.Args
		}
	}
	estimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, usageOutput)
	resp := map[string]any{"id": id, "object": "response", "created_at": created, "status": "completed", "model": model, "output": output, "usage": estimate.Values, "m365": localUsageMetadata(estimate.Source)}
	_ = emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

func (s *Server) runOpenAIAdapter(r *http.Request, o oaiReq) (map[string]any, []byte, int, error) {
	o.Stream = false
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	rr := httptest.NewRecorder()
	s.openaiChat(rr, r2)
	var out map[string]any
	err := json.Unmarshal(rr.Body.Bytes(), &out)
	return out, rr.Body.Bytes(), rr.Code, err
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeResponsesError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body responsesRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeResponsesError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeResponsesError(w, 400, "invalid_request_error", err.Error())
		return
	}
	tenant := apiKeyTenant(r)
	if body.PreviousResponseID != "" {
		s.responseMu.Lock()
		prior, ok := s.responseMessages[tenant][body.PreviousResponseID]
		messages := append([]oaiMsg(nil), prior.Messages...)
		s.responseMu.Unlock()
		if !ok || len(messages) == 0 {
			writeResponsesError(w, 400, "invalid_request_error", "unknown previous_response_id")
			return
		}
		o.Messages = append(messages, o.Messages...)
	}
	if body.Stream {
		s.streamResponsesAdapter(w, r, o, firstNonEmpty(body.Model, "m365-copilot"))
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeResponsesError(w, status, "upstream_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream protocol error: "+err.Error())
		return
	}
	if !responsesOutputHasContent(out) {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "ChatHub returned an empty response; no reusable message was created")
		return
	}
	msg, _ := openAIChoice(out)
	outputForUsage := ""
	if msg != nil {
		outputForUsage = fmt.Sprint(msg["content"])
		if calls, ok := msg["tool_calls"].([]any); ok {
			outputForUsage += fmt.Sprint(calls)
		}
	}
	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, outputForUsage)
	out["usage"] = estimate.Values
	out["m365_usage_source"] = estimate.Source
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		Model:        firstNonEmpty(body.Model, "m365-copilot"),
		Endpoint:     "/v1/responses",
		InputTokens:  int64(estimate.Values["input_tokens"].(int)),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
	// Retain the normalized history so a subsequent previous_response_id can
	// validate its function_call_output against the original tool call.
	if _, ok := out["id"].(string); ok {
		// Use the same public response id that writeResponsesResult exposes.
		publicID := "resp_" + uuid.NewString()
		out["m365_response_id"] = publicID
		stored := append([]oaiMsg(nil), o.Messages...)
		if msg, _ := openAIChoice(out); msg != nil {
			if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
				converted := make([]map[string]any, 0, len(calls))
				for _, call := range calls {
					if m, ok := call.(map[string]any); ok {
						converted = append(converted, m)
					}
				}
				stored = append(stored, oaiMsg{Role: "assistant", ToolCalls: converted})
			} else {
				if text, _ := msg["content"].(string); text != "" {
					stored = append(stored, oaiMsg{Role: "assistant", Content: text})
				}
			}
		}
		s.responseMu.Lock()
		bucket := s.responseMessages[tenant]
		if bucket == nil {
			bucket = map[string]respHistory{}
			s.responseMessages[tenant] = bucket
		}
		for k, h := range bucket {
			if time.Since(h.At) > time.Hour {
				delete(bucket, k)
			}
		}
		if len(bucket) >= maxResponsesPerTenant {
			var oldestKey string
			var oldestAt time.Time
			for k, h := range bucket {
				if oldestKey == "" || h.At.Before(oldestAt) {
					oldestKey, oldestAt = k, h.At
				}
			}
			delete(bucket, oldestKey)
		}
		bucket[publicID] = respHistory{At: time.Now(), Messages: stored}
		s.responseMu.Unlock()
	}
	writeResponsesResult(w, firstNonEmpty(body.Model, "m365-copilot"), body.Stream, out)
}

func responsesOutputHasContent(src map[string]any) bool {
	msg, _ := openAIChoice(src)
	if msg == nil {
		return false
	}
	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	text, _ := msg["content"].(string)
	return strings.TrimSpace(text) != ""
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeAnthropicError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	model := firstNonEmpty(body.Model, "m365-copilot")
	if body.Stream {
		// Translate incrementally. Buffering the whole completion first made
		// Claude Code wait for the full answer and then receive it as a single
		// text_delta, which also duplicated it into content_block_start.
		s.streamAnthropicMessages(w, r, o, model, startedAt)
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeAnthropicError(w, status, "api_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream protocol error: "+err.Error())
		return
	}
	msg, _ := openAIChoice(out)
	outputText := ""
	if msg != nil {
		if text, ok := msg["content"].(string); ok {
			outputText = text
		}
		if calls, ok := msg["tool_calls"].([]any); ok {
			outputText += mustJSON(calls)
		}
	}
	estimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, outputText)
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		Model:        model,
		Endpoint:     "/v1/messages",
		InputTokens:  int64(estimate.Values["input_tokens"].(int)),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
	writeAnthropicResultUsage(w, model, body.Stream, out, estimate)
}
