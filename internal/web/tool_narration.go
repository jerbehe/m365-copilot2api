package web

import (
	"regexp"
	"strings"
)

// Tool selection is a protocol decision, not an answer. Upstream sometimes
// narrates it instead of emitting a call — "I am choosing the shell command tool
// to inspect relevant files", "选择shell_command工具，可能需要检查特定文件" — or
// echoes the router envelope without its argument payload ("CALL_TOOL
// shell_command"). Forwarding either gives the client a turn that announces work
// and then stops, with no call to act on.

// routerEnvelopeMarker matches the router protocol's own tokens. They are never
// user-facing: the gateway asked for them, so it must also remove them.
var routerEnvelopeMarker = regexp.MustCompile(`(?im)^[ \t>*-]*(?:CALL_TOOL|NO_TOOL_NEEDED)\b[ \t]*:?[ \t]*`)

// bareRouterMarker matches a whole answer that is nothing but the router marker
// and a name — "CALL_TOOL shell_command". Stripping the marker leaves a bare
// identifier, which reads as neither a call nor an answer.
var bareRouterMarker = regexp.MustCompile(`(?is)^[ \t>*-]*(?:CALL_TOOL|call_tool)\b[ \t]*:?[ \t]*[A-Za-z0-9_.-]+[ \t\r\n]*$`)

// toolIntentPhrase matches a statement of intent to use a tool, in the languages
// upstream answers in. The tool name is checked separately, so these stay broad
// enough to catch paraphrases without matching ordinary prose about tools.
var toolIntentPhrase = regexp.MustCompile(`(?i)(I (?:am|will|'ll|'m)\s+(?:going to\s+)?(?:now\s+)?(?:choos|select|us|call|invok|pick)|I\s+(?:choose|select|use|call|invoke|need)\b|(?:choosing|selecting|calling|invoking|using)\s+the\b|将(?:要)?(?:使用|调用|选择)|(?:需要|打算|准备)(?:使用|调用|选择)|^\s*(?:选择|使用|调用)|工具(?:来|以|去)?(?:检查|查看|读取|执行|实现|完成)|tool\s+to\s+(?:inspect|check|read|run|implement|create|verify))`)

// stripToolProtocolMarkers removes router envelope tokens from text that is about
// to be forwarded as prose. It must NOT trim surrounding whitespace: it runs on
// individual stream deltas, where a delta's leading newline is the separator
// between markdown blocks — trimming it glues paragraphs and table rows
// together.
func stripToolProtocolMarkers(text string) string {
	return routerEnvelopeMarker.ReplaceAllString(text, "")
}

// stripToolProtocolMarkersWhole is the whole-text variant: the complete answer
// is known, so the stray whitespace a removed marker leaves at the edges can be
// trimmed safely.
func stripToolProtocolMarkersWhole(text string) string {
	return strings.TrimSpace(routerEnvelopeMarker.ReplaceAllString(text, ""))
}

// isToolIntentNarration reports whether text is a statement about which tool to
// use rather than an answer or a call. It requires both a declared tool's name
// and intent phrasing, so a genuine answer that happens to mention a tool is not
// suppressed.
//
// Length is the third guard: a long passage that names a tool is explanation the
// user asked for, not a stray selection notice.
func isToolIntentNarration(text string, tools []map[string]any) bool {
	if bareRouterMarker.MatchString(text) {
		return true
	}
	trimmed := strings.TrimSpace(stripToolProtocolMarkers(text))
	if trimmed == "" || len([]rune(trimmed)) > 400 {
		return false
	}
	// Code or a patch envelope means real content, whatever else is present.
	if strings.Contains(trimmed, "```") || strings.Contains(trimmed, applyPatchBegin) {
		return false
	}
	if !mentionsDeclaredTool(trimmed, tools) {
		return false
	}
	return toolIntentPhrase.MatchString(trimmed)
}

// mentionsDeclaredTool reports whether text names one of the declared tools. Both
// the literal name and its spaced form are checked: a model writing prose turns
// shell_command into "shell command".
func mentionsDeclaredTool(text string, tools []map[string]any) bool {
	lower := strings.ToLower(text)
	for name := range allowedToolNames(tools) {
		if name == "" {
			continue
		}
		lowerName := strings.ToLower(name)
		if strings.Contains(lower, lowerName) {
			return true
		}
		if spaced := strings.ReplaceAll(lowerName, "_", " "); spaced != lowerName && strings.Contains(lower, spaced) {
			return true
		}
	}
	return false
}

// toolNarrationNotice replaces a turn that only announced a tool. An empty
// assistant message is rejected downstream as an empty upstream response, so say
// what happened instead of leaving the client with a bare announcement.
const toolNarrationNotice = "上游只说明了要使用哪个工具，没有产生可执行的工具调用。请重试当前请求。"

// narrationGate withholds streamed text while it still reads as nothing but a
// tool-selection announcement.
//
// Streaming cannot recall what it already sent, so the decision has to be made
// before the first delta goes out. Holding everything until the answer is
// complete would defeat streaming, so the gate holds only while
// isToolIntentNarration keeps matching: real content makes the text stop
// matching — it grows past the length bound, or brings a fence or a patch — and
// everything held is released at once.
//
// The gate is inert when the request declares no tools, so ordinary chat streams
// exactly as before.
type narrationGate struct {
	tools []map[string]any
	held  strings.Builder
	open  bool
}

func newNarrationGate(tools []map[string]any) *narrationGate {
	return &narrationGate{tools: tools, open: len(tools) == 0}
}

// Feed reports the text that may be emitted now. It returns "" while the answer
// so far is still only an announcement.
func (g *narrationGate) Feed(part string) string {
	if g.open {
		return part
	}
	g.held.WriteString(part)
	if isToolIntentNarration(g.held.String(), g.tools) {
		return ""
	}
	g.open = true
	out := g.held.String()
	g.held.Reset()
	return out
}

// Close reports the text still held when the stream ended, and whether the whole
// answer turned out to be an announcement.
func (g *narrationGate) Close() (string, bool) {
	if g.open {
		return "", false
	}
	held := g.held.String()
	g.held.Reset()
	g.open = true
	if strings.TrimSpace(held) == "" {
		return "", false
	}
	return held, isToolIntentNarration(held, g.tools)
}
