package web

import (
	"os"
	"strings"
)

// conversationReuseEnabled 控制网关是否把请求隐式绑定到已存在的 M365 云端对话。
//
// 复用是延迟优化：命中后云端对话已含全部历史，只需发送增量，端到端从 3-5s 降到
// ~1s。代价是命中判定错误时模型会带着别的上下文作答，而且增量切片可能丢掉开头的
// system/历史消息——症状表现为"答非所问"或"回答里混进了另一个对话的内容"。
// M365_CONV_REUSE=0 关掉全部隐式复用，让每个请求都新建云端对话，用于隔离这类问题。
//
// 调用方显式指定的续接不受此开关影响：session_key 字段与 X-M365-Session-Id 头是
// 调用方主动声明"继续这个对话"，不属于网关的隐式推断。
func conversationReuseEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("M365_CONV_REUSE"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}
