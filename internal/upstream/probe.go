// probe.go 主动配额探针（quota scan 的上游出口）。
//
// 为什么要单独一条出口：ChatStreamContext 走的是"给客户端转发 SSE"的热路径，
// 它只回 (reader, status, body, err)，响应头在函数内就丢了；而配额扫描最需要
// 的恰恰是响应头里的重置时刻（retry-after / x-ratelimit-reset，见
// ParseRetryAfter）。为了这一条需求去改热路径的返回签名，等于给每个真实请求
// 多加一个返回值；单独写一条"最小请求 + 拿头 + 读完就关"的探针出口更便宜。
//
// 与热路径保持一致的部分：body 仍过 prepareBody（指纹脱敏 / 副作用清洗），
// 请求头仍走 ChatHeaders（账号头同形），鉴权与路径仍由 chatBase/chatPaths 决定
// ——否则探到的配额线和真实请求走的不是同一条线，结论没有意义。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ProbeBodyLimit 探针响应体上限：配额线判定只看状态码与错误码，1MB 足够读完
// 上游的错误信封（实测 402/429 信封 < 2KB）。
const ProbeBodyLimit = 1 << 20

// ProbeTimeout 单次探针的硬上限：探针是后台动作，卡住只会拖慢同一轮里其它
// 账号，宁可超时算"未探明"。
const ProbeTimeout = 60 * time.Second

// ProbeModelQuota 对 (账号, 模型) 这条配额线发一次最小对话请求。
//
// 返回：HTTP 状态码、响应头、响应体（≤ProbeBodyLimit）、传输层错误。
// 200 表示这条线此刻还能用（上游已开流，函数立即关闭 body——不读完 SSE，
// 探针不关心内容）；≥400 时 body 交给调用方过 Classify。
func (c *Client) ProbeModelQuota(ctx context.Context, a *auth.Auth, model string) (int, http.Header, []byte, error) {
	if a == nil {
		return 0, nil, nil, errNoAccount
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	// 上限 1 token + 极短 prompt：这是能触发"配额耗尽"判定的最小请求。
	// 不传 tools / reasoning：探针要的是配额线状态，不是模型能力。
	raw, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]any{{"role": "user", "content": "1"}},
		"stream":     true,
		"max_tokens": 1,
	})
	if err != nil {
		return 0, nil, nil, err
	}
	prepared := c.prepareBody(raw, a.Realm(), a.UID, "")
	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}

	paths := c.chatPaths(a)
	if len(paths) == 0 {
		return 0, nil, nil, errNoChatPath
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+paths[0], bytes.NewReader(prepared))
	if err != nil {
		return 0, nil, nil, err
	}
	c.ChatHeaders(req, a, "", ChatMeta{})

	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		// 传输层失败：清空连接池里的死连接，理由同 ChatStreamContext。
		roundTripCloseIdle(c.chatHTTP().Transport)
		return 0, nil, nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 400 {
		// 200：已开流即证明这条线活着。不读完（探针不需要正文），
		// defer 里只丢弃 64KB 以内并关连接，避免占用上游并发额度。
		return resp.StatusCode, resp.Header, nil, nil
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, ProbeBodyLimit))
	if rerr != nil {
		return resp.StatusCode, resp.Header, body, rerr
	}
	return resp.StatusCode, resp.Header, body, nil
}

// ProbeError 便于错误分类的哨兵（调用方只需 log）。
var errNoAccount = &Error{Kind: ErrClient, Status: 0, Msg: "probe: nil account"}
var errNoChatPath = &Error{Kind: ErrClient, Status: 0, Msg: "probe: no chat path"}
