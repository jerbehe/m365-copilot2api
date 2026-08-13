package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func messageItemPhase(t *testing.T, output []any) (string, bool) {
	t.Helper()
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] != "message" {
			continue
		}
		phase, ok := item["phase"].(string)
		return phase, ok
	}
	t.Fatalf("no message item in %#v", output)
	return "", false
}

func outputItemsOf(t *testing.T, body []byte) []any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	out, _ := d["output"].([]any)
	return out
}

// Text accompanied by a tool call is a preamble: the turn continues once the call
// returns. Without the phase the client reads "phase unknown" as a final answer,
// so every mid-turn narration rendered as another finished answer.
func TestMessagePhaseIsCommentaryAlongsideAToolCall(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "m", false, narratedToolCall())
	phase, ok := messageItemPhase(t, outputItemsOf(t, rr.Body.Bytes()))
	if !ok || phase != messagePhaseCommentary {
		t.Fatalf("phase=%q present=%v, want commentary", phase, ok)
	}
}

// Text with no call is the turn's answer.
func TestMessagePhaseIsFinalAnswerWithoutAToolCall(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "m", false, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "the answer"}}},
	})
	phase, ok := messageItemPhase(t, outputItemsOf(t, rr.Body.Bytes()))
	if !ok || phase != messagePhaseFinalAnswer {
		t.Fatalf("phase=%q present=%v, want final_answer", phase, ok)
	}
	// The empty-turn placeholder is an answer too, not narration.
	items := responsesOutputItems(map[string]any{})
	phase, ok = messageItemPhase(t, items)
	if !ok || phase != messagePhaseFinalAnswer {
		t.Fatalf("placeholder phase=%q present=%v", phase, ok)
	}
}

// The live stream cannot know the phase when output_item.added goes out: no call
// has arrived yet. The terminal item is where the client reads it.
func TestLiveStreamMessagePhaseFollowsToolCalls(t *testing.T) {
	run := func(withCall bool) string {
		var frames []string
		emit := func(name string, v any) error { frames = append(frames, name+" "+mustJSON(v)); return nil }
		translateResponsesStream(emit, "m", oaiReq{}, func(h innerStreamHandler) (int, string, error) {
			if err := h.Text("I will read the file."); err != nil {
				return 0, "", err
			}
			if withCall {
				if err := h.ToolStart(0, "call_1", "read_file", "function"); err != nil {
					return 0, "", err
				}
				if err := h.ToolArgs(0, `{"path":"/etc/hostname"}`); err != nil {
					return 0, "", err
				}
			}
			return 200, "", nil
		})
		for _, f := range frames {
			payload, ok := strings.CutPrefix(f, "response.output_item.done ")
			if !ok {
				continue
			}
			var ev struct {
				Item struct {
					Type  string `json:"type"`
					Phase string `json:"phase"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Item.Type == "message" {
				return ev.Item.Phase
			}
		}
		t.Fatal("no terminal message item")
		return ""
	}
	if got := run(true); got != messagePhaseCommentary {
		t.Errorf("with a call: phase=%q want commentary", got)
	}
	if got := run(false); got != messagePhaseFinalAnswer {
		t.Errorf("without a call: phase=%q want final_answer", got)
	}
}

// A compaction turn emits its own item type and must not gain a phase.
func TestCompactionItemHasNoPhase(t *testing.T) {
	rr := httptest.NewRecorder()
	writeCompactionResult(rr, "m", false, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "summary"}}},
	})
	for _, raw := range outputItemsOf(t, rr.Body.Bytes()) {
		item, _ := raw.(map[string]any)
		if item["type"] != "compaction" {
			t.Fatalf("compaction turn produced %#v", item)
		}
		if _, present := item["phase"]; present {
			t.Fatalf("compaction item carries a phase: %#v", item)
		}
	}
}
