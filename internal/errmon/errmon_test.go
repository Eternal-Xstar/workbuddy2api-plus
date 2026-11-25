package errmon

import (
	"testing"
	"time"
)

func TestSummaryBucketsAndSeverity(t *testing.T) {
	m := New()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	m.Note(Event{At: now.Add(-1 * time.Hour), Code: "11128", Kind: "channel_refused", Status: 400, UID: "u1", Model: "deepseek-v4.1-flash"})
	m.Note(Event{At: now.Add(-2 * time.Hour), Code: "11128", Kind: "channel_refused", Status: 400, UID: "u2"})
	m.Note(Event{At: now.Add(-3 * time.Hour), Code: "6004", Kind: "soft_rate", Status: 429, UID: "u1"})
	m.Note(Event{At: now.Add(-30 * time.Hour), Code: "old", Kind: "server", Status: 500}) // 窗口外

	s := m.Summary(24, now)
	if s.Total != 3 {
		t.Fatalf("窗口内事件 = %d，期望 3", s.Total)
	}
	if len(s.Buckets) != 2 {
		t.Fatalf("桶数 = %d，期望 2（窗口外的旧事件不计）", len(s.Buckets))
	}
	// 红级（channel_refused）必须排在最前。
	if s.Buckets[0].Kind != "channel_refused" {
		t.Fatalf("排序不对，首桶 = %+v", s.Buckets[0])
	}
	if s.Buckets[0].Severity != "bad" || s.Buckets[0].Count != 2 {
		t.Fatalf("红桶聚合不对: %+v", s.Buckets[0])
	}
	if len(s.Buckets[0].UIDs) != 2 {
		t.Fatalf("涉及账号数 = %v", s.Buckets[0].UIDs)
	}
	if s.Buckets[0].First.After(s.Buckets[0].Last) {
		t.Fatal("first/last 颠倒")
	}
	if len(s.Alerts) != 2 || s.Alerts[0] == "" {
		t.Fatalf("alerts = %v", s.Alerts)
	}
	// 429 归黄级。
	for _, b := range s.Buckets {
		if b.Kind == "soft_rate" && b.Severity != "warn" {
			t.Fatalf("soft_rate 应为 warn，实得 %q", b.Severity)
		}
	}
}

// 环形缓冲有界：写超出容量不 panic，且只保留最近 ringLimit 条。
func TestRingIsBounded(t *testing.T) {
	m := New()
	now := time.Now()
	for i := 0; i < ringLimit+50; i++ {
		m.Note(Event{At: now, Code: "x", Kind: "server", Status: 500})
	}
	s := m.Summary(24, now.Add(time.Second))
	if s.Total != ringLimit {
		t.Fatalf("容量 = %d，期望 %d", s.Total, ringLimit)
	}
}

// 空监控不能 panic，且返回空桶/空告警。
func TestEmpty(t *testing.T) {
	var m *Monitor
	m.Note(Event{}) // nil 接收者安全
	s := New().Summary(0, time.Now())
	if s.WindowHours != 24 || s.Total != 0 || len(s.Buckets) != 0 || len(s.Alerts) != 0 {
		t.Fatalf("空摘要不对: %+v", s)
	}
}

// 没有 code 时按 kind 兜底展示。
func TestBucketWithoutCode(t *testing.T) {
	m := New()
	now := time.Now()
	m.Note(Event{At: now, Kind: "hard_credit", Status: 402})
	s := m.Summary(24, now)
	if len(s.Buckets) != 1 || s.Buckets[0].Severity != "bad" {
		t.Fatalf("无 code 的桶不对: %+v", s.Buckets)
	}
	if s.Alerts[0] == "" {
		t.Fatal("告警文案为空")
	}
}
