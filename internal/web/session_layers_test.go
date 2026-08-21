package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func layeredRequest(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.9.9.9:5555"
	r.Header.Set("User-Agent", "layered-probe")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// 层 1-3：三个客户端上报的会话头都能作为会话主键，且优先级高于内容判定。
func TestExternalSessionHeadersMatch(t *testing.T) {
	for _, h := range []string{"session-id", "thread-id", "X-Claude-Code-Session-Id"} {
		t.Run(h, func(t *testing.T) {
			t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
			sr := openSessionResolver()

			bindReq := layeredRequest(map[string]string{h: "external-123"})
			sr.Bind("", "conv-ext", "acc1",
				&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "第一问"}}},
				"回答一", bindReq)

			// 带同一个头再来，内容完全不同也必须命中——头是最高优先级的续接语义。
			res := sr.Resolve(layeredRequest(map[string]string{h: "external-123"}),
				&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "毫无关联的新提问"}}})
			if res.IsNew {
				t.Fatalf("%s 应作为会话主键命中", h)
			}
			if res.ConversationID != "conv-ext" {
				t.Fatalf("应命中 conv-ext, got %s", res.ConversationID)
			}
			if res.MatchedBy == "" || res.MatchedBy[:7] != "header_" {
				t.Fatalf("MatchedBy 应标出来源头, got %q", res.MatchedBy)
			}
		})
	}
}

// 优先级：多个头同时出现时按 externalSessionHeaders 的顺序取第一个。
func TestExternalSessionHeaderPriority(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	// 两个不同的会话，分别以 session-id 和 thread-id 为主键。
	sr.Bind("", "conv-session", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "a"}}}, "r",
		layeredRequest(map[string]string{"session-id": "S1"}))
	sr.Bind("", "conv-thread", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "b"}}}, "r",
		layeredRequest(map[string]string{"thread-id": "T1"}))

	// 同时带两个头：session-id 优先级更高。
	res := sr.Resolve(layeredRequest(map[string]string{"session-id": "S1", "thread-id": "T1"}),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "x"}}})
	if res.ConversationID != "conv-session" {
		t.Fatalf("session-id 优先级应高于 thread-id, got %s", res.ConversationID)
	}
}

// 会话头由客户端控制，必须做租户隔离，否则换个 API key 猜中 ID 就能接进别人的对话。
func TestExternalSessionHeaderIsolatedByTenant(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	withKey := func(key string) *http.Request {
		r := layeredRequest(map[string]string{"session-id": "shared-id"})
		return r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, key))
	}

	sr.Bind("", "conv-alice", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "alice 的问题"}}}, "回答",
		withKey("alice-key"))

	// 另一个租户带着同样的 session-id 进来，不得命中。
	res := sr.Resolve(withKey("bob-key"), &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "bob 的问题"}}})
	if !res.IsNew {
		t.Fatalf("跨租户的同名 session-id 不得复用, got conv=%s matched=%s", res.ConversationID, res.MatchedBy)
	}
}

// 层 4：首条 user 消息前 N 字符一致即复用，即使后续内容已经分叉。
func TestPromptPrefixMatchesDespiteDivergedHistory(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_PROMPT_PREFIX_CHARS", "20")
	sr := openSessionResolver()
	req := layeredRequest(nil)

	sr.Bind("", "conv-prompt", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "帮我重构这个模块的错误处理"},
			{Role: "assistant", Content: "第一轮的回答"},
			{Role: "user", Content: "只改第一处"},
		}}, "第二轮的回答", req)

	// 首句相同，但后续历史被客户端换掉了——内容键会失配，指纹层仍应命中。
	res := sr.Resolve(req, &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "帮我重构这个模块的错误处理"},
		{Role: "user", Content: "换个完全不同的后续"},
	}})
	if res.IsNew {
		t.Fatal("首句一致时应复用同一云端对话")
	}
	if res.MatchedBy != "prompt_prefix" {
		t.Fatalf("应由提示词指纹命中, got %q", res.MatchedBy)
	}
	// 内容已分叉，云端状态相对本次请求未知，必须发全量。只发最后一条会让模型
	// 丢失任务上下文与工作区描述，进而拒绝继续（"未提供执行桥/工作区未挂载"）。
	if res.HistoryLen != 0 {
		t.Fatalf("分叉后应发全量, want 0, got %d", res.HistoryLen)
	}
}

// 层 4 的锚点是首条 user 消息，不是 flatten 后的原始开头：agent 客户端的开头是
// 两万字节级的固定 system 指令，按原始前缀取指纹会把它的全部会话判成同一个。
func TestPromptPrefixIgnoresSharedSystemPreamble(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_PROMPT_PREFIX_CHARS", "50")
	sr := openSessionResolver()
	req := layeredRequest(nil)

	bigSystem := oaiMsg{Role: "system", Content: "You are Codex, a coding agent. " +
		"Follow these very long and entirely identical instructions in every single conversation."}

	sr.Bind("", "conv-task-a", "acc1",
		&oaiReq{Messages: []oaiMsg{bigSystem, {Role: "user", Content: "任务A：修复登录超时"}}},
		"回答A", req)

	// 同一客户端的另一个任务：system 完全相同，首句不同 -> 必须是新会话。
	res := sr.Resolve(req, &oaiReq{Messages: []oaiMsg{
		bigSystem, {Role: "user", Content: "任务B：升级依赖版本"},
	}})
	if !res.IsNew {
		t.Fatalf("system 相同但任务不同必须新建会话, got conv=%s matched=%s", res.ConversationID, res.MatchedBy)
	}
}

// 提示词指纹层同样要防跨客户端串号：多人共用一个 API key（或完全不启用鉴权）时，
// 只按租户隔离会让任何人打出同样首句就接进别人的对话。
func TestPromptPrefixIsolatedByClientFingerprint(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	alice := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	alice.RemoteAddr = "203.0.113.10:1111"
	alice.Header.Set("User-Agent", "client-a")

	bob := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	bob.RemoteAddr = "198.51.100.20:2222"
	bob.Header.Set("User-Agent", "client-b")

	sr.Bind("", "conv-alice", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}}, "回答", alice)

	res := sr.Resolve(bob, &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if !res.IsNew {
		t.Fatalf("不同客户端打出同样首句不得复用, got conv=%s matched=%s", res.ConversationID, res.MatchedBy)
	}
}

// M365_PROMPT_PREFIX_CHARS=0 关闭整层，只保留会话头与内容键。
func TestPromptPrefixLayerCanBeDisabled(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_PROMPT_PREFIX_CHARS", "0")
	sr := openSessionResolver()
	req := layeredRequest(nil)

	if promptPrefixKey([]oaiMsg{{Role: "user", Content: "x"}}, promptPrefixChars()) != "" {
		t.Fatal("关闭时不应产生指纹")
	}

	sr.Bind("", "conv-off", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "首句"},
			{Role: "user", Content: "原本的第二句"},
		}}, "回答", req)

	// 首句相同但第二句起就分叉：内容键不构成前缀，指纹层已关 -> 必须新建。
	// （只改首句之后的内容才能区分两层；仅首句相同时内容键本来就该命中。）
	res := sr.Resolve(req, &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "首句"},
		{Role: "user", Content: "换成完全不同的第二句"},
	}})
	if !res.IsNew {
		t.Fatalf("指纹层关闭后不应命中, matched=%s", res.MatchedBy)
	}
}
