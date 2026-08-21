package web

// 这个文件曾承载 conv-cache：按 account+model+系统提示哈希缓存"上一个云端对话"，
// 供下一轮请求直接复用以省掉建对话的 3-5s。它已被整体移除。
//
// 移除的原因不是它没用，而是它不可能正当地起作用。复用判定的顺序是
// sessionResolver 先跑、conv-cache 兜底，而 conv-cache 分支只在 Resolve 未命中时
// 才执行。Resolve 未命中意味着没有任何会话历史构成当前消息的严格前缀——唯一的
// 例外是 Resolve 还额外要求 IP/UA 指纹一致。于是 conv-cache 能补上的命中恰好只有
// 指纹不一致的那些，而那正是 sessionResolver 为防跨客户端串号故意拒绝的情形：
// 缓存键里没有任何客户端身份（同一 API key + 同一模型 + 同一系统提示在多客户端
// 场景下完全重合），保留它等于开一条绕过身份隔离的旁路。
//
// 云端对话复用现在只有三条路径，各自都有明确的身份依据：
//   - X-M365-Session-Id / session_key：调用方显式声明要续接哪个对话
//   - user 字段映射：调用方声明的用户身份，命中后仍用会话历史核对增量边界
//   - 内容键（sessionResolver）：历史前缀 + IP/UA 指纹双重匹配

// StartConvCacheGC 保留为空实现：main.go 在启动序列里调用它，而 conv-cache 已移除。
// 留一个显式的空壳比让启动流程少一步更清楚——将来若再引入缓存，挂载点还在。
func (s *Server) StartConvCacheGC() {}

// RefreshExpiredTokens 在启动时把已过期的账号 token 刷一遍，避免首个请求撞上过期。
func (s *Server) RefreshExpiredTokens() {
	if s == nil || s.tokens == nil {
		return
	}
	s.tokens.RefreshAllExpired()
}
