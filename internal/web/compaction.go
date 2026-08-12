package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Codex marks a remote-compaction turn ("remote compaction v2") by appending a
// bare compaction_trigger item to the Responses input instead of using a
// separate endpoint. Such a turn must be answered with exactly one `compaction`
// output item whose encrypted_content carries the handoff summary; the client
// counts only that item type and aborts the turn with "expected exactly one
// compaction output item" when it sees an ordinary assistant message.
const compactionTriggerItemType = "compaction_trigger"

// compactionSummaryPrefix labels a replayed summary so the model treats it as
// carried-over context rather than a fresh user instruction. encrypted_content
// is opaque to the client, so the gateway keeps the summary as plain text and
// can read its own earlier summaries back.
const compactionSummaryPrefix = "Compacted context carried over from the earlier part of this conversation:\n"

// compactionInstruction drives the summary. The trigger item carries no prompt
// of its own: the server side owns the wording, so it has to be supplied here.
const compactionInstruction = `You are performing a CONTEXT CHECKPOINT COMPACTION of the conversation above. Write a handoff summary that lets another assistant resume the task without access to anything else.

Cover the user's goal and explicit requirements; decisions taken and the reasons for them; work already completed, including files created or modified and commands run with their outcomes; the current state and what still remains; constraints, conventions, and pitfalls discovered along the way; and any values that must survive verbatim, such as paths, identifiers, and signatures.

Reply with the summary alone, as plain prose and lists. Do not address the user, do not ask questions, and do not call any tool.`

// compactionContextKey marks the inner chat request as serving a compaction
// turn. It travels through the request context rather than the request body so a
// client cannot set it: it relaxes the completion-evidence guard, which exists to
// stop unverified success claims from reaching the caller.
type compactionContextKey struct{}

// withCompactionTurn tags r as the inner request of a compaction turn.
func withCompactionTurn(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), compactionContextKey{}, true))
}

// isCompactionTurn reports whether this request is summarizing a conversation.
//
// A handoff summary necessarily narrates completed work ("created parser.go"),
// but the tool calls that did the work belong to earlier turns and reach the
// gateway as plain history rather than as tool evidence. The completion guard
// would therefore read the summary as an unsupported success claim and replace
// it wholesale, installing that replacement as the conversation's entire
// remaining context.
func isCompactionTurn(r *http.Request) bool {
	v, _ := r.Context().Value(compactionContextKey{}).(bool)
	return v
}

// return. The client's item-id convention for this type is the "cmp" prefix.
func compactionOutputItem(summary string) map[string]any {
	return map[string]any{
		"type":              "compaction",
		"id":                "cmp_" + uuid.NewString(),
		"encrypted_content": summary,
	}
}

// writeCompactionResult renders a non-streaming compaction turn, or replays a
// completed one as SSE. It mirrors writeResponsesResult but emits the compaction
// item in place of the assistant message, and drops any tool call: a compaction
// response that carries a second output item is rejected by the client.
func writeCompactionResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	id := firstNonEmpty(mapString(src, "m365_response_id"), "resp_"+uuid.NewString())
	msg, _ := openAIChoice(src)
	summary, _ := msg["content"].(string)
	item := compactionOutputItem(summary)
	output := []any{item}

	usage, _ := src["usage"].(map[string]any)
	usageSource, _ := src["m365_usage_source"].(string)
	if usage == nil {
		estimate := estimateResponsesUsage(model, nil, nil, nil, summary)
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
	emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

// compactionSummary extracts the handoff summary from an inner completion. It
// returns false when the summary is unusable: emitting an empty compaction item
// would install an empty context and silently discard the whole conversation, so
// the turn has to fail instead.
func compactionSummary(src map[string]any) (string, bool) {
	msg, _ := openAIChoice(src)
	if msg == nil {
		return "", false
	}
	summary, _ := msg["content"].(string)
	if strings.TrimSpace(summary) == "" {
		return "", false
	}
	return summary, true
}

// mapString reads a string field without turning a missing key into the literal
// "<nil>" that fmt.Sprint would produce.
func mapString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}
