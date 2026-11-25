package server

import (
	"encoding/json"
	"testing"
)

// 预告句判据：只认"明显没写完"的尾部，正常短答不能被续。
func TestNarrationTail(t *testing.T) {
	cases := map[string]bool{
		"5G 已降到 10 dBm（降低约 32 倍）。重新连接并测试下行：": true,
		"验证路由器状态并做对比实验（恢复到 25dBm 看是否必然重启）：":     true,
		"先看完整表单……":                          true,
		"```go\nfunc main() {":                 true, // 未闭合代码块
		"这是结论。":                              false,
		"好的！":                                false,
		"短":                                  false, // 太短不判
		"我认为这个方案可行，但要注意边界条件":                 true, // 无句末标点
	}
	for text, want := range cases {
		if got := narrationTail(text); got != want {
			t.Fatalf("narrationTail(%q) = %v, want %v", text, got, want)
		}
	}
}

// 空回复判定：finish=length + 正文空 + 无工具。
func TestAggregateStarved(t *testing.T) {
	mk := func(finish, content string, tool bool) map[string]any {
		msg := map[string]any{"role": "assistant", "content": content}
		if tool {
			msg["tool_calls"] = []any{map[string]any{"id": "c1"}}
		}
		return map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": msg}}}
	}
	if !aggregateStarved(mk("length", "", false)) {
		t.Fatal("length + 空正文 应判空回复")
	}
	if aggregateStarved(mk("length", "half", false)) {
		t.Fatal("有正文的截断不是空回复")
	}
	if aggregateStarved(mk("stop", "", false)) {
		t.Fatal("stop 不是空回复")
	}
	if aggregateStarved(mk("length", "", true)) {
		t.Fatal("有工具调用不算空回复")
	}
}

// 预告句 / 纯工具轮判定：没有工具声明时两者都不判。
func TestAggregateNarrationAndToolOnly(t *testing.T) {
	mk := func(content string) map[string]any {
		return map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": content},
		}}}
	}
	tail := "分析完毕，接着验证："
	done := "这是完整结论，无需再动。"

	if aggregateNarration(mk(tail), 0) {
		t.Fatal("没有工具声明不该判预告句")
	}
	if !aggregateNarration(mk(tail), 3) {
		t.Fatal("有工具声明 + 预告尾部 应判 narration")
	}
	if aggregateNarration(mk(done), 3) {
		t.Fatal("写完的短答不是 narration")
	}
	if !aggregateToolOnly(mk(done), 3) {
		t.Fatal("有工具声明 + 句号收尾 + 有正文 应判 toolonly")
	}
	if aggregateToolOnly(mk(done), 0) {
		t.Fatal("没有工具声明不该判 toolonly")
	}
}

