package web

import (
	"encoding/json"
	"regexp"
	"strings"
)

// fencedToolCall matches a fenced block whose info string names a tool. The
// closing fence is optional and may follow the body without a newline: upstream
// truncates long answers mid-block, and a strict pattern used to leave the whole
// block unparsed, which then leaked raw tool syntax into the assistant text.
var fencedToolCall = regexp.MustCompile("(?s)```([A-Za-z0-9_-]+)[ \t]*\r?\n(.*?)(?:\r?\n?```|$)")

// shellFenceNames are the info strings that denote a shell command block rather
// than a tool named after itself.
var shellFenceNames = []string{"bash", "sh", "shell", "powershell", "cmd"}

func isShellFenceName(name string) bool {
	for _, n := range shellFenceNames {
		if name == n {
			return true
		}
	}
	return false
}

// declaredShell returns the shell-ish tool name the client actually
// declared (bash/sh/shell/powershell/cmd), or "" if none. Forcing an
// undeclared bash call on clients that don't support it (issue #12) makes
// them error out and loop, so conversion only happens for declared tools.
func declaredShell(allowed map[string]bool) string {
	for _, n := range shellFenceNames {
		if allowed[n] {
			return n
		}
	}
	return ""
}

// isCustomTool reports whether a declared tool uses the grammar-constrained
// custom shape (Codex `exec`), whose body is a raw string rather than a JSON
// argument object.
func isCustomTool(name string, tools []map[string]any) bool {
	return toolType(name, tools) == "custom"
}

// looksLikeJSONObject reports whether a fence body was meant to be a JSON
// argument object. A body that opens with `{` but fails to parse is a truncated
// or garbled call: it must never be turned into a fabricated argument value.
func looksLikeJSONObject(body string) bool {
	return strings.HasPrefix(body, "{")
}

func fencedToolCalls(text string, tools []map[string]any, choice any) []detectedToolCall {
	allowed := allowedToolNames(tools)
	shell := declaredShell(allowed)
	var out []detectedToolCall
	// A patch envelope is not fenced: the grammar emits it as bare text, so it has
	// to be claimed before the fence scan or it reaches the client as prose.
	if allowed[applyPatchToolName] && toolChoiceAllows(choice, applyPatchToolName) {
		if patch, _, _, ok := applyPatchEnvelope(text); ok {
			out = append(out, applyPatchCall(patch, len(out)))
		}
	}
	for _, m := range fencedToolCall.FindAllStringSubmatch(text, -1) {
		name := m[1]
		args := strings.TrimSpace(m[2])
		var v any
		parsed := json.Unmarshal([]byte(args), &v) == nil
		// Auto-convert bash/shell code blocks to tool calls, but only when
		// the client declared the tool.
		if isShellFenceName(name) {
			converted := name
			if !allowed[name] {
				if shell == "" {
					continue
				}
				converted = shell
			}
			if m, ok := v.(map[string]any); ok {
				if cmd, hasCmd := m["command"]; hasCmd && cmd != "" {
					cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": m["timeout"], "workdir": m["workdir"]})
					out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
					continue
				}
			}
			if !parsed && args != "" && !looksLikeJSONObject(args) {
				cmdBytes, _ := json.Marshal(map[string]any{"command": args})
				out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
				continue
			}
			continue
		}
		if !allowed[name] || !toolChoiceAllows(choice, name) || args == "" {
			continue
		}
		// Grammar-constrained custom tools carry a raw script. Accept both the
		// bridged {"input":"..."} object and a bare body.
		if isCustomTool(name, tools) {
			if obj, ok := v.(map[string]any); ok {
				if in, hasInput := obj["input"].(string); hasInput && in != "" {
					b, _ := json.Marshal(map[string]any{"input": in})
					out = append(out, detectedToolCall{ID: callID(name, string(b), len(out)), Type: "custom", Name: name, Arguments: b})
				}
				continue
			}
			if looksLikeJSONObject(args) {
				// Truncated or garbled JSON: emitting it as a raw script would
				// run broken code, so drop the call and let the caller retry.
				continue
			}
			b, _ := json.Marshal(map[string]any{"input": args})
			out = append(out, detectedToolCall{ID: callID(name, string(b), len(out)), Type: "custom", Name: name, Arguments: b})
			continue
		}
		if !parsed || v == nil {
			continue
		}
		b, _ := json.Marshal(v)
		out = append(out, detectedToolCall{ID: callID(name, string(b), len(out)), Type: toolType(name, tools), Name: name, Arguments: b})
	}
	// Also check for plain JSON objects with a "command" field (not in fenced blocks)
	if len(out) == 0 && shell != "" {
		for i := 0; i < len(text); i++ {
			if text[i] != '{' {
				continue
			}
			end := strings.Index(text[i:], "\n")
			if end < 0 {
				end = len(text) - i
			}
			line := text[i : i+end]
			braceEnd := strings.LastIndex(line, "}")
			if braceEnd < 0 {
				continue
			}
			if !strings.Contains(line[:braceEnd+1], `"command"`) {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line[:braceEnd+1]), &obj) != nil {
				continue
			}
			if cmd, hasCmd := obj["command"]; hasCmd && cmd != "" {
				cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": obj["timeout"], "workdir": obj["workdir"]})
				out = append(out, detectedToolCall{ID: callID(shell, string(cmdBytes), len(out)), Type: "function", Name: shell, Arguments: cmdBytes})
				break
			}
		}
	}
	return out
}

