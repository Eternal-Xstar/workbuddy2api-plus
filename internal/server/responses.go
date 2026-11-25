// responses.go 原生 /v1/responses 端点（Responses API ↔ Chat Completions 转译）。
//
// 为什么需要：Codex 系客户端只调 /v1/responses，不认 /v1/chat/completions
// （见 payload.go 里那条注释）。网关只暴露 chat 形态时，这类客户端连不上。
//
// 实现取向：**不在响应层重写一遍上游链路**，而是把 Responses 请求翻译成一次
// chat 请求，复用既有 chatCompletions 的全部能力（选号 / 在途租约 / 会话粘性 /
// 429 冷却 / 提示词改写 / 指纹脱敏 / SSE 帧白名单重建），只在"出站字节"这一层
// 做形态转换：
//
//	POST /v1/responses
//	  ① buildChatFromResponses：input/instructions/tools → messages/tools
//	  ② h.chatCompletions(responsesTranslator{w}, 合成的 chat 请求)
//	  ③ responsesTranslator：把 chat.completion(.chunk) 还原成 Responses 事件族
//
// 这样做的代价是翻译层必须理解两种形态（见本文件下半部分）；收益是账号池、
// 轮转、冷却这些真正贵重的逻辑一行都不用复制——否则第二份实现迟早与主链路漂移。
package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// responsesBodyLimit 入站 /v1/responses 请求体上限（与 chat 同口径的本地加固）。
const responsesBodyLimit = 32 << 20

// responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, responsesBodyLimit))
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), "")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "body is not valid JSON: "+err.Error(), "invalid_json")
		return
	}
	chatBody, model, stream, buildErr := buildChatFromResponses(req)
	if buildErr != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", buildErr.Error(), "invalid_request")
		return
	}

	// 合成的 chat 请求：只换 body / 方法 / 路径，其余（超时、取消、来源）沿用原请求。
	creq := r.Clone(r.Context())
	creq.Method = http.MethodPost
	creq.URL.Path = "/v1/chat/completions"
	creq.Body = io.NopCloser(bytes.NewReader(chatBody))
	creq.ContentLength = int64(len(chatBody))
	creq.Header = r.Header.Clone()
	creq.Header.Set("Content-Type", "application/json")

	tw := &responsesTranslator{
		dst:     w,
		flusher: nil,
		model:   model,
		stream:  stream,
		id:      newResponseID(),
		created: time.Now().Unix(),
		tools:   map[int]*responseToolItem{},
		seq:     0,
	}
	if fl, ok := w.(http.Flusher); ok {
		tw.flusher = fl
	}
	h.chatCompletions(tw, creq)
	tw.finish()
}

// writeResponsesError 以 Responses 的错误信封回错（与 chat 的 error 形状不同：
// Responses 用 error.type/message/code，不含 type:"api_error" 这一层）。
func writeResponsesError(w http.ResponseWriter, status int, typ, msg, code string) {
	obj := map[string]any{"message": msg, "type": typ}
	if code != "" {
		obj["code"] = code
	}
	writeJSON(w, status, map[string]any{"error": obj})
}

// newResponseID 生成 resp_<24 hex>。
func newResponseID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	return "resp_" + hex.EncodeToString(b[:])
}

// newItemID 生成 msg_/fc_ 前缀的条目 id。
func newItemID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// 请求翻译：Responses → Chat Completions
// ---------------------------------------------------------------------------

