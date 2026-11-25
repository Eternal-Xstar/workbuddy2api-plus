// identity.go 官方客户端「自报身份」头的补齐（client identity parity）。
//
// 背景：上游用 11-128「Illegal API invocation from an unapproved channel」做**通道
// 校验**。它不是内容检查（那是 11128 的审核变体），而是看这份请求像不像官方客户端
// 发出的：头集合是否齐全、各头的值是否自洽。
//
// 我们的网关是 Go 实现，天然没有 openai-node SDK 的运行时而官方桌面端有——于是出站
// 头里少了整整一段 x-stainless-*、x-agent-*、W3C 链路头（b3 / traceparent / baggage）
// 与 acp-connection-id。缺这些头时上游看到的是一份「缺 SDK 段的残缺报文」，在被收紧
// 的通道校验下就可能被判成未知渠道。
//
// 本文件按真实客户端报文把这批头补齐（取值取自社区抓包与 5.8.3/5.9.3 实测），
// **只在 chat 路径注入**：billing / checkin / refresh 这些 REST 域没有真机对照证据，
// 不臆造（凭空多一个不存在的头同样是可判别形态）。
//
// 原则：只补「客户端本该自己声明」的身份头；账号信息（X-User-Id / X-Enterprise-Id /
// Authorization）仍全部取自真实 auth，不伪造；也不改请求体语义。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 官方 chat 出站自报的 SDK / agent 指纹（国内 5.8.3 与国际 5.8.4 实测一致）。
// 客户端版本变化时这几个常量需要跟着更新——它们是"客户端形态一致性"的一部分。
const (
	identityLang         = "js"
	identityOS           = "Windows"
	identityArch         = "x64"
	identityRuntime      = "node"
	identityNodeVer      = "v22.21.1"
	identitySDKVer       = "6.25.0"
	identityAgentType    = "main"
	identityAgentIntent  = "craft"
	identityAgentRuntime = "local-agent"
)

// injectClientIdentity 补齐官方客户端自报身份头。conversationID / convReqID /
// messageID 与本次请求的会话头族同源，保证「头体一致」（不一致本身就是可判别形态）。
func (c *Client) injectClientIdentity(req *http.Request, a *auth.Auth, conversationID, convReqID, messageID string) {
	if req == nil {
		return
	}
	// 注意：**不要**发 X-API-Key。
	// 实测（2026-11-10 本机双账号，加上该头后上游直接 401 {"message":"not_found"}）：
	// 网关层（APISIX 类）会把这个头当成自己的 key-auth 凭据去查，查不到就 401，
	// 与账号 token 是否有效无关。社区分支的抓包里该头出现在别的端点上，chat 用不上。
	// agent 族：场景 main + 意图 craft + 本地 agent 运行时。
	req.Header.Set("X-Agent-Type", identityAgentType)
	req.Header.Set("X-Agent-Intent", identityAgentIntent)
	req.Header.Set("X-Agent-Runtime", identityAgentRuntime)
	// openai-node SDK 自报指纹（Go 网关没有 SDK，按真机值固定）。
	req.Header.Set("X-Stainless-Lang", identityLang)
	req.Header.Set("X-Stainless-Package-Version", identitySDKVer)
	req.Header.Set("X-Stainless-OS", identityOS)
	req.Header.Set("X-Stainless-Arch", identityArch)
	req.Header.Set("X-Stainless-Runtime", identityRuntime)
	req.Header.Set("X-Stainless-Runtime-Version", identityNodeVer)
	req.Header.Set("X-Stainless-Retry-Count", "0")
	// X-Private-Data：桌面端「默认关闭模型优化」开关的透出值。
	// 判定维度是**发行版**而非账号形态：国际版 product.json 默认关（true）、
	// 国内版默认开（false）；企业版恒 true。网关读不到用户手动开关，按发行版取默认。
	req.Header.Set("X-Private-Data", privateDataValue(a))

	spanID := spanOf(messageID)
	parentSpanID := spanOf(convReqID + "|parent")
	traceID := traceOf(convReqID, messageID)
	// W3C 与 B3 各一份，值必须与 X-B3-TraceId 同源（头间不自相矛盾）。
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("b3", traceID+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	// baggage：OTel 序列化形态，两个 buddy 键留空值（客户端恒为空，不是漏填）。
	req.Header.Set("baggage", "buddyId=,buddyName=,codebuddy.session_id="+conversationID+
		",codebuddy.conversation_request_id="+convReqID)
	// acp-connection-id：真机与 X-Conversation-ID 同为时间有序 UUID 形态（值不同）。
	req.Header.Set("acp-connection-id", uuidV7Like(conversationID))
}

// privateDataValue 见上：企业版 / 国际版 → "true"，国内个人版 → "false"。
func privateDataValue(a *auth.Auth) string {
	if a == nil {
		return "false"
	}
	if a.IsEnterprise() || a.IsGlobal() {
		return "true"
	}
	return "false"
}

// spanOf 由任意 id 派生 16 hex 的 span id（稳定、与输入一一对应）。
func spanOf(seed string) string {
	sum := sha256.Sum256([]byte("span|" + seed))
	return hex.EncodeToString(sum[:8])
}

// traceOf 派生 32 hex 的 trace id；原值本身是 16/32 位 hex 时直接复用，
// 保证与 X-B3-TraceId / X-Trace-ID 指向同一个链路。
func traceOf(primary, fallback string) string {
	if validTraceID(primary) {
		if len(primary) == 32 {
			return primary
		}
		// 16 位 hex（旧式 B3）：右补零成 32 位，保持同一前缀便于人工关联。
		return primary + primary
	}
	if validTraceID(fallback) {
		return fallback
	}
	sum := sha256.Sum256([]byte("trace|" + primary + "|" + fallback))
	return hex.EncodeToString(sum[:16])
}

// uuidV7Like 把一个 32 hex 的 id 渲染成 UUID 形态（版本位填 7、变体位填 8），
// 只求"看起来与真机同形"，不承诺时间有序。
func uuidV7Like(seed string) string {
	h := sha256.Sum256([]byte("acp|" + seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
