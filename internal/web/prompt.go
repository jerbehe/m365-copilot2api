package web

import (
	"fmt"
	"m365-copilot2api/internal/chathub"
	"strings"
)

func flattenPromptMessages(messages []oaiMsg, attachments []chathub.Attachment) (string, []chathub.Attachment) {
	var b strings.Builder
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		txt, files := parseContent(m.Content)
		attachments = append(attachments, files...)
		txt = strings.TrimSpace(txt)
		if len(m.ToolCalls) > 0 {
			if txt != "" {
				b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
			}
			b.WriteString(fmt.Sprintf("\n[%s tool_calls]\n%s\n", role, mustJSON(m.ToolCalls)))
			continue
		}
		if role == "tool" {
			txt = compactToolResult(txt, 4000)
			b.WriteString(fmt.Sprintf("\n[tool result id=%s]\n%s\n", m.ToolCallID, txt))
			continue
		}
		if txt == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
	}
	return strings.TrimSpace(b.String()), attachments
}
