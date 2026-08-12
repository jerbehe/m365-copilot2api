package chathub

import (
	"strings"
	"testing"
	"unicode/utf8"
)

type frame struct {
	text   string
	cursor bool
}

func cursorChunks(s ...string) []frame {
	out := make([]frame, 0, len(s))
	for _, v := range s {
		out = append(out, frame{text: v, cursor: true})
	}
	return out
}

// replayFrames mirrors chatWithHandlers: writeAtCursor frames append, bot
// snapshots replace, and the completion frame flushes what is still buffered. It
// returns what the client received and the authoritative answer.
func replayFrames(frames []frame) (emitted, authoritative string) {
	var b answerBuffer
	var out strings.Builder
	for _, f := range frames {
		if f.cursor {
			out.WriteString(b.Append(f.text))
			continue
		}
		out.WriteString(b.Replace(f.text))
	}
	out.WriteString(b.Flush())
	return out.String(), b.Text()
}

func TestAnswerBufferForwardsTheAuthoritativeAnswerOnce(t *testing.T) {
	cases := []struct {
		name   string
		frames []frame
		want   string
	}{
		{
			name:   "cursor chunks only",
			frames: cursorChunks("第一段：", "分布式", "系统"),
			want:   "第一段：分布式系统",
		},
		{
			name:   "growing snapshots",
			frames: []frame{{text: "第一段："}, {text: "第一段：分布式"}},
			want:   "第一段：分布式",
		},
		{
			name:   "repeated snapshot emits nothing extra",
			frames: []frame{{text: "已完成"}, {text: "已完成"}, {text: "已完成"}},
			want:   "已完成",
		},
		{
			// A snapshot may shorten the answer. Waiting for two snapshots to agree
			// means the discarded text was never forwarded.
			name:   "shortening rewrite is absorbed",
			frames: []frame{{text: "第一段：分布式"}, {text: "第一段："}},
			want:   "第一段：",
		},
		{
			name:   "cursor chunks between snapshots",
			frames: []frame{{text: "abc"}, {text: "d", cursor: true}, {text: "abcd"}, {text: "abcde"}},
			want:   "abcde",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emitted, authoritative := replayFrames(tc.frames)
			if emitted != tc.want {
				t.Fatalf("emitted %q, want %q", emitted, tc.want)
			}
			if authoritative != tc.want {
				t.Fatalf("authoritative %q, want %q", authoritative, tc.want)
			}
		})
	}
}

// A cursor chunk whose text also terminates the buffer ("常" after "…常") is new
// text. Overlap matching used to swallow it, desynchronising the buffer until the
// next snapshot looked unrelated and the whole answer was re-sent.
func TestAnswerBufferKeepsRepeatedCursorChunk(t *testing.T) {
	frames := cursorChunks("日志表", "常", "常", "包含数百万条数据")
	frames = append(frames, frame{text: "日志表常常包含数百万条数据"})
	frames = append(frames, cursorChunks("，没有索引就会全表扫描")...)
	frames = append(frames, frame{text: "日志表常常包含数百万条数据，没有索引就会全表扫描"})

	want := "日志表常常包含数百万条数据，没有索引就会全表扫描"
	emitted, authoritative := replayFrames(frames)
	if emitted != want {
		t.Fatalf("emitted %q (len %d), want %q (len %d)", emitted, len(emitted), want, len(want))
	}
	if authoritative != want {
		t.Fatalf("authoritative %q, want %q", authoritative, want)
	}
}

