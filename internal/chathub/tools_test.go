package chathub

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClientPluginsWebSearchBuiltIn(t *testing.T) {
	// Both the dedicated web_search type and the function form used by CLI
	// clients must enable M365's server-side search:
	// {"Id":"BingWebSearch","Source":"BuiltIn"}, so Copilot searches
	// server-side.
	byType := Tool{Type: "web_search", Function: nil}
	byFunction := Tool{Type: "function", Function: json.RawMessage(`{"name":"web_search","description":"search the web","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}`)}
	for _, tc := range []struct {
		name string
		tool Tool
	}{
		{"dedicated type", byType},
		{"CLI function", byFunction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugins := clientPlugins([]Tool{tc.tool}, "")
			if len(plugins) != 1 {
				t.Fatalf("plugins = %#v", plugins)
			}
			m, _ := plugins[0].(map[string]any)
			if m["Id"] != "BingWebSearch" || m["Source"] != "BuiltIn" {
				t.Fatalf("web_search plugin = %#v, want BingWebSearch/BuiltIn", m)
			}
		})
	}
}

func TestClientPluginsOtherToolsStillClient(t *testing.T) {
	fn := Tool{Type: "function", Function: json.RawMessage(`{"name":"get_weather","description":"weather","parameters":{"type":"object"}}`)}
	plugins := clientPlugins([]Tool{fn}, "")
	if len(plugins) != 1 {
		t.Fatalf("plugins = %#v", plugins)
	}
	m, _ := plugins[0].(map[string]any)
	if m["Id"] != "get_weather" || m["Source"] != "Client" {
		t.Fatalf("ordinary tool plugin = %#v, want get_weather/Client", m)
	}
}

func TestToolProtocolPromptDeclaresWebSearch(t *testing.T) {
	ws := Tool{Type: "web_search", Function: nil}
	prompt := toolProtocolPrompt("Find the latest price.", []Tool{ws}, "auto", false)
	if !strings.Contains(prompt, "web_search") || !strings.Contains(prompt, `"query"`) {
		t.Fatalf("web_search declaration missing from prompt:\n%s", prompt)
	}
	fn := Tool{Type: "function", Function: json.RawMessage(`{"name":"get_weather","description":"weather","parameters":{"type":"object"}}`)}
	prompt = toolProtocolPrompt("What is the weather?", []Tool{fn}, "auto", false)
	if !strings.Contains(prompt, "get_weather") || strings.Contains(prompt, "web_search") {
		t.Fatalf("function tool rendering broken:\n%s", prompt)
	}
}
