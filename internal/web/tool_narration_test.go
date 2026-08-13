package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func declaredTools(names ...string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": n, "parameters": map[string]any{"type": "object"}}})
	}
	return out
}

// A statement about which tool to use is a protocol artifact, not an answer: no
// call follows it, so the client shows an announcement and stops.
func TestToolIntentNarrationIsRecognized(t *testing.T) {
	tools := declaredTools("shell_command", "read_file")
	narration := []string{
		"I am choosing the shell command tool to inspect relevant files and potentially implement login tests.",
		"选择shell_command工具，可能需要检查特定文件以实现热身服务。",
		"I will use shell_command to check the repository.",
		"CALL_TOOL shell_command",
		"Selecting the read_file tool to inspect the config.",
		"将使用 read_file 工具查看配置。",
	}
	for _, text := range narration {
		if !isToolIntentNarration(text, tools) {
			t.Errorf("not recognized as narration: %q", text)
		}
	}
}

// A genuine answer must survive, even when it mentions a tool.
func TestToolIntentNarrationSparesRealAnswers(t *testing.T) {
	tools := declaredTools("shell_command", "read_file")
	answers := []string{
		"The hostname is example.local.",
		"read_file returned 42 lines; the config sets timeout=30.",
		"Here is the script:\n```python\nprint(1)\n```",
		"*** Begin Patch\n*** Update File: a.go\n+x\n*** End Patch",
		// Long-form explanation the user actually asked for.
		strings.Repeat("The shell_command tool works by spawning a subprocess and capturing its output. ", 8),
		// No tool named: ordinary prose about choosing things.
		"I am choosing the simpler approach for now.",
	}
	for _, text := range answers {
		if isToolIntentNarration(text, tools) {
			t.Errorf("real answer suppressed: %q", text)
		}
	}
	// With no tools declared, nothing is narration.
	if isToolIntentNarration("I will use shell_command", nil) {
		t.Error("narration detected with no declared tools")
	}
}

func TestStripToolProtocolMarkers(t *testing.T) {
	cases := map[string]string{
		"CALL_TOOL: read_file":  "read_file",
		"CALL_TOOL read_file":   "read_file",
		"NO_TOOL_NEEDED":        "",
		"NO_TOOL_NEEDED\nHello": "Hello",
		"ordinary answer":       "ordinary answer",
	}
	for in, want := range cases {
		if got := stripToolProtocolMarkers(in); got != want {
			t.Errorf("strip(%q)=%q want %q", in, got, want)
		}
	}
}

// A bare marker naming a tool is a decision the model failed to complete. It
// must count as parsed-with-no-call so the caller runs the answer turn, rather
// than unparsed, which would forward the fragment as prose.
func TestBareCallToolMarkerIsNotAnAnswer(t *testing.T) {
	tools := declaredTools("shell_command")
	calls, parsed := parseModelToolDecision("CALL_TOOL shell_command", tools, "auto")
	if !parsed || len(calls) != 0 {
		t.Fatalf("parsed=%v calls=%d", parsed, len(calls))
	}
	// The complete envelope still yields a call.
	calls, parsed = parseModelToolDecision(`CALL_TOOL: shell_command({"command":"ls"})`, tools, "auto")
	if !parsed || len(calls) != 1 {
		t.Fatalf("complete envelope: parsed=%v calls=%d", parsed, len(calls))
	}
}

// The narration gate decides before the first delta goes out, because streaming
// cannot recall what it already sent.
func TestNarrationGateHoldsThenReleasesRealContent(t *testing.T) {
	tools := declaredTools("shell_command")
	g := newNarrationGate(tools)
	if out := g.Feed("I will use shell_command"); out != "" {
		t.Fatalf("narration leaked: %q", out)
	}
	// Real content arrives: everything held is released at once, in order.
	out := g.Feed(" to run this:\n```python\nprint(1)\n```")
	if !strings.Contains(out, "I will use shell_command") || !strings.Contains(out, "print(1)") {
		t.Fatalf("release lost content: %q", out)
	}
	// Once open the gate is transparent.
	if got := g.Feed("more"); got != "more" {
		t.Fatalf("gate still holding: %q", got)
	}
	held, narrated := g.Close()
	if held != "" || narrated {
		t.Fatalf("held=%q narrated=%v after release", held, narrated)
	}
}

func TestNarrationGateReportsPureNarrationOnClose(t *testing.T) {
	g := newNarrationGate(declaredTools("shell_command"))
	if out := g.Feed("选择shell_command工具，可能需要检查特定文件。"); out != "" {
		t.Fatalf("narration leaked: %q", out)
	}
	held, narrated := g.Close()
	if !narrated || held == "" {
		t.Fatalf("held=%q narrated=%v", held, narrated)
	}
}

