package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// anthropicStreamWriter emits the Anthropic Messages SSE event sequence
// incrementally.
//
// The order and shape are what Claude Code and the Anthropic SDKs require:
//
//	message_start          — carries id/model/role and the input token count
//	ping                   — sent by the real API; keeps proxies from idling out
//	content_block_start    — one per block, index strictly increasing from 0
//	content_block_delta    — text_delta / thinking_delta / input_json_delta
//	content_block_stop     — closes the open block before the next one starts
//	message_delta          — stop_reason plus the final output token count
//	message_stop
//
// A tool_use block starts with an empty input object and its arguments arrive as
// input_json_delta.partial_json fragments that the client concatenates. Sending
// the arguments both in content_block_start and as a delta makes the client parse
// the JSON twice, so start blocks are always empty.
type anthropicStreamWriter struct {
	w     http.ResponseWriter
	f     http.Flusher
	id    string
	model string

	started   bool
	aborted   bool
	openBlock bool
	// nextIndex is the index the next content block will use.
	nextIndex int
	// blockKind is the kind of the currently open block, so a text delta after a
	// tool block correctly opens a new one.
	blockKind    string
	outputTokens int
	stopReason   string
	inputTokens  int
}

func newAnthropicStreamWriter(w http.ResponseWriter, model string, inputTokens int) *anthropicStreamWriter {
	f, _ := w.(http.Flusher)
	return &anthropicStreamWriter{w: w, f: f, id: "msg_" + uuid.NewString(), model: model, inputTokens: inputTokens, stopReason: "end_turn"}
}

func (a *anthropicStreamWriter) emit(name string, v any) error {
	if a.aborted {
		return nil
	}
	if err := sseWriteFrame(a.w, a.f, name, v); err != nil {
		a.aborted = true
		return err
	}
	return nil
}

// Start writes the headers and the message_start/ping pair. It is idempotent so
// callers may invoke it eagerly before the first upstream token.
func (a *anthropicStreamWriter) Start() error {
	if a.started {
		return nil
	}
	a.started = true
	a.w.Header().Set("Content-Type", "text/event-stream")
	a.w.Header().Set("Cache-Control", "no-cache")
	a.w.Header().Set("Connection", "keep-alive")
	a.w.Header().Set("X-Accel-Buffering", "no")
	if err := a.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": a.id, "type": "message", "role": "assistant", "model": a.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": a.inputTokens, "output_tokens": 0},
	}}); err != nil {
		return err
	}
	return a.emit("ping", map[string]any{"type": "ping"})
}

