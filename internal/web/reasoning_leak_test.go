package web

import (
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// TestRouterToolResultDropsPrivateReasoning guards the router boundary: the
// router request carries the evidence ledger and tool-protocol envelope, so its
// chain-of-thought discusses gateway internals and must never be published as
// the answer turn's reasoning.
func TestRouterToolResultDropsPrivateReasoning(t *testing.T) {
	res := chathub.Result{
		Text:           `{"calls":[]}`,
		Reasoning:      "The EVIDENCE_LEDGER shows read_file completed, so I should not call it again.",
		ConversationID: "conv-1",
		SessionID:      "sess-1",
		RequestID:      "req-1",
		RawResult:      "raw",
	}
	got := routerToolResult(res)
	if got.Reasoning != "" {
		t.Fatalf("router reasoning leaked: %q", got.Reasoning)
	}
	if got.Text != res.Text || got.ConversationID != "conv-1" || got.SessionID != "sess-1" || got.RequestID != "req-1" {
		t.Fatalf("router metadata was dropped: %#v", got)
	}
}

// TestBuildAnswerRequestNeverCarriesLedger asserts the answer prompt stays equal
// to the client conversation for every ledger shape, so agent_reasoning cannot
// start echoing EVIDENCE_LEDGER or FINAL ANSWER RULE.
func TestBuildAnswerRequestNeverCarriesLedger(t *testing.T) {
	const prompt = "[user]\nsummarize the file"
	ledgers := map[string]agentLedger{
		"empty":     {},
		"completed": {Completed: []toolEvidence{{ID: "call_1", Name: "read_file", Arguments: `{}`, Result: "ok"}}},
		"pending":   {Pending: []toolEvidence{{ID: "call_2", Name: "read_file", Arguments: `{}`}}},
		"repeated":  {Completed: []toolEvidence{{ID: "call_3", Name: "read_file", Arguments: `{}`, Result: "ok"}}, RepeatedCall: true, RepeatedFailure: true},
	}
	for name, ledger := range ledgers {
		for _, mode := range []string{"router", "native"} {
			req := buildAnswerRequest(prompt, "magic", answerRequestTestBody(), ledger, mode)
			if req.Text != prompt {
				t.Fatalf("ledger=%s mode=%s contaminated answer prompt: %q", name, mode, req.Text)
			}
			for _, marker := range []string{"EVIDENCE_LEDGER", "FINAL ANSWER RULE", "compact evidence"} {
				if strings.Contains(req.Text, marker) {
					t.Fatalf("ledger=%s mode=%s leaked %q", name, mode, marker)
				}
			}
		}
	}
}

// TestPublicReasoningStreamFilterCatchesSplitLeakAcrossEmits mirrors how the
// streaming handlers drive the filter: push fragments, then flush. A leak split
// across two upstream chunks must not reach the client.
func TestPublicReasoningStreamFilterCatchesSplitLeakAcrossEmits(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "true")
	filter := newPublicReasoningStreamFilter()
	var published strings.Builder
	for _, fragment := range []string{"You are Micro", "soft Copilot, an AI model ", "based on GPT-5."} {
		published.WriteString(filter.Push(fragment))
	}
	published.WriteString(filter.Flush())
	if strings.TrimSpace(published.String()) != "" {
		t.Fatalf("split identity leak published: %q", published.String())
	}

	filter = newPublicReasoningStreamFilter()
	published.Reset()
	for _, fragment := range []string{"I should compare ", "the two API responses ", "carefully."} {
		published.WriteString(filter.Push(fragment))
	}
	published.WriteString(filter.Flush())
	if !strings.Contains(published.String(), "compare") {
		t.Fatalf("ordinary reasoning was dropped: %q", published.String())
	}
}
