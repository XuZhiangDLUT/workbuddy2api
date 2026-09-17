package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 竖线族标记的四种写法（全角双/单竖线、无前导竖线、中间层转码的半角形态）。
const (
	dsmlDouble = "｜｜DSML｜｜"
	dsmlSingle = "｜DSML｜"
	dsmlBare   = "<DSML｜"
	dsmlAscii  = "|DSML|"
)

// leakBlock 拼出一段真实形态的泄漏块：parameter 载荷 + 竖线族闭合标签（模型漏掉起始包装时，
// 竖线族只出现在闭合标签上）。
func leakBlock(prefix string) string {
	return prefix + "<parameter name=\"todos\">[{\"content\":\"x\"}]</" + dsmlDouble + " parameter>\n</" + dsmlDouble + " calls>"
}

// leakFrameReasoning 把一段文本包成 OpenAI 流式帧（reasoning_content 通道）。
func leakFrameReasoning(text string) string {
	b, _ := json.Marshal(text)
	return `{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":` + string(b) + `}}]}`
}

// toolCallFrame 一帧结构化 tool_calls（泄漏与正常调用同时出现的对照）。
const toolCallFrame = `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"todo_write","arguments":"{}"}}]}}]}`

// leakStreamBody 拼 SSE body（帧间 "\n\n"，尾部 [DONE]）。
func leakStreamBody(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestHasToolMarkupLeak(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"双竖线泄漏块", leakBlock("Let me batch these.\n"), true},
		{"单竖线", "see " + dsmlSingle + " parameter>", true},
		{"无前导竖线", "see " + dsmlBare + " calls>", true},
		{"半角转码", "see " + dsmlAscii + " tool_calls>", true},
		{"正常中文文本", "我先看一下目录结构，然后批量执行。", false},
		{"行内代码引用（讨论格式）", "输出形态是 `</" + dsmlDouble + " calls>` 这样的标记", false},
		{"围栏代码块引用", "```\n</" + dsmlDouble + " calls>\n```\n以上是示例。", false},
		{"未闭合围栏不吞后文", "```\ncode\n</" + dsmlDouble + " calls>", true},
		{"落单反引号不吞后文", "thinking `unclosed </" + dsmlDouble + " calls>", true},
		// 截断形态（2026-09-17 15:59:50 / 16:02:41 两次实测）：上游在写出闭合标签前把流截断，
		// 竖线族一个都没出现，只剩尾部悬挂的标签片段——只认竖线族会整段漏检。
		{"尾部悬挂 </", "And read var_upper files.</", true},
		{"尾部悬挂裸开标签", "<parameter name=\"git_bash\">", true},
		{"尾部悬挂 tool_calls 开标签", "…\n<tool_calls>", true},
		{"正常句号结尾不算", "And read var_upper files.", false},
		{"尾部 HTML 闭合标签不算", "见 </div>", false},
		{"正文中间的裸开标签不算（非尾部）", "这里提到 <parameter name=\"x\"> 但后面还有正文。", false},
		{"行内代码里的尾部悬挂不算", "格式就是 `<parameter name=\"x\">`", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasToolMarkupLeak(c.text); got != c.want {
				t.Errorf("hasToolMarkupLeak(%q)=%v want %v", c.text, got, c.want)
			}
		})
	}
}

// TestToolMarkupBlockStartCutsPayload 切点必须落在块首：块前的正常思维链保留，块内载荷不留。
func TestToolMarkupBlockStartCutsPayload(t *testing.T) {
	text := leakBlock("前文正常思维链。\n")
	i := toolMarkupBlockStart(text)
	if i < 0 {
		t.Fatal("block start not found")
	}
	cut := text[:i]
	if !strings.Contains(cut, "前文正常思维链。") || strings.Contains(cut, "todos") {
		t.Errorf("cut=%q want 保留块前文本且不含载荷", cut)
	}
}

// TestToolMarkupBlockStartHandlesDanglingTail 截断形态的切点：没有竖线标记，切点取最后一个
// 开标签；只剩 "</" 时取最后那个 '<'——保证残留的标签片段不留在正文里。
func TestToolMarkupBlockStartHandlesDanglingTail(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // 期望保留的前缀
	}{
		{"尾部悬挂裸开标签", "正常思维链。<parameter name=\"git_bash\">", "正常思维链。"},
		{"尾部只剩 </", "正常思维链。</", "正常思维链。"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := toolMarkupBlockStart(c.text)
			if i < 0 {
				t.Fatalf("block start not found in %q", c.text)
			}
			if got := c.text[:i]; got != c.want {
				t.Errorf("cut=%q want %q", got, c.want)
			}
		})
	}
}

