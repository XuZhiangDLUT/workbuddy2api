package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestChatStreamToolMarkupLeakLogged502 上游 200 但把工具调用标记当文本吐出、且全程没有
// tool_calls 时：重发预算用尽后写 error 帧 + [DONE] 兜底（wire 200），但这是上游缺陷不是成功
// ——日志状态必须收敛为 502 观测（与空流 502 同口径），否则客户端「既没正文也没工具调用」
// 的静默空转在流水里显示成 200 假成功。
func TestChatStreamToolMarkupLeakLogged502(t *testing.T) {
	withChatLog(t)
	body := leakStreamUpstreamBody()
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, body, true // 每次都泄漏：首发 + 一次重发都不干净
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	})
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2 (首发泄漏 + 重发一次后预算用尽)", calls)
	}
	if rec.Code != 200 {
		t.Fatalf("wire code=%d want 200 (headers already sent before leak detected)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "tool_markup_leak") {
		t.Errorf("client should receive tool-markup-leak error frame: %s", rec.Body)
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("log row status must be 502 (markup leak is upstream failure), got:\n%s", out)
	}
}

// leakStreamUpstreamBody 上游泄漏样本：思维链 + 无 tool_calls 的 DSML 块。
func leakStreamUpstreamBody() string {
	return "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"Let me batch these.\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"<parameter name=\\\"todos\\\">[]</" + "｜｜DSML｜｜" + " parameter>\"}}]}\n\n" +
		"data: [DONE]\n\n"
}

// TestChatStreamToolMarkupLeakRetriesSameAccount 首发泄漏且**什么都没产出**时，handler 就地
// 重发一次（同号——泄漏是模型行为、与账号无关；单账号池也必须能重发）：客户端最终拿到完整答案、
// 恰好一个 [DONE]、泄漏标记不透出、流水记 200 而不是 502。
func TestChatStreamToolMarkupLeakRetriesSameAccount(t *testing.T) {
	withChatLog(t)
	ok := "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"重发成功\"}}]}\n\n" +
		"data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
		"data: [DONE]\n\n"
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if calls == 1 {
			return 200, leakStreamUpstreamBody(), true
		}
		return 200, ok, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	})
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2 (首发泄漏 + 一次重发)", calls)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "重发成功") {
		t.Errorf("重发后的正文应透出: %q", body)
	}
	if strings.Contains(body, "tool_markup_leak") {
		t.Errorf("重发成功时不应写 error 帧: %q", body)
	}
	if strings.Contains(body, "｜｜DSML｜｜") || strings.Contains(body, "todos") {
		t.Errorf("泄漏标记/载荷不得透出: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	if !strings.Contains(out, "| 200 |") {
		t.Errorf("重发成功应记 200，got:\n%s", out)
	}
}

// TestChatStreamToolMarkupLeakWithContentNotRetried 泄漏前已经吐过正文：不得重发（重发会把两段
// 正文拼在一起），按失败收尾并记 502——正文本身已透出，客户端仍能看到。
func TestChatStreamToolMarkupLeakWithContentNotRetried(t *testing.T) {
	withChatLog(t)
	body := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"半截正文\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"<parameter name=\\\"todos\\\">[]</" + "｜｜DSML｜｜" + " parameter>\"}}]}\n\n" +
		"data: [DONE]\n\n"
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, body, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	})
	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1（已吐正文时不得重发）", calls)
	}
	if got := rec.Body.String(); !strings.Contains(got, "半截正文") {
		t.Errorf("已透出的正文应保留: %q", got)
	}
	if !strings.Contains(rec.Body.String(), "tool_markup_leak") {
		t.Errorf("应按失败收尾（error 帧）: %q", rec.Body)
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("应记 502，got:\n%s", out)
	}
}

// TestChatSyncToolMarkupLeakRetries 非流式同样重发一次：客户端还没收到任何输出，重发安全。
func TestChatSyncToolMarkupLeakRetries(t *testing.T) {
	withChatLog(t)
	ok := "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"重发成功\"}}]}\n\n" +
		"data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if calls == 1 {
			return 200, leakStreamUpstreamBody(), true
		}
		return 200, ok, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	})
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2", calls)
	}
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "重发成功") {
		t.Errorf("重发后的正文应返回: %q", rec.Body)
	}
	if !strings.Contains(out, "| 200 |") {
		t.Errorf("重发成功应记 200，got:\n%s", out)
	}
}