// closeBlock stops the open content block, if any.
func (a *anthropicStreamWriter) closeBlock() error {
	if !a.openBlock {
		return nil
	}
	index := a.nextIndex - 1
	a.openBlock = false
	a.blockKind = ""
	return a.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

// openBlockOfKind ensures a block of the requested kind is open, closing a block
// of a different kind first. It returns the block's index.
func (a *anthropicStreamWriter) openBlockOfKind(kind string, block map[string]any) (int, error) {
	if a.openBlock && a.blockKind == kind && kind != "tool_use" {
		return a.nextIndex - 1, nil
	}
	if err := a.closeBlock(); err != nil {
		return 0, err
	}
	index := a.nextIndex
	a.nextIndex++
	a.openBlock = true
	a.blockKind = kind
	return index, a.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
}

// Text forwards an answer-text delta.
func (a *anthropicStreamWriter) Text(delta string) error {
	if delta == "" {
		return nil
	}
	if err := a.Start(); err != nil {
		return err
	}
	index, err := a.openBlockOfKind("text", map[string]any{"type": "text", "text": ""})
	if err != nil {
		return err
	}
	a.outputTokens += heuristicTokenCount(delta)
	return a.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": delta}})
}

// Thinking forwards a chain-of-thought delta as an Anthropic thinking block.
func (a *anthropicStreamWriter) Thinking(delta string) error {
	if delta == "" {
		return nil
	}
	if err := a.Start(); err != nil {
		return err
	}
	index, err := a.openBlockOfKind("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
	if err != nil {
		return err
	}
	a.outputTokens += heuristicTokenCount(delta)
	return a.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": delta}})
}

// ToolStart opens a tool_use block. Anthropic requires a non-empty id and name
// here; arguments follow via ToolArgs.
func (a *anthropicStreamWriter) ToolStart(id, name string) (int, error) {
	if err := a.Start(); err != nil {
		return 0, err
	}
	if id == "" {
		id = "call_" + uuid.NewString()
	}
	a.stopReason = "tool_use"
	return a.openBlockOfKind("tool_use", map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
}

// ToolArgs forwards an arguments fragment for the open tool_use block.
func (a *anthropicStreamWriter) ToolArgs(index int, fragment string) error {
	if fragment == "" {
		return nil
	}
	a.outputTokens += heuristicTokenCount(fragment)
	return a.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": fragment}})
}

// Finish closes the stream. An answer that produced no block still gets an empty
// text block, because a message with zero content is rejected by strict clients.
func (a *anthropicStreamWriter) Finish() error {
	if err := a.Start(); err != nil {
		return err
	}
	if a.nextIndex == 0 {
		if _, err := a.openBlockOfKind("text", map[string]any{"type": "text", "text": ""}); err != nil {
			return err
		}
	}
	if err := a.closeBlock(); err != nil {
		return err
	}
	if err := a.emit("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": a.stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": a.outputTokens},
	}); err != nil {
		return err
	}
	return a.emit("message_stop", map[string]any{"type": "message_stop"})
}

// Failed reports an upstream failure. Before any block exists the stream can
// still be salvaged as an error event; afterwards the turn is ended with
// stop_reason so the client does not hang waiting for message_stop.
func (a *anthropicStreamWriter) Failed(message string) error {
	if a.nextIndex == 0 && !a.openBlock {
		if err := a.Start(); err != nil {
			return err
		}
		if err := a.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}}); err != nil {
			return err
		}
		return a.emit("message_stop", map[string]any{"type": "message_stop"})
	}
	a.stopReason = "end_turn"
	return a.Finish()
}

// streamAnthropicMessages translates the gateway's internal OpenAI SSE stream
// into Anthropic Messages events as it arrives.
func (s *Server) streamAnthropicMessages(w http.ResponseWriter, r *http.Request, o oaiReq, model string, startedAt time.Time) {
	inputEstimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, "")
	inputTokens, _ := inputEstimate.Values["input_tokens"].(int)
	out := newAnthropicStreamWriter(w, model, inputTokens)

	toolIndex := map[int]int{}
	status, plainErr, err := s.pipeOpenAIStream(r, o, innerStreamHandler{
		Text:      out.Text,
		Reasoning: out.Thinking,
		ToolStart: func(index int, id, name, _ string) error {
			blockIndex, err := out.ToolStart(id, name)
			if err != nil {
				return err
			}
			toolIndex[index] = blockIndex
			return nil
		},
		ToolArgs: func(index int, fragment string) error {
			return out.ToolArgs(toolIndex[index], fragment)
		},
		Error: func(message string) error { return out.Failed(message) },
	})
	if err != nil {
		// A write error means the client is gone; there is nothing left to send.
		return
	}
	if status >= http.StatusBadRequest {
		message := firstNonEmpty(plainErr, "upstream protocol error")
		// The inner handler rejected the request before writing SSE. Nothing has
		// been emitted yet in that case, so a plain JSON error is still possible and
		// reports the reason far more clearly than an SSE error frame.
		if !out.started {
			writeAnthropicError(w, status, "api_error", message)
			return
		}
		_ = out.Failed(message)
	} else {
		_ = out.Finish()
	}
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		Model:        model,
		Endpoint:     "/v1/messages",
		InputTokens:  int64(inputTokens),
		OutputTokens: int64(out.outputTokens),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
}

// countAnthropicTokens answers /v1/messages/count_tokens. Claude Code calls it
// before every turn to decide whether to compact the conversation; a 404 makes it
// fall back to a local guess, and a malformed body makes it error out.
func (s *Server) countAnthropicTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&body) != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model := firstNonEmpty(body.Model, "m365-copilot")
	estimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, "")
	tokens, _ := estimate.Values["input_tokens"].(int)
	jsonOut(w, map[string]any{
		"input_tokens": tokens,
		"m365":         localUsageMetadata(estimate.Source),
	})
}