// 空回复救援体：**只抬预算，思考设置原样保留**（用户硬要求：长会话不影响思考强度）；
// 探活级别的小额度不救。
func TestRescueBodyStarved(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":500,"reasoning_effort":"high","thinking":{"type":"enabled","budget_tokens":2048}}`)
	out, ok := rescueBodyStarved(body, rescueBudgetFloor, 2)
	if !ok {
		t.Fatal("应可改写")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	th, _ := obj["thinking"].(map[string]any)
	if th["type"] != "enabled" {
		t.Fatalf("thinking.type 被改了: %v", th["type"])
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort 被改了: %v", obj["reasoning_effort"])
	}
	// 500 × 2 = 1000 < floor 4096 → 抬到 floor。
	if mt, _ := obj["max_tokens"].(float64); int64(mt) != rescueBudgetFloor {
		t.Fatalf("max_tokens = %v, want %d", obj["max_tokens"], rescueBudgetFloor)
	}

	// 大预算时按倍数抬：8000 × 2 = 16000。
	big := []byte(`{"model":"m","messages":[],"max_tokens":8000}`)
	bo, ok := rescueBodyStarved(big, rescueBudgetFloor, 2)
	if !ok {
		t.Fatal("大预算也应能抬")
	}
	var bo2 map[string]any
	_ = json.Unmarshal(bo, &bo2)
	if mt, _ := bo2["max_tokens"].(float64); int64(mt) != 16000 {
		t.Fatalf("大预算倍率 = %v, want 16000", bo2["max_tokens"])
	}

	// 探活级别的小额度 → 不救。
	probe := []byte(`{"model":"m","messages":[],"max_tokens":1}`)
	if _, ok := rescueBodyStarved(probe, rescueBudgetFloor, 2); ok {
		t.Fatal("max_tokens=1 不该触发救援")
	}
}

// 续写体：已输出正文作为 assistant 前缀 + 续写指令。
func TestRescueBodyContinue(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"do it"}],"tools":[{"type":"function"}]}`)
	out, ok := rescueBodyContinue(body, "已写到这里：")
	if !ok {
		t.Fatal("应可改写")
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	last, _ := msgs[1].(map[string]any)
	if last["role"] != "assistant" || last["content"] != "已写到这里：" {
		t.Fatalf("前缀消息 = %+v", last)
	}
	nudge, _ := msgs[2].(map[string]any)
	if nudge["role"] != "user" {
		t.Fatalf("续写指令 = %+v", nudge)
	}
	if _, ok := obj["tools"]; !ok {
		t.Fatal("tools 必须原样保留")
	}
	if _, ok := rescueBodyContinue(body, "  "); ok {
		t.Fatal("空前缀不该改写")
	}
}

// 合并：narration 接正文、toolonly 只采纳工具、usage 相加。
func TestMergeContinuation(t *testing.T) {
	mk := func(finish, content string, tool bool, usage map[string]any) map[string]any {
		msg := map[string]any{"role": "assistant", "content": content}
		if tool {
			msg["tool_calls"] = []any{map[string]any{"id": "call_1", "type": "function"}}
		}
		return map[string]any{
			"choices": []any{map[string]any{"finish_reason": finish, "message": msg}},
			"usage":   usage,
		}
	}
	orig := mk("stop", "结论如下：", false, map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(5), "total_tokens": float64(15)})

	// narration：续写段给正文 → 拼接。
	rescueText := mk("stop", "第一步先重启。", false, map[string]any{"completion_tokens": float64(7), "total_tokens": float64(7)})
	merged, ok := mergeContinuation(orig, rescueText, true)
	if !ok {
		t.Fatal("应合并")
	}
	if got := choiceContent(merged); got != "结论如下：第一步先重启。" {
		t.Fatalf("合并正文 = %q", got)
	}
	if u, _ := merged["usage"].(map[string]any); u["completion_tokens"] != float64(12) {
		t.Fatalf("usage 未相加: %+v", u)
	}

	// toolonly：续写段只给正文 → 不采纳。
	orig2 := mk("stop", "这是结论。", false, nil)
	if _, ok := mergeContinuation(orig2, rescueText, false); ok {
		t.Fatal("toolonly 不该采纳正文")
	}
	// toolonly：续写段给工具调用 → 采纳。
	rescueTool := mk("tool_calls", "", true, nil)
	merged2, ok := mergeContinuation(orig2, rescueTool, false)
	if !ok {
		t.Fatal("toolonly 应采纳工具调用")
	}
	chs, _ := merged2["choices"].([]any)
	c, _ := chs[0].(map[string]any)
	if c["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", c["finish_reason"])
	}
}

// 能力保护（用户硬要求）：救援体不得改动思考设置、工具声明与工具选择。
// 这条测试是"红线"——任何将来想通过改 body 提升成功率的改动，都必须先让它变红。
func TestRescueBodyPreservesCapabilities(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":100,` +
		`"thinking":{"type":"enabled","budget_tokens":4096},"reasoning_effort":"high",` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],` +
		`"tool_choice":"auto","parallel_tool_calls":true}`)
	out, ok := rescueBodyStarved(body, 4096, 2)
	if !ok {
		t.Fatal("应可改写")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	var want map[string]any
	_ = json.Unmarshal(body, &want)

	for _, key := range []string{"thinking", "reasoning_effort", "tools", "tool_choice", "parallel_tool_calls"} {
		if !jsonEqual(got[key], want[key]) {
			t.Fatalf("%s 被改动了：%v → %v", key, want[key], got[key])
		}
	}
	if mt, _ := got["max_tokens"].(float64); int64(mt) != 4096 {
		t.Fatalf("预算应抬到 4096，实得 %v", got["max_tokens"])
	}

	// 续写体同样不得丢工具声明。
	cbody, cok := rescueBodyContinue(body, "已写到这里")
	if !cok {
		t.Fatal("续写体应可构造")
	}
	var cgot map[string]any
	_ = json.Unmarshal(cbody, &cgot)
	if !jsonEqual(cgot["tools"], want["tools"]) {
		t.Fatalf("续写体丢了 tools：%v", cgot["tools"])
	}
	if !jsonEqual(cgot["thinking"], want["thinking"]) {
		t.Fatalf("续写体改了 thinking：%v", cgot["thinking"])
	}
}

// jsonEqual 比较任意 JSON 值的语义相等（经一次序列化归一化）。
func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}