// buildChatFromResponses 把 Responses 请求体翻译成 chat/completions 请求体。
// 返回翻译后的 body、模型名（回显用）与 stream 标志。
func buildChatFromResponses(req map[string]any) ([]byte, string, bool, error) {
	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, "", false, fmt.Errorf("model is required")
	}
	stream, _ := req["stream"].(bool)

	msgs := make([]map[string]any, 0, 4)
	// instructions 等价于 chat 的 system 消息（Responses 没有独立的 system 字段）。
	if ins, ok := req["instructions"].(string); ok && strings.TrimSpace(ins) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ins})
	}
	inMsgs, err := responsesInputToMessages(req["input"])
	if err != nil {
		return nil, "", false, err
	}
	if len(inMsgs) == 0 {
		return nil, "", false, fmt.Errorf("input is required")
	}
	msgs = append(msgs, inMsgs...)

	out := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   stream,
	}
	// 采样参数直通（同名同义）。
	for _, k := range []string{"temperature", "top_p", "parallel_tool_calls"} {
		if v, ok := req[k]; ok && v != nil {
			out[k] = v
		}
	}
	// max_output_tokens → max_tokens。
	if v, ok := req["max_output_tokens"].(float64); ok && v > 0 {
		out["max_tokens"] = int(v)
	}
	// reasoning.effort → reasoning_effort（网关上游按该字段选思考档）。
	if rs, ok := req["reasoning"].(map[string]any); ok {
		if eff, ok := rs["effort"].(string); ok && eff != "" {
			out["reasoning_effort"] = eff
		}
	}
	if tools := responsesToolsToChat(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if tc, ok := responsesToolChoiceToChat(req["tool_choice"]); ok {
			out["tool_choice"] = tc
		}
	}
	// text.format.json_schema → response_format（结构化输出）。
	if tf, ok := req["text"].(map[string]any); ok {
		if fmtObj, ok := tf["format"].(map[string]any); ok {
			if rf := responsesFormatToChat(fmtObj); rf != nil {
				out["response_format"] = rf
			}
		}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, "", false, err
	}
	return body, model, stream, nil
}

// responsesInputToMessages 把 Responses 的 input（字符串或条目数组）翻成 chat messages。
func responsesInputToMessages(input any) ([]map[string]any, error) {
	switch v := input.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []map[string]any{{"role": "user", "content": v}}, nil
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "function_call_output":
				// 工具回执 → chat 的 tool 消息。
				callID, _ := m["call_id"].(string)
				msg := map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      flattenContent(m["output"]),
				}
				out = append(out, msg)
			case "function_call":
				// 历史里的工具调用 → assistant.tool_calls（保持多轮工具语义）。
				name, _ := m["name"].(string)
				callID, _ := m["call_id"].(string)
				args, _ := m["arguments"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				out = append(out, map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":       callID,
						"type":     "function",
						"function": map[string]any{"name": name, "arguments": args},
					}},
				})
			case "reasoning", "item_reference":
				// 思考片段 / 引用条目：chat 形态没有对应物，丢弃（不臆造）。
				continue
			case "message", "":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system"
				}
				out = append(out, map[string]any{"role": role, "content": responsesContentToChat(m["content"])})
			default:
				// 未知条目类型：按 message 兜底（有 role 才收）。
				if role, ok := m["role"].(string); ok && role != "" {
					if role == "developer" {
						role = "system"
					}
					out = append(out, map[string]any{"role": role, "content": responsesContentToChat(m["content"])})
				}
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("input must be a string or an array of items")
	}
}

// responsesContentToChat 把 Responses 的 content（字符串或 part 数组）翻成 chat content。
// 纯文本 part 会拍平成字符串（与官方客户端行为一致，也少一层嵌套）。
func responsesContentToChat(content any) any {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		parts := make([]map[string]any, 0, len(v))
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := pm["type"].(string)
			switch typ {
			case "input_text", "output_text", "text", "":
				if txt, ok := pm["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": txt})
				}
			case "input_image":
				url := ""
				switch iv := pm["image_url"].(type) {
				case string:
					url = iv
				case map[string]any:
					url, _ = iv["url"].(string)
				}
				if url != "" {
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": url},
					})
				}
			case "input_file":
				// 文件条目 chat 侧无对应形态（网关不做文件解析），丢弃。
				continue
			}
		}
		if len(parts) == 1 && parts[0]["type"] == "text" {
			return parts[0]["text"]
		}
		return parts
	default:
		return ""
	}
}

