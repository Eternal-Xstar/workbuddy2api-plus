// Package errmon 上游错误的近因聚合：把"日志里一闪而过的 WARN"变成面板上的红标。
//
// 为什么需要：11-128 通道校验、6004 限流、402 余额不足这类错误平时只出现在
// 日志文件里，一个人在本机部署时根本不会盯着看，等到"怎么不能用了"才发现。
// 这里保存最近 N 条**上游错误事件**（有界，不写盘、不掉内存），按 code/kind
// 聚合出窗口内次数、首次末次、涉及账号，并给出建议严重度。
//
// 只记元数据：code / kind / HTTP 状态 / 账号 uid / 模型名 / 时间。不记请求体、
// 不记响应正文、不记 token。
package errmon

import (
	"sort"
	"sync"
	"time"
)

// ringLimit 内存里保留的事件条数上限（有界，进程重启清零）。
const ringLimit = 300

// Event 一次上游错误。
type Event struct {
	At     time.Time `json:"at"`
	Code   string    `json:"code,omitempty"`   // 上游业务码（11128 / 11-128 / 6004 …）
	Kind   string    `json:"kind,omitempty"`   // 网关分类（channel_refused / soft_rate …）
	Status int       `json:"status,omitempty"` // HTTP 状态码
	UID    string    `json:"uid,omitempty"`    // 账号 uid（脱敏由调用方决定）
	Model  string    `json:"model,omitempty"`
}

// Bucket 一组同类错误的聚合结果。
type Bucket struct {
	Code   string    `json:"code,omitempty"`
	Kind   string    `json:"kind,omitempty"`
	Status int       `json:"status,omitempty"`
	Count  int       `json:"count"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
	UIDs   []string  `json:"uids,omitempty"`
	Models []string  `json:"models,omitempty"`
	// Severity "" (提示) / "warn" / "bad"
	Severity string `json:"severity"`
	// Hint 一句话人话解释，面板红条直接显示。
	Hint string `json:"hint,omitempty"`
}

// Summary 聚合结果。
type Summary struct {
	WindowHours int      `json:"window_hours"`
	Total       int      `json:"total"`
	Buckets     []Bucket `json:"buckets"`
	Alerts      []string `json:"alerts"` // 需要顶部红标显示的句子（最多 3 条）
}

// Monitor 有界环形缓冲 + 聚合。
type Monitor struct {
	mu   sync.Mutex
	ring []Event
	next int
	full bool
}

// New 构造。
func New() *Monitor { return &Monitor{ring: make([]Event, ringLimit)} }

// Note 记录一次错误。code 为空时按 status 归桶。
func (m *Monitor) Note(e Event) {
	if m == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	m.mu.Lock()
	m.ring[m.next] = e
	m.next = (m.next + 1) % ringLimit
	if m.next == 0 {
		m.full = true
	}
	m.mu.Unlock()
}

// events 返回窗口内事件快照（新→旧无序，聚合自己排序）。
func (m *Monitor) events(since time.Time) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.next
	if m.full {
		n = ringLimit
	}
	out := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		e := m.ring[i]
		if e.At.IsZero() || e.At.Before(since) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// severity 按 kind 给建议严重度与提示语。
//
// 判断口径（与 handler 的错误分类一致）：
//   - channel_refused（11-128）：请求形态被通道校验拦下 → 红。它不影响账号健康，
//     但用户侧表现为"突然不能用了"，必须显式告警。
//   - hard_credit / account_restricted：账号级硬故障 → 红。
//   - waf_block / forbidden：出口 IP 被打 → 红。
//   - soft_rate / upstream_busy / server / timeout：可自愈 → 黄。
//   - 其余：提示级。
func severity(kind string) (string, string) {
	switch kind {
	case "channel_refused":
		return "bad", "通道校验拦截（11-128）：请求形态被上游判为非官方客户端"
	case "hard_credit":
		return "bad", "账号余额不足（402）：某个号的积分已耗尽"
	case "account_restricted":
		return "bad", "账号被上游限制（11140）：官方客户端也未必可用"
	case "waf_block", "forbidden":
		return "bad", "出口 IP 被上游风控（403）：换网/换代理才会好"
	case "soft_rate", "upstream_busy":
		return "warn", "上游限流/排队：会在重置时间自动恢复"
	case "server":
		return "warn", "上游 5xx：服务端波动，重试即可"
	case "timeout":
		return "warn", "上游超时：网络或上游拥塞"
	default:
		return "", ""
	}
}

// Summary 聚合最近 hours 小时内的事件；hours<=0 时按 24 小时。
func (m *Monitor) Summary(hours int, now time.Time) Summary {
	if hours <= 0 {
		hours = 24
	}
	if now.IsZero() {
		now = time.Now()
	}
	since := now.Add(-time.Duration(hours) * time.Hour)
	evs := m.events(since)

	type key struct{ code, kind string }
	idx := map[key]*Bucket{}
	uidSeen := map[key]map[string]bool{}
	modelSeen := map[key]map[string]bool{}
	for _, e := range evs {
		k := key{e.Code, e.Kind}
		b := idx[k]
		if b == nil {
			b = &Bucket{Code: e.Code, Kind: e.Kind, Status: e.Status, First: e.At}
			sev, hint := severity(e.Kind)
			b.Severity, b.Hint = sev, hint
			idx[k] = b
			uidSeen[k] = map[string]bool{}
			modelSeen[k] = map[string]bool{}
		}
		b.Count++
		if e.At.Before(b.First) {
			b.First = e.At
		}
		if e.At.After(b.Last) {
			b.Last = e.At
		}
		if b.Status == 0 {
			b.Status = e.Status
		}
		if e.UID != "" {
			uidSeen[k][e.UID] = true
		}
		if e.Model != "" {
			modelSeen[k][e.Model] = true
		}
	}

	out := make([]Bucket, 0, len(idx))
	for k, b := range idx {
		b.UIDs = keysOf(uidSeen[k])
		b.Models = keysOf(modelSeen[k])
		out = append(out, *b)
	}
	// 严重度优先，其次次数多者在前；同分按 code 稳定排序。
	rank := map[string]int{"bad": 0, "warn": 1, "": 2}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Severity] != rank[out[j].Severity] {
			return rank[out[i].Severity] < rank[out[j].Severity]
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Code+out[i].Kind < out[j].Code+out[j].Kind
	})

	alerts := make([]string, 0, 3)
	for _, b := range out {
		if b.Severity == "" {
			continue
		}
		label := b.Code
		if label == "" {
			label = b.Kind
		}
		line := label + "：" + b.Hint + "（近 " + itoa(hours) + "h " + itoa(b.Count) + " 次"
		if n := len(b.UIDs); n > 0 {
			line += "，涉及 " + itoa(n) + " 个号"
		}
		line += "）"
		alerts = append(alerts, line)
		if len(alerts) == 3 {
			break
		}
	}

	return Summary{WindowHours: hours, Total: len(evs), Buckets: out, Alerts: alerts}
}

func keysOf(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// itoa 避免为了两个数字引入 strconv 依赖噪声（本包只用于拼提示语）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
