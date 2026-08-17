package web

import "testing"

func TestContentPolicyBlock(t *testing.T) {
	if !isContentPolicyBlock("很抱歉，我无法响应。我可以提供其他方面的帮助吗？") {
		t.Fatal("M365 refusal was not detected")
	}
	if isContentPolicyBlock("OK") {
		t.Fatal("ordinary response was classified as a refusal")
	}
}
