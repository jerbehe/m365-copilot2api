package web

import "testing"

// codexExecTools mirrors what Codex declares: a grammar-constrained custom
// `exec` tool whose body is a raw script, alongside ordinary function tools.
func codexExecTools() []map[string]any {
	return []map[string]any{
		{"type": "custom", "function": map[string]any{
			"name": "exec",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"input": map[string]any{"type": "string"}},
				"required":   []string{"input"},
			},
		}},
		{"type": "function", "function": map[string]any{"name": "view_image"}},
	}
}

func TestFencedCustomExecAcceptsBridgedAndBareBody(t *testing.T) {
	cases := []struct {
		name, text, wantArgs string
	}{
		{"bridged input object", "```exec\n{\"input\":\"ls -la\"}\n```", `{"input":"ls -la"}`},
		{"bare script body", "```exec\nls -la\n```", `{"input":"ls -la"}`},
		// Upstream truncates long answers, so the closing fence is often absent.
		{"unclosed fence", "```exec\n{\"input\":\"ls -la\"}", `{"input":"ls -la"}`},
		{"no newline before close", "```exec\n{\"input\":\"ls\"}```", `{"input":"ls"}`},
		{"prose around fence", "我来检测。\n```exec\n{\"input\":\"pwd\"}\n```\n继续", `{"input":"pwd"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := fencedToolCalls(tc.text, codexExecTools(), "auto")
			if len(calls) != 1 {
				t.Fatalf("expected 1 call, got %d (%+v)", len(calls), calls)
			}
			if calls[0].Name != "exec" || calls[0].Type != "custom" {
				t.Fatalf("unexpected call shape: %+v", calls[0])
			}
			if string(calls[0].Arguments) != tc.wantArgs {
				t.Fatalf("arguments = %s, want %s", calls[0].Arguments, tc.wantArgs)
			}
		})
	}
}

// A fence body that opens with `{` but does not parse is a truncated call.
// Treating it as a raw script would execute broken code.
func TestFencedCustomExecRejectsTruncatedJSON(t *testing.T) {
	text := "```exec\n{\"input\":\"const wanted = [\\\"a\\\"]; text(r);\"\n```"
	if calls := fencedToolCalls(text, codexExecTools(), "auto"); len(calls) != 0 {
		t.Fatalf("truncated JSON must not become a call, got %+v", calls)
	}
}

func TestFencedUndeclaredToolStaysText(t *testing.T) {
	if calls := fencedToolCalls("```exec\nls -la\n```", nil, "auto"); len(calls) != 0 {
		t.Fatalf("undeclared exec must not convert, got %+v", calls)
	}
}

// stripToolFences is the last guard before prose reaches the client: a leaked
// ```exec block ends the turn without a tool call, so the client renders a wall
// of script and then stalls.
func TestStripToolFencesWithholdsOnlyToolBlocks(t *testing.T) {
	cases := []struct {
		name, text, want string
		wantWithheld     bool
	}{
		{
			name:         "declared tool fence removed",
			text:         "我会继续检测。\n```exec\n{\"input\":\"ls\"}\n```\n完成。",
			want:         "我会继续检测。\n\n完成。",
			wantWithheld: true,
		},
		{
			name:         "truncated tool fence removed",
			text:         "我会继续检测。```exec\n{}}; } catch (e) { }\n",
			want:         "我会继续检测。",
			wantWithheld: true,
		},
		{
			name:         "undeclared language fence kept",
			text:         "示例代码：\n```python\nprint(1)\n```",
			want:         "示例代码：\n```python\nprint(1)\n```",
			wantWithheld: false,
		},
		{
			name:         "plain prose untouched",
			text:         "这是普通回答，没有围栏。",
			want:         "这是普通回答，没有围栏。",
			wantWithheld: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, withheld := stripToolFences(tc.text, codexExecTools())
			if withheld != tc.wantWithheld {
				t.Fatalf("withheld = %t, want %t", withheld, tc.wantWithheld)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A response that was nothing but unusable tool syntax must not become an empty
// message: downstream treats an empty answer as an upstream failure, and the
// client just sees the turn stall.
func TestWithholdToolFencesSubstitutesNoticeWhenNothingRemains(t *testing.T) {
	got, withheld := withholdToolFences("```exec\n{\"input\":\"ls\"}\n```", codexExecTools())
	if !withheld {
		t.Fatal("expected the block to be withheld")
	}
	if got != withheldToolFenceNotice {
		t.Fatalf("got %q, want the withheld notice", got)
	}

	// Surrounding prose is a usable answer and must be kept as-is.
	got, withheld = withholdToolFences("先说明。\n```exec\n{\"input\":\"ls\"}\n```", codexExecTools())
	if !withheld || got != "先说明。\n" {
		t.Fatalf("got %q withheld=%t, want the prose preserved", got, withheld)
	}
}
