package upstream

import (
	"fmt"
	"testing"
)

// 连续 3 次「解析失败」→ 判定疑似改版；*Error 与传输错误都不计入。
func TestContractDriftDetection(t *testing.T) {
	ResetTaskWatch()
	defer ResetTaskWatch()

	parseErr := fmt.Errorf("parse failed: invalid character 'x' (body: <html>)")
	for i := 0; i < watchThreshold-1; i++ {
		noteTaskResult("/v2/billing/meter/daily-checkin", parseErr)
	}
	hs := TaskHealthSnapshot()
	if len(hs) != 1 || hs[0].Kind != "checkin" || hs[0].Suspected {
		t.Fatalf("阈值未到就判了：%+v", hs)
	}
	if hs[0].Consecutive != watchThreshold-1 {
		t.Fatalf("consecutive = %d", hs[0].Consecutive)
	}

	noteTaskResult("/v2/billing/meter/daily-checkin", parseErr)
	hs = TaskHealthSnapshot()
	if len(hs) != 1 || !hs[0].Suspected || hs[0].Since.IsZero() {
		t.Fatalf("第 %d 次应判定疑似改版：%+v", watchThreshold, hs)
	}

	// 业务错误（*Error）不算契约漂移：账号额度/限流不是"上游改版"。
	noteTaskResult("/billing/meter/claim-compensation", &Error{Kind: ErrClient, Status: 400, Msg: "code=10001"})
	// 传输层错误也不算。
	noteTaskResult("/billing/meter/claim-compensation", fmt.Errorf("dial tcp 1.2.3.4:443: i/o timeout"))

	var comp *TaskHealth
	for i := range TaskHealthSnapshot() {
		if TaskHealthSnapshot()[i].Kind == "compensation" {
			comp = &TaskHealthSnapshot()[i]
		}
	}
	// TaskHealthSnapshot 返回副本：重新取一次做断言。
	for _, h := range TaskHealthSnapshot() {
		if h.Kind == "compensation" {
			if h.Consecutive != 0 || h.Suspected {
				t.Fatalf("业务/传输错误被当成契约漂移：%+v", h)
			}
			comp = &h
		}
	}
	if comp == nil {
		t.Fatal("compensation 未记账")
	}
	if comp.LastError == "" {
		t.Fatal("失败应记录 LastError")
	}

	// 一次成功即清零。
	noteTaskResult("/v2/billing/meter/daily-checkin", nil)
	for _, h := range TaskHealthSnapshot() {
		if h.Kind == "checkin" {
			if h.Consecutive != 0 || h.Suspected || h.LastOKAt.IsZero() {
				t.Fatalf("成功后未复位：%+v", h)
			}
		}
	}
}

// 未归类的路径不记账。
func TestWatchIgnoresUnknownPaths(t *testing.T) {
	ResetTaskWatch()
	defer ResetTaskWatch()
	noteTaskResult("/v1/some/unknown/path", fmt.Errorf("parse failed"))
	if hs := TaskHealthSnapshot(); len(hs) != 0 {
		t.Fatalf("未归类路径不该记账：%+v", hs)
	}
}

// 路径归类：URL 带 query 也能识别。
func TestWatchKindOf(t *testing.T) {
	cases := map[string]string{
		"/v2/billing/meter/daily-checkin":           "checkin",
		"/billing/meter/compensation-status":        "compensation",
		"/v2/report":                                "report",
		"/v2/activity/growth/tasks/accept":          "growth",
		"/v2/activity/travel/status":                "travel",
		"/v2/billing/meter/get-user-resource?x=1":   "balance",
		"/v2/plugin/auth/token/refresh":             "auth",
		"/v2/enterprises/personal/models":           "catalog",
		"/nope":                                     "",
	}
	for path, want := range cases {
		if got := watchKindOf(path); got != want {
			t.Fatalf("watchKindOf(%q) = %q, want %q", path, got, want)
		}
	}
}
