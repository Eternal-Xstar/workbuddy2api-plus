package reqlog

import (
	"testing"
	"time"
)

// 分位数与滑动窗口：平均值的毛病是"9 次快 + 1 次慢"看起来和"每次都中速"一样，
// 这里把 p50/p95 与近 5 分钟/1 小时失败率钉住。
func TestSnapshotPercentilesAndWindow(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	base := time.Now()

	// 100 条成功：耗时 1..100ms，TTFB 同值。
	for i := 1; i <= 100; i++ {
		r.Record(Event{
			Time: base, Status: 200, OK: true,
			DurationMs: int64(i), TTFBMs: int64(i),
		})
	}
	// 20 条失败（500）混进最近窗口。
	for i := 0; i < 20; i++ {
		r.Record(Event{Time: base, Status: 500, OK: false, DurationMs: 5})
	}

	s := r.Snapshot()
	if s.Completed != 120 || s.Succeeded != 100 {
		t.Fatalf("counts = %+v", s)
	}
	// 分位数取桶上界：p50 应落在 50~100 之间，p95 明显大于 p50。
	if s.P50DurationMs <= 0 || s.P50DurationMs > 100 {
		t.Fatalf("p50 = %v", s.P50DurationMs)
	}
	if s.P95DurationMs < s.P50DurationMs || s.P95DurationMs > 100 {
		t.Fatalf("p95 = %v", s.P95DurationMs)
	}
	if s.P50TTFBMs <= 0 || s.P95TTFBMs < s.P50TTFBMs {
		t.Fatalf("ttfb 分位 = %v / %v", s.P50TTFBMs, s.P95TTFBMs)
	}
	// 滑动窗口：120 条全在最近，失败率 = 20/120 ≈ 16.7%。
	w := s.Window5m
	if w.Completed != 120 || w.Failed != 20 {
		t.Fatalf("window5m = %+v", w)
	}
	if w.FailRate < 16 || w.FailRate > 17 {
		t.Fatalf("window5m fail_rate = %v", w.FailRate)
	}
	if s.Window1h.Completed != 120 {
		t.Fatalf("window1h = %+v", s.Window1h)
	}
}

// 窗口只统计落在窗口内的分钟桶：把事件放到 2 小时前，5 分钟窗口应为空。
func TestWindowExcludesOldEvents(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	old := time.Now().Add(-2 * time.Hour)
	r.Record(Event{Time: old, Status: 500, OK: false, DurationMs: 10})
	r.Record(Event{Time: time.Now(), Status: 200, OK: true, DurationMs: 10})

	s := r.Snapshot()
	if s.Window5m.Completed != 1 || s.Window5m.Failed != 0 {
		t.Fatalf("window5m = %+v（旧事件不该计入）", s.Window5m)
	}
	// 进程级成功率仍然是 1/2。
	if s.SuccessRate != 50 {
		t.Fatalf("success_rate = %v", s.SuccessRate)
	}
}

// 无样本时不给分位数（omitempty 语义，面板显示 — 而不是 0ms）。
func TestSnapshotPercentilesEmpty(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	s := r.Snapshot()
	if s.P50DurationMs != 0 || s.P95DurationMs != 0 || s.P50TTFBMs != 0 {
		t.Fatalf("空样本不该有分位数：%+v", s)
	}
}