// The regression that motivated answerBuffer, taken from a real event stream:
// ChatHub streams a markdown link, then rewrites it to a bare path in every later
// snapshot. Diffing snapshots against forwarded bytes found a 23-byte common
// prefix and re-sent the whole tail each time, streaming a 101-byte answer as 368
// bytes of repeated paths.
func TestAnswerBufferSurvivesUpstreamRewrite(t *testing.T) {
	const final = "已创建 SVG 文件：/mnt/data/x/pelican.svg\n\n内容是一只卡通鹈鹕骑自行车。"
	frames := []frame{
		{text: "已"},
		{text: "创建", cursor: true},
		{text: " SVG", cursor: true},
		{text: " 文件：[pelican.svg](/mnt/data/x", cursor: true},
		{text: "/", cursor: true},
		{text: "已创建 SVG 文件：/mnt/data/x/pelican.svg\n\n内容是一只卡"},
		{text: "已创建 SVG 文件：/mnt/data/x/pelican.svg\n\n内容是一只卡通"},
		{text: "鹈鹕骑自行车。", cursor: true},
		{text: final},
		{text: final},
	}
	emitted, authoritative := replayFrames(frames)

	if authoritative != final {
		t.Fatalf("authoritative %q, want %q", authoritative, final)
	}
	// The rewritten link never reaches the client, so the stream matches the
	// authoritative answer exactly.
	if emitted != final {
		t.Fatalf("emitted %q (len %d), want %q (len %d)", emitted, len(emitted), final, len(final))
	}
	if strings.Contains(emitted, "[pelican.svg]") {
		t.Fatalf("forwarded the rewritten markdown link: %q", emitted)
	}
	if !utf8.ValidString(emitted) {
		t.Fatalf("emitted text is not valid UTF-8: %q", emitted)
	}
}

// Two snapshots agreeing on a prefix settle it, so output stays incremental
// instead of arriving in one lump at the completion frame.
func TestAnswerBufferStreamsIncrementally(t *testing.T) {
	var b answerBuffer
	if got := b.Replace("第一段："); got != "" {
		t.Fatalf("a single snapshot must not be trusted, got %q", got)
	}
	if got := b.Replace("第一段：分布式"); got != "第一段：" {
		t.Fatalf("second snapshot delta = %q, want the agreed prefix", got)
	}
	if got := b.Append("系统"); got != "" {
		t.Fatalf("text past the agreed prefix is provisional, got %q", got)
	}
	if got := b.Replace("第一段：分布式系统"); got != "分布式" {
		t.Fatalf("delta = %q, want the newly agreed text", got)
	}
	if got := b.Flush(); got != "系统" {
		t.Fatalf("flush = %q, want the remaining tail", got)
	}
}

// Whatever is still buffered must reach the client when upstream completes.
func TestAnswerBufferFlushReleasesBufferedTail(t *testing.T) {
	var b answerBuffer
	if got := b.Append("提"); got != "" {
		t.Fatalf("unconfirmed cursor text must be withheld, got %q", got)
	}
	if got := b.Append("交"); got != "" {
		t.Fatalf("unconfirmed cursor text must be withheld, got %q", got)
	}
	if got := b.Flush(); got != "提交" {
		t.Fatalf("flush returned %q, want %q", got, "提交")
	}
	if b.Emitted() != len("提交") {
		t.Fatalf("emitted=%d, want %d after flush", b.Emitted(), len("提交"))
	}
}

// Every delta must be valid UTF-8 on its own: clients decode fragments as they
// arrive, so a cut inside a multi-byte rune renders as a replacement character.
func TestAnswerBufferNeverSplitsRunes(t *testing.T) {
	const settled = "第一段：分布式系统"
	var b answerBuffer
	b.Replace(settled)
	var deltas []string
	deltas = append(deltas, b.Replace(settled))
	// A rewrite that keeps only part of the settled text, then extends it.
	deltas = append(deltas, b.Replace("第一段：分布"))
	deltas = append(deltas, b.Replace("第一段：分布式架构说明"))
	deltas = append(deltas, b.Flush())
	joined := ""
	for _, d := range deltas {
		if !utf8.ValidString(d) {
			t.Fatalf("delta %q is not valid UTF-8", d)
		}
		joined += d
	}
	if !utf8.ValidString(joined) {
		t.Fatalf("client text %q is not valid UTF-8", joined)
	}
	if !strings.HasSuffix(joined, "架构说明") {
		// Text already forwarded cannot be recalled, so "系统" stays on screen; what
		// matters is that the corrected tail arrives intact and on rune boundaries.
		t.Fatalf("client text %q does not end in the corrected tail", joined)
	}
}

