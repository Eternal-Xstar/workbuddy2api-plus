package upstream

import (
	"encoding/json"
	"testing"
)

func decodeEffort(t *testing.T, body []byte) (string, bool) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("body 不是 JSON: %v", err)
	}
	v, ok := obj["reasoning_effort"].(string)
	return v, ok
}

// 下限为空 → 一个字节都不动（零回归）。
func TestApplyMinEffortDisabled(t *testing.T) {
	body := []byte(`{"model":"m","reasoning_effort":"low"}`)
	out := ApplyMinEffort(body, "", map[string][]string{"m": {"low", "high", "max"}})
	if string(out) != string(body) {
		t.Fatalf("空下限不该改写：%s", out)
	}
}

// 客户端给了低档 → 抬到下限（max）。
func TestApplyMinEffortUpgrades(t *testing.T) {
	efforts := map[string][]string{"m": {"low", "high", "max"}}
	out := ApplyMinEffort([]byte(`{"model":"m","reasoning_effort":"low"}`), "max", efforts)
	if got, ok := decodeEffort(t, out); !ok || got != "max" {
		t.Fatalf("low → %q, want max", got)
	}
}

// 客户端已经 >= 下限 → 不动（不降级）。
func TestApplyMinEffortNeverDowngrades(t *testing.T) {
	efforts := map[string][]string{"m": {"low", "high", "max"}}
	out := ApplyMinEffort([]byte(`{"model":"m","reasoning_effort":"max"}`), "high", efforts)
	if got, _ := decodeEffort(t, out); got != "max" {
		t.Fatalf("max 被降级成 %q", got)
	}
}

// 客户端没写档位 → 补成支持集合里 >= 下限的最高档。
func TestApplyMinEffortFills(t *testing.T) {
	efforts := map[string][]string{"m": {"low", "high", "max"}}
	out := ApplyMinEffort([]byte(`{"model":"m","messages":[]}`), "max", efforts)
	if got, ok := decodeEffort(t, out); !ok || got != "max" {
		t.Fatalf("未写档位 → %q(ok=%v), want max", got, ok)
	}
}

// 模型不支持该档（hy3 只有 low/high，下限 max）→ 退到它自己支持的最高档，不能丢思考。
func TestApplyMinEffortClampsToSupported(t *testing.T) {
	efforts := map[string][]string{"hy3": {"low", "high"}}
	out := ApplyMinEffort([]byte(`{"model":"hy3","reasoning_effort":"low"}`), "max", efforts)
	if got, _ := decodeEffort(t, out); got != "high" {
		t.Fatalf("hy3 应夹到 high，实得 %q", got)
	}
}

// 兼容客户端的驼峰键 reasoningEffort。
func TestApplyMinEffortCamelKey(t *testing.T) {
	efforts := map[string][]string{"m": {"low", "high", "max"}}
	out := ApplyMinEffort([]byte(`{"model":"m","reasoningEffort":"low"}`), "max", efforts)
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["reasoningEffort"] != "max" {
		t.Fatalf("驼峰键没被抬：%v", obj["reasoningEffort"])
	}
	if _, has := obj["reasoning_effort"]; has {
		t.Fatal("不该凭空多出一个下划线键")
	}
}

// 未知模型（无支持集合）→ 用全局序里的下限值。
func TestApplyMinEffortUnknownModel(t *testing.T) {
	out := ApplyMinEffort([]byte(`{"model":"whatever","reasoning_effort":"low"}`), "max", nil)
	if got, _ := decodeEffort(t, out); got != "max" {
		t.Fatalf("未知模型下限应为 max，实得 %q", got)
	}
}

// 只改档位，其它字段（tools/thinking/messages）一个不动。
func TestApplyMinEffortKeepsEverythingElse(t *testing.T) {
	efforts := map[string][]string{"m": {"low", "high", "max"}}
	body := []byte(`{"model":"m","thinking":{"type":"enabled"},"tools":[{"type":"function"}],` +
		`"tool_choice":"auto","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)
	out := ApplyMinEffort(body, "max", efforts)
	var before, after map[string]any
	_ = json.Unmarshal(body, &before)
	_ = json.Unmarshal(out, &after)
	for _, k := range []string{"thinking", "tools", "tool_choice", "messages", "model"} {
		b, _ := json.Marshal(before[k])
		a, _ := json.Marshal(after[k])
		if string(b) != string(a) {
			t.Fatalf("%s 被改动了：%s → %s", k, b, a)
		}
	}
}
