package chathub

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// replayFrames mirrors chatWithHandlers: writeAtCursor frames are appended
// verbatim, bot snapshots go through snapshotDelta.
func replayFrames(frames []frame) string {
	var streamed strings.Builder
	for _, f := range frames {
		if f.cursor {
			streamed.WriteString(f.text)
			continue
		}
		streamed.WriteString(snapshotDelta(streamed.String(), f.text))
	}
	return streamed.String()
}

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

func TestSnapshotDeltaEmitsOnlyUnstreamedSuffix(t *testing.T) {
	cases := []struct {
		name     string
		streamed string
		snapshot string
		want     string
	}{
		{"first snapshot", "", "第一段：分布式", "第一段：分布式"},
		{"growing snapshot", "第一段：", "第一段：分布式", "分布式"},
		{"identical snapshot", "第一段：", "第一段：", ""},
		{"stale shorter snapshot", "第一段：分布式", "第一段：", ""},
		{"empty snapshot", "已有文本", "", ""},
		{"diverged tail", "abc常常出现", "abc常见于此", "见于此"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapshotDelta(tc.streamed, tc.snapshot); got != tc.want {
				t.Fatalf("snapshotDelta(%q, %q) = %q, want %q", tc.streamed, tc.snapshot, got, tc.want)
			}
		})
	}
}

func TestSnapshotDeltaNeverSplitsRunes(t *testing.T) {
	// 一 (E4 B8 80) and 丁 (E4 B8 81) share their first two bytes, so a byte-wise
	// common prefix lands mid-rune and must be walked back to the boundary.
	got := snapshotDelta("前缀一", "前缀丁")
	if got != "丁" {
		t.Fatalf("got %q, want %q", got, "丁")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("delta %q is not valid UTF-8", got)
	}
}

// TestReplayRepeatedCursorChunkDoesNotDuplicate reproduces the upstream shape
// that broke overlap matching: a cursor chunk whose text also terminates the
// already streamed buffer ("常" after "…常"). Swallowing it desynchronised the
// buffer and made the next snapshot re-emit the entire answer.
func TestReplayRepeatedCursorChunkDoesNotDuplicate(t *testing.T) {
	frames := cursorChunks("日志表", "常", "常", "包含数百万条数据")
	frames = append(frames, frame{text: "日志表常常包含数百万条数据"})
	frames = append(frames, cursorChunks("，没有索引就会全表扫描")...)
	frames = append(frames, frame{text: "日志表常常包含数百万条数据，没有索引就会全表扫描"})

	want := "日志表常常包含数百万条数据，没有索引就会全表扫描"
	if got := replayFrames(frames); got != want {
		t.Fatalf("replay = %q (len %d), want %q (len %d)", got, len(got), want, len(want))
	}
}

// TestReplayInterleavedSnapshotsAndCursorChunks mirrors the observed upstream
// pattern: bursts of cursor chunks punctuated by monotonic full snapshots.
func TestReplayInterleavedSnapshotsAndCursorChunks(t *testing.T) {
	full := "索引是数据库中用于加速查询的数据结构，常常以 B+ 树实现。"
	var frames []frame
	streamed := ""
	runes := []rune(full)
	for i := 0; i < len(runes); i += 3 {
		end := min(i+3, len(runes))
		chunk := string(runes[i:end])
		frames = append(frames, frame{text: chunk, cursor: true})
		streamed += chunk
		// Upstream repeats the accumulated text as a snapshot every other burst.
		if (i/3)%2 == 1 {
			frames = append(frames, frame{text: streamed})
		}
	}
	frames = append(frames, frame{text: full})

	if got := replayFrames(frames); got != full {
		t.Fatalf("replay = %q (len %d), want %q (len %d)", got, len(got), full, len(full))
	}
}
