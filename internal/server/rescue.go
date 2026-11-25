// rescue.go 三种「答不上来 / 答一半」的救援，全部在网关侧完成，客户端无感。
//
// 背景（真机实测 + 上游协议取证，与 workbuddy2api-panel 分支同源结论）：
//
//	① starved   思考吃光预算：reasoning 与正文共用 max_tokens，没有独立思考上限。
//	            小预算下思考能独吞全部额度 → finish_reason=length、正文零字节。
//	            实测 max_tokens ≤ 300 必现，400 起时好时坏。
//	② narration 说完一句预告就停：finish_reason=stop、零 tool_calls、零报错，尾句是
//	            「接着验证：」「先看完整表单。」这类未完成的预告。
//	③ toolonly  句号收尾但其实该动手：形态与 narration 相同，只是尾部看着写完了。
//	            这种**只采纳工具调用**——把正常短答续成废话比不续更糟。
//
// 统一做法：
//   - starved：把 thinking 关掉、删掉思考档位、预算抬到下限，同账号同模型重发一次。
//   - narration / toolonly：已输出正文作为 assistant 前缀 + 一句「接着写完，需要工具就直接
//     调用」，重发一次；narration 采纳正文或工具，toolonly 只采纳工具。
//
// 流式（DeepSeek 桌面版这类客户端默认流式）额外做一层「暂存」：见到第一段正文/工具调用
// 之前不往客户端写，于是「什么都不产出」的坏流可以被整段替换掉，客户端完全看不到失败；
// 一旦见到正文就立刻转为直通，不增加任何延迟。暂存上限兜底，避免长思考把内存撑爆。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/Eternal-Xstar/workbuddy2api-plus/internal/auth"
	"github.com/Eternal-Xstar/workbuddy2api-plus/internal/upstream"
)

const (
	// rescueMinBudget 低于此额度不救：官方客户端用 max_tokens=1 做探活
	// （"Reply with OK."），按原样失败即可，不值得多打一次上游。
	rescueMinBudget = 16
	// rescueBudgetFloor 救援段预算下限：关掉思考后正文要写得下。首段若给得小，
	// 直接沿用会让救援段同样写不完（"思考吃光"变成"正文被截"）。
	rescueBudgetFloor = 4096
	// rescueHoldCap 流式暂存上限：超过就放弃救援、把已暂存字节原样吐出，
	// 避免长思考把内存顶爆（宁可丢救援能力也不丢请求）。
	rescueHoldCap = 256 << 10
	// starvationRiskBudget 预算低于此值时被判「可能被思考吃光」，流式路径才挂暂存。
	starvationRiskBudget = 600
	// rescueNudge 续写指令：只要求"接着写"，不要求换个说法，避免重复已输出内容。
	rescueNudge = "继续，从上面中断的地方接着写，不要重复已经写过的内容；如果需要调用工具，直接调用。"
)

// ---- 判定 ----

// outputBudget 读客户端给的输出预算（两个键任一）。
func outputBudget(obj map[string]any) (int64, string) {
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := obj[k].(float64); ok && v > 0 {
			return int64(v), k
		}
		if v, ok := obj[k].(int64); ok && v > 0 {
			return v, k
		}
	}
	return 0, ""
}

