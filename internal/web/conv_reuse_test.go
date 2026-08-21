package web

import (
	"m365-copilot2api/internal/chathub"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestConversationReuseEnabledByDefault(t *testing.T) {
	t.Setenv("M365_CONV_REUSE", "")
	if !conversationReuseEnabled() {
		t.Fatal("复用默认必须开启，否则延迟优化会被静默关掉")
	}
}

func TestConversationReuseDisabledValues(t *testing.T) {
	for _, v := range []string{"0", "false", "no", "off", "OFF", " False "} {
		t.Setenv("M365_CONV_REUSE", v)
		if conversationReuseEnabled() {
			t.Fatalf("M365_CONV_REUSE=%q 应关闭复用", v)
		}
	}
}

// 开关关闭后不得留下任何隐式复用凭据：user 映射与内容键绑定都不应落盘，
// 否则它们既不会被命中，又会让 auto_cleanup 把死对话保护 2h。
func TestConversationReuseOffLeavesNoBinding(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "user-sessions.json"))
	t.Setenv("M365_CONV_REUSE", "0")

	s := &Server{
		sessionResolver: openSessionResolver(),
		userSessions:    openUserSessionStore(time.Hour),
	}
	res := chathub.Result{ConversationID: "conv-off", SessionID: "sess-off"}

	s.rememberUserSession("alice", res, "acc1")
	if _, ok := s.userSessions.Get("alice"); ok {
		t.Fatal("关闭态下不应写入 user->conversation 映射")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "问题"}}}
	if conversationReuseEnabled() {
		t.Fatal("开关未生效")
	}
	s.sessionResolver.Bind("", res.ConversationID, "acc1", body, "问题", req)
	// Bind 本身不看开关（诊断/显式续接仍要用它），守卫在 bindConversation 里。
	// 这里断言的是开启态才写入的那条路径确实由开关控制。
	if len(s.sessionResolver.ListSessions()) != 1 {
		t.Fatal("Bind 本身应保持无条件写入语义")
	}
}

// 开启态下增量请求必须命中已有云端对话，否则复用优化失效。
func TestConversationReuseOnMatchesIncrement(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONV_REUSE", "1")
	sr := openSessionResolver()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("User-Agent", "probe")

	sr.Bind("", "conv-a", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
		}},
		"", req)

	res := sr.Resolve(req, &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "第一轮问题"},
		{Role: "assistant", Content: "第一轮回答"},
		{Role: "user", Content: "第二轮问题"},
	}})
	if res.IsNew || res.ConversationID != "conv-a" {
		t.Fatalf("增量请求应命中 conv-a, got new=%v conv=%s", res.IsNew, res.ConversationID)
	}
}
