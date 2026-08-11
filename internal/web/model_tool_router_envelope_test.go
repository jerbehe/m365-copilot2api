package web

import "testing"

// The router asks for a JSON envelope, but encoding/json ignores unknown fields.
// Any JSON object therefore used to decode into an empty envelope and be reported
// as a valid "no tool needed" decision, silently dropping the real call.
func TestRouterRejectsJSONWithoutCallsKey(t *testing.T) {
	cases := []string{
		`{"input":"ls -la"}`,
		`抱歉，接口返回 {"error":"404 Not Found"}`,
		`{"name":"exec","arguments":{"input":"ls"}}`,
	}
	for _, in := range cases {
		calls, parsed := parseModelToolDecision(in, codexExecTools(), "auto")
		if parsed {
			t.Errorf("input %q reported a valid decision (calls=%d); a missing calls key must fall through to repair", in, len(calls))
		}
	}
}

func TestRouterAcceptsExplicitlyEmptyCalls(t *testing.T) {
	calls, parsed := parseModelToolDecision(`{"calls":[]}`, codexExecTools(), "auto")
	if !parsed || len(calls) != 0 {
		t.Fatalf("explicit empty envelope must parse as a no-tool decision, got parsed=%t calls=%d", parsed, len(calls))
	}
}

// The model sometimes answers the router with the client's own fenced syntax
// instead of the envelope. Those calls must be recovered, not forwarded as prose.
func TestRouterRecoversFencedExecCall(t *testing.T) {
	cases := map[string]string{
		"json body":      "我来检测工具。\n```exec\n{\"input\":\"ls -la\"}\n```\n",
		"bare script":    "```exec\nls -la\n```",
		"unclosed fence": "```exec\n{\"input\":\"ls -la\"}",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			calls, parsed := parseModelToolDecision(in, codexExecTools(), "auto")
			if !parsed || len(calls) != 1 {
				t.Fatalf("parsed=%t calls=%d, want one recovered call", parsed, len(calls))
			}
			if calls[0].Name != "exec" || string(calls[0].Arguments) != `{"input":"ls -la"}` {
				t.Fatalf("unexpected call: %+v", calls[0])
			}
		})
	}
}

// A truncated fence body is not a usable call, and it is not a no-tool decision
// either: it must fall through so the caller runs its repair turn.
func TestRouterFallsThroughOnTruncatedFence(t *testing.T) {
	in := "我会继续检测。```exec\n{\"input\":\"const wanted = [\\\"a\\\"]; text(r);\"\n```"
	if calls, parsed := parseModelToolDecision(in, codexExecTools(), "auto"); parsed {
		t.Fatalf("truncated fence must not parse, got calls=%d", len(calls))
	}
}