// messageText 取 chat 消息正文（string 或 part 数组）。
func messageText(msg map[string]any) string {
	switch c := msg["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// choiceParts 取 aggregate/frame 里第一个 choice 的关键字段。
func choiceParts(resp map[string]any) (finish string, content string, hasTool bool, msg map[string]any) {
	chs, _ := resp["choices"].([]any)
	if len(chs) == 0 {
		return "", "", false, nil
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return "", "", false, nil
	}
	finish, _ = c["finish_reason"].(string)
	msg, _ = c["message"].(map[string]any)
	if msg == nil {
		msg, _ = c["delta"].(map[string]any)
	}
	if msg != nil {
		content = messageText(msg)
		if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
			hasTool = true
		}
	}
	return finish, content, hasTool, msg
}

// aggregateStarved 聚合体是否属于「预算被思考吃光、正文零产出」。
// 三个条件同时成立：finish_reason=length、正文为空、没有 tool_calls。
func aggregateStarved(resp map[string]any) bool {
	finish, content, hasTool, _ := choiceParts(resp)
	if hasTool || strings.TrimSpace(content) != "" {
		return false
	}
	return finish == "length"
}

// hasOutput 报告聚合体是否产出了客户端可见的东西（正文或工具调用）。
func hasOutput(resp map[string]any) bool {
	_, content, hasTool, _ := choiceParts(resp)
	return hasTool || strings.TrimSpace(content) != ""
}

// narrationTail 尾部是否像「话没说完」。判据刻意窄：
//   - 以冒号 / 省略号 / 逗号顿号收尾（"接着验证：" "…" "，"）；
//   - 结尾是未闭合的 ``` 代码块；
//   - 长度足够却没有句末标点（。！？.!?；;）与收尾括号引号。
func narrationTail(text string) bool {
	t := strings.TrimRight(text, " \t\r\n")
	if len([]rune(t)) < 8 {
		return false
	}
	last := []rune(t)[len([]rune(t))-1]
	switch last {
	case '：', ':', '…', '，', ',', '、', '（', '(', '“', '"':
		return true
	}
	// 未闭合代码块：``` 计数为奇数。
	if strings.Count(t, "```")%2 == 1 {
		return true
	}
	switch last {
	case '。', '！', '？', '.', '!', '?', '；', ';', '）', ')', '】', '」', '』', '”', '\'':
		return false
	}
	return true
}

// aggregateNarration 聚合体是否属于「说一句预告就停」。
// declaredTools>0 才判——没有工具声明的纯聊天不该被续写。
func aggregateNarration(resp map[string]any, declaredTools int) bool {
	if declaredTools == 0 {
		return false
	}
	finish, content, hasTool, _ := choiceParts(resp)
	if hasTool || finish != "stop" {
		return false
	}
	return narrationTail(content)
}

// aggregateToolOnly 句号收尾、但其实该动手：形态与 narration 相同，只是尾部"写完了"。
// 单独判出来是为了**只采纳工具调用**（把正常短答续成废话比不续更糟）。
func aggregateToolOnly(resp map[string]any, declaredTools int) bool {
	if declaredTools == 0 {
		return false
	}
	finish, content, hasTool, _ := choiceParts(resp)
	if hasTool || finish != "stop" {
		return false
	}
	return strings.TrimSpace(content) != "" && !narrationTail(content)
}

// ---- 请求体改写 ----

// rescueBodyStarved 把请求体改写成「只抬预算」的救援形态。
//
// **绝不动思考设置**（用户的硬要求：长会话不要影响思考强度）。病因是"思考与正文
// 共用一份预算、预算太小"，不是"思考不该开"——所以正确的修法是给足预算，而不是
// 把思考关掉：关掉思考换来的正文，是拿回答质量换空白之后的二次损失。
//
// 新预算 = max(budget*multiplier, floor)；客户端没给预算时用 floor。
// 返回 false 的都不猜：body 不可解析、额度小到不值得重发（探活请求）、抬不上去。
func rescueBodyStarved(body []byte, floor int64, multiplier float64) ([]byte, bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	budget, key := outputBudget(obj)
	if budget > 0 && budget < rescueMinBudget {
		return nil, false
	}
	if floor <= 0 {
		floor = rescueBudgetFloor
	}
	if multiplier <= 1 {
		multiplier = 1
	}
	target := int64(float64(budget) * multiplier)
	if target < floor {
		target = floor
	}
	if budget > 0 && target <= budget {
		return nil, false // 抬不上去就没必要重发
	}
	if key == "" {
		key = "max_tokens"
	}
	obj[key] = target
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rescueBodyContinue 把请求体改写成「接着写」形态：已输出正文作为 assistant 前缀，
// 再补一条续写指令。prefix 为空则不改写。
func rescueBodyContinue(body []byte, prefix string) ([]byte, bool) {
	if strings.TrimSpace(prefix) == "" {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return nil, false
	}
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": prefix},
		map[string]any{"role": "user", "content": rescueNudge},
	)
	obj["messages"] = msgs
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// ---- 流式暂存（stream guard）----

// rescueSink 夹在 StreamHint 与真实 ResponseWriter 之间：
//   - hold 期间（还没见到正文/工具调用）把帧攒在内存里，不写客户端；
//   - 一旦见到正文或工具调用，立刻把攒下的帧原样吐出并转为直通；
//   - 终止帧 [DONE] 由调用方补（StreamHint 用 WithSuppressDone 挂起）。
//
// 这样「整段什么都没产出」的坏流可以被救援段整体替换，客户端看不到失败，
// 而正常流只多一次内存拷贝（首帧正文之前的部分）。
type rescueSink struct {
	dst     http.ResponseWriter
	flusher http.Flusher

	hold    bool
	held    bytes.Buffer
	pending bytes.Buffer // 未成帧的尾巴
	sawText bool

	done bool
}

func newRescueSink(dst http.ResponseWriter) *rescueSink {
	s := &rescueSink{dst: dst, hold: true}
	if fl, ok := dst.(http.Flusher); ok {
		s.flusher = fl
	}
	return s
}

// Header/WriteHeader 透传（StreamHint 会设置 SSE 头）。
func (s *rescueSink) Header() http.Header { return s.dst.Header() }
func (s *rescueSink) WriteHeader(code int) {
	// 头一旦下发就无法再换；暂存只影响 body，状态码照常透传。
	s.dst.WriteHeader(code)
}

// Write 按帧判断是否已经"有产出"。
func (s *rescueSink) Write(p []byte) (int, error) {
	if !s.hold {
		return s.dst.Write(p)
	}
	s.pending.Write(p)
	for {
		raw := s.pending.Bytes()
		i := bytes.Index(raw, []byte("\n\n"))
		if i < 0 {
			break
		}
		frame := append([]byte(nil), raw[:i+2]...)
		s.pending.Next(i + 2)
		s.held.Write(frame)
		if frameHasVisibleOutput(frame) {
			s.sawText = true
			s.release()
			// 释放后残余（本帧之后可能还有 pending）交给直通。
			if s.pending.Len() > 0 {
				rest := s.pending.Bytes()
				s.pending.Reset()
				_, _ = s.dst.Write(append([]byte(nil), rest...))
			}
			return len(p), nil
		}
		if s.held.Len() > rescueHoldCap {
			// 攒太多（长思考）：放弃救援，原样吐出，别把内存顶爆。
			s.release()
			return len(p), nil
		}
	}
	return len(p), nil
}

// release 结束暂存：把攒下的字节一次性写出并转为直通。
func (s *rescueSink) release() {
	if !s.hold {
		return
	}
	s.hold = false
	if s.held.Len() > 0 {
		_, _ = s.dst.Write(s.held.Bytes())
		s.held.Reset()
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Flush 只在直通期透传（暂存期不刷，否则攒的帧就提前泄给客户端了）。
func (s *rescueSink) Flush() {
	if !s.hold && s.flusher != nil {
		s.flusher.Flush()
	}
}

// starvedNoOutput 报告首段是否「一帧正文/工具都没有」。
func (s *rescueSink) starvedNoOutput() bool { return s.hold && !s.sawText }

// discard 丢弃暂存内容（救援成功时调用：坏流的帧不该再发给客户端）。
func (s *rescueSink) discard() {
	s.held.Reset()
	s.pending.Reset()
	s.hold = false
}

// finish 收尾：把还没吐出的字节吐出（救援失败时的原样兜底），并补终止帧。
func (s *rescueSink) finish() {
	s.release()
	if s.pending.Len() > 0 {
		_, _ = s.dst.Write(s.pending.Bytes())
		s.pending.Reset()
	}
	if s.done {
		return
	}
	s.done = true
	_, _ = s.dst.Write([]byte("data: [DONE]\n\n"))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// frameHasVisibleOutput 判断一帧 SSE 是否带客户端可见产出（正文或工具调用）。
func frameHasVisibleOutput(frame []byte) bool {
	payload := framePayload(frame)
	if payload == "" || payload == "[DONE]" {
		return false
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return false
	}
	_, content, hasTool, _ := choiceParts(obj)
	return hasTool || content != ""
}

// framePayload 取一帧里 data: 后面的内容。
func framePayload(frame []byte) string {
	for _, line := range strings.Split(string(frame), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return ""
}

// ---- 救援执行 ----

// tryStarvedStream 在流式路径上做空回复救援：首段一帧正文都没有时，用「关思考 +
// 预算抬底」的 body 重发，并把救援段直接接在同一条客户端流里。
//
// 前置条件（调用方保证）：sink 仍在暂存（客户端一个字节都没收到）、未出现上游 error 帧。
// 返回 true 表示救援段已接管这条流。
func (h *Handler) tryStarvedStream(ctx context.Context, sink *rescueSink, a *auth.Auth, body []byte,
	clientIP string, meta upstream.ChatMeta, declaredTools map[string]bool, st *chatStat) bool {
	if h.cfg.Upstream == nil || a == nil {
		return false
	}
	rc, status, _, err := h.cfg.Upstream.ChatStreamContext(ctx, a, body, clientIP, meta)
	if err != nil || status >= 400 || rc == nil {
		if rc != nil {
			rc.Close()
		}
		return false
	}
	// 丢掉首段的暂存帧：那一段是"什么都没产出"的坏流，不该发给客户端。
	sink.discard()
	stats := newChatStatsReaderSince(rc, st.start)
	_ = upstream.StreamHint(sink, stats, upstream.FrameHintFunc(func() upstream.HintContext {
		return upstream.HintContext{}
	}), upstream.WithMarkupRepair(upstream.NewMarkupRepair(declaredTools, true)),
		upstream.WithSuppressDone())
	rc.Close()
	return true
}
// 失败一律返回 false：救援是"锦上添花"，任何异常都必须让原响应照常返回。
func (h *Handler) rescueAggregate(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta upstream.ChatMeta) (map[string]any, bool) {
	if h.cfg.Upstream == nil || a == nil {
		return nil, false
	}
	rc, status, _, err := h.cfg.Upstream.ChatStreamContext(ctx, a, body, clientIP, meta)
	if err != nil || status >= 400 || rc == nil {
		if rc != nil {
			rc.Close()
		}
		return nil, false
	}
	resp, aerr := upstream.Aggregate(rc)
	rc.Close()
	if aerr != nil {
		return nil, false
	}
	return resp, true
}

// rescueAggregateIfNeeded 非流式救援入口：按「空回复 → 预告句 → 句号但该动手」顺序判定，
// 命中才重发一次并返回合并后的响应；都不命中就原样返回（指针相同，调用方可据此跳过）。
func (h *Handler) rescueAggregateIfNeeded(ctx context.Context, resp map[string]any, body []byte,
	a *auth.Auth, clientIP string, meta upstream.ChatMeta, declaredTools map[string]bool, model string) map[string]any {
	if resp == nil || h.cfg.RescueDisabled {
		return resp
	}
	nTools := len(declaredTools)

	// ① 空回复：思考吃光预算。
	if aggregateStarved(resp) {
		if rb, ok := rescueBodyStarved(body, h.cfg.RescueBudgetFloor, h.cfg.RescueBudgetMultiplier); ok {
			if r2, ok2 := h.rescueAggregate(ctx, a, rb, clientIP, meta); ok2 && hasOutput(r2) {
				log.Printf("INFO: [server] non-stream model=%s: 空回复已救援（starved → 抬预算重发，思考设置不动）", model)
				return r2
			}
		}
		return resp
	}

	// ② 说到一半（预告句）：续写，采纳正文或工具调用。
	// **默认关闭**：单人自用的桌面客户端不声明 tools，这条对它等于不存在；要用
	// Codex/Claude Code 时在 config 里开 rescue.continue_enabled。它不限制工具，
	// 只在"声明了工具却一个都没回"时补一次。
	if h.cfg.RescueContinue && aggregateNarration(resp, nTools) {
		prefix := choiceContent(resp)
		if rb, ok := rescueBodyContinue(body, prefix); ok {
			if r2, ok2 := h.rescueAggregate(ctx, a, rb, clientIP, meta); ok2 {
				if merged, ok3 := mergeContinuation(resp, r2, true); ok3 {
					log.Printf("INFO: [server] non-stream model=%s: 预告句已续写（narration）", model)
					return merged
				}
			}
		}
		return resp
	}

	// ③ 句号收尾但其实该动手：只采纳工具调用，正文一律不要。同样默认关闭。
	if h.cfg.RescueContinue && aggregateToolOnly(resp, nTools) {
		prefix := choiceContent(resp)
		if rb, ok := rescueBodyContinue(body, prefix); ok {
			if r2, ok2 := h.rescueAggregate(ctx, a, rb, clientIP, meta); ok2 {
				if merged, ok3 := mergeContinuation(resp, r2, false); ok3 {
					log.Printf("INFO: [server] non-stream model=%s: 工具轮已续写（toolonly，只采纳 tool_calls）", model)
					return merged
				}
			}
		}
	}
	return resp
}

// choiceContent 取第一个 choice 的正文。
func choiceContent(resp map[string]any) string {
	_, content, _, _ := choiceParts(resp)
	return content
}

// mergeContinuation 把续写段并回原响应。
//
//   - allowText=true（narration）：正文接在原正文后面，或采纳续写段的工具调用；
//   - allowText=false（toolonly）：**只**采纳工具调用，正文不动（把正常短答续成
//     废话比不续更糟）。
//
// usage 按段相加：两段都是真实消耗，报少了会让面板的积分/延迟统计失真。
func mergeContinuation(orig, rescue map[string]any, allowText bool) (map[string]any, bool) {
	if orig == nil || rescue == nil {
		return nil, false
	}
	_, origText, _, _ := choiceParts(orig)
	finish, rescueText, rescueTool, rescueMsg := choiceParts(rescue)
	if !rescueTool && !(allowText && strings.TrimSpace(rescueText) != "") {
		return nil, false
	}
	chs, _ := orig["choices"].([]any)
	if len(chs) == 0 {
		return nil, false
	}
	choice, _ := chs[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		msg = map[string]any{"role": "assistant"}
		choice["message"] = msg
	}
	if rescueTool {
		if tcs, ok := rescueMsg["tool_calls"]; ok {
			msg["tool_calls"] = tcs
		}
		choice["finish_reason"] = "tool_calls"
		// 工具轮里正文保留原段（模型已经说过的前言不删），但不追加续写段的正文。
	}
	if allowText && !rescueTool && strings.TrimSpace(rescueText) != "" {
		msg["content"] = origText + rescueText
		choice["finish_reason"] = finish
	}
	// usage 相加（两段都是真实消耗）。
	if u1, ok := orig["usage"].(map[string]any); ok {
		if u2, ok2 := rescue["usage"].(map[string]any); ok2 {
			for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
				a1, _ := u1[k].(float64)
				a2, _ := u2[k].(float64)
				if a2 > 0 {
					u1[k] = a1 + a2
				}
			}
		}
	}
	return orig, true
}
// rescueHoldApplies 报告这次请求是否值得挂「先攒后发」的暂存。
//
// 为什么不能一律挂：暂存期间 reasoning 帧不会发给客户端，客户端（DeepSeek 桌面版
// 会显示"思考中…"）在长思考时看起来就是卡死。所以只在**预算小到可能被思考吃光**时
// 才挂——那种情况下思考本身也只有几秒，攒住的代价可以忽略。
//
// 判据（真机统计：max_tokens ≤ 300 必现空回复，400 起时好时坏）：
//   - 客户端没给输出预算（多数桌面客户端）→ 挂（上游默认预算未知，风险未知时以体验兜底）；
//   - 给了预算且 < starvationRiskBudget(600) → 挂；
//   - 预算 ≥ 600 → 不挂（正常长思考照常流式外泄）。
func rescueHoldApplies(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return false
	}
	budget, _ := outputBudget(obj)
	if budget <= 0 {
		return true
	}
	return budget < starvationRiskBudget
}