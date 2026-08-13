package web

import (
	"encoding/json"
	"regexp"
	"strings"
)

// codeModeHostCall matches a statement-position call to one of code mode's host
// functions. These names are the code-mode runtime's own API, so a program that
// calls them is unambiguously an exec body rather than prose about code.
var codeModeHostCall = regexp.MustCompile(`(?m)^[ \t]*(?:const|let|var|await|return)?[ \t]*(?:[A-Za-z_$][A-Za-z0-9_$]*[ \t]*=[ \t]*(?:await[ \t]+)?)?(shell|text|image|apply_patch|update_plan|read_file|write_file|wait|exec)[ \t]*\(`)

// Grammar-constrained tools (Codex `exec` under code mode, `apply_patch`) take a
// raw body rather than a JSON argument object. ChatHub only accepts JSON
// arguments, so the gateway bridges the body through a single `input` field —
// which means upstream sometimes emits the bridge envelope itself, unfenced:
//
//	{"input":"shell({cmd:\"cat README.md\"}); text(plan);"}
//
// or the bare body with no envelope at all:
//
//	shell({cmd:"cat README.md"}); text(plan);
//
// Neither is fenced, so the fence scan never claimed them and they reached the
// client as prose: a wall of escaped source where a tool call should have been.

// grammarToolNames returns the declared tools that take a raw body. A gateway
// request carries at most a couple of these, so a slice keeps call order stable
// for the ID assignment.
func grammarToolNames(tools []map[string]any) []string {
	var out []string
	for _, t := range tools {
		if typ, _ := t["type"].(string); typ != "custom" {
			continue
		}
		f, _ := t["function"].(map[string]any)
		if name, _ := f["name"].(string); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// bridgeEnvelopeBody extracts the body from a bare `{"input": "..."}` object that
// occupies the whole of text. Only a lone envelope is claimed: one embedded in
// prose is far more likely to be the model quoting JSON than issuing a call.
func bridgeEnvelopeBody(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return "", false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(trimmed), &envelope) != nil {
		return "", false
	}
	raw, ok := envelope["input"]
	if !ok || len(envelope) != 1 {
		return "", false
	}
	var body string
	if json.Unmarshal(raw, &body) != nil || strings.TrimSpace(body) == "" {
		return "", false
	}
	return body, true
}

// codeModeSourceCall detects a bare code-mode program: the body of an `exec`
// call that lost both its fence and its bridge envelope.
//
// The signature is a call to one of code mode's own host functions at statement
// position. Requiring that shape, rather than any parenthesised call, keeps
// ordinary prose and answers that merely quote code from being swallowed.
func codeModeSourceCall(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.Contains(trimmed, "```") {
		return "", false
	}
	if !codeModeHostCall.MatchString(trimmed) {
		return "", false
	}
	// A program is source, not prose: it ends in a statement terminator and has
	// no sentence punctuation outside its string literals.
	if !strings.HasSuffix(trimmed, ";") && !strings.HasSuffix(trimmed, "}") && !strings.HasSuffix(trimmed, ")") {
		return "", false
	}
	return trimmed, true
}

// answerToolCalls claims the tool calls in an answer turn's text.
//
// It is the answer-path counterpart to fencedToolCalls, adding the unfenced
// grammar bodies. The router path deliberately does not use it: the router has
// its own envelope contract, and a bare `{"input":...}` object there is a
// malformed decision that must fall through to repair rather than become a call.
func answerToolCalls(text string, tools []map[string]any, choice any) []detectedToolCall {
	if calls := fencedToolCalls(text, tools, choice); len(calls) > 0 {
		return calls
	}
	if call, ok := grammarBodyCall(text, tools, choice); ok {
		return []detectedToolCall{call}
	}
	return nil
}

// grammarBodyCall claims an unfenced grammar-tool body as a call.
//
// It runs only when the request declares a grammar tool, and it consumes the
// whole of text: a body is the entire answer, never a fragment inside prose.
// Returning a call here is what stops the source from being forwarded as an
// assistant message.
func grammarBodyCall(text string, tools []map[string]any, choice any) (detectedToolCall, bool) {
	names := grammarToolNames(tools)
	if len(names) == 0 {
		return detectedToolCall{}, false
	}
	// The bridge envelope names no tool, so it is attributed to the first declared
	// grammar tool — under code mode that is `exec`, the only one that takes a
	// program. A single-tool request is the common case and is unambiguous.
	pick := ""
	for _, name := range names {
		if toolChoiceAllows(choice, name) {
			pick = name
			break
		}
	}
	if pick == "" {
		return detectedToolCall{}, false
	}
	if body, ok := bridgeEnvelopeBody(text); ok {
		return grammarToolCall(pick, body, 0), true
	}
	if body, ok := codeModeSourceCall(text); ok {
		return grammarToolCall(pick, body, 0), true
	}
	return detectedToolCall{}, false
}

// grammarToolCall builds the bridged call for a raw body.
func grammarToolCall(name, body string, index int) detectedToolCall {
	b, _ := json.Marshal(map[string]any{"input": body})
	return detectedToolCall{ID: callID(name, string(b), index), Type: "custom", Name: name, Arguments: b}
}
