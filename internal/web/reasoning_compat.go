package web

// reasoningFields exposes both common OpenAI reasoning_content and the
// agent_reasoning compatibility alias expected by agent clients.
func reasoningFields(text string) map[string]any {
	return map[string]any{
		"reasoning_content": text,
		"agent_reasoning":   text,
	}
}

func reasoningText(message map[string]any) string {
	for _, key := range []string{"reasoning_content", "agent_reasoning", "reasoning"} {
		if text, ok := message[key].(string); ok && text != "" {
			return text
		}
	}
	return ""
}
