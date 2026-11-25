package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Eternal-Xstar/workbuddy2api-plus/internal/auth"
)

// 官方客户端自报身份头必须齐全（11-128 通道校验看的就是"头集合像不像官方客户端"）。
func TestChatHeadersCarryClientIdentity(t *testing.T) {
	c := New()
	c.UserAgent = "test"
	a := &auth.Auth{UID: "u-1", AccessToken: "at-1", Domain: ""}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{ConversationID: "0123456789abcdef0123456789abcdef",
		ConversationRequestID: "fedcba9876543210fedcba9876543210"})

	want := []string{
		"X-Agent-Type", "X-Agent-Intent", "X-Agent-Runtime",
		"X-Stainless-Lang", "X-Stainless-Package-Version", "X-Stainless-OS",
		"X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version",
		"X-Stainless-Retry-Count", "X-Private-Data", "X-B3-ParentSpanId",
		"b3", "traceparent", "baggage", "acp-connection-id",
	}
	for _, h := range want {
		if got := req.Header.Get(h); got == "" {
			t.Fatalf("缺少身份头 %s", h)
		}
	}
	// 红线：X-API-Key 绝不能发（会命中网关层 key-auth → 401 not_found）
	if got := req.Header.Get("X-API-Key"); got != "" {
		t.Fatalf("不该发 X-API-Key: %q", got)
	}
	// 链路头自洽：traceparent / b3 / X-B3-TraceId 指向同一个 trace。
	trace := req.Header.Get("X-B3-TraceId")
	tp := req.Header.Get("traceparent")
	if !strings.Contains(tp, "-"+trace+"-") {
		t.Fatalf("traceparent(%q) 与 X-B3-TraceId(%q) 不一致", tp, trace)
	}
	if !strings.HasPrefix(req.Header.Get("b3"), trace+"-") {
		t.Fatalf("b3(%q) 与 X-B3-TraceId(%q) 不一致", req.Header.Get("b3"), trace)
	}
	// baggage 里的会话键必须与 X-Conversation-ID 同源。
	if !strings.Contains(req.Header.Get("baggage"), "codebuddy.session_id=0123456789abcdef0123456789abcdef") {
		t.Fatalf("baggage = %q", req.Header.Get("baggage"))
	}
	// acp-connection-id 是 UUID 形态。
	acp := req.Header.Get("acp-connection-id")
	if len(acp) != 36 || strings.Count(acp, "-") != 4 {
		t.Fatalf("acp-connection-id 形态不对: %q", acp)
	}
}

// X-Private-Data 按发行版取默认：国内个人版 false，国际版/企业版 true。
func TestPrivateDataValue(t *testing.T) {
	cn := &auth.Auth{UID: "u", AccessToken: "t"}
	if got := privateDataValue(cn); got != "false" {
		t.Fatalf("国内个人版 = %q", got)
	}
	global := &auth.Auth{UID: "u", AccessToken: "t", Domain: "www.workbuddy.ai"}
	if got := privateDataValue(global); got != "true" {
		t.Fatalf("国际版 = %q", got)
	}
	ent := &auth.Auth{UID: "u", AccessToken: "t", EnterpriseID: "e-1"}
	if got := privateDataValue(ent); got != "true" {
		t.Fatalf("企业版 = %q", got)
	}
	if got := privateDataValue(nil); got != "false" {
		t.Fatalf("nil = %q", got)
	}
}

// 同一会话的两次请求：会话级的东西（acp-connection-id / baggage / trace id）稳定，
// 而 span id 每次请求都该是新的（一个请求一个 span，这正是真机行为）。
func TestIdentityIsDeterministic(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u-1", AccessToken: "at-1"}
	build := func() *http.Request {
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
		c.ChatHeaders(req, a, "", ChatMeta{ConversationID: "0123456789abcdef0123456789abcdef", ConversationRequestID: "fedcba9876543210fedcba9876543210"})
		return req
	}
	r1, r2 := build(), build()
	for _, h := range []string{"acp-connection-id", "baggage", "X-B3-TraceId"} {
		if r1.Header.Get(h) != r2.Header.Get(h) {
			t.Fatalf("%s 应稳定: %q vs %q", h, r1.Header.Get(h), r2.Header.Get(h))
		}
	}
	// span 每请求新生成（traceparent / b3 的 span 段因此不同），这是对的。
	if r1.Header.Get("traceparent") == r2.Header.Get("traceparent") {
		t.Fatal("两次请求的 traceparent 完全相同，span 没有被重新生成")
	}
}
