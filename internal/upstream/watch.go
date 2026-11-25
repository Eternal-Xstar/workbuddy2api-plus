// watch.go 上游「契约漂移」探测（静默失效告警）。
//
// 背景：这套网关的所有自动化（签到 / 活跃上报 / 旅行 / 夜猫子 / 补偿 / 成长任务）
// 都压在硬编码的上游路径与信封结构上。上游改版时最危险的形态不是报错，而是
// **静默失效**——HTTP 200、业务 code=0，但字段改名/结构变浅，于是：
//
//   - 签到"成功"了但积分没涨；
//   - 成长任务的 current 恒为 0，队列永远显示"没有待办"；
//   - 余额解析成 0，号被积分保底挡在选号之外。
//
// 判定口径（刻意保守，避免误报）：
//
//   - 只有 **非 *Error 的错误** 才算"契约漂移"。*Error 是上游用我们认识的信封
//     说了话（401/402/429/11102…），那是账号或额度问题；而 json.Unmarshal 失败、
//     200 却返回非 JSON、读 body 失败，才是"它的形状变了/我们看不懂了"。
//   - 同一条路径族（kind）连续 3 次都是这类错误 → 标记 Suspected 并打一次 WARN，
//     之后同类失败只累加计数、不重复刷屏；任一次成功即清零。
//   - 未归类的路径（模型目录等）不记账，避免把无关抖动算进来。
//
// 与 panel/scheduler 的关系：本文件只做判定与计数，不参与任何重试或熔断；
// 面板读 TaskHealthSnapshot() 显示红标，日志读 WARN 行。零侵入既有链路。
package upstream

import (
	"errors"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// watchThreshold 连续多少次契约漂移才判定"上游疑似改版"。
const watchThreshold = 3

// TaskHealth 一类任务的契约健康度。
type TaskHealth struct {
	Kind        string    `json:"kind"`
	Consecutive int       `json:"consecutive"`
	Suspected   bool      `json:"suspected"`
	Since       time.Time `json:"since,omitempty"`     // Suspected 起点
	LastOKAt    time.Time `json:"last_ok,omitempty"`   // 最近一次成功
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
}

var (
	watchMu  sync.Mutex
	watchMap = map[string]*TaskHealth{}
)

// watchKindOf 把上游路径归到「一条契约」上。同一路径族 = 同一个 kind。
//
// 归类的粒度取向：签到 / 补偿 / 报表 / 成长 / 旅行 / 余额 / 鉴权 / 目录。
// 同一族内不同子路径（status vs claim）共享计数——它们同属一次改版的受害者。
func watchKindOf(rawPath string) string {
	path := rawPath
	if u, err := url.Parse(rawPath); err == nil && u.Path != "" {
		path = u.Path
	}
	switch {
	case strings.Contains(path, "daily-checkin"):
		return "checkin"
	case strings.Contains(path, "compensation"):
		return "compensation"
	case strings.Contains(path, "/report"):
		return "report"
	case strings.Contains(path, "growth"):
		return "growth"
	case strings.Contains(path, "/activity/"):
		return "travel"
	case strings.Contains(path, "get-user-resource"), strings.Contains(path, "get-enterprise-user-usage"):
		return "balance"
	case strings.Contains(path, "/plugin/auth/"), strings.Contains(path, "/plugin/login/"):
		return "auth"
	case strings.Contains(path, "/enterprises/"), strings.Contains(path, "/v3/config"), strings.Contains(path, "/models"):
		return "catalog"
	}
	return ""
}

// contractDrift 报告 err 是否属于"上游形状变了/我们看不懂"这一类失败。
//
// 传输层失败（连不上、超时）也不算：那是网络与账号出口的问题，与上游契约无关。
func contractDrift(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if errors.As(err, &ue) {
		return false // 上游用我们认识的信封回了错：账号/额度/限流，不是契约漂移
	}
	msg := err.Error()
	// transport / 本地错误一律排除（net 层错误文案特征稳定，足够判断）。
	for _, marker := range []string{
		"connection refused", "connection reset", "no such host", "i/o timeout",
		"context deadline exceeded", "context canceled", "tls: ", "EOF",
	} {
		if strings.Contains(msg, marker) {
			return false
		}
	}
	return true // 剩下的是 parse failed / 非 JSON 200 / 读 body 失败
}

// noteTaskResult 由 doJSON 在每次短 RPC 出口调用（kind=="" 时忽略）。
func noteTaskResult(rawPath string, err error) {
	kind := watchKindOf(rawPath)
	if kind == "" {
		return
	}
	now := time.Now()
	watchMu.Lock()
	h := watchMap[kind]
	if h == nil {
		h = &TaskHealth{Kind: kind}
		watchMap[kind] = h
	}
	if err == nil {
		h.Consecutive = 0
		h.Suspected = false
		h.Since = time.Time{}
		h.LastOKAt = now
		watchMu.Unlock()
		return
	}
	h.LastError = err.Error()
	h.LastErrorAt = now
	drift := contractDrift(err)
	if drift {
		h.Consecutive++
	}
	newlySuspected := false
	if drift && h.Consecutive >= watchThreshold && !h.Suspected {
		h.Suspected = true
		h.Since = now
		newlySuspected = true
	}
	consec := h.Consecutive
	watchMu.Unlock()

	if newlySuspected {
		log.Printf("WARN: [upstream] %s 任务连续 %d 次契约漂移，上游疑似改版 —— 最近错误: %s",
			kind, consec, truncate(err.Error(), 200))
	}
}

// TaskHealthSnapshot 返回全部已记账种类的健康度（按 kind 排序，深拷贝）。
func TaskHealthSnapshot() []TaskHealth {
	watchMu.Lock()
	out := make([]TaskHealth, 0, len(watchMap))
	for _, h := range watchMap {
		out = append(out, *h)
	}
	watchMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// SuspectedKinds 返回当前被判"疑似改版"的种类名（面板红标直接渲染）。
func SuspectedKinds() []string {
	var out []string
	for _, h := range TaskHealthSnapshot() {
		if h.Suspected {
			out = append(out, h.Kind)
		}
	}
	return out
}

// ResetTaskWatch 清空计数（仅测试使用）。
func ResetTaskWatch() {
	watchMu.Lock()
	watchMap = map[string]*TaskHealth{}
	watchMu.Unlock()
}