// TestStreamToolMarkupLeakErrorsAndSuppresses 无 tool_calls 的泄漏：返回哨兵错误 + error 帧，
// 泄漏标记与载荷不透出，块前的正常思维链保留，恰好一个 [DONE]。
func TestStreamToolMarkupLeakErrorsAndSuppresses(t *testing.T) {
	raw := leakStreamBody(
		leakFrameReasoning("Let me batch these."),
		leakFrameReasoning(leakBlock("")),
	)
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw))
	if !IsToolMarkupLeakError(err) {
		t.Fatalf("err=%v want tool-markup-leak", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, dsmlDouble) || strings.Contains(body, "todos") {
		t.Errorf("泄漏标记/载荷不得透出: %q", body)
	}
	if !strings.Contains(body, "Let me batch these.") {
		t.Errorf("块前的正常思维链应保留: %q", body)
	}
	if !strings.Contains(body, `"code":"tool_markup_leak"`) {
		t.Errorf("error 帧缺失: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
}

// TestStreamToolMarkupLeakWithToolCallNoError 泄漏与结构化 tool_calls 同时出现：不报错，
// 泄漏文本仍被裁掉，工具调用照常透出（避免把「模型已经正常调工具」的流误判为失败）。
func TestStreamToolMarkupLeakWithToolCallNoError(t *testing.T) {
	raw := leakStreamBody(
		leakFrameReasoning(leakBlock("think. ")),
		toolCallFrame,
	)
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatalf("err=%v want nil", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, dsmlDouble) || strings.Contains(body, "todos") {
		t.Errorf("泄漏标记/载荷不得透出: %q", body)
	}
	if !strings.Contains(body, "todo_write") {
		t.Errorf("结构化 tool_calls 必须透出: %q", body)
	}
	if strings.Contains(body, "tool_markup_leak") {
		t.Errorf("有 tool_calls 时不应写 error 帧: %q", body)
	}
}

func TestAggregateToolMarkupLeakCases(t *testing.T) {
	t.Run("无 tool_calls 报错", func(t *testing.T) {
		_, err := Aggregate(strings.NewReader(leakStreamBody(leakFrameReasoning(leakBlock("think. ")))))
		if !IsToolMarkupLeakError(err) {
			t.Fatalf("err=%v want tool-markup-leak", err)
		}
	})
	t.Run("有 tool_calls 保留聚合结果并裁掉泄漏块", func(t *testing.T) {
		resp, err := Aggregate(strings.NewReader(leakStreamBody(leakFrameReasoning(leakBlock("think. ")), toolCallFrame)))
		if err != nil {
			t.Fatal(err)
		}
		msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if _, ok := msg["tool_calls"]; !ok {
			t.Error("tool_calls 应保留")
		}
		rc, _ := msg["reasoning_content"].(string)
		if strings.Contains(rc, dsmlDouble) || strings.Contains(rc, "todos") {
			t.Errorf("泄漏块应被裁掉: %q", rc)
		}
		if !strings.Contains(rc, "think.") {
			t.Errorf("块前文本应保留: %q", rc)
		}
	})
}

// TestStreamAttemptLeakWithoutOutput 泄漏且什么都没产出：StreamAttempt 不写 [DONE]、不写 error 帧、
// 抑制半途尝试的 finish_reason，并把「可以安全重发」的观测交回调用方。
func TestStreamAttemptLeakWithoutOutput(t *testing.T) {
	raw := leakStreamBody(
		leakFrameReasoning("Let me batch these."),
		leakFrameReasoning(leakBlock("")),
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	rec := httptest.NewRecorder()
	out, err := StreamAttempt(rec, strings.NewReader(raw), nil)
	if !IsToolMarkupLeakError(err) {
		t.Fatalf("err=%v want tool-markup-leak", err)
	}
	if out.ContentText || out.ToolCalls {
		t.Errorf("outcome=%+v want 无正文无工具调用（可安全重发）", out)
	}
	if !out.Leaked {
		t.Errorf("outcome=%+v want Leaked=true", out)
	}
	body := rec.Body.String()
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("重发场景不应写 [DONE]（客户端要继续读下一条尝试）: %q", body)
	}
	if strings.Contains(body, "tool_markup_leak") {
		t.Errorf("重发场景不应写 error 帧: %q", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("半途尝试的 finish_reason 应被抑制: %q", body)
	}
	if strings.Contains(body, dsmlDouble) || strings.Contains(body, "todos") {
		t.Errorf("泄漏标记/载荷不得透出: %q", body)
	}
	if !strings.Contains(body, "Let me batch these.") {
		t.Errorf("块前思维链应保留: %q", body)
	}
}

// TestStreamAttemptLeakWithContentNotRetryable 泄漏前已吐正文：观测必须标出 ContentText，
// handler 据此不再重发（避免两段正文拼在一起）。
func TestStreamAttemptLeakWithContentNotRetryable(t *testing.T) {
	const contentFrame = `{"id":"c1","choices":[{"index":0,"delta":{"content":"半截正文"}}]}`
	raw := leakStreamBody(contentFrame, leakFrameReasoning(leakBlock("")))
	rec := httptest.NewRecorder()
	out, err := StreamAttempt(rec, strings.NewReader(raw), nil)
	if !IsToolMarkupLeakError(err) {
		t.Fatalf("err=%v want tool-markup-leak", err)
	}
	if !out.ContentText {
		t.Errorf("outcome=%+v want ContentText=true（已吐正文，不可重发）", out)
	}
}

// TestWriteToolMarkupLeakTail 重发预算用尽时的收尾：error 帧 + 恰好一个 [DONE]。
func TestWriteToolMarkupLeakTail(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteToolMarkupLeakTail(rec)
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"tool_markup_leak"`) {
		t.Errorf("error 帧缺失: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
}
