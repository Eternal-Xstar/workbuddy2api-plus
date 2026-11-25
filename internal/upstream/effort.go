// effort.go 思考档位的「下限」策略（upstream.min_effort）。
//
// 为什么需要它：客户端（DeepSeek Harness / Codex 之类）不一定把"推理强度"暴露成
// 用户可选项，默认只会发 `high` 或不发。而上游模型自己声明了 `low/high/max` 三档，
// 想要"始终用最深的思考"就只能在网关侧兜住。
//
// 语义（刻意只往上抬，绝不往下压）：
//   - min_effort 为空 → 完全不动（原样透传，零回归）；
//   - 客户端没写档位 → 补成"支持档位里 ≥ 下限的最高档"；
//   - 客户端写了档位 → 取 max(客户端档位, 下限)，再夹到模型支持的集合里；
//   - 模型不支持该档（如 hy3 只有 low/high，下限却是 max）→ 退到它自己支持的最高档。
//
// 与「不影响思考能力」的红线一致：本文件只会**增加**思考量，不会减少；也不触碰
// thinking.type、tools、tool_choice 等任何其它字段。
package upstream

import (
	"encoding/json"
	"strings"
)

// effortOrder 档位由弱到强（与 payload.go 的 effortRank 同序，独立一份避免耦合）。
var effortOrder = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

func effortIndex(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	for i, v := range effortOrder {
		if v == s {
			return i
		}
	}
	return -1
}

// ApplyMinEffort 按 minEffort 下限调整请求体里的思考档位。minEffort 为空或无法
// 解析 body 时原样返回。返回的字节可能指向新分配的切片。
func ApplyMinEffort(body []byte, minEffort string, efforts map[string][]string) []byte {
	floor := effortIndex(minEffort)
	if floor < 0 || len(body) == 0 {
		return body
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	model, _ := obj["model"].(string)

	// 目标档位：模型声明了支持集合就夹进集合，否则用全局序里的下限。
	target := minEffort
	if supported, ok := efforts[model]; ok && len(supported) > 0 {
		best, bestIdx := "", -1
		for _, s := range supported {
			idx := effortIndex(s)
			if idx < 0 || idx < floor {
				continue
			}
			if idx > bestIdx {
				best, bestIdx = s, idx
			}
		}
		if best == "" {
			// 没有 ≥ 下限的档位：退到它支持的最高档（而不是丢掉思考）。
			for _, s := range supported {
				idx := effortIndex(s)
				if idx > bestIdx {
					best, bestIdx = s, idx
				}
			}
		}
		if best == "" {
			return body
		}
		target = best
	}

	key := "reasoning_effort"
	if _, has := obj["reasoning_effort"]; !has {
		if _, hasAlt := obj["reasoningEffort"]; hasAlt {
			key = "reasoningEffort"
		}
	}
	if cur, ok := obj[key].(string); ok && effortIndex(cur) >= floor {
		return body // 客户端已经不低于下限：一个字节都不改
	}
	obj[key] = target
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
