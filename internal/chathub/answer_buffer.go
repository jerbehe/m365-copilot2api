package chathub

import (
	"strings"
	"unicode/utf8"
)

// minOpeningBytes is how much settled text must accumulate before the first
// delta is forwarded.
//
// ChatHub's first two snapshots routinely agree on a one- or two-character opener
// ("I", "我将") and upstream then rewrites the whole answer, including that opener
// — most visibly when it switches the answer's language. Once those bytes are with
// the client they cannot be recalled, so the rewrite has to be re-sent and the
// stale opener stays on screen. Holding the first delta until the settled prefix
// has some substance costs a few milliseconds and removes that case; the
// completion flush ignores the threshold, so a short answer is never withheld.
const minOpeningBytes = 12

// answerBuffer decides how much of a ChatHub answer is safe to forward, given
// that upstream rewrites text it has already sent.
//
// Two frame shapes carry answer text:
//
//	writeAtCursor — a provisional incremental chunk appended at the cursor.
//	  It is always new text, even when it repeats what precedes it ("常" arriving
//	  right after text already ending in "常").
//
//	bot message text — the complete answer so far. It is authoritative, and it may
//	  rewrite earlier text rather than just extend it: ChatHub converts a markdown
//	  link to a bare path mid-answer, so "…[a.svg](/mnt/x" becomes "…/mnt/x/a.svg".
//
// Forwarded bytes cannot be recalled, so text is forwarded only once two
// consecutive snapshots agree on it. Rewrites land on the recent tail, so a
// prefix that survives a second snapshot is settled. Anything newer stays
// buffered until the next snapshot confirms it or the completion frame flushes it.
//
// Weaker rules fail on real traffic. Diffing each snapshot against forwarded
// bytes cannot express a rewrite at all: it finds a short common prefix and
// re-sends the whole tail, then repeats that for every later snapshot — which
// streamed a 101-byte answer as 368 bytes of duplicated file paths. Trusting a
// single snapshot is not enough either, because the first snapshot of a link is
// itself later rewritten, interleaving both versions in the client's output.
//
// The invariant is that forwarded is always a prefix of the authoritative text.
// A rewrite that breaks it re-sends everything past the surviving common prefix,
// so the client ends up with the corrected answer. Resuming at a stale byte offset
// instead silently dropped a character: "I" had been forwarded from an English
// opener ChatHub rewrote to Chinese, and byte 1 of "当前目录…" is inside 当's
// encoding, so the client received "I前目录…".
type answerBuffer struct {
	authoritative string
	// lastSnapshot is the previous authoritative snapshot, used to find the
	// prefix two snapshots agree on.
	lastSnapshot string
	haveSnapshot bool
	confirmed    int
	// forwarded is the text already handed to the client. It is kept verbatim
	// rather than as an offset so a rewrite can be measured against what the
	// client actually saw.
	forwarded string
}

// Append records a provisional cursor chunk. It returns the text to forward,
// which is empty unless snapshots have already confirmed text past the cursor.
func (b *answerBuffer) Append(chunk string) string {
	if chunk == "" {
		return ""
	}
	b.authoritative += chunk
	return b.take(b.confirmed, false)
}

// Replace records an authoritative snapshot. What the last two snapshots agree on
// is settled and may be forwarded.
func (b *answerBuffer) Replace(snapshot string) string {
	if snapshot == "" {
		return ""
	}
	b.authoritative = snapshot
	if b.haveSnapshot {
		// Track the agreement of the two most recent snapshots, which may shrink:
		// after a rewrite, a prefix confirmed earlier is no longer corroborated, and
		// keeping the old offset would forward the next snapshot's text on that one
		// snapshot alone — exactly the failure mode two-snapshot agreement exists to
		// prevent.
		b.confirmed = commonPrefixRunes(b.lastSnapshot, snapshot)
	}
	b.lastSnapshot = snapshot
	b.haveSnapshot = true
	return b.take(b.confirmed, false)
}

// Flush confirms and releases everything buffered. Callers must invoke it once
// upstream signals completion, or trailing text would never reach the client.
func (b *answerBuffer) Flush() string {
	b.confirmed = len(b.authoritative)
	return b.take(b.confirmed, true)
}

// Text returns the upstream's authoritative answer so far.
func (b *answerBuffer) Text() string { return b.authoritative }

// Emitted reports how many bytes have been forwarded.
func (b *answerBuffer) Emitted() int { return len(b.forwarded) }

// take forwards authoritative text up to limit. final releases the text
// unconditionally; otherwise the first delta waits for minOpeningBytes.
//
// The returned delta always continues the text the client already has: if the
// authoritative text diverged from what was forwarded, the divergent tail is
// re-sent rather than resuming at a byte offset that no longer aligns.
func (b *answerBuffer) take(limit int, final bool) string {
	if limit > len(b.authoritative) {
		limit = len(b.authoritative)
	}
	if !final && b.forwarded == "" && limit < minOpeningBytes {
		return ""
	}
	target := b.authoritative[:limit]
	if strings.HasPrefix(target, b.forwarded) {
		delta := target[len(b.forwarded):]
		b.forwarded = target
		return delta
	}
	if strings.HasPrefix(b.forwarded, target) {
		// A shortening rewrite: everything confirmed so far was already sent, and
		// the extra bytes cannot be recalled. Keep them so the next comparison is
		// against what the client actually has.
		return ""
	}
	// Upstream rewrote text the client already received. Emitting only the growth
	// would resume at a stale offset and corrupt the answer, so re-send everything
	// past the surviving common prefix and let the duplicate stand.
	common := commonPrefixRunes(b.forwarded, target)
	delta := target[common:]
	b.forwarded = target
	return delta
}

// commonPrefixRunes returns the length in bytes of the longest common prefix of a
// and b, truncated to a rune boundary.
func commonPrefixRunes(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	for n > 0 && n < len(a) && !utf8.RuneStart(a[n]) {
		n--
	}
	return n
}
