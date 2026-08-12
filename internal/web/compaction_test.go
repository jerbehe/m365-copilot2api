package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// compactionInput mirrors what Codex sends on a remote-compaction turn: the
// history, then a bare compaction_trigger item.
func compactionInput(history ...any) []any {
	return append(append([]any(nil), history...), map[string]any{"type": compactionTriggerItemType})
}

func userItem(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func TestCompactionTriggerIsDetectedAndNotSentAsHistory(t *testing.T) {
	r := responsesRequest{Model: "gpt-5.6-sol", Input: compactionInput(userItem("build the parser"))}
	o, isCompaction, err := r.openAIWithCompaction()
	if err != nil {
		t.Fatal(err)
	}
	if !isCompaction {
		t.Fatal("compaction_trigger did not mark the request as a compaction turn")
	}
	prompt, _ := flattenPromptMessages(o.Messages, nil)
	if strings.Contains(prompt, compactionTriggerItemType) {
		t.Fatalf("trigger item leaked into the prompt: %s", prompt)
	}
	if !strings.Contains(prompt, "build the parser") {
		t.Fatalf("history was dropped: %s", prompt)
	}
	// The trigger carries no wording of its own, so the gateway has to ask for the
	// summary or the model just answers the last user turn again.
	if !strings.Contains(prompt, "CONTEXT CHECKPOINT COMPACTION") {
		t.Fatalf("compaction instruction missing: %s", prompt)
	}
}

// A compaction response may contain exactly one compaction item, so a tool call
// in that turn is fatal to the client. Withhold the tools instead.
func TestCompactionTurnDropsTools(t *testing.T) {
	r := responsesRequest{
		Model:      "gpt-5.6-sol",
		Input:      compactionInput(userItem("hi")),
		Tools:      []map[string]any{{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}}},
		ToolChoice: "auto",
	}
	o, isCompaction, err := r.openAIWithCompaction()
	if err != nil {
		t.Fatal(err)
	}
	if !isCompaction {
		t.Fatal("expected a compaction turn")
	}
	if len(o.Tools) != 0 || o.ToolChoice != nil {
		t.Fatalf("tools=%#v tool_choice=%#v", o.Tools, o.ToolChoice)
	}
}

// After a compaction the client replays the item it was given. Its summary has to
// come back as context, not as the item's raw JSON.
func TestCompactionItemReplaysAsContext(t *testing.T) {
	r := responsesRequest{Model: "gpt-5.6-sol", Input: []any{
		map[string]any{"type": "compaction", "id": "cmp_1", "encrypted_content": "SUMMARY_MARKER"},
		userItem("continue"),
	}}
	o, isCompaction, err := r.openAIWithCompaction()
	if err != nil {
		t.Fatal(err)
	}
	if isCompaction {
		t.Fatal("replaying a compaction item is an ordinary turn, not a compaction turn")
	}
	prompt, _ := flattenPromptMessages(o.Messages, nil)
	if !strings.Contains(prompt, "SUMMARY_MARKER") {
		t.Fatalf("summary was dropped: %s", prompt)
	}
	if strings.Contains(prompt, "encrypted_content") {
		t.Fatalf("raw item JSON leaked into the prompt: %s", prompt)
	}
}

func TestCompactionResultReturnsExactlyOneCompactionItem(t *testing.T) {
	rr := httptest.NewRecorder()
	writeCompactionResult(rr, "gpt-5.6-sol", false, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "handoff summary"}}},
	})
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	output, _ := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("want exactly one output item, got %#v", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "compaction" {
		t.Fatalf("item type=%v", item["type"])
	}
	if item["encrypted_content"] != "handoff summary" {
		t.Fatalf("summary not carried: %#v", item)
	}
	if id, _ := item["id"].(string); !strings.HasPrefix(id, "cmp_") {
		t.Fatalf("item id=%q, client expects the cmp prefix", id)
	}
}

// The client counts compaction items across output_item.done events and aborts
// unless it sees exactly one before response.completed.
func TestStreamingCompactionResultEmitsOneDoneEvent(t *testing.T) {
	rr := httptest.NewRecorder()
	writeCompactionResult(rr, "gpt-5.6-sol", true, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "handoff summary"}}},
	})
	body := rr.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_item.added", "event: response.completed", `"type":"compaction"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
	if n := strings.Count(body, "event: response.output_item.done"); n != 1 {
		t.Fatalf("want 1 output_item.done, got %d: %s", n, body)
	}
	if strings.Contains(body, `"type":"message"`) {
		t.Fatalf("a message item would make the client count 0 compaction items: %s", body)
	}
}

// A handoff summary necessarily narrates completed work, but the tool calls that
// did the work reach the gateway as plain history. The completion guard must not
// replace the summary, since that replacement becomes the installed context.
func TestCompactionTurnBypassesCompletionEvidenceGuard(t *testing.T) {
	summary := "Created parser.go and verified the tokenizer successfully."
	ledger := buildAgentLedger(nil)
	if completionEvidenceAllows(summary, ledger) {
		t.Fatal("precondition: this summary should trip the guard without the bypass")
	}
	plain := httptest.NewRequest("POST", "/v1/responses", strings.NewReader("{}"))
	if isCompactionTurn(plain) {
		t.Fatal("an untagged request must not claim to be a compaction turn")
	}
	if !isCompactionTurn(withCompactionTurn(plain)) {
		t.Fatal("withCompactionTurn did not tag the request")
	}
}

// An empty summary would install an empty context and lose the conversation, so
// the turn must fail rather than emit an empty compaction item.
func TestCompactionSummaryRejectsEmptyCompletion(t *testing.T) {
	cases := map[string]map[string]any{
		"no choices":   {},
		"empty string": {"choices": []any{map[string]any{"message": map[string]any{"content": ""}}}},
		"whitespace":   {"choices": []any{map[string]any{"message": map[string]any{"content": "  \n "}}}},
	}
	for name, src := range cases {
		if summary, ok := compactionSummary(src); ok {
			t.Fatalf("%s: accepted %q", name, summary)
		}
	}
	summary, ok := compactionSummary(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "real summary"}}},
	})
	if !ok || summary != "real summary" {
		t.Fatalf("summary=%q ok=%v", summary, ok)
	}
}
