package web

import "testing"

func TestReasoningFieldsExposeAgentCompatibility(t *testing.T) {
	fields := reasoningFields("analysis")
	if fields["reasoning_content"] != "analysis" || fields["agent_reasoning"] != "analysis" {
		t.Fatalf("missing compatible reasoning fields: %#v", fields)
	}
}

func TestReasoningTextAcceptsSupportedAliases(t *testing.T) {
	for _, key := range []string{"reasoning_content", "agent_reasoning", "reasoning"} {
		if got := reasoningText(map[string]any{key: "analysis"}); got != "analysis" {
			t.Fatalf("key %s was not recognized: %q", key, got)
		}
	}
}
