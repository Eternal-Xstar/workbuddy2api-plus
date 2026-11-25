package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeChat 把翻译出来的 chat body 解成 map，便于断言。
func decodeChat(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("chat body 不是合法 JSON: %v (%s)", err, raw)
	}
	return m
}

func TestBuildChatFromResponses_StringInput(t *testing.T) {
	body, model, stream, err := buildChatFromResponses(map[string]any{
		"model":  "deepseek-v4-flash",
		"input":  "你好",
		"stream": true,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if model != "deepseek-v4-flash" || !stream {
		t.Fatalf("model=%q stream=%v", model, stream)
	}
	chat := decodeChat(t, body)
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "你好" {
		t.Fatalf("message = %+v", first)
	}
}

func TestBuildChatFromResponses_InstructionsToolsAndLimits(t *testing.T) {
	body, _, _, err := buildChatFromResponses(map[string]any{
		"model":             "glm-5.2",
		"instructions":      "be terse",
		"max_output_tokens": float64(64),
		"temperature":       0.3,
		"reasoning":         map[string]any{"effort": "low"},
		"tools": []any{map[string]any{
			"type":       "function",
			"name":       "get_weather",
			"parameters": map[string]any{"type": "object"},
		}},
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "out", "schema": map[string]any{"type": "object"},
		}},
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hi"}},
		}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	chat := decodeChat(t, body)
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %+v（期望 system + user）", msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be terse" {
		t.Fatalf("system message = %+v", sys)
	}
	if chat["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens = %v", chat["max_tokens"])
	}
	if chat["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v", chat["reasoning_effort"])
	}
	if _, ok := chat["response_format"].(map[string]any); !ok {
		t.Fatalf("response_format 未翻译: %+v", chat["response_format"])
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool function = %+v", fn)
	}
}

func TestResponsesInputToMessages_ToolRoundTrip(t *testing.T) {
	msgs, err := responsesInputToMessages([]any{
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "f", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "42"},
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("msgs = %+v", msgs)
	}
	if msgs[0]["role"] != "assistant" {
		t.Fatalf("第一轮应为 assistant.tool_calls: %+v", msgs[0])
	}
	if msgs[1]["role"] != "tool" || msgs[1]["tool_call_id"] != "call_1" || msgs[1]["content"] != "42" {
		t.Fatalf("tool 回执 = %+v", msgs[1])
	}
}

func TestBuildChatFromResponses_RejectsMissingModelOrInput(t *testing.T) {
	if _, _, _, err := buildChatFromResponses(map[string]any{"input": "x"}); err == nil {
		t.Fatal("缺 model 应报错")
	}
	if _, _, _, err := buildChatFromResponses(map[string]any{"model": "m"}); err == nil {
		t.Fatal("缺 input 应报错")
	}
}

// 流式：chat SSE 帧 → Responses 事件族。
func TestResponsesTranslator_Stream(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := &responsesTranslator{
		dst: rec, flusher: rec, model: "glm-5.2", stream: true,
		id: "resp_test", created: 1700000000, tools: map[int]*responseToolItem{},
	}
	frames := []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := tr.Write([]byte(f)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tr.finish()

	body := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	events := parseEvents(t, body)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed",
	}
	if len(events) < len(want) {
		t.Fatalf("事件数 = %d，期望至少 %d：%s", len(events), len(want), body)
	}
	for i, name := range want {
		if events[i].name != name {
			t.Fatalf("第 %d 个事件 = %q，期望 %q\n%s", i, events[i].name, name, body)
		}
	}
	// 正文拼起来必须是完整文本。
	if got := textOf(events, "response.output_text.delta"); got != "Hello" {
		t.Fatalf("delta 拼接 = %q", got)
	}
	// completed 里带宽 full usage 与 output_text。
	var completed map[string]any
	for _, ev := range events {
		if ev.name == "response.completed" {
			completed = ev.data["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatal("缺 response.completed")
	}
	if completed["output_text"] != "Hello" || completed["status"] != "completed" {
		t.Fatalf("completed = %+v", completed)
	}
	usage, _ := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(2) || usage["total_tokens"] != float64(5) {
		t.Fatalf("usage = %+v", usage)
	}
}

// 流式工具调用：参数分片累积成完整 arguments。
func TestResponsesTranslator_StreamToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := &responsesTranslator{
		dst: rec, flusher: rec, model: "glm-5.2", stream: true,
		id: "resp_t", created: 1, tools: map[int]*responseToolItem{},
	}
	for _, f := range []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\""}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
	} {
		if _, err := tr.Write([]byte(f)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tr.finish()
	events := parseEvents(t, rec.Body.String())
	var item map[string]any
	for _, ev := range events {
		if ev.name == "response.output_item.done" {
			item, _ = ev.data["item"].(map[string]any)
		}
	}
	if item == nil {
		t.Fatalf("缺 output_item.done：%s", rec.Body.String())
	}
	if item["type"] != "function_call" || item["name"] != "f" || item["arguments"] != `{"a":1}` || item["call_id"] != "call_1" {
		t.Fatalf("function_call item = %+v", item)
	}
}

// 非流式：聚合后的 chat.completion → response 资源。
func TestResponsesTranslator_NonStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := &responsesTranslator{dst: rec, model: "glm-5.2", id: "resp_n", created: 99, tools: map[int]*responseToolItem{}}
	tr.WriteHeader(http.StatusOK)
	_, _ = tr.Write([]byte(`{"id":"c1","model":"glm-5.2","choices":[{"message":{"role":"assistant","content":"42"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`))
	tr.finish()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body 不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if resp["object"] != "response" || resp["output_text"] != "42" || resp["id"] != "resp_n" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp["status"] != "completed" {
		t.Fatalf("status = %v", resp["status"])
	}
	out, _ := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output = %+v", out)
	}
}