// withheldToolFenceNotice replaces prose that consisted solely of unusable tool
// syntax. An empty assistant message would be rejected downstream as an empty
// upstream response, so state plainly that no call could be issued.
const withheldToolFenceNotice = "上游返回的是无法执行的工具调用语法，没有产生有效的工具调用。请重试当前请求。"

// stripToolFences removes fenced blocks that name a declared tool from text that
// is about to be forwarded as assistant prose, reporting whether anything was
// withheld. Tool syntax must never reach the client as an ordinary message: a
// leaked ```exec block ends the turn without a tool call, so the client shows a
// wall of script and then stalls.
func stripToolFences(text string, tools []map[string]any) (string, bool) {
	allowed := allowedToolNames(tools)
	shell := declaredShell(allowed)
	withheld := false
	// The patch envelope is bare text, so it is removed by span rather than by
	// the fence pattern.
	if allowed[applyPatchToolName] {
		if _, start, end, ok := applyPatchEnvelope(text); ok {
			text = text[:start] + text[end:]
			withheld = true
		}
	}
	// An unfenced grammar body occupies the whole answer, so claiming it leaves
	// nothing to forward. Withholding it here is what keeps the escaped source out
	// of the assistant message.
	if !withheld {
		if _, ok := grammarBodyCall(text, tools, nil); ok {
			return "", true
		}
	}
	var b strings.Builder
	last := 0
	for _, loc := range fencedToolCall.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]]
		if !allowed[name] && !(isShellFenceName(name) && shell != "") {
			continue
		}
		b.WriteString(text[last:loc[0]])
		last = loc[1]
		withheld = true
	}
	if last == 0 {
		return text, withheld
	}
	b.WriteString(text[last:])
	return b.String(), true
}

// withholdToolFences strips tool syntax from an assistant answer, substituting a
// notice when nothing usable remains.
func withholdToolFences(text string, tools []map[string]any) (string, bool) {
	stripped, withheld := stripToolFences(text, tools)
	if !withheld {
		return text, false
	}
	if strings.TrimSpace(stripped) == "" {
		return withheldToolFenceNotice, true
	}
	return stripped, true
}

// toolPreamble returns the prose that accompanied a tool call, with the call's
// own syntax removed. A turn that both narrates and calls a tool must deliver
// both: dropping the narration loses the model's stated intent, and clients show
// nothing but a bare call.
func toolPreamble(text string, tools []map[string]any) string {
	stripped, _ := stripToolFences(text, tools)
	return strings.TrimSpace(stripped)
}
