package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func historyProbeRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.1.2.3:9999"
	r.Header.Set("User-Agent", "history-probe")
	return r
}

// D 的回归：ContextHistory 末项必须是模型回复。曾经这里被传入请求侧的 flatten
// 文本，导致客户端下一轮回传真实回复时前缀比对必然失配、内容键复用永不命中。
func TestBindStoresAnswerNotPrompt(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := historyProbeRequest()

	answer := "这是模型的回答"
	sr.Bind("sess-1", "conv-1", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "问题一"},
		}},
		answer, req)

	sess, ok := sr.GetSession("sess-1")
	if !ok {
		t.Fatal("会话未登记")
	}
	if len(sess.ContextHistory) != 3 {
		t.Fatalf("期望 2 条请求消息 + 1 条回复，got %d", len(sess.ContextHistory))
	}
	last := sess.ContextHistory[2]
	if last.Role != "assistant" {
		t.Fatalf("末项角色应为 assistant, got %s", last.Role)
	}
	if got := contentToString(last.Content); got != answer {
		t.Fatalf("末项应为模型回复 %q, got %q", answer, got)
	}
	if strings.Contains(contentToString(last.Content), "[system]") {
		t.Fatal("末项含 flatten 标记，说明存入的是请求文本而非回复")
	}
}

// D 的端到端效果：存对回复后，客户端回传该回复的下一轮必须命中前缀，
// 且 HistoryLen 恰好等于云端已有的条数。
func TestPrefixMatchWorksAfterCorrectBind(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := historyProbeRequest()

	sr.Bind("", "conv-1", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "问题一"},
		}},
		"回答一", req)

	// 客户端按 OpenAI 惯例把助手回复原样回传，再追加新问题。
	res := sr.Resolve(req, &oaiReq{Messages: []oaiMsg{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "问题一"},
		{Role: "assistant", Content: "回答一"},
		{Role: "user", Content: "问题二"},
	}})
	if res.IsNew {
		t.Fatal("回传真实回复的续轮请求必须命中已有云端对话")
	}
	if res.ConversationID != "conv-1" {
		t.Fatalf("应命中 conv-1, got %s", res.ConversationID)
	}
	if res.HistoryLen != 3 {
		t.Fatalf("云端已有 3 条，增量起点应为 3, got %d", res.HistoryLen)
	}
}

// B 的回归：仅尾部相同、不构成严格前缀时必须新建会话。旧实现会走后缀匹配并把
// 尾部匹配条数当作前缀长度返回，上层据此切掉开头含 system 提示的若干条。
func TestSuffixOnlyOverlapDoesNotReuse(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := historyProbeRequest()

	sr.Bind("", "conv-old", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "很早的问题"},
			{Role: "user", Content: "共同尾部一"},
		}},
		"共同尾部二", req)

	// 开头完全不同，只有末尾两条与已记录会话相同（模拟客户端压缩上下文）。
	res := sr.Resolve(req, &oaiReq{Messages: []oaiMsg{
		{Role: "system", Content: "完全不同的系统提示"},
		{Role: "user", Content: "共同尾部一"},
		{Role: "assistant", Content: "共同尾部二"},
	}})
	if !res.IsNew {
		t.Fatalf("仅尾部重合不得复用云端对话, matched=%s conv=%s history=%d",
			res.MatchedBy, res.ConversationID, res.HistoryLen)
	}
}

// E/L 的回归：超过上限的历史保留尾部片段供管理界面展示，但必须被标记为截断
// 并排除在所有前缀匹配之外——片段不是任何消息序列的前缀，拿它比对必然错位。
func TestOversizedHistoryKeepsTailButBlocksReuse(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := historyProbeRequest()

	msgs := make([]oaiMsg, maxContextHistory+5)
	for i := range msgs {
		msgs[i] = oaiMsg{Role: "user", Content: strings.Repeat("x", 8) + string(rune('a'+i%26))}
	}
	sr.Bind("sess-big", "conv-big", "acc1", &oaiReq{Messages: msgs}, "回答", req)

	sess, ok := sr.GetSession("sess-big")
	if !ok {
		t.Fatal("会话本身仍应登记，供显式续接与清理保护使用")
	}
	if !sess.HistoryTruncated {
		t.Fatal("超限历史必须标记 HistoryTruncated")
	}
	// 管理界面依赖这份历史展示对话内容，不能是空的。
	if len(sess.ContextHistory) == 0 {
		t.Fatal("应保留尾部片段供详情页展示")
	}
	if len(sess.ContextHistory) > maxContextHistory+1 {
		t.Fatalf("片段不应超过上限+1条回复, got %d", len(sess.ContextHistory))
	}

	// 截断会话不得被内容键匹配命中。
	if res := sr.Resolve(req, &oaiReq{Messages: msgs}); !res.IsNew {
		t.Fatalf("截断会话不应被内容键命中, matched=%s", res.MatchedBy)
	}
	// 也不得被 user/conv-id 路径的核对通过。
	if n, ok := sr.ConversationPrefixLen("conv-big", msgs); ok {
		t.Fatalf("截断会话不应核对通过, got n=%d", n)
	}

	// 显式续接仍可用，但增量起点必须归零（发全量而非错位增量）。
	explicitReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	explicitReq.Header.Set(sessionHeaderName, "sess-big")
	res := sr.Resolve(explicitReq, &oaiReq{Messages: msgs})
	if res.IsNew || res.MatchedBy != "explicit" {
		t.Fatalf("显式续接应命中, got new=%v matched=%s", res.IsNew, res.MatchedBy)
	}
	if res.HistoryLen != 0 {
		t.Fatalf("截断会话的 HistoryLen 必须为 0 以发送全量, got %d", res.HistoryLen)
	}
}

