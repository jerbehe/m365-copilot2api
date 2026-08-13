package web

import (
	"encoding/json"
	"regexp"
	"strings"
)

// The apply_patch tool is grammar-constrained: its body is a literal patch
// envelope, not JSON. Codex declares it as a custom tool whose format.syntax is
// lark, and the model emits the envelope directly.
const (
	applyPatchToolName = "apply_patch"
	applyPatchBegin    = "*** Begin Patch"
	applyPatchEnd      = "*** End Patch"
)

// applyPatchHeredoc matches the shell-style wrapper the model reaches for when it
// treats apply_patch as a command rather than a tool:
//
//	apply_patch <<'PATCH'
//	*** Begin Patch
//	...
//	*** End Patch
//	PATCH
//
// The wrapper is not part of the patch, and forwarding it verbatim gave clients a
// half-rendered heredoc instead of an edit.
var applyPatchHeredoc = regexp.MustCompile(`(?m)^[ \t]*` + applyPatchToolName + `[ \t]*<<[-]?[ \t]*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?[ \t]*\r?\n`)

// isGrammarTool reports whether a declared tool takes a raw body under a grammar
// rather than a JSON argument object.
func isGrammarTool(t map[string]any) bool {
	if typ, _ := t["type"].(string); typ != "custom" {
		return false
	}
	format, _ := t["format"].(map[string]any)
	if format == nil {
		return false
	}
	kind, _ := format["type"].(string)
	return kind == "grammar"
}

// applyPatchEnvelope extracts the first patch envelope in text, along with the
// span it occupies, so the caller can both issue the call and withhold the
// syntax from the prose it forwards.
//
// The end marker is optional: upstream truncates long answers, and a patch that
// lost its trailing marker is still the complete set of hunks the model wrote up
// to that point. Requiring the marker made a truncated edit fall through and
// reach the client as raw text.
func applyPatchEnvelope(text string) (patch string, start, end int, ok bool) {
	begin := strings.Index(text, applyPatchBegin)
	if begin < 0 {
		return "", 0, 0, false
	}
	// A heredoc wrapper immediately before the envelope belongs to the call. Its
	// terminator after the envelope does too, so remember which word closes it.
	span := begin
	terminator := ""
	if loc := applyPatchHeredoc.FindStringSubmatchIndex(text[:begin]); loc != nil && strings.TrimSpace(text[loc[1]:begin]) == "" {
		span = loc[0]
		terminator = text[loc[2]:loc[3]]
	}
	body := text[begin:]
	if i := strings.Index(body, applyPatchEnd); i >= 0 {
		stop := begin + i + len(applyPatchEnd)
		return text[begin:stop], span, begin + i + len(applyPatchEnd) + heredocTerminatorLen(text[stop:], terminator), true
	}
	// Truncated: take the rest, minus a dangling heredoc terminator.
	patch = strings.TrimRight(body, " \t\r\n")
	if lines := strings.Split(patch, "\n"); len(lines) > 1 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last != "" && (last == terminator || isHeredocTerminatorWord(last)) {
			patch = strings.TrimRight(strings.Join(lines[:len(lines)-1], "\n"), " \t\r\n")
		}
	}
	return patch, span, len(text), true
}

// heredocTerminatorLen measures the terminator line that closes a heredoc, so it
// is withheld along with the patch rather than left behind as stray prose.
func heredocTerminatorLen(rest, terminator string) int {
	if terminator == "" {
		return 0
	}
	trimmed := strings.TrimLeft(rest, " \t\r\n")
	if !strings.HasPrefix(trimmed, terminator) {
		return 0
	}
	consumed := len(rest) - len(trimmed) + len(terminator)
	// Take the line ending too, so the removal does not leave a blank line.
	for _, b := range rest[consumed:] {
		if b != '\r' && b != '\n' {
			break
		}
		consumed++
	}
	return consumed
}

// isHeredocTerminatorWord reports whether a line looks like a heredoc terminator
// (PATCH, EOF) rather than patch content. Patch lines carry a leading +, -, space
// or ***, so an all-caps bare word cannot be one.
func isHeredocTerminatorWord(line string) bool {
	if line == "" || strings.HasPrefix(line, "***") {
		return false
	}
	if line != strings.ToUpper(line) {
		return false
	}
	return !strings.ContainsAny(line, " \t+-@")
}

// applyPatchCall converts a patch envelope into the custom tool call shape the
// gateway bridges grammar tools through.
func applyPatchCall(patch string, index int) detectedToolCall {
	b, _ := json.Marshal(map[string]any{"input": patch})
	return detectedToolCall{ID: callID(applyPatchToolName, string(b), index), Type: "custom", Name: applyPatchToolName, Arguments: b}
}
