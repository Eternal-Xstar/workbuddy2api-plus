// pin.go 人工指定账号（"只测这个号"）。
//
// 面板「试聊」现在可以锁定某个账号来发一条请求，用于单号排障：
// 这个号到底还能不能用？它是冷却了、还是模型被限、还是 token 挂了？
//
// 协议：请求头 `X-WB2API-Account: <uid>`。
//   - 命中时**只**用这个号，不再轮换（换号会让排查失去意义）；
//   - 忽略冷却/熔断/模型级负缓存——人工测试的目的正是探测该号当前的真实状态；
//   - 但仍拒绝「人工禁用/暂停」与「在途占满」（那是人自己的意图或资源上限）；
//   - 该号失败直接把它这次的真实上游错误回给客户端，不再兜底换号。
package server

import (
	"net/http"
	"strings"
)

// PinHeader 指定账号的请求头名。
const PinHeader = "X-WB2API-Account"

// pinUIDOf 取请求里的指定账号 uid（去空白）。空 = 未指定。
func pinUIDOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(PinHeader))
}
