// Package quota 主动配额扫描（quota scan）。
//
// 背景：上游每条配额线（账号 × 模型）只有在被真实请求撞到 429 时才"有记录"。
// 在那之前没人知道它离限额还有多远，等到真撞上，客户端已经吃了一次失败——
// 而这套网关的整个价值就是"客户端不该看到失败"。
//
// 本包补的就是前半段：按固定间隔为每个 (账号 × 模型) 发一个最小请求（1 token、
// 极短 prompt，见 upstream.ProbeModelQuota），主动看这条配额线是否已被打满：
//
//   - 拿到 429 + 重置时刻 → 立刻在池里登记**模型级**冷却（不是账号级），让真实
//     请求不再撞同一个坑；之后这条线不再主动查（等本地冷却到期再恢复）；
//   - 请求 200 → 这条线活着，继续按间隔查；
//   - 传输失败 / 解析失败 → 不算查过（下次提前重试），只记日志。
//
// 判定口径刻意保守：只有 429 才动冷却。402（余额耗尽）在真实请求路径上已有
// 完整处置（硬冷却到次日 04:00），后台探针再去动账号状态只会重复罚号；其余
// ≥400 一律只记账不处置。
//
// 状态落在 data/quota-scan.json（原子替换、0600），重启后每个组合的"上次查询
// 时刻"仍然有效，不会重启就把所有模型重查一遍。
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Eternal-Xstar/buddygate/internal/auth"
	"github.com/Eternal-Xstar/buddygate/internal/pool"
	"github.com/Eternal-Xstar/buddygate/internal/upstream"
)

// DefaultEvery 默认扫描间隔：6 小时（与 hub 版同口径）。
const DefaultEvery = 6 * time.Hour

// DefaultBlockWindow 上游没给重置时刻时的模型级冷却窗口：1 小时。
//
// 为什么不传零值：pool.CooldownSoftForModel 在 resetAt 为零时退化为**账号级**
// 有界退避（连其它模型一起罚），而探针只掌握"这条线打满了"这一个事实，罚账号
// 是过度反应。给一个保守窗口把它变成模型级冷却，语义才对得上。
const DefaultBlockWindow = time.Hour

// Pair 一条配额线（账号 × 模型）的扫描记录。
type Pair struct {
	UID    string    `json:"uid"`
	Model  string    `json:"model"`
	LastAt time.Time `json:"last_at"`           // 上次成功探明的时刻（失败不推进）
	Status int       `json:"status"`            // 上次 HTTP 状态码
	Kind   string    `json:"kind,omitempty"`    // 上次错误分类
	Until  time.Time `json:"until,omitempty"`   // 已登记的 429 窗口截止
	Detail string    `json:"detail,omitempty"`  // 截断后的错误摘要
}

// Outcome 一次探针的结果（Prober 的返回类型）。
type Outcome struct {
	Status     int
	Kind       upstream.ErrKind
	RetryAfter time.Duration
	Err        error
	Detail     string
}

// Prober 探针接口：生产实现是 UpstreamProber，测试可注入假实现（不碰网络）。
type Prober interface {
	Probe(ctx context.Context, a *auth.Auth, model string) Outcome
}

// UpstreamProber 真实探针：走 upstream.ProbeModelQuota。
type UpstreamProber struct{ Client *upstream.Client }

