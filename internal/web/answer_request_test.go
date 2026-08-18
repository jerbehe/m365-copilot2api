package web

import (
	"encoding/json"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func answerRequestTestBody() oaiReq {
	return oaiReq{
		ConversationID: "conversation-1",
		SessionID:      "session-1",
		Tools: []chathub.Tool{{
			Type:     "function",
			Function: json.RawMessage(`{"name":"read_file","parameters":{"type":"object"}}`),
		}},
		ToolChoice: "auto",
	}
}

func TestBuildAnswerRequestRouterOmitsNativePlugins(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "router")
	if len(req.Tools) != 0 || req.ToolChoice != nil {
		t.Fatalf("router answer leaked native tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
	if req.Text != "[user]\nhello" {
		t.Fatalf("empty ledger changed answer prompt: %q", req.Text)
	}
}

func TestBuildAnswerRequestNativeForwardsTools(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "native")
	if len(req.Tools) != 1 || req.ToolChoice != "auto" {
		t.Fatalf("native answer lost tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
}

func TestBuildAnswerRequestDoesNotInjectRouterEvidence(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{ID: "call_1", Name: "read_file", Arguments: `{}`, Result: "ok"}}}
	req := buildAnswerRequest("[user]\nsummarize", "magic", answerRequestTestBody(), ledger, "router")
	if req.Text != "[user]\nsummarize" {
		t.Fatalf("router evidence contaminated final answer prompt: %q", req.Text)
	}
}
