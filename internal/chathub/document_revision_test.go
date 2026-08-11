package chathub

import "testing"

func TestIsDocumentRevisionFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame map[string]any
		want  bool
	}{
		{"message type", map[string]any{"messages": []any{map[string]any{"messageType": "DocumentRevision"}}}, true},
		{"content type", map[string]any{"messages": []any{map[string]any{"contentType": "document_revision"}}}, true},
		{"revision key", map[string]any{"documentRevision": map[string]any{"text": "duplicate"}}, true},
		{"normal answer", map[string]any{"messages": []any{map[string]any{"messageType": "Chat", "text": "answer"}}}, false},
		// An answer that merely mentions the phrase is ordinary text and must stream.
		{"answer discussing revisions", map[string]any{"messages": []any{
			map[string]any{"messageType": "Chat", "text": "document revision 是文档修订功能"},
		}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDocumentRevisionFrame(tc.frame); got != tc.want {
				t.Fatalf("got %t, want %t", got, tc.want)
			}
		})
	}
}