// Requests without tools must stream byte-for-byte as before.
func TestNarrationGateIsInertWithoutTools(t *testing.T) {
	g := newNarrationGate(nil)
	for _, part := range []string{"I will use shell_command", " and stop"} {
		if got := g.Feed(part); got != part {
			t.Fatalf("gate altered a tool-less stream: %q -> %q", part, got)
		}
	}
	if held, narrated := g.Close(); held != "" || narrated {
		t.Fatalf("held=%q narrated=%v", held, narrated)
	}
}

// apply_patch is grammar-constrained: the model emits a bare patch envelope, and
// dropping the tool left it narrating the patch as prose instead.
func TestApplyPatchToolIsForwardedToUpstream(t *testing.T) {
	r := responsesRequest{Model: "gpt-5.6-sol", Input: "edit a file", Tools: []map[string]any{
		{"type": "custom", "name": "apply_patch", "description": "Use apply_patch to edit files.",
			"format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: begin_patch hunk+ end_patch"}},
		{"type": "function", "name": "shell_command", "parameters": map[string]any{"type": "object"}},
	}}
	o, _, err := r.openAIWithCompaction()
	if err != nil {
		t.Fatal(err)
	}
	var patch *string
	for _, tool := range o.Tools {
		var f map[string]any
		if err := json.Unmarshal(tool.Function, &f); err != nil {
			t.Fatal(err)
		}
		if f["name"] == applyPatchToolName {
			if tool.Type != "custom" {
				t.Fatalf("apply_patch type=%q, want custom", tool.Type)
			}
			desc, _ := f["description"].(string)
			patch = &desc
			// The body travels as one string field; ChatHub takes JSON arguments only.
			params, _ := f["parameters"].(map[string]any)
			props, _ := params["properties"].(map[string]any)
			if _, ok := props["input"]; !ok {
				t.Fatalf("apply_patch parameters=%#v, want an input string", params)
			}
		}
	}
	if patch == nil {
		t.Fatalf("apply_patch was dropped; forwarded %d tools", len(o.Tools))
	}
	// The grammar has to travel in the description: format.definition is not
	// visible once the tool is bridged, and without it the model invents a wrapper.
	if !strings.Contains(*patch, "begin_patch") {
		t.Fatalf("grammar missing from description: %q", *patch)
	}
}

func TestApplyPatchEnvelopeIsClaimedAsACall(t *testing.T) {
	tools := []map[string]any{{"type": "custom", "function": map[string]any{"name": applyPatchToolName}}}
	body := "*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"
	text := "I will fix it.\n" + body + "\nDone."
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 || calls[0].Name != applyPatchToolName || calls[0].Type != "custom" {
		t.Fatalf("calls=%#v", calls)
	}
	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["input"] != body {
		t.Fatalf("patch body=%q want %q", args["input"], body)
	}
	// The envelope must not also reach the client as prose.
	stripped, withheld := stripToolFences(text, tools)
	if !withheld || strings.Contains(stripped, "Begin Patch") {
		t.Fatalf("stripped=%q withheld=%v", stripped, withheld)
	}
	if !strings.Contains(stripped, "I will fix it.") || !strings.Contains(stripped, "Done.") {
		t.Fatalf("prose around the patch was lost: %q", stripped)
	}
}

// The heredoc wrapper is what the client rendered half-parsed. It belongs to the
// call, not to the prose.
func TestApplyPatchHeredocWrapperIsAbsorbed(t *testing.T) {
	tools := []map[string]any{{"type": "custom", "function": map[string]any{"name": applyPatchToolName}}}
	text := "apply_patch <<'PATCH'\n*** Begin Patch\n*** Update File: a.go\n+x\n*** End Patch\nPATCH\n"
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%#v", calls)
	}
	stripped, withheld := stripToolFences(text, tools)
	if !withheld {
		t.Fatal("heredoc not withheld")
	}
	for _, leak := range []string{"apply_patch <<", "PATCH", "Begin Patch"} {
		if strings.Contains(stripped, leak) {
			t.Fatalf("%q leaked into prose: %q", leak, stripped)
		}
	}
}

// Upstream truncates long answers. A patch that lost its end marker is still the
// hunks written so far, and must not fall through as raw text.
func TestApplyPatchTruncatedEnvelopeStillParses(t *testing.T) {
	text := "*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new"
	patch, _, _, ok := applyPatchEnvelope(text)
	if !ok || !strings.Contains(patch, "+new") {
		t.Fatalf("patch=%q ok=%v", patch, ok)
	}
	if _, _, _, ok := applyPatchEnvelope("no patch here"); ok {
		t.Fatal("plain text reported as a patch")
	}
}
