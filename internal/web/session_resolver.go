package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// sessionBinding 记录一次内容键复用的会话。
//
// 参与匹配判定的只有两项：IPFingerprint（跨客户端隔离）与 ContextHistory（内容
// 前缀比对）。UserField 与 ContextFinger 现在只是 /v1/sessions 上的诊断元数据，
// 没有任何逻辑读取它们——曾经的三个反查索引已连同它们的查询路径一起移除。
type sessionBinding struct {
	SessionID      string    `json:"sessionId"`
	ConversationID string    `json:"conversationId"`
	AccountID      string    `json:"accountId"`
	CreatedAt      time.Time `json:"createdAt"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
	IPFingerprint  string    `json:"ipFingerprint,omitempty"`
	UserField      string    `json:"userField,omitempty"`
	ContextFinger  string    `json:"contextFinger,omitempty"`
	// ContextHistory 持久化保存最近一次协议的消息，供重启后继续做内容前缀匹配，
	// 避免进程重启导致所有会话键全部失效。
	ContextHistory []oaiMsg `json:"contextHistory,omitempty"`
	// HistoryTruncated 表示 ContextHistory 只是尾部片段（消息数超过 maxContextHistory）。
	// 片段不是任何消息序列的前缀，拿它做前缀比对会从第一条起就错位，所以匹配路径
	// 必须跳过这类会话；但管理界面仍要能看到最近的对话内容，故保留片段而非丢弃。
	HistoryTruncated bool `json:"historyTruncated,omitempty"`
	// ClientPrefixLen 是 ContextHistory 里"客户端逐字发来"的前缀长度，其后是网关
	// 追加的本轮模型回复。前缀比对只允许用这一段：客户端下一轮必然逐字重发自己
	// 的消息，而模型回复会被它按自己的格式重新渲染——工具轮尤其明显，客户端回传
	// 的是 content:null + tool_calls，与网关存下的纯文本对不上，逐字比对必然失配。
	ClientPrefixLen int `json:"clientPrefixLen,omitempty"`
}

type sessionResolver struct {
	mu          sync.Mutex
	path        string
	sessions    map[string]sessionBinding
	ttl         time.Duration
	contextTTL  time.Duration
	maxSessions int
	persist     *persistStore
}

const defaultMaxSessions = 1000

func openSessionResolver() *sessionResolver {
	// 闂茬疆 2 灏忔椂鍗宠涓鸿繃鏈燂紙鐢ㄦ埛锛? 灏忔椂涓嶆椿璺冨凡缁忕畻涔咃級銆備細璇濊繃鏈熷悗
	// 浠?sessions.json 鍓旈櫎锛屼簯绔璇濅氦缁?auto_cleanup 鎸夌浉鍚岀獥鍙ｅ洖鏀躲€?
	ttl := 2 * time.Hour
	if v := os.Getenv("M365_SESSION_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			ttl = d
		}
	}
	contextTTL := 2 * time.Hour
	if v := os.Getenv("M365_CONTEXT_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			contextTTL = d
		}
	}
	path := os.Getenv("M365_SESSION_CACHE")
	if path == "" {
		path = "sessions.json"
	}
	sr := &sessionResolver{
		path:        path,
		sessions:    map[string]sessionBinding{},
		ttl:         ttl,
		contextTTL:  contextTTL,
		maxSessions: defaultMaxSessions,
	}
	sr.persist = &persistStore{flush: sr.flush}
	sr.loadLocked()
	return sr
}

func (sr *sessionResolver) loadLocked() {
	b, err := os.ReadFile(sr.path)
	if err != nil {
		return
	}
	var list []sessionBinding
	if err := json.Unmarshal(b, &list); err != nil {
		// 该文件曾与 sessionStore 共用同一个环境变量，磁盘上可能留着对象格式的
		// 旧内容。静默忽略会让人以为绑定丢失是别的原因，这里说明清楚。
		log.Printf("[session-resolver] ignoring unparsable cache %s: %v", sr.path, err)
		return
	}
	now := time.Now().UTC()
	for _, s := range list {
		if now.Sub(s.LastUsedAt) > sr.ttl {
			continue
		}
		sr.reindexLocked(s)
	}
}

// flush 在锁内生成快照，锁外写盘。
func (sr *sessionResolver) flush() error {
	sr.mu.Lock()
	list := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		list = append(list, s)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	sr.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(sr.path, b, 0o600)
}

// reindexLocked 登记会话。曾经这里还维护 byUserField/byIPFinger/byContext 三个
// 反查索引，但它们只有写入和删除前的自比较，从来没有任何查询点——匹配逻辑
// （matchContextLocked）一直是直接遍历 sessions 的。三个 map × 上限 1000 条会话
// 的常驻开销换不到任何东西，已连同字段一起移除。
func (sr *sessionResolver) reindexLocked(s sessionBinding) {
	sr.sessions[s.SessionID] = s
}

func (sr *sessionResolver) evictLocked() {
	now := time.Now().UTC()
	for id, s := range sr.sessions {
		if now.Sub(s.LastUsedAt) > sr.ttl {
			sr.dropLocked(id)
		}
	}
	if len(sr.sessions) > sr.maxSessions {
		// Bound memory by dropping the least recently used sessions.
		ids := make([]string, 0, len(sr.sessions))
		last := make(map[string]time.Time, len(sr.sessions))
		for id, s := range sr.sessions {
			ids = append(ids, id)
			last[id] = s.LastUsedAt
		}
		sort.Slice(ids, func(i, j int) bool { return last[ids[i]].Before(last[ids[j]]) })
		for _, id := range ids[:len(sr.sessions)-sr.maxSessions] {
			sr.dropLocked(id)
		}
	}
}

func (sr *sessionResolver) dropLocked(id string) {
	delete(sr.sessions, id)
}

type ResolveResult struct {
	SessionID      string
	ConversationID string
	AccountID      string
	MatchedBy      string
	IsNew          bool
	// HistoryLen 鏄鐢ㄥ懡涓椂"浜戠瀵硅瘽宸插寘鍚殑娑堟伅鏉℃暟"锛?
	// 鍗冲閲忓彂閫佺殑璧风偣涓嬫爣锛坆ody.Messages[HistoryLen:] 鍙彂鏂板閮ㄥ垎锛夈€?
	HistoryLen int
}

func clientIPFingerprint(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ua := r.Header.Get("User-Agent")
	data := host + "|" + ua
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func contextFingerprint(messages []oaiMsg) string {
	if len(messages) == 0 {
		return ""
	}
	var parts []string
	limit := len(messages)
	if limit > 3 {
		limit = 3
	}
	for i := len(messages) - limit; i < len(messages); i++ {
		m := messages[i]
		parts = append(parts, m.Role+":"+contentToString(m.Content))
	}
	data := strings.Join(parts, "||")
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func (sr *sessionResolver) Resolve(r *http.Request, body *oaiReq) ResolveResult {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	explicitID := r.Header.Get("X-M365-Session-Id")

	// 客户端显式指定的会话 ID 是最高优先级的续接语义：不参与任何身份判定，
	// 由调用方主动决定要继续哪个云端对话。Bind 在 sessionID 为空时直接用这个
	// 头值当会话主键，所以按主键查一次即可。这里曾经还套了一层 byExplicit 索引，
	// 但那个 map 从未有过写入点、恒为空，查询永远走不到，已连同字段一起移除。
	if explicitID != "" {
		if sess, ok := sr.sessions[explicitID]; ok {
			sess.LastUsedAt = time.Now().UTC()
			sr.sessions[explicitID] = sess
			sr.persist.markDirty()
			// 历史被截断过时 len(ContextHistory) 不等于云端已有的条数，拿它当增量
			// 起点会切错位置。此时返回 0，让上层发送全量而不是错位的增量。
			historyLen := len(sess.ContextHistory)
			if sess.HistoryTruncated {
				historyLen = 0
			}
			historyLen = incrementStart(historyLen, len(body.Messages))
			return ResolveResult{
				SessionID:      sess.SessionID,
				ConversationID: sess.ConversationID,
				AccountID:      sess.AccountID,
				MatchedBy:      "explicit",
				IsNew:          false,
				HistoryLen:     historyLen,
			}
		}
	}

	// 鍐呭閿細鍗忚娑堟伅鍚嶅簭鍒椾弗鏍肩瓑浜庢煇涓凡璁板綍浼氳瘽鐨勫巻鍙叉椂鐩存帴澶嶇敤杩欎釜
	// 浜戠瀵硅瘽锛屼絾鍙湪鍚屼竴 IP/UA 鎸囩汗涓嬶紝閬垮厤鐭秷鎭湪涓嶅悓鐢ㄦ埛闂翠簰绔?
	// HistoryLen 杩斿洖璇ュ墠缂€闀垮害锛屼笂灞傛嵁姝ゅ彧鍙戦€?messages[HistoryLen:] 澧為噺銆?
	ipFinger := clientIPFingerprint(r)
	if bestID, n := sr.matchContextLocked(ipFinger, body.Messages); bestID != "" {
		sess := sr.sessions[bestID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[bestID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_prefix_%d", n),
			IsNew:          false,
			HistoryLen:     incrementStart(n, len(body.Messages)),
		}
	}

	// 内容不构成严格前缀时一律新建会话。历史上这里还有一层"后缀匹配"兜底
	// （客户端本地截断历史后仍复用该会话），但后缀命中意味着云端已有的消息
	// 落在客户端消息序列的中间或末尾，而增量发送只能表达"跳过前 N 条"这一种
	// 形状。任何 HistoryLen 取值都会导致重复发送或丢掉开头的 system 提示，
	// 因此该路径已移除：宁可多花一次建对话的延迟，也不交付错误的上下文。

	return ResolveResult{IsNew: true}
}

// matchContextLocked 浠庡叏閮ㄤ細璇濅腑鎵惧埌鍏?contextHistory 涓ユ牸浣滀负娑堟伅鍓嶇紑鐨?
// 閭ｄ釜浼氳瘽锛涘彧閫夊墠缂€鏈€闀跨殑涓€涓紝閬垮厤鐭墠缂€鍦ㄤ笉鍚屼細璇濋棿浜掓挒銆傝繑鍥?
// (sessionID, 鍖归厤鍒扮殑娑堟伅鏉℃暟)銆?
func (sr *sessionResolver) matchContextLocked(ipFinger string, messages []oaiMsg) (string, int) {
	if len(messages) == 0 {
		return "", 0
	}
	type match struct {
		id     string
		n      int
		recent time.Time
	}
	best := match{}
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		// 截断的历史只是尾部片段，不是任何消息序列的前缀，比对必然从第一条起错位。
		if sess.HistoryTruncated {
			continue
		}
		n := contextPrefixLen(sess.comparableHistory(), messages)
		if n < 1 {
			continue
		}
		n = skipAssistantEcho(n, messages)
		if n > best.n || (n == best.n && sess.LastUsedAt.After(best.recent)) {
			best = match{id: id, n: n, recent: sess.LastUsedAt}
		}
	}
	return best.id, best.n
}

// contextPrefixLen 杩斿洖 hist 鏄惁涓ユ牸鏄?msgs 鐨勫墠缂€銆俬ist 涓虹┖鎴栦笉鏄墠缂€
// 鏃惰繑鍥?0锛涘懡涓椂杩斿洖 len(hist)锛屽嵆澧為噺鍙戦€佽捣鐐广€?
// comparableHistory 返回可用于逐字比对的那一段历史：客户端自己发来的消息。
// ClientPrefixLen 为 0 的记录来自旧版本（字段是后加的），此时退回整段历史，
// 行为与升级前一致。
func (s sessionBinding) comparableHistory() []oaiMsg {
	if s.ClientPrefixLen <= 0 || s.ClientPrefixLen > len(s.ContextHistory) {
		return s.ContextHistory
	}
	return s.ContextHistory[:s.ClientPrefixLen]
}

// skipAssistantEcho 把增量起点推过客户端回传的那条模型回复。
//
// 云端对话在客户端消息之后还存着上一轮的模型回复，客户端下一轮会把它回传，
// 但渲染格式由客户端决定：工具调用轮回传的是 content:null + tool_calls，和网关
// 存下的纯文本对不上。所以这条消息一律不参与逐字比对，只靠 role 识别并跳过。
// 不跳过就会把它当成新增内容重新发一遍，云端对话里出现重复的助手轮。
// 位置上不是 assistant 时（客户端丢弃了回复，或直接追加了新提问）不跳，
// 否则会吞掉真正的新增消息。
func skipAssistantEcho(n int, msgs []oaiMsg) int {
	if n >= 0 && n < len(msgs) && msgs[n].Role == "assistant" {
		return n + 1
	}
	return n
}

// incrementStart 把"云端已有条数"夹成一个可用的增量起点下标。
//
// 上层按 messages[HistoryLen:] 切增量，且只在 HistoryLen < len(messages) 时才切。
// 当已有条数等于消息总数时（客户端发来的内容被云端对话完整包含，没有新增），
// 原样返回会落到那个守卫之外，于是整段历史被重新灌进一个已存着同样内容的对话，
// 造成上下文重复。这里夹到 len-1：至少留一条作为增量，把重复限制在一条消息内。
func incrementStart(historyLen, total int) int {
	if total <= 0 {
		return 0
	}
	if historyLen >= total {
		return total - 1
	}
	if historyLen < 0 {
		return 0
	}
	return historyLen
}

func contextPrefixLen(hist, msgs []oaiMsg) int {
	if len(hist) == 0 || len(msgs) < len(hist) {
		return 0
	}
	for i := range hist {
		if !messagesEqual(hist[i], msgs[i]) {
			return 0
		}
	}
	return len(hist)
}

// messagesEqual 鍒ゅ畾涓ゆ潯娑堟伅鍦ㄤ細璇濋敭鎰忎箟涓婄瓑浠凤細role 涓庢枃鏈唴瀹逛竴鑷淬€?
// 蹇界暐 tool_calls 鐨?ID 缁嗚妭锛堜細璇濋敭鍙叧蹇冨唴瀹瑰浣曡妯″瀷娑堝寲锛夈€?
func messagesEqual(a, b oaiMsg) bool {
	if a.Role != b.Role {
		return false
	}
	ta := contentToString(a.Content)
	tb := contentToString(b.Content)
	if ta != tb {
		return false
	}
	if (a.ToolCalls == nil) != (b.ToolCalls == nil) {
		return false
	}
	for i := range a.ToolCalls {
		if i >= len(b.ToolCalls) {
			return false
		}
		if toolCallEqual(a.ToolCalls[i], b.ToolCalls[i]) {
			continue
		}
		return false
	}
	return len(a.ToolCalls) == len(b.ToolCalls)
}

// toolCallEqual 比较 name 与 arguments，忽略 ID：同一段工具调用重放时
// ID 由客户端重新生成，不应影响会话键。
func toolCallEqual(x, y map[string]any) bool {
	xFunc, _ := x["function"].(map[string]any)
	yFunc, _ := y["function"].(map[string]any)
	xn, _ := xFunc["name"].(string)
	yn, _ := yFunc["name"].(string)
	if xn != yn {
		return false
	}
	xa, _ := xFunc["arguments"].(string)
	ya, _ := yFunc["arguments"].(string)
	return xa == ya
}

// Bind 登记一轮完成的对话。assistantText 必须是模型这一轮的回复：它会作为
// assistant 消息追加进 ContextHistory，客户端下一轮回传同一条回复时前缀比对
// 才能命中。这里曾用 args ...any + 类型 switch 取参，调用方误传请求侧的 flatten
// 文本也不会报错，历史末项因此长期是请求文本的副本、复用永不命中。签名改为
// 显式参数，让这类错传变成编译错误。
func (sr *sessionResolver) Bind(sessionID, conversationID, accountID string, body *oaiReq, assistantText string, r *http.Request) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	now := time.Now().UTC()
	history, truncated := cloneMessages(body.Messages)
	// 记录客户端逐字消息的边界，再把本轮回复追加进去。回复只服务于管理界面展示
	// 与"云端已比客户端多一轮"这个事实，不参与逐字比对。
	clientPrefixLen := len(history)
	if strings.TrimSpace(assistantText) != "" {
		history = append(history, oaiMsg{Role: "assistant", Content: assistantText})
	}
	explicitID := r.Header.Get("X-M365-Session-Id")
	if explicitID != "" && sessionID == "" {
		sessionID = explicitID
	}
	// 同一云端对话只保留一条记录：内容键命中后增量轮次更新已存在会话，
	// 而不是每次 Bind 都新建一条，避免 sessions.json 膨胀。
	if sessionID != "" {
		if sess, ok := sr.sessions[sessionID]; ok {
			sess.ConversationID = conversationID
			sess.AccountID = accountID
			sess.LastUsedAt = now
			sess.UserField = body.User
			sess.IPFingerprint = clientIPFingerprint(r)
			sess.ContextFinger = contextFingerprint(history)
			sess.ContextHistory = history
			sess.HistoryTruncated = truncated
			sess.ClientPrefixLen = clientPrefixLen
			sr.sessions[sessionID] = sess
			sr.reindexLocked(sess)
			sr.persist.markDirty()
			return
		}
	}
	if sessionID == "" {
		for sid, sess := range sr.sessions {
			if sess.ConversationID == conversationID {
				sess.LastUsedAt = now
				sess.AccountID = accountID
				sess.UserField = body.User
				sess.IPFingerprint = clientIPFingerprint(r)
				sess.ContextFinger = contextFingerprint(history)
				sess.ContextHistory = history
				sess.HistoryTruncated = truncated
				sess.ClientPrefixLen = clientPrefixLen
				sr.sessions[sid] = sess
				sr.reindexLocked(sess)
				sr.persist.markDirty()
				return
			}
		}
		sessionID = uuid.NewString()
	}

	sess := sessionBinding{
		SessionID:        sessionID,
		ConversationID:   conversationID,
		AccountID:        accountID,
		CreatedAt:        now,
		LastUsedAt:       now,
		IPFingerprint:    clientIPFingerprint(r),
		UserField:        body.User,
		ContextFinger:    contextFingerprint(history),
		ContextHistory:   history,
		HistoryTruncated: truncated,
		ClientPrefixLen:  clientPrefixLen,
	}

	sr.reindexLocked(sess)
	sr.persist.markDirty()
}

func (sr *sessionResolver) GetSession(sessionID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	s, ok := sr.sessions[sessionID]
	return s, ok
}

func (sr *sessionResolver) GetConversation(conversationID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	for _, session := range sr.sessions {
		if session.ConversationID == conversationID {
			// 防御性拷贝，避免调用方改到锁内的切片。这里不能复用 cloneMessages：
			// 它带 maxContextHistory 上限，会把已经存好的历史再截一次。
			out := make([]oaiMsg, len(session.ContextHistory))
			copy(out, session.ContextHistory)
			session.ContextHistory = out
			return session, true
		}
	}
	return sessionBinding{}, false
}

func (sr *sessionResolver) ListSessions() []sessionBinding {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	return out
}

// ConversationPrefixLen 判断 messages 是否以该云端对话已有的历史为严格前缀，
// 命中时返回云端已有的消息条数（即增量发送的起点下标）。
//
// 供那些只知道"该复用哪个对话"、却不知道云端已有什么内容的复用路径使用
// （例如按 user 字段索引的映射）。查不到历史或内容不构成前缀时返回 (0,false)：
// 此时必须放弃复用，因为猜错增量边界会把已有历史重发一遍或漏掉开头几条。
func (sr *sessionResolver) ConversationPrefixLen(conversationID string, messages []oaiMsg) (int, bool) {
	if conversationID == "" || len(messages) == 0 {
		return 0, false
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	best := 0
	for _, sess := range sr.sessions {
		if sess.ConversationID != conversationID {
			continue
		}
		if sess.HistoryTruncated {
			continue
		}
		if n := contextPrefixLen(sess.comparableHistory(), messages); n >= 1 {
			if n = skipAssistantEcho(n, messages); n > best {
				best = n
			}
		}
	}
	return best, best > 0
}

func (sr *sessionResolver) DeleteSession(sessionID string) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if _, ok := sr.sessions[sessionID]; !ok {
		return false
	}
	sr.dropLocked(sessionID)
	sr.persist.markDirty()
	return true
}

// UnbindByConversation drops every session bound to the given conversation.
// Called after an automatic cleanup deletes the cloud conversation, so the
// anti-CrossID resolver never reuses a dead conversation.
func (sr *sessionResolver) UnbindByConversation(conversationID string) int {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	removed := 0
	for sid, s := range sr.sessions {
		if s.ConversationID != conversationID {
			continue
		}
		sr.dropLocked(sid)
		removed++
	}
	if removed > 0 {
		sr.persist.markDirty()
	}
	return removed
}

// maxContextHistory 限制单条会话在内存/磁盘上保留的消息条数。上限存在的原因是
// 1000 条会话 × 完整历史会把常驻内存推到不可接受的量级。
const maxContextHistory = 128

// cloneMessages 复制消息用于会话历史，第二个返回值表示是否发生了截断。
//
// 超限时保留尾部片段并标记 truncated：片段对前缀比对毫无用处（contextPrefixLen
// 会从第一条起就失配），所以匹配路径必须靠这个标记跳过该会话；但管理界面的对话
// 详情要靠这份历史展示内容，整条丢弃会让超长对话在界面上变成 0 条消息。
// 于是分开处理：留着给人看，标记起来不给匹配用。
func cloneMessages(msgs []oaiMsg) ([]oaiMsg, bool) {
	truncated := false
	if len(msgs) > maxContextHistory {
		log.Printf("[session-resolver] history %d exceeds cap %d, keeping tail for display and excluding this session from prefix matching", len(msgs), maxContextHistory)
		msgs = msgs[len(msgs)-maxContextHistory:]
		truncated = true
	}
	out := make([]oaiMsg, len(msgs))
	copy(out, msgs)
	return out, truncated
}