// flattenContent 把 function_call_output 的 output 拍平成字符串。
func flattenContent(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, it := range t {
			if m, ok := it.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
					continue
				}
			}
			if s, ok := it.(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// responsesToolsToChat 把 Responses 的 tools 翻成 chat 的 tools（只保留 function 型）。
func responsesToolsToChat(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	var dropped []string
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := m["type"].(string); typ != "function" {
			// 上游 chat 形态只认 function 工具；web_search / file_search / mcp 等宿主
			// 侧工具没有对应物。**不能静默丢**——那等于在用户不知情时削掉工具能力，
			// 所以记一条 WARN（客户端若依赖它会看到"模型不调用工具"，日志里能对上原因）。
			if typ != "" {
				dropped = append(dropped, typ)
			}
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d, ok := m["description"].(string); ok && d != "" {
			fn["description"] = d
		}
		if p, ok := m["parameters"]; ok && p != nil {
			fn["parameters"] = p
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	if len(dropped) > 0 {
		log.Printf("WARN: [server] responses: 上游 chat 形态不支持 %d 个非 function 工具，已跳过（客户端可能因此看不到这些工具被调用）: %s",
			len(dropped), strings.Join(dropped, ", "))
	}
	return out
}

// responsesToolChoiceToChat 把 tool_choice 翻成 chat 形态。
func responsesToolChoiceToChat(v any) (any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case string:
		switch t {
		case "auto", "none", "required":
			return t, true
		}
		return nil, false
	case map[string]any:
		if typ, _ := t["type"].(string); typ == "function" {
			if name, _ := t["name"].(string); name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
			}
		}
	}
	return nil, false
}

// responsesFormatToChat 把 text.format 翻成 chat 的 response_format。
func responsesFormatToChat(f map[string]any) map[string]any {
	typ, _ := f["type"].(string)
	switch typ {
	case "json_schema":
		js := map[string]any{}
		if n, ok := f["name"].(string); ok && n != "" {
			js["name"] = n
		}
		if s, ok := f["schema"]; ok {
			js["schema"] = s
		}
		if st, ok := f["strict"].(bool); ok {
			js["strict"] = st
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	case "json_object":
		return map[string]any{"type": "json_object"}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 响应翻译：Chat Completions → Responses
// ---------------------------------------------------------------------------

// responseToolItem 流式期间累积的一个 function_call 输出条目。
type responseToolItem struct {
	id       string // fc_...
	callID   string // 上游 tool_call id
	name     string
	args     strings.Builder
	outputAt int // 在 output 数组里的下标
	done     bool
}

// responsesTranslator 实现 http.ResponseWriter，夹在 chatCompletions 与真实
// ResponseWriter 之间：吞掉 chat 形态的字节、吐出 Responses 形态的字节。
type responsesTranslator struct {
	dst     http.ResponseWriter
	flusher http.Flusher

	model   string
	stream  bool
	id      string
	created int64
	seq     int

	hdr         http.Header // chat 侧写的头一律丢弃（我们只发自己的）
	status      int
	body        bytes.Buffer // 非流式 / 错误体
	streamBytes []byte       // 流式帧的未消费尾巴

	headersSent bool
	started     bool

	msgItemID string
	msgOpen   bool
	msgClosed bool
	msgText   strings.Builder
	msgIndex  int // message 在 output 数组里的下标（有 reasoning 时后移一位）

	// reasoning（思考）条目：上游 delta.reasoning_content / delta.reasoning 的口子。
	// 不映射它的话，思考型模型在小上限下会出现「usage 有输出、output 为空」的假象。
	reasonItemID string
	reasonOpen   bool
	reasonText   strings.Builder
	reasonIndex  int

	// nextOutput 下一个可分配的 output_index（reasoning / message / function_call 共用）。
	nextOutput int

	tools map[int]*responseToolItem
	order []int

	usage     map[string]any
	completed bool
	failed    bool
}

// Header 返回一份私有头，chat 侧设置的 Content-Type 等不会外泄。
func (t *responsesTranslator) Header() http.Header {
	if t.hdr == nil {
		t.hdr = http.Header{}
	}
	return t.hdr
}

// WriteHeader 记录状态码；不立即下发（流式要靠第一帧决定头，错误要等 body 齐）。
func (t *responsesTranslator) WriteHeader(code int) {
	if t.status == 0 {
		t.status = code
	}
}

// Write 流式走帧解析，非流式 / 错误体走缓冲。
func (t *responsesTranslator) Write(p []byte) (int, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	if !t.stream || t.status >= 400 {
		t.body.Write(p)
		return len(p), nil
	}
	t.streamBytes = append(t.streamBytes, p...)
	t.drainFrames()
	return len(p), nil
}

// Flush 透传 flush（SSE 需要逐帧可见）。
func (t *responsesTranslator) Flush() {
	if t.headersSent && t.flusher != nil {
		t.flusher.Flush()
	}
}

// drainFrames 消费完整 SSE 帧（以空行分隔），处理残余缓冲。
func (t *responsesTranslator) drainFrames() {
	for {
		i := bytes.Index(t.streamBytes, []byte("\n\n"))
		if i < 0 {
			return
		}
		frame := t.streamBytes[:i]
		t.streamBytes = t.streamBytes[i+2:]
		t.handleFrame(string(frame))
	}
}

// handleFrame 处理一帧 `data: {...}`。
func (t *responsesTranslator) handleFrame(frame string) {
	payload := ""
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if payload == "" {
		return
	}
	if payload == "[DONE]" {
		t.completeStream()
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return
	}
	if errObj, ok := obj["error"].(map[string]any); ok {
		t.failStream(errObj)
		return
	}
	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		t.usage = u
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	ch, _ := choices[0].(map[string]any)
	delta, _ := ch["delta"].(map[string]any)
	if delta != nil {
		if txt, ok := delta["content"].(string); ok && txt != "" {
			t.appendText(txt)
		}
		if rt, ok := delta["reasoning_content"].(string); ok && rt != "" {
			t.appendReasoning(rt)
		} else if rt, ok := delta["reasoning"].(string); ok && rt != "" {
			t.appendReasoning(rt)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				t.appendToolCall(tc)
			}
		}
	}
	if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
		// finish_reason 只用于收尾判定，正文/工具在 delta 里已经流过。
		t.completeStream()
	}
}

// writeHeaders 首次真正下发时发送 Responses 的流式头。
func (t *responsesTranslator) writeHeaders() {
	if t.headersSent {
		return
	}
	t.headersSent = true
	h := t.dst.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	t.dst.WriteHeader(http.StatusOK)
}

// send 写一个 Responses SSE 事件（event: <type> + data: {...}）。
func (t *responsesTranslator) send(event string, obj map[string]any) {
	if !t.headersSent {
		t.writeHeaders()
	}
	obj["type"] = event
	obj["sequence_number"] = t.seq
	t.seq++
	raw, err := json.Marshal(obj)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(t.dst, "event: %s\ndata: %s\n\n", event, raw)
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// responseObject 构造 response 资源骨架（created/in_progress/completed 共用）。
func (t *responsesTranslator) responseObject(status string, output []any) map[string]any {
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id":                 t.id,
		"object":             "response",
		"created_at":         t.created,
		"status":             status,
		"model":              t.model,
		"output":             output,
		"parallel_tool_calls": true,
		"tool_choice":        "auto",
		"tools":              []any{},
		"truncation":         "disabled",
		"metadata":           map[string]any{},
	}
}

// startStream 发 created + in_progress（每个流只发一次）。
func (t *responsesTranslator) startStream() {
	if t.started {
		return
	}
	t.started = true
	t.send("response.created", map[string]any{"response": t.responseObject("in_progress", nil)})
	t.send("response.in_progress", map[string]any{"response": t.responseObject("in_progress", nil)})
}

// appendReasoning 追加思考增量：首个分片建 reasoning 条目，后续发 summary delta。
func (t *responsesTranslator) appendReasoning(txt string) {
	t.startStream()
	if !t.reasonOpen {
		t.reasonOpen = true
		t.reasonItemID = newItemID("rs")
		t.reasonIndex = t.nextOutput
		t.nextOutput++
		t.send("response.output_item.added", map[string]any{
			"output_index": t.reasonIndex,
			"item": map[string]any{
				"id":      t.reasonItemID,
				"type":    "reasoning",
				"summary": []any{},
			},
		})
	}
	t.reasonText.WriteString(txt)
	t.send("response.reasoning_summary_text.delta", map[string]any{
		"item_id":       t.reasonItemID,
		"output_index":  t.reasonIndex,
		"summary_index": 0,
		"delta":         txt,
	})
}

// appendText 追加正文增量并维护 message 条目的 added/delta 事件。
func (t *responsesTranslator) appendText(txt string) {
	t.startStream()
	if !t.msgOpen {
		t.msgOpen = true
		t.msgItemID = newItemID("msg")
		t.msgIndex = t.nextOutput
		t.nextOutput++
		t.send("response.output_item.added", map[string]any{
			"output_index": t.msgIndex,
			"item": map[string]any{
				"id":      t.msgItemID,
				"type":    "message",
				"role":    "assistant",
				"status":  "in_progress",
				"content": []any{},
			},
		})
		t.send("response.content_part.added", map[string]any{
			"item_id":       t.msgItemID,
			"output_index":  t.msgIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	t.msgText.WriteString(txt)
	t.send("response.output_text.delta", map[string]any{
		"item_id":       t.msgItemID,
		"output_index":  t.msgIndex,
		"content_index": 0,
		"delta":         txt,
	})
}

// appendToolCall 处理一个 tool_calls 增量（首片建条目，参数分片累积）。
func (t *responsesTranslator) appendToolCall(v any) {
	tc, ok := v.(map[string]any)
	if !ok {
		return
	}
	t.startStream()
	idx := 0
	if f, ok := tc["index"].(float64); ok {
		idx = int(f)
	}
	item := t.tools[idx]
	if item == nil {
		item = &responseToolItem{id: newItemID("fc"), outputAt: t.nextOutput}
		t.nextOutput++
		t.tools[idx] = item
		t.order = append(t.order, idx)
		name := ""
		if fn, ok := tc["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
		item.name = name
		if id, ok := tc["id"].(string); ok {
			item.callID = id
		}
		t.send("response.output_item.added", map[string]any{
			"output_index": item.outputAt,
			"item": map[string]any{
				"id":        item.id,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   item.callID,
				"name":      item.name,
				"arguments": "",
			},
		})
	}
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		return
	}
	if name, ok := fn["name"].(string); ok && name != "" && item.name == "" {
		item.name = name
	}
	if args, ok := fn["arguments"].(string); ok && args != "" {
		item.args.WriteString(args)
		t.send("response.function_call_arguments.delta", map[string]any{
			"item_id":      item.id,
			"output_index": item.outputAt,
			"delta":        args,
		})
	}
}

// failedOutput 构造失败态的 response 资源。
func (t *responsesTranslator) failStream(errObj map[string]any) {
	if t.failed {
		return
	}
	t.failed = true
	if !t.started {
		t.startStream()
	}
	resp := t.responseObject("failed", nil)
	resp["error"] = errObj
	t.send("response.failed", map[string]any{"response": resp})
}

// completeStream 收尾：补 done 事件并 emit response.completed（幂等）。
func (t *responsesTranslator) completeStream() {
	if t.completed || t.failed {
		return
	}
	t.startStream()
	t.completed = true
	output := make([]any, 0, 2+len(t.order))
	if t.reasonOpen {
		text := t.reasonText.String()
		t.send("response.reasoning_summary_text.done", map[string]any{
			"item_id": t.reasonItemID, "output_index": t.reasonIndex,
			"summary_index": 0, "text": text,
		})
		item := map[string]any{
			"id":   t.reasonItemID,
			"type": "reasoning",
			"summary": []any{map[string]any{
				"type": "summary_text", "text": text,
			}},
		}
		t.send("response.output_item.done", map[string]any{"output_index": t.reasonIndex, "item": item})
		output = append(output, item)
	}
	if t.msgOpen {
		text := t.msgText.String()
		item := map[string]any{
			"id":     t.msgItemID,
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": text, "annotations": []any{},
			}},
		}
		t.send("response.output_text.done", map[string]any{
			"item_id": t.msgItemID, "output_index": t.msgIndex, "content_index": 0, "text": text,
		})
		t.send("response.content_part.done", map[string]any{
			"item_id": t.msgItemID, "output_index": t.msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
		t.send("response.output_item.done", map[string]any{"output_index": t.msgIndex, "item": item})
		output = append(output, item)
	}
	for _, idx := range t.order {
		it := t.tools[idx]
		if it == nil {
			continue
		}
		args := it.args.String()
		t.send("response.function_call_arguments.done", map[string]any{
			"item_id": it.id, "output_index": it.outputAt, "arguments": args,
		})
		item := map[string]any{
			"id":        it.id,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   it.callID,
			"name":      it.name,
			"arguments": args,
		}
		t.send("response.output_item.done", map[string]any{"output_index": it.outputAt, "item": item})
		output = append(output, item)
	}
	resp := t.responseObject("completed", output)
	if t.usage != nil {
		resp["usage"] = responsesUsage(t.usage)
	}
	if t.msgOpen {
		resp["output_text"] = t.msgText.String()
	}
	t.send("response.completed", map[string]any{"response": resp})
}

// finish 在 chatCompletions 返回后调用：处理错误、非流式与非帧式的收尾。
func (t *responsesTranslator) finish() {
	// 错误路径：chat 侧回了 OpenAI 错误信封（4xx/5xx），这里换成 Responses 形状。
	if t.status >= 400 {
		typ, msg, code := parseChatError(t.body.Bytes())
		writeResponsesError(t.dst, t.status, typ, msg, code)
		return
	}
	if t.stream {
		// 没有 finish_reason 但流已结束：补收尾（异常但可恢复的流）。
		t.completeStream()
		return
	}
	// 非流式：把聚合后的 chat.completion 翻成 response 资源。
	resp, err := responsesFromChatCompletion(t.body.Bytes(), t.id, t.created, t.model)
	if err != nil {
		writeResponsesError(t.dst, http.StatusBadGateway, "upstream_parse", "translate chat completion: "+err.Error(), "upstream_parse")
		return
	}
	writeJSON(t.dst, http.StatusOK, resp)
}

// parseChatError 从 chat 的错误信封里取 type/message/code。
func parseChatError(raw []byte) (string, string, string) {
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		typ := env.Error.Type
		if typ == "" {
			typ = "api_error"
		}
		return typ, env.Error.Message, env.Error.Code
	}
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = "gateway error"
	}
	return "api_error", truncateStr(msg, 300), ""
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// responsesUsage 把 chat usage 映射成 Responses usage（字段名不同，语义相同）。
func responsesUsage(u map[string]any) map[string]any {
	get := func(k string) int64 {
		if v, ok := u[k].(float64); ok {
			return int64(v)
		}
		return 0
	}
	in := get("prompt_tokens")
	out := get("completion_tokens")
	total := get("total_tokens")
	if total == 0 {
		total = in + out
	}
	cached := int64(0)
	if det, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := det["cached_tokens"].(float64); ok {
			cached = int64(v)
		}
	}
	return map[string]any{
		"input_tokens":          in,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         out,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          total,
	}
}

// responsesFromChatCompletion 把非流式的 chat.completion 翻成 Responses 资源。
func responsesFromChatCompletion(raw []byte, id string, created int64, fallbackModel string) (map[string]any, error) {
	var cc struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          any `json:"content"`
				ReasoningContent any `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &cc); err != nil {
		return nil, err
	}
	model := fallbackModel
	if cc.Model != "" {
		model = cc.Model
	}
	output := make([]any, 0, 2)
	text := ""
	if len(cc.Choices) > 0 {
		// 思考片段先入 output（与流式一致：reasoning 在前，message 在后）。
		if reasoning := flattenContent(cc.Choices[0].Message.ReasoningContent); reasoning != "" {
			output = append(output, map[string]any{
				"id":   newItemID("rs"),
				"type": "reasoning",
				"summary": []any{map[string]any{
					"type": "summary_text", "text": reasoning,
				}},
			})
		}
		text = flattenContent(cc.Choices[0].Message.Content)
		if text != "" {
			output = append(output, map[string]any{
				"id":     newItemID("msg"),
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []any{map[string]any{
					"type": "output_text", "text": text, "annotations": []any{},
				}},
			})
		}
		for _, tc := range cc.Choices[0].Message.ToolCalls {
			output = append(output, map[string]any{
				"id":        newItemID("fc"),
				"type":      "function_call",
				"status":    "completed",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			})
		}
	}
	resp := map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          created,
		"status":              "completed",
		"model":               model,
		"output":              output,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"truncation":          "disabled",
		"metadata":            map[string]any{},
	}
	if text != "" {
		resp["output_text"] = text
	}
	if cc.Usage != nil {
		resp["usage"] = responsesUsage(cc.Usage)
	}
	return resp, nil
}