// 错误路径：chat 的 error 信封 → Responses 的 error 信封，状态码不变。
func TestResponsesTranslator_ErrorPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := &responsesTranslator{dst: rec, model: "m", stream: true, id: "resp_e", tools: map[int]*responseToolItem{}}
	tr.WriteHeader(http.StatusServiceUnavailable)
	_, _ = tr.Write([]byte(`{"error":{"message":"no healthy account","type":"api_error","code":"no_healthy_account"}}`))
	tr.finish()

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body: %v", err)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["message"] != "no healthy account" || errObj["code"] != "no_healthy_account" {
		t.Fatalf("error = %+v", errObj)
	}
}

// 上游 error 帧（200 流中）→ response.failed。
func TestResponsesTranslator_StreamErrorFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := &responsesTranslator{dst: rec, flusher: rec, model: "m", stream: true, id: "resp_f", tools: map[int]*responseToolItem{}}
	_, _ = tr.Write([]byte(`data: {"error":{"code":6004,"message":"rate limited"}}` + "\n\n"))
	tr.finish()
	events := parseEvents(t, rec.Body.String())
	found := false
	for _, ev := range events {
		if ev.name == "response.failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺 response.failed：%s", rec.Body.String())
	}
}

// 思考内容：必须先作为 reasoning 条目进 output，且 message 的 output_index 后移。
func TestResponsesTranslator_ReasoningThenMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := &responsesTranslator{
		dst: rec, flusher: rec, model: "deepseek-v4-flash", stream: true,
		id: "resp_r", created: 1, tools: map[int]*responseToolItem{},
	}
	for _, f := range []string{
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"先想一下"},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"答案"},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
	} {
		if _, err := tr.Write([]byte(f)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tr.finish()
	events := parseEvents(t, rec.Body.String())

	var reasoningDelta string
	for _, ev := range events {
		if ev.name == "response.reasoning_summary_text.delta" {
			reasoningDelta, _ = ev.data["delta"].(string)
		}
	}
	if reasoningDelta != "先想一下" {
		t.Fatalf("reasoning delta = %q，事件：%s", reasoningDelta, rec.Body.String())
	}

	var completed map[string]any
	for _, ev := range events {
		if ev.name == "response.completed" {
			completed, _ = ev.data["response"].(map[string]any)
		}
	}
	out, _ := completed["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output = %+v（期望 reasoning + message 两条）", out)
	}
	first, _ := out[0].(map[string]any)
	second, _ := out[1].(map[string]any)
	if first["type"] != "reasoning" || second["type"] != "message" {
		t.Fatalf("输出顺序错：%v / %v", first["type"], second["type"])
	}
	if completed["output_text"] != "答案" {
		t.Fatalf("output_text = %v", completed["output_text"])
	}
}

// ---- 测试辅助 ----

type sseEvent struct {
	name string
	data map[string]any
}

// parseEvents 把 Responses SSE 流解析成事件列表。
func parseEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name string
		var payload string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event:") {
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if name == "" || payload == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("事件 payload 不是 JSON: %v (%s)", err, payload)
		}
		out = append(out, sseEvent{name: name, data: m})
	}
	return out
}

// textOf 把某个事件里的指定字段拼起来（用于 delta 累加）。
func textOf(events []sseEvent, name string) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.name != name {
			continue
		}
		if s, ok := ev.data["delta"].(string); ok {
			b.WriteString(s)
		}
		if s, ok := ev.data["text"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}
