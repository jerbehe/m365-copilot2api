package chathub

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolProtocolPrompt follows the community-compatible M365 convention:
// definitions are wrapped in <tools>, and calls are emitted as a fenced block
// whose info string is the exact tool name.
func toolProtocolPrompt(text string, tools []Tool, choice any, hasPlugins bool) string {
	if len(tools) == 0 || strings.EqualFold(fmt.Sprint(choice), "none") {
		if hasPlugins {
			return text
		}
		return fmt.Sprintf("Please answer the following request in full. Do not truncate or abbreviate your response.\n\n%s", text)
	}
	if hasPlugins {
		return fmt.Sprintf("[system] You are operating on the caller's Windows machine. The caller has provided real tools (bash, read, edit, write, glob, grep, etc.) that run locally on Windows PowerShell 5.1. These tools are the ONLY way to execute commands, run code, read files, or interact with the filesystem. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT emit ```python or ```code blocks for execution — if you need to run code, use the bash tool. Do NOT mention /mnt/data, Linux containers, cloud sandboxes, or claim the execution environment has changed. Do NOT claim any tool is unavailable. Do NOT output environment diagnostics instead of tool calls. When you decide to use a tool, call it immediately.\n\n%s", text)
	}
	var defs []string
	for _, t := range tools {
		if strings.EqualFold(t.Type, "web_search") {
			defs = append(defs, webSearchDecl)
			continue
		}
		var f struct {
			Name, Description string
			Parameters        json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		if strings.EqualFold(f.Name, "web_search") {
			defs = append(defs, webSearchDecl)
			continue
		}
		params := strings.TrimSpace(string(f.Parameters))
		if params == "" || params == "null" {
			params = "{}"
		}
		defs = append(defs, fmt.Sprintf("%s — %s\n```%s\n%s\n```", f.Name, f.Description, f.Name, params))
	}
	if len(defs) == 0 {
		return text
	}
	return fmt.Sprintf("You are an execution agent on the caller's Windows machine. The tools below are real, active, and callable right now. The bash tool runs Windows PowerShell 5.1; Windows paths like D:\\ are directly accessible. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT emit backtick-backtick-backtick-python or backtick-backtick-backtick-code blocks for execution — if you need to run code, use the bash tool. Do NOT mention Linux containers, /mnt/data, cloud sandboxes, or claim the execution environment has changed.\nWhen the user's request requires a tool, call it by emitting one or more fenced blocks. Each block's info string is the exact tool name and its body is a JSON object of arguments. For independent operations, emit multiple blocks in one response. Do not analyze whether tools are registered or available — they are. Do not say a tool is unavailable. Do not wrap the call in XML or Markdown prose. Wait for the tool result before claiming completion.\n\n<tools>\n%s\n</tools>\n\nUser request:\n%s", strings.Join(defs, "\n\n"), text)
}

// webSearchDecl lets the upstream model know web_search exists and how to
// call it, even when the client sent no JSON schema for it.
const webSearchDecl = "web_search \u2014 Search the web for current, up-to-date information.\n```web_search\n{\"query\": \"<search text>\"}\n```"