// A rewrite that replaces text the client already received must re-send the
// corrected tail rather than resume at a stale offset. Resuming dropped the first
// character of the new text when the byte offset landed inside a rune: "I" from an
// English opener, rewritten to "当前目录…", arrived as "I前目录…".
func TestAnswerBufferResendsRewrittenPrefix(t *testing.T) {
	var b answerBuffer
	// Force "I" out despite the opening threshold, as an older build would have.
	b.forwarded = "I"
	b.authoritative = "I"
	b.lastSnapshot = "I"
	b.haveSnapshot = true
	b.confirmed = 1

	second := b.Replace("当前目录为空，没有任何文件")
	third := b.Replace("当前目录为空，没有任何文件")
	fourth := b.Flush()

	client := "I" + second + third + fourth
	if !utf8.ValidString(client) {
		t.Fatalf("client text %q is not valid UTF-8", client)
	}
	if !strings.HasSuffix(client, "当前目录为空，没有任何文件") {
		t.Fatalf("client text %q lost the rewritten answer", client)
	}
	if b.Text() != "当前目录为空，没有任何文件" {
		t.Fatalf("authoritative=%q", b.Text())
	}
}

// The opening delta is withheld until enough text has settled, because ChatHub
// routinely rewrites a one- or two-character opener — including switching the
// answer's language — and forwarded bytes cannot be recalled.
func TestAnswerBufferWithholdsShortOpener(t *testing.T) {
	var b answerBuffer
	b.Replace("I")
	if got := b.Replace("I"); got != "" {
		t.Fatalf("a %d-byte agreed opener must be withheld, got %q", len("I"), got)
	}
	// Upstream rewrites the opener entirely; nothing stale was forwarded.
	b.Replace("当前目录为空，没有任何文件")
	settled := b.Replace("当前目录为空，没有任何文件")
	if strings.HasPrefix(settled, "I") {
		t.Fatalf("forwarded the discarded opener: %q", settled)
	}
	if settled+b.Flush() != "当前目录为空，没有任何文件" {
		t.Fatalf("client text = %q", settled+b.Flush())
	}
}

// A short answer must never be swallowed by the opening threshold: the completion
// flush releases whatever is buffered.
func TestAnswerBufferFlushesAnswerShorterThanThreshold(t *testing.T) {
	var b answerBuffer
	if got := b.Append("好"); got != "" {
		t.Fatalf("unconfirmed cursor text must be withheld, got %q", got)
	}
	if got := b.Flush(); got != "好" {
		t.Fatalf("flush = %q, want %q", got, "好")
	}
}

// A confirmed prefix must never be re-sent while upstream merely extends it.
func TestAnswerBufferNeverResendsUnchangedPrefix(t *testing.T) {
	const base = "abcdefghijklmn"
	var b answerBuffer
	b.Replace(base)
	if got := b.Replace(base); got != base {
		t.Fatalf("agreed prefix delta=%q, want %q", got, base)
	}
	// Growth needs a second agreeing snapshot before it is safe to forward.
	if got := b.Replace(base + "op"); got != "" {
		t.Fatalf("unconfirmed growth emitted %q", got)
	}
	if got := b.Replace(base + "op"); got != "op" {
		t.Fatalf("delta=%q, want %q", got, "op")
	}
	// A shortening rewrite discards text the client already has; nothing can be
	// recalled, so nothing more is sent.
	if got := b.Replace("ab"); got != "" {
		t.Fatalf("shortening rewrite emitted %q", got)
	}
	// Later divergent growth re-sends only past the surviving common prefix.
	b.Replace("abxyz")
	if got := b.Flush(); strings.HasPrefix(got, "ab") {
		t.Fatalf("delta %q re-sent the unchanged prefix", got)
	}
}
