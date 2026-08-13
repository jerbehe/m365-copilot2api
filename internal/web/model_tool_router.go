package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

func modelToolRouterPrompt(prompt string, tools []map[string]any, choice any) string {
	defs, _ := json.Marshal(tools)
	mode := normalizedToolChoiceMode(choice)
	rules := `- If a tool is needed, respond with: CALL_TOOL: tool_name({"arg1":"value1"})
- If no tool is needed, respond with: NO_TOOL_NEEDED
- Only use tools from the available list above
- Validate all arguments against the tool's schema
- Do not invent tools that are not in the list
- Respond with the marker alone. Do not explain, announce, or restate which tool
  you are choosing: the answer is consumed by a program, not read by a person,
  and a sentence like "I will use the shell tool to check the files" is discarded
  as neither a call nor an answer
- CALL_TOOL requires the parenthesised argument object. The tool name on its own
  is not a call`
	// Multi-turn: completed tool evidence (tool[...], tool_calls:) was already
	// acted upon, so re-invoking those tools would duplicate work.
	if strings.Contains(prompt, "tool_calls:") || strings.Contains(prompt, "tool[call_") {
		rules += `
- Completed evidence must not be repeated: tool_calls/tool[call_x] rows are prior results already delivered to the user, never re-invoke them
- Only start a new tool call when fresh unfinished work remains on the current request`
	}
	return fmt.Sprintf(`You are a tool selection assistant. Based on the user request, decide which tool to call next.

Available tools: %s

MODE: %s

Rules:
%s

User request and evidence:
%s`, defs, mode, rules, prompt)
}

// parseNamedToolWithoutArguments reports whether text is a bare CALL_TOOL marker
// naming a declared tool with no argument object. Such a fragment cannot become a
// call, and it must not become prose either.
func parseNamedToolWithoutArguments(text string, tools []map[string]any, choice any) bool {
	upper := strings.ToUpper(text)
	if !strings.Contains(upper, "CALL_TOOL") {
		return false
	}
	if strings.Contains(text, "(") {
		// An argument list is present; a failure to parse it belongs to the
		// envelope path above, which has already run.
		return false
	}
	for name := range allowedToolNames(tools) {
		if name != "" && strings.Contains(text, name) && toolChoiceAllows(choice, name) {
			return true
		}
	}
	return false
}

func parseModelToolDecision(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	text = strings.TrimSpace(text)
	// Try the new natural language format first: CALL_TOOL: name({...})
	if strings.HasPrefix(text, "CALL_TOOL:") || strings.HasPrefix(text, "call_tool:") {
		parts := strings.SplitN(text, ":", 2)
		if len(parts) == 2 {
			rest := strings.TrimSpace(parts[1])
			start := strings.Index(rest, "(")
			end := strings.LastIndex(rest, ")")
			if start > 0 && end > start {
				name := strings.TrimSpace(rest[:start])
				argsStr := rest[start+1 : end]
				var args map[string]any
				if json.Unmarshal([]byte(argsStr), &args) == nil && toolChoiceAllows(choice, name) {
					fn := toolFunction(name, tools)
					if fn != nil {
						b, _ := json.Marshal(args)
						return []detectedToolCall{{ID: callID(name, string(b), 0), Type: toolType(name, tools), Name: name, Arguments: b}}, true
					}
				}
			}
		}
	}
	if strings.Contains(text, "NO_TOOL_NEEDED") || strings.Contains(text, "no_tool_needed") {
		return nil, true
	}
	// "CALL_TOOL shell_command" — the marker and a tool name, but no argument
	// object. It is a decision the model failed to complete, not an answer, so
	// report it as parsed with no call: the caller then falls through to the
	// answer turn instead of forwarding the fragment as prose.
	if calls := parseNamedToolWithoutArguments(text, tools, choice); calls {
		return nil, true
	}
	// The model sometimes answers with the client's own fenced call syntax
	// (```exec { ... }```) instead of the requested envelope. Recover those calls
	// here: otherwise the block falls through and is forwarded as assistant
	// prose, which ends the turn without any tool call.
	if calls := fencedToolCalls(text, tools, choice); len(calls) > 0 {
		return calls, true
	}
	// Fallback: try the old JSON format
	if i := strings.Index(text, "```"); i >= 0 {
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(text[i+3:], "```"), "json"))
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	// Calls is a pointer so a missing "calls" key is distinguishable from an
	// explicitly empty list. encoding/json ignores unknown fields, so any JSON
	// object — a bare argument payload, an upstream error blob — used to decode
	// cleanly into an empty envelope and be reported as a valid "no tool needed"
	// decision, silently dropping the real call.
	var envelope struct {
		Calls *[]struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"calls"`
	}
	if json.Unmarshal([]byte(text[start:end+1]), &envelope) != nil || envelope.Calls == nil {
		return nil, false
	}
	out := make([]detectedToolCall, 0, len(*envelope.Calls))
	for i, c := range *envelope.Calls {
		fn := toolFunction(c.Name, tools)
		if fn == nil || c.Arguments == nil || !toolChoiceAllows(choice, c.Name) || schemaValid(c.Arguments, fn) != nil {
			continue
		}
		b, _ := json.Marshal(c.Arguments)
		out = append(out, detectedToolCall{ID: callID(c.Name, string(b), i), Type: toolType(c.Name, tools), Name: c.Name, Arguments: b})
	}
	return out, true
}
