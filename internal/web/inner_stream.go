package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// innerStreamHandler receives the decoded fragments of the gateway's internal
// OpenAI-compatible SSE stream. Every callback may abort the translation by
// returning an error.
//
// Protocol adapters (Anthropic, Responses) translate this neutral fragment
// stream instead of buffering the whole completion, so a client sees text as
// ChatHub produces it.
type innerStreamHandler struct {
	// Text receives an answer-text delta.
	Text func(string) error
	// Reasoning receives a chain-of-thought delta.
	Reasoning func(string) error
	// ToolStart is called once per tool call, before its first arguments
	// fragment. typ is "function" or "custom".
	ToolStart func(index int, id, name, typ string) error
	// ToolArgs receives an arguments fragment for a started tool call.
	ToolArgs func(index int, fragment string) error
	// Error receives an in-band upstream error frame.
	Error func(message string) error
}

// pipeOpenAIStream runs the OpenAI-compatible handler against a cloned request
// and dispatches its SSE frames to h. It returns the inner handler's HTTP status
// (200 when the handler never wrote one explicitly), the plain-text body it wrote
// instead of SSE (empty on success), and the first callback or scan error.
//
// The inner handler streams into an io.Pipe rather than an
// httptest.ResponseRecorder so translation is incremental: buffering the whole
// completion first would make every protocol adapter deliver the answer as one
// lump after upstream finished.
func (s *Server) pipeOpenAIStream(r *http.Request, o oaiReq, h innerStreamHandler) (int, string, error) {
	o.Stream = true
	b, _ := json.Marshal(o)
	inner := r.Clone(r.Context())
	inner.Method = http.MethodPost
	inner.Body = io.NopCloser(bytes.NewReader(b))
	inner.ContentLength = int64(len(b))

	pr, pw := io.Pipe()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	done := make(chan struct{})
	go func() {
		s.openaiChat(irw, inner)
		_ = pw.Close()
		close(done)
	}()

	// The inner handler rejects a bad request with http.Error, i.e. a plain-text
	// body rather than SSE. Keep it so the caller can report the real reason
	// instead of a generic "upstream protocol error".
	var plain strings.Builder
	err := dispatchInnerStream(pr, h, &plain)
	// Close the read half before waiting: a callback error abandons the stream
	// mid-flight, and the inner handler would otherwise block forever on its next
	// write, never reaching close(done).
	_ = pr.Close()
	<-done
	status := irw.status
	if status == 0 {
		status = http.StatusOK
	}
	return status, strings.TrimSpace(plain.String()), err
}

// dispatchInnerStream decodes the gateway's internal OpenAI SSE frames from r and
// dispatches them to h. Lines that are not SSE data frames are copied to plain,
// which carries the inner handler's plain-text error body. It is separated from
// the pipe plumbing so protocol translation can be tested against a recorded
// stream.
func dispatchInnerStream(r io.Reader, h innerStreamHandler, plain *strings.Builder) error {
	started := map[int]bool{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			// ": connected" and blank lines are SSE framing, not an error body.
			if plain != nil && line != "" && !strings.HasPrefix(line, ":") {
				if plain.Len() > 0 {
					plain.WriteByte('\n')
				}
				plain.WriteString(line)
			}
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if errObj, ok := chunk["error"].(map[string]any); ok {
			msg, _ := errObj["message"].(string)
			if h.Error != nil {
				return h.Error(msg)
			}
			return nil
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if v, ok := delta["reasoning_content"].(string); ok && v != "" && h.Reasoning != nil {
			if err := h.Reasoning(v); err != nil {
				return err
			}
		}
		if v, ok := delta["content"].(string); ok && v != "" && h.Text != nil {
			if err := h.Text(v); err != nil {
				return err
			}
		}
		rawCalls, _ := delta["tool_calls"].([]any)
		for _, raw := range rawCalls {
			tc, _ := raw.(map[string]any)
			index := 0
			if v, ok := tc["index"].(float64); ok {
				index = int(v)
			}
			fn, _ := tc["function"].(map[string]any)
			if !started[index] {
				started[index] = true
				id, _ := tc["id"].(string)
				name, _ := fn["name"].(string)
				typ, _ := tc["type"].(string)
				if typ == "" {
					typ = "function"
				}
				if h.ToolStart != nil {
					if err := h.ToolStart(index, id, name, typ); err != nil {
						return err
					}
				}
			}
			if args, ok := fn["arguments"].(string); ok && args != "" && h.ToolArgs != nil {
				if err := h.ToolArgs(index, args); err != nil {
					return err
				}
			}
		}
	}
	return scanner.Err()
}
