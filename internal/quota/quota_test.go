package quota

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeProber 按 (uid|model) 返回预设结果，记录调用次数。
type fakeProber struct {
	out   map[string]Outcome
	calls int
	seen  []string
}

func (f *fakeProber) Probe(_ context.Context, a *auth.Auth, model string) Outcome {
	f.calls++
	key := a.UID + "|" + model
	f.seen = append(f.seen, key)
	return f.out[key]
}

func newTestScanner(t *testing.T, p *pool.Pool, pr Prober, every time.Duration) (*Scanner, *State) {
	t.Helper()
	st := Load(filepath.Join(t.TempDir(), "quota-scan.json"))
	return &Scanner{
		Pool:   p,
		State:  st,
		Models: []string{"m1", "m2"},
		Every:  every,
		Prober: pr,
		Logf:   func(string, ...any) {},
	}, st
}

// 429 → 登记模型级冷却（不是账号级），且同一条线在窗口内不再重探。
func TestScannerMarksModelCooldownOn429(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	fp := &fakeProber{out: map[string]Outcome{
		"u1|m1": {Status: 429, Kind: upstream.ErrSoftRate, RetryAfter: 30 * time.Minute},
		"u1|m2": {Status: 200},
	}}
	sc, st := newTestScanner(t, p, fp, time.Hour)

	sum := sc.RunOnce(context.Background())
	if sum.Scanned != 2 || sum.Blocked != 1 || sum.Healthy != 1 {
		t.Fatalf("summary = %+v, want scanned=2 blocked=1 healthy=1", sum)
	}
	rows := st.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	var m1 *Pair
	for i := range rows {
		if rows[i].Model == "m1" {
			m1 = &rows[i]
		}
	}
	if m1 == nil || m1.Until.IsZero() || !m1.Until.After(time.Now()) {
		t.Fatalf("m1 未登记冷却窗口: %+v", m1)
	}
	if m1.Kind != upstream.ErrSoftRate.String() {
		t.Fatalf("m1 kind = %q, want %q", m1.Kind, upstream.ErrSoftRate.String())
	}

	// 池里应出现该模型的锁（模型级，不罚账号级）。
	locks := p.ModelLockView()
	found := false
	for _, l := range locks {
		if l.Model == "m1" && l.Locked >= 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("ModelLockView 未出现 m1 的模型级锁: %+v", locks)
	}
	// 账号本身不该被整体冷却（429 只锁模型）。
	if s, ok := p.Status("u1"); ok && s.Cooling && s.CoolKind != "" && len(s.RateLimitedModels) == 0 {
		t.Fatalf("账号被整体冷却了（应为模型级）: %+v", s)
	}

	// 第二轮：m1 在冷却窗口内、m2 未到 Every → 一个都不探。
	if sum2 := sc.RunOnce(context.Background()); sum2.Scanned != 0 || sum2.Skipped < 2 {
		t.Fatalf("第二轮 summary = %+v, want scanned=0 skipped>=2", sum2)
	}
	if fp.calls != 2 {
		t.Fatalf("prober calls = %d, want 2（第二轮不该再打上游）", fp.calls)
	}
}

// 传输失败：不推进 LastAt，但记一个短退避，避免每轮重打深睡的账号。
func TestScannerProbeFailureBackoff(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	fp := &fakeProber{out: map[string]Outcome{
		"u1|m1": {Status: 0, Err: errors.New("dial tcp: i/o timeout")},
		"u1|m2": {Status: 0, Err: errors.New("dial tcp: i/o timeout")},
	}}
	sc, st := newTestScanner(t, p, fp, time.Hour)
	sum := sc.RunOnce(context.Background())
	if sum.Failed != 2 || sum.Blocked != 0 {
		t.Fatalf("summary = %+v, want failed=2 blocked=0", sum)
	}
	for _, row := range st.Rows() {
		if !row.LastAt.IsZero() {
			t.Fatalf("%s LastAt 不该推进: %+v", row.Model, row)
		}
		if row.Until.IsZero() {
			t.Fatalf("%s 应记短退避: %+v", row.Model, row)
		}
	}
}

// 状态落盘与重载：重启后"上次查询时刻"仍然有效。
func TestStatePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota-scan.json")
	st := Load(path)
	now := time.Now()
	st.Mark("u1", "m1", Pair{Status: 200}, true, now)
	if err := st.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	re := Load(path)
	if re.Due("u1", "m1", now.Add(time.Minute), time.Hour) {
		t.Fatal("重载后 1 分钟内不该重探")
	}
	if !re.Due("u1", "m1", now.Add(2*time.Hour), time.Hour) {
		t.Fatal("重载后 2 小时应重探")
	}
	rows := re.Rows()
	if len(rows) != 1 || rows[0].Status != 200 {
		t.Fatalf("rows = %+v", rows)
	}
}

// disabled 账号不参与扫描（省一次上游调用）。
func TestScannerSkipsDisabledAccounts(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "manual")
	fp := &fakeProber{out: map[string]Outcome{}}
	sc, _ := newTestScanner(t, p, fp, time.Hour)
	sum := sc.RunOnce(context.Background())
	if sum.Scanned != 0 {
		t.Fatalf("summary = %+v, want scanned=0", sum)
	}
}