// Probe 实现 Prober。
func (p UpstreamProber) Probe(ctx context.Context, a *auth.Auth, model string) Outcome {
	if p.Client == nil {
		return Outcome{Err: fmt.Errorf("probe: nil upstream client")}
	}
	status, header, body, err := p.Client.ProbeModelQuota(ctx, a, model)
	out := Outcome{Status: status, Err: err}
	if err != nil && status == 0 {
		out.Detail = truncate(err.Error(), 200)
		return out
	}
	if header != nil {
		if d, ok := upstream.ParseRetryAfter(header); ok {
			out.RetryAfter = d
		}
	}
	if status >= 400 {
		out.Kind = upstream.Classify(status, string(body))
		out.Detail = truncate(string(body), 200)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// State 扫描状态（每组合的上次时刻 + 上次结果），带落盘。
type State struct {
	mu    sync.Mutex
	path  string
	pairs map[string]Pair
}

func pairKey(uid, model string) string { return uid + "\x00" + model }

// Load 读取状态文件；文件不存在 / 损坏都退化为空状态（下次照样能扫）。
func Load(path string) *State {
	s := &State{path: path, pairs: map[string]Pair{}}
	if path == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var disk struct {
		Pairs []Pair `json:"pairs"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		log.Printf("[quota] 解析 %s 失败（从空状态开始）: %v", path, err)
		return s
	}
	for _, p := range disk.Pairs {
		if p.UID == "" || p.Model == "" {
			continue
		}
		s.pairs[pairKey(p.UID, p.Model)] = p
	}
	return s
}

// Save 原子落盘（tmp + rename，0600）。
func (s *State) Save() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	out := struct {
		UpdatedAt time.Time `json:"updated_at"`
		Pairs     []Pair    `json:"pairs"`
	}{UpdatedAt: time.Now(), Pairs: make([]Pair, 0, len(s.pairs))}
	for _, p := range s.pairs {
		out.Pairs = append(out.Pairs, p)
	}
	s.mu.Unlock()

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Due 报告这条线此刻是否该探（上次探明时刻 + every 之后；已有 429 窗口的不再探）。
func (s *State) Due(uid, model string, now time.Time, every time.Duration) bool {
	if every <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pairs[pairKey(uid, model)]
	if !ok {
		return true
	}
	if !p.Until.IsZero() && now.Before(p.Until) {
		return false // 已登记 429 窗口：等它自然到期
	}
	if p.LastAt.IsZero() {
		return true
	}
	return !now.Before(p.LastAt.Add(every))
}

// Mark 记录一次探明结果。ok=false（传输失败）时只更新 Detail，不推进 LastAt。
func (s *State) Mark(uid, model string, p Pair, ok bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := pairKey(uid, model)
	prev := s.pairs[k]
	p.UID, p.Model = uid, model
	if ok {
		p.LastAt = now
	} else {
		// 失败不推进 LastAt，但把"退避"记到 Until 上，避免每次循环都重打一只
		// 深睡的账号：10 分钟后再说。
		p.LastAt = prev.LastAt
		if p.Until.IsZero() {
			p.Until = now.Add(10 * time.Minute)
		}
	}
	s.pairs[k] = p
}

// Forget 清掉一条线（账号被移除 / 模型不再关注时）。
func (s *State) Forget(uid, model string) {
	s.mu.Lock()
	delete(s.pairs, pairKey(uid, model))
	s.mu.Unlock()
}

// Rows 返回当前全部记录（面板 / /status 展示用，按 uid、model 排序）。
func (s *State) Rows() []Pair {
	s.mu.Lock()
	out := make([]Pair, 0, len(s.pairs))
	for _, p := range s.pairs {
		out = append(out, p)
	}
	s.mu.Unlock()
	sortPairs(out)
	return out
}

// sortPairs 稳定排序（uid 再 model），让 /status 与面板的展示顺序跨轮一致。
func sortPairs(ps []Pair) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].UID != ps[j].UID {
			return ps[i].UID < ps[j].UID
		}
		return ps[i].Model < ps[j].Model
	})
}

// Summary 一轮扫描的统计。
type Summary struct {
	Scanned int // 实际发出的探针数
	Blocked int // 判定为 429 并登记模型级冷却的条数
	Healthy int // 200
	Failed  int // 传输 / 解析失败
	Skipped int // 未到期 / 已阻塞 / 无关注模型
	Pairs   int // 状态表条目数
}

// Scanner 把状态、探针、账号池串成一轮扫描。
type Scanner struct {
	Pool  *pool.Pool
	State *State
	// Models 要扫描的模型名列表（来自 config schedule.quota_scan_models）。
	Models []string
	// Every 同一 (账号, 模型) 的最短重扫间隔。
	Every time.Duration
	// BlockWindow 无重置时刻时的模型级冷却窗口（<=0 用 DefaultBlockWindow）。
	BlockWindow time.Duration
	// Prober 可注入（测试）；nil 时用 UpstreamProber。
	Prober Prober
	// Logf 日志出口（nil = log.Printf）。
	Logf func(format string, args ...any)

	mu sync.Mutex
}

func (s *Scanner) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// RunOnce 跑一轮扫描（同步；调用方决定节拍）。
func (s *Scanner) RunOnce(ctx context.Context) Summary {
	var sum Summary
	if s == nil || s.Pool == nil || s.State == nil {
		return sum
	}
	if len(s.Models) == 0 {
		return sum
	}
	every := s.Every
	if every <= 0 {
		every = DefaultEvery
	}
	window := s.BlockWindow
	if window <= 0 {
		window = DefaultBlockWindow
	}
	prober := s.Prober
	if prober == nil {
		prober = UpstreamProber{Client: nil}
	}
	accts := s.Pool.List()
	now := time.Now()
	for _, st := range accts {
		if st.Disabled {
			sum.Skipped += len(s.Models)
			continue
		}
		acct := s.Pool.AuthByUID(st.UID)
		if acct == nil {
			continue
		}
		for _, model := range s.Models {
			if model == "" {
				continue
			}
			// 整个池子在该域已被判死这条模型：不必再逐号探（选号侧本来就会跳过）。
			if s.Pool.ModelBlockedForRealm(model, acct.Realm()).Blocked {
				sum.Skipped++
				continue
			}
			if !s.State.Due(st.UID, model, now, every) {
				sum.Skipped++
				continue
			}
			if err := ctx.Err(); err != nil {
				return sum
			}
			out := prober.Probe(ctx, acct, model)
			sum.Scanned++
			p := Pair{Status: out.Status, Detail: out.Detail}
			if out.Kind != upstream.ErrNone {
				p.Kind = out.Kind.String()
			}
			switch {
			case out.Status == 0 || (out.Err != nil && out.Status == 0):
				// 传输层失败：不推进 LastAt，短暂退避。
				sum.Failed++
				s.State.Mark(st.UID, model, p, false, now)
				s.logf("[quota] 探针失败 uid=%s model=%s: %v", st.UID, model, out.Err)
			case out.Status == 429 || out.Kind == upstream.ErrSoftRate:
				until := out.RetryAfter
				if until <= 0 {
					until = window
				}
				resetAt := now.Add(until)
				p.Until = resetAt
				p.Kind = upstream.ErrSoftRate.String()
				s.State.Mark(st.UID, model, p, true, now)
				// 模型级冷却（不动账号级）：真实请求从此不再分配到这条线。
				s.Pool.CooldownSoftForModel(st.UID, window, resetAt, model, "quota scan: 429")
				sum.Blocked++
				s.logf("[quota] 配额线已打满 uid=%s model=%s reset=%s（已登记模型级冷却）",
					st.UID, model, resetAt.Format(time.RFC3339))
			case out.Status >= 400:
				sum.Failed++
				s.State.Mark(st.UID, model, p, true, now)
				s.logf("[quota] 上游 %d kind=%s uid=%s model=%s body=%s",
					out.Status, p.Kind, st.UID, model, out.Detail)
			default:
				sum.Healthy++
				s.State.Mark(st.UID, model, p, true, now)
			}
		}
	}
	if err := s.State.Save(); err != nil {
		s.logf("[quota] 状态落盘失败: %v", err)
	}
	s.mu.Lock()
	sum.Pairs = len(s.State.Rows())
	s.mu.Unlock()
	if sum.Scanned > 0 {
		s.logf("[quota] 本轮：探 %d · 通 %d · 打满 %d · 失败 %d · 跳过 %d",
			sum.Scanned, sum.Healthy, sum.Blocked, sum.Failed, sum.Skipped)
	}
	return sum
}
