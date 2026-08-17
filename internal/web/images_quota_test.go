package web

import "testing"

func TestImageQuotaRefusal(t *testing.T) {
	for _, text := range []string{
		"Sorry, I can't generate any more images today.",
		"Sorry, try again tomorrow.",
		"抱歉，我今天无法再生成图片。请明天再试。",
	} {
		if !isImageQuotaRefusal(text) {
			t.Fatalf("quota refusal not detected: %q", text)
		}
	}
	if isImageQuotaRefusal("Here is your generated image.") {
		t.Fatal("ordinary image response misclassified")
	}
}
