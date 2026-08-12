package web

import (
	"context"
	"net/http"
)

// streamOptions is the OpenAI stream_options request field.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// streamUsage implements the OpenAI stream_options.include_usage contract.
//
// When a client opts in, two things change. Every ordinary chunk carries an
// explicit "usage": null, so the field is always present and a client need not
// treat its absence as a protocol variant. Then one extra chunk is emitted after
// the terminal finish_reason chunk and before [DONE], carrying the request's
// token usage and an empty choices array.
//
// The zero value is the opt-out case and decorates nothing, so paths that never
// look at stream options keep emitting exactly what they emitted before.
type streamUsage struct {
	enabled bool
	// prompt is fixed once the request is flattened; completion accumulates as
	// text is emitted, because the total is only known at the terminal chunk.
	prompt     int64
	completion int64
}

// newStreamUsage reports the opt-in state for one request.
func newStreamUsage(opts *streamOptions, prompt string) streamUsage {
	if opts == nil || !opts.IncludeUsage {
		return streamUsage{}
	}
	return streamUsage{enabled: true, prompt: EstimateTokens(prompt)}
}

// addCompletion accounts for text delivered to the client.
func (u *streamUsage) addCompletion(text string) {
	if !u.enabled || text == "" {
		return
	}
	u.completion += EstimateTokens(text)
}

// decorate stamps "usage": null onto an ordinary chunk. The spec is explicit
// that every non-terminal chunk carries the field with a null value.
func (u streamUsage) decorate(chunk map[string]any) map[string]any {
	if u.enabled {
		chunk["usage"] = nil
	}
	return chunk
}

// values renders the usage object for the terminal chunk.
func (u streamUsage) values() map[string]any {
	return map[string]any{
		"prompt_tokens":     u.prompt,
		"completion_tokens": u.completion,
		"total_tokens":      u.prompt + u.completion,
	}
}

// writeFinal emits the extra usage chunk. It must come after the chunk carrying
// finish_reason and before [DONE]; choices is always empty, so a client that
// accumulates deltas by index is unaffected.
func (u streamUsage) writeFinal(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, id, model string, created int64) {
	if !u.enabled {
		return
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{},
		"usage":   u.values(),
	}
	_ = sseRaw(ctx, w, flusher, "data: "+mustJSON(chunk)+"\n\n")
}