// 重复 Bind 同一会话时，截断标记必须跟着当轮历史重新计算。旧实现只在新建和
// 按 conversationID 去重两条路径上赋值，走 sessionID 命中分支时会留下上一轮的
// 陈旧标记：历史涨过上限后标记仍是 false，片段又会被当作前缀参与比对。
func TestRebindRecomputesTruncationFlag(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := historyProbeRequest()

	short := []oaiMsg{{Role: "user", Content: "短历史"}}
	sr.Bind("sess-x", "conv-x", "acc1", &oaiReq{Messages: short}, "回答", req)
	if sess, _ := sr.GetSession("sess-x"); sess.HistoryTruncated {
		t.Fatal("短历史不应标记截断")
	}

	// 同一会话主键再次 Bind，这轮消息超出上限。
	long := make([]oaiMsg, maxContextHistory+3)
	for i := range long {
		long[i] = oaiMsg{Role: "user", Content: "消息" + string(rune('a'+i%26))}
	}
	sr.Bind("sess-x", "conv-x", "acc1", &oaiReq{Messages: long}, "回答", req)
	sess, ok := sr.GetSession("sess-x")
	if !ok {
		t.Fatal("会话应仍存在")
	}
	if !sess.HistoryTruncated {
		t.Fatal("历史涨过上限后必须重新标记为截断，否则片段会被当作前缀比对")
	}

	// 再缩回短历史，标记必须清掉，否则该会话永久失去复用能力。
	sr.Bind("sess-x", "conv-x", "acc1", &oaiReq{Messages: short}, "回答", req)
	if sess, _ := sr.GetSession("sess-x"); sess.HistoryTruncated {
		t.Fatal("历史缩回上限内后必须清除截断标记")
	}
}

// N 的回归：客户端消息被云端对话完整包含时（没有新增内容），增量起点必须被夹到
// len-1。原样返回会等于消息总数，落到上层 "HistoryLen < len(Messages)" 守卫之外，
// 于是整段历史被重新灌进一个已存着同样内容的对话里。
func TestIncrementStartClampsWhenNothingNew(t *testing.T) {
	cases := []struct {
		historyLen, total, want int
	}{
		{0, 3, 0},  // 无历史，全量
		{2, 3, 2},  // 正常增量
		{3, 3, 2},  // 完整包含 -> 夹到 len-1
		{9, 3, 2},  // 历史比消息还长（错位）-> 同样夹住
		{2, 0, 0},  // 空消息
		{-1, 3, 0}, // 防御性下界
	}
	for _, c := range cases {
		if got := incrementStart(c.historyLen, c.total); got != c.want {
			t.Errorf("incrementStart(%d,%d)=%d, want %d", c.historyLen, c.total, got, c.want)
		}
	}
}

// 端到端：客户端把含助手回复的完整历史原样重发（没有新的用户消息）时，
// 复用仍可命中，但增量起点不得等于消息总数，否则整段历史会被重复发送。
func TestFullyContainedResendDoesNotResendEverything(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := historyProbeRequest()

	msgs := []oaiMsg{
		{Role: "user", Content: "问题一"},
		{Role: "assistant", Content: "回答一"},
	}
	// Bind 后云端历史恰好等于 msgs（请求消息 + 回复）。
	sr.Bind("", "conv-1", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "问题一"}}},
		"回答一", req)

	res := sr.Resolve(req, &oaiReq{Messages: msgs})
	if res.IsNew {
		t.Fatal("完整包含的重发仍应命中已有对话")
	}
	if res.HistoryLen >= len(msgs) {
		t.Fatalf("增量起点不得等于消息总数（会导致全量重发）, got %d, total %d", res.HistoryLen, len(msgs))
	}
}

// 详情接口必须能显示超长对话的内容，否则 E 的修法会让管理界面丢失可见性。
func TestConversationDetailShowsTruncatedHistory(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := historyProbeRequest()

	msgs := make([]oaiMsg, maxContextHistory+10)
	for i := range msgs {
		msgs[i] = oaiMsg{Role: "user", Content: "消息" + string(rune('a'+i%26))}
	}
	sr.Bind("", "conv-long", "acc1", &oaiReq{Messages: msgs}, "最后的回答", req)

	sess, found := sr.GetConversation("conv-long")
	if !found {
		t.Fatal("详情接口应能查到该对话")
	}
	if len(sess.ContextHistory) == 0 {
		t.Fatal("详情页会显示 0 条消息——超长对话在界面上不可见")
	}
	last := sess.ContextHistory[len(sess.ContextHistory)-1]
	if contentToString(last.Content) != "最后的回答" {
		t.Fatalf("末项应是模型回复, got %q", contentToString(last.Content))
	}
}
