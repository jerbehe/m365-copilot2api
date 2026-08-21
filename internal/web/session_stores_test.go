package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// A / convCache 的共同前提：只知道"该复用哪个对话"的路径，必须能核对
// 客户端消息是否真的以该对话已有内容为前缀，并给出准确的增量起点。
func TestConversationPrefixLenVerifiesBeforeReuse(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	sr.Bind("", "conv-1", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "问题一"},
		}},
		"回答一", req)

	// 真续轮：云端已有 3 条，增量从下标 3 开始。
	n, ok := sr.ConversationPrefixLen("conv-1", []oaiMsg{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "问题一"},
		{Role: "assistant", Content: "回答一"},
		{Role: "user", Content: "问题二"},
	})
	if !ok || n != 3 {
		t.Fatalf("续轮应核对通过且 n=3, got n=%d ok=%v", n, ok)
	}

	// 另一个对话：系统提示相同但内容分叉，必须拒绝复用。
	if n, ok := sr.ConversationPrefixLen("conv-1", []oaiMsg{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "完全不同的问题"},
	}); ok {
		t.Fatalf("内容分叉不得核对通过, got n=%d", n)
	}

	// 未知对话 ID 一律拒绝。
	if _, ok := sr.ConversationPrefixLen("conv-unknown", []oaiMsg{{Role: "user", Content: "x"}}); ok {
		t.Fatal("未知对话不得核对通过")
	}
}

// H 的回归：同一对话多轮复用只刷新 LastUsedAt，CreatedAt 必须保持首次记录的值，
// 否则 max_age 模式下持续活跃的对话永远不会过期。
func TestRecordPreservesCreatedAt(t *testing.T) {
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	cm := openConversationManager()

	cm.Record("conv-x", "acc1", "第一轮标题")
	var created time.Time
	for _, c := range cm.List() {
		if c.ID == "conv-x" {
			created = c.CreatedAt
		}
	}
	if created.IsZero() {
		t.Fatal("首轮未记录 CreatedAt")
	}

	// 人为把 CreatedAt 推到过去，再复用一轮：它不应被重置成当前时间。
	cm.mu.Lock()
	entry := cm.data["conv-x"]
	entry.CreatedAt = created.Add(-3 * time.Hour)
	cm.data["conv-x"] = entry
	aged := entry.CreatedAt
	cm.mu.Unlock()

	cm.Record("conv-x", "acc1", "第二轮标题")

	for _, c := range cm.List() {
		if c.ID != "conv-x" {
			continue
		}
		if !c.CreatedAt.Equal(aged) {
			t.Fatalf("CreatedAt 被重置: want %v, got %v", aged, c.CreatedAt)
		}
		if !c.LastUsedAt.After(aged) {
			t.Fatal("LastUsedAt 未刷新")
		}
		if c.Title != "第二轮标题" {
			t.Fatalf("标题应更新, got %q", c.Title)
		}
		return
	}
	t.Fatal("conv-x 记录丢失")
}

// H 的直接后果：CreatedAt 不被重置后，max_age 模式才能回收长期活跃的老对话。
func TestMaxAgeCleanupReclaimsLongLivedConversation(t *testing.T) {
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_CLEANUP_MODE", string(CleanupMaxAge))
	t.Setenv("M365_CLEANUP_MAX_AGE_HOURS", "1")
	cm := openConversationManager()

	cm.Record("conv-old", "acc1", "标题")
	cm.mu.Lock()
	entry := cm.data["conv-old"]
	entry.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	cm.data["conv-old"] = entry
	cm.mu.Unlock()

	// 又用了一轮：旧实现会把 CreatedAt 刷成现在，从此永不过期。
	cm.Record("conv-old", "acc1", "标题")

	cleaned := cm.Cleanup()
	for _, id := range cleaned {
		if id == "conv-old" {
			return
		}
	}
	t.Fatalf("超过 max_age 的对话应被回收, cleaned=%v", cleaned)
}

// C 的回归：session_key 存储与 sessionResolver 不得共用同一个文件路径。
func TestSessionStoresUseSeparateFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_SESSION_KEY_CACHE", "")
	t.Setenv("M365_USER_SESSION_CACHE", "")
	t.Setenv("M365_DATA_DIR", dir)

	resolver := openSessionResolver()
	keys := openSessionStore()
	users := openUserSessionStore(time.Hour)

	if keys.path == resolver.path {
		t.Fatalf("session_key 存储与 resolver 共用了 %s，两者格式不兼容会互相覆盖", keys.path)
	}
	if users.path == resolver.path {
		t.Fatalf("user session 存储与 resolver 共用了 %s", users.path)
	}
	if keys.path == users.path {
		t.Fatalf("session_key 与 user session 共用了 %s", keys.path)
	}
}

// C 的加固：磁盘上留着对方格式的旧内容时，必须干净地从空表开始。
func TestSessionStoreIgnoresForeignFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-keys.json")
	// resolver 的数组格式，不是 sessionStore 的对象 map。
	if err := writeFileAtomic(path, []byte(`[{"sessionId":"a","conversationId":"b"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_SESSION_KEY_CACHE", path)

	store := openSessionStore()
	if got := len(store.list()); got != 0 {
		t.Fatalf("异构格式应被忽略, got %d 条", got)
	}
	// 仍可正常写入。
	store.upsert(conversation{ID: "k1", ConversationID: "c1"})
	if _, ok := store.get("k1"); !ok {
		t.Fatal("忽略旧内容后应能正常写入")
	}
}
