package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 未指定账号 → pinUIDOf 返回空，走正常轮换。
func TestPinUIDOfEmpty(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := pinUIDOf(r); got != "" {
		t.Fatalf("pinUIDOf = %q", got)
	}
	r.Header.Set(PinHeader, "   ")
	if got := pinUIDOf(r); got != "" {
		t.Fatalf("空白应视为未指定，实得 %q", got)
	}
	r.Header.Set(PinHeader, " uid-1 ")
	if got := pinUIDOf(r); got != "uid-1" {
		t.Fatalf("应去空白，实得 %q", got)
	}
}

// 指名一个不存在的账号 → 400 + account_pinned_unusable，且不退化成轮换。
func TestPinnedAccountNotFound(t *testing.T) {
	var hits int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		hits++
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set(PinHeader, "no-such-uid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "account_pinned_unusable") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("指定账号不可用时不该打上游，实际打了 %d 次", hits)
	}
}

// 指名存在的账号：只打这一个号（上游 1 次），不轮换。
func TestPinnedAccountIsUsed(t *testing.T) {
	var hits int
	var lastAuthz string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		hits++
		lastAuthz = authz
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "other", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "pin-uid", AccessToken: "at1", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set(PinHeader, "pin-uid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if hits != 1 {
		t.Fatalf("上游被打 %d 次，指定账号应只打 1 次", hits)
	}
	if lastAuthz != "Bearer at1" {
		t.Fatalf("打错号了：%s", lastAuthz)
	}
}
