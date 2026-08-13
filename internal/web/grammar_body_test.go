package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func execTool() []map[string]any {
	return []map[string]any{{"type": "custom", "function": map[string]any{"name": "exec"}}}
}

// Under code mode the whole answer is an `exec` program. Upstream emits it
// unfenced — sometimes wrapped in the bridge envelope, sometimes bare — and
// neither shape was claimed, so escaped source reached the client as prose.
func TestUnfencedGrammarBodyBecomesACall(t *testing.T) {
	tools := execTool()
	cases := map[string]string{
		"bridge envelope": `{"input":"shell({cmd:\"cat README.md\",workdir:\".\",timeout_ms:10000}); text(plan);"}`,
		"bare program":    "shell({cmd:\"cat README.md\",workdir:\".\",timeout_ms:10000}); text(plan); text(changed);",
		"multi-line":      "const out = await shell({cmd:\"ls\"});\ntext(out);",
	}
	for name, text := range cases {
		calls := answerToolCalls(text, tools, "auto")
		if len(calls) != 1 {
			t.Fatalf("%s: calls=%#v", name, calls)
		}
		if calls[0].Name != "exec" || calls[0].Type != "custom" {
			t.Fatalf("%s: call=%#v", name, calls[0])
		}
		var args map[string]string
		if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(args["input"], "shell(") {
			t.Fatalf("%s: body lost: %q", name, args["input"])
		}
		// The envelope must not be double-wrapped: the bridge field holds the
		// program, not the JSON that carried it.
		if strings.HasPrefix(strings.TrimSpace(args["input"]), `{"input"`) {
			t.Fatalf("%s: envelope wrapped twice: %q", name, args["input"])
		}
		// Claiming the call is only half the fix; the source must also stop being
		// forwarded as prose.
		stripped, withheld := stripToolFences(text, tools)
		if !withheld || strings.TrimSpace(stripped) != "" {
			t.Fatalf("%s: stripped=%q withheld=%v", name, stripped, withheld)
		}
	}
}

// A real answer must survive, including one that quotes code.
func TestUnfencedGrammarBodySparesProse(t *testing.T) {
	tools := execTool()
	answers := []string{
		"The README documents four commands.",
		"Run `bun install` first, then `bun run build`.",
		"Here is the call:\n```exec\n{\"input\":\"shell({cmd:\\\"ls\\\"});\"}\n```\nThat lists the files.",
		// Prose that mentions a host function but is not a program.
		"The shell(...) helper takes a cmd field and returns its output.",
		// JSON that is not the bridge envelope.
		`{"error":"404 Not Found"}`,
		`{"input":"ls","extra":1}`,
	}
	for _, text := range answers {
		if call, ok := grammarBodyCall(text, tools, "auto"); ok {
			t.Errorf("prose claimed as a call: %q -> %#v", text, call)
		}
	}
	// With no grammar tool declared the check is inert.
	fnTools := []map[string]any{{"type": "function", "function": map[string]any{"name": "shell_command"}}}
	if _, ok := grammarBodyCall(`{"input":"shell({cmd:\"ls\"});"}`, fnTools, "auto"); ok {
		t.Error("claimed a body with no grammar tool declared")
	}
	if _, ok := grammarBodyCall(`{"input":"shell({cmd:\"ls\"});"}`, nil, "auto"); ok {
		t.Error("claimed a body with no tools at all")
	}
}

// The fenced form still wins: it names its tool explicitly.
func TestFencedFormStillPreferred(t *testing.T) {
	tools := execTool()
	text := "```exec\n{\"input\":\"shell({cmd:\\\"ls\\\"});\"}\n```"
	calls := answerToolCalls(text, tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%#v", calls)
	}
	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["input"] != `shell({cmd:"ls"});` {
		t.Fatalf("fenced body=%q", args["input"])
	}
}

// tool_choice must be honoured: a body for a forbidden tool is not a call.
func TestGrammarBodyRespectsToolChoice(t *testing.T) {
	tools := execTool()
	body := `{"input":"shell({cmd:\"ls\"});"}`
	if _, ok := grammarBodyCall(body, tools, map[string]any{"type": "function", "function": map[string]any{"name": "other"}}); ok {
		t.Error("claimed a call the tool_choice forbids")
	}
	if _, ok := grammarBodyCall(body, tools, "auto"); !ok {
		t.Error("auto choice rejected a valid body")
	}
}

// The router has its own envelope contract: a bare {"input":...} there is a
// malformed decision that must reach the repair pass, not become a call.
func TestRouterDoesNotClaimBridgeEnvelope(t *testing.T) {
	calls, parsed := parseModelToolDecision(`{"input":"ls -la"}`, codexExecTools(), "auto")
	if parsed {
		t.Fatalf("router accepted a bare bridge envelope: calls=%d", len(calls))
	}
}
