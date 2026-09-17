// tool_markup.go 上游把工具调用标记当正文/思维链吐出（grammar leakage）的识别与拒发。
//
// 根因（本机实证：dsh 走 /v1/chat/completions 复现）：deepseek-v4.1-flash 在 agent 多轮里
// 偶发不走结构化 tool_calls，而把 DeepSeek 原生 DSML 调用语法写进 reasoning_content
// （少数情况下是 content），形态如：
//
//	…Let me batch these.</thinking>
//	<parameter name="todos">[{"content":"…","status":"pending"}]</parameter>
//
// 上游解析链路没有把它翻回 tool_calls，整段标记于是以纯文本透出。OpenAI 兼容客户端只认
// content 与 tool_calls：既拿不到正文也拿不到工具调用，agent 循环据此收尾——表现为
// 「思考到一半卡住、没有任何动作」，且不报错、不重试（dsh-agent-loop 对无 tool_calls 的
// 助手步直接返回 completed）。
//
// 同类形态不是本网关独有（公开记录）：
//   - vllm-project/vllm#48931：长上下文下模型漏掉 <｜DSML｜tool_calls> 起始包装，解析器只认
//     该起始串，整块 DSML 被当作 content 返回且不产生 tool_calls，客户端挂住；
//   - sgl-project/sglang#39632：该退化分支在生产实测对约 4–6% 的带工具 DeepSeek V4 请求触发；
//   - pi-tool-repair 的 tool-call-grammar-leakage-survey：把「工具语法泄漏」列为独立故障
//     类别，并指出只清理标记而不报错会留下 finish_reason=stop 的静默空转。
//
// 本文件只做「识别 + 报错」，不做「修复（recover）」：把泄漏标记翻回 tool_calls 需要解析
// 逆向格式（变体多、易误伤），留待后续；当前目标是让故障响亮——泄漏标记不再当正文透出，
// 且以错误收尾，使客户端重试/报错而不是静默空转。
package upstream

import (
	"errors"
	"strings"
)

// errToolMarkupLeak 上游 200 但把工具调用标记当文本吐出，且没有产出结构化 tool_calls。
// 与 errEmptyStream 同族：流式路径 HTTP 头已发出（只能 200），用 error 帧 + 哨兵错误让
// handler 把观测收敛为 502（上游缺陷，不是成功）。
var errToolMarkupLeak = errors.New("upstream leaked tool-call markup as text without tool_calls")

// IsToolMarkupLeakError 报告错误是否为「工具调用标记泄漏」——供 handler 流式路径收敛失败
// 观测（与非流式 Aggregate 共用同一哨兵，errors.Is 判定）。
func IsToolMarkupLeakError(err error) bool { return errors.Is(err, errToolMarkupLeak) }

// toolMarkupMarkers 触发的标记：DeepSeek DSML 的竖线族。全角竖线（U+FF5C）双写最常见，
// 单写与无前导竖线是同一语法的变体，半角 | 是中间层转码形态。
//
// 竖线族**只出现在闭合标签上**，因此它认不出「上游在写出闭合标签前就把流截断」的形态
// （2026-09-17 15:59:50 / 16:02:41 两次实测：尾部只剩 "</"，一个竖线都没有）——那一路由
// hasDanglingToolMarkupTail 兜（只认尾部，不认正文中间，避免误伤技术讨论）。
var toolMarkupMarkers = []string{
	"｜｜DSML｜｜",
	"｜DSML｜",
	"<DSML｜",
	"|DSML|",
}

// toolMarkupOpenTags 调用块的开标签前缀：① 判定「尾部悬挂」（模型开了块、流就断了）；
// ② 泄漏块切点回看（块载荷要从开标签处切掉）。
var toolMarkupOpenTags = []string{"<parameter", "<invoke", "<tool_calls", "<function_calls"}

// toolMarkupLeakErrorFrame 泄漏且无 tool_calls 时的出站 error 帧（流式收尾与重试预算用尽共用，
// 保证两处文案一致）。
const toolMarkupLeakErrorFrame = `{"error":{"message":"upstream leaked tool-call markup as text without tool_calls","type":"upstream_error","code":"tool_markup_leak"}}`

// hasToolMarkupLeak 报告文本里是否出现「非代码引用形态」的工具调用标记：
// 竖线族标记（任意位置）或尾部悬挂的标签片段（见 hasDanglingToolMarkupTail）。
// 代码引用（行内 `…` 与成对围栏代码块）豁免：正常讨论该格式时标记通常写在反引号里，
// 直接命中会把「讨论语法的回答」误判成泄漏。
func hasToolMarkupLeak(text string) bool {
	if text == "" {
		return false
	}
	scan := stripQuotedSyntax(text)
	for _, m := range toolMarkupMarkers {
		if strings.Contains(scan, m) {
			return true
		}
	}
	return hasDanglingToolMarkupTail(scan)
}

// hasDanglingToolMarkupTail 报告文本**尾部**是否悬挂着未完成的工具调用标记：
//   - 以 "</" 收尾：模型刚开始写闭合标签，上游就把流截断了（本机 15:59:50 / 16:02:41 实测）。
//     这种形态里一个竖线族标记都没有，只认 toolMarkupMarkers 会整段漏检。
//   - 以完整的调用开标签收尾：模型开了块、还没写参数流就断了（本机 13:08 实测）。
//
// 只认**尾部**：正文中间出现这些串不算（正常回答不会以 "</" 或裸开标签结束）；讨论格式时
// 通常写在代码引用里，已被 stripQuotedSyntax 排除。
func hasDanglingToolMarkupTail(scan string) bool {
	t := strings.TrimRight(scan, " \t\r\n")
	if t == "" {
		return false
	}
	if strings.HasSuffix(t, "</") {
		return true
	}
	return isToolMarkupOpenTag(tailTag(t))
}

// tailTag 取尾部那一个标签（最后一个 '<' 起到末尾）。无 '<'，或不以 '>' 收尾，或标签内部还有
// '>'（说明不是单个标签）时返回空串。
func tailTag(t string) string {
	i := strings.LastIndexByte(t, '<')
	if i < 0 {
		return ""
	}
	tag := t[i:]
	if !strings.HasSuffix(tag, ">") || strings.ContainsRune(tag[:len(tag)-1], '>') {
		return ""
	}
	return tag
}

// isToolMarkupOpenTag 报告 tag 是否为调用块的开标签。
func isToolMarkupOpenTag(tag string) bool {
	if tag == "" {
		return false
	}
	for _, open := range toolMarkupOpenTags {
		if strings.HasPrefix(tag, open) {
			return true
		}
	}
	return false
}

// firstToolMarkupIndex 返回文本中最早出现的竖线族标记下标（无则 -1）。
func firstToolMarkupIndex(text string) int {
	at := -1
	for _, m := range toolMarkupMarkers {
		if i := strings.Index(text, m); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	return at
}

// toolMarkupBlockStart 返回泄漏块的起点下标（无泄漏则 -1）：先定位最早的竖线族标记，再往前
// 回看该块的开标签，取最近的作为切点；没有竖线标记（截断形态）时交给 danglingToolMarkupStart。
//
// 为什么需要回看：竖线族只出现在闭合标签上（模型漏掉起始包装时更甚），只按标记切会把
// <parameter name="…">{…} 这段载荷留在正文里——「不把标记当文本透出」就没做到。
// 回看**仅在已判定泄漏后**用于定切点，判定本身仍走 hasToolMarkupLeak，因此不会因为正常
// 文本里出现 <parameter 而误判。
func toolMarkupBlockStart(text string) int {
	marker := firstToolMarkupIndex(text)
	if marker < 0 {
		return danglingToolMarkupStart(text)
	}
	start := marker
	for _, open := range toolMarkupOpenTags {
		if i := strings.LastIndex(text[:marker], open); i >= 0 && i < start {
			start = i
		}
	}
	return start
}

// danglingToolMarkupStart 尾部悬挂形态的切点：最后一个开标签；若只留下 "</"（没有开标签），
// 取最后那个 '<'。调用方保证文本已判定为泄漏，故必能定位到切点。
func danglingToolMarkupStart(text string) int {
	t := strings.TrimRight(text, " \t\r\n")
	if !hasDanglingToolMarkupTail(t) {
		return -1
	}
	start := -1
	for _, open := range toolMarkupOpenTags {
		if i := strings.LastIndex(t, open); i > start {
			start = i
		}
	}
	if start >= 0 {
		return start
	}
	return strings.LastIndexByte(t, '<')
}

// stripQuotedSyntax 去掉代码引用形态的片段：成对围栏代码块（``` 行之间）与行内代码
// （同一行内成对反引号之间）。
//
// 只删**成对**的引用：落单的反引号与落单的围栏原样保留，否则模型思维链里一段未闭合的 ```
// 会把后续真正的泄漏标记一起藏掉，守卫静默失效（这类思维链里未闭合代码块很常见）。
func stripQuotedSyntax(s string) string {
	lines := strings.Split(s, "\n")
	// 围栏行下标；奇数个时忽略最后一个——它没有配对的闭合围栏，其后的内容必须继续参与扫描。
	var fences []int
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			fences = append(fences, i)
		}
	}
	if len(fences)%2 == 1 {
		fences = fences[:len(fences)-1]
	}
	drop := make([]bool, len(lines))
	for k := 0; k+1 < len(fences); k += 2 {
		for i := fences[k]; i <= fences[k+1]; i++ {
			drop[i] = true
		}
	}
	var b strings.Builder
	for i, ln := range lines {
		if drop[i] {
			continue
		}
		b.WriteString(stripInlineCode(ln))
		b.WriteString("\n")
	}
	return b.String()
}

// stripInlineCode 删除同一行内成对反引号之间的内容；落单反引号原样保留（不吞后文）。
func stripInlineCode(line string) string {
	for {
		i := strings.IndexByte(line, '`')
		if i < 0 {
			return line
		}
		j := strings.IndexByte(line[i+1:], '`')
		if j < 0 {
			return line
		}
		line = line[:i] + line[i+1+j+1:]
	}
}

// guardToolMarkupFields 就地过一遍一条 assistant 字段集（流式 delta 或整条 message）的文本通道：
//   - 已进入泄漏态（*leaking）→ 整段文本通道删除（标记之后的文本一律不透出）；
//   - 未进入泄漏态但命中标记 → 截断到泄漏块起点（保留块前的正常文本），置 *leaking；
//   - 记录该字段集是否带结构化 tool_calls（*anyToolCall）——决定流末尾是报错还是照常收尾。
func guardToolMarkupFields(fields map[string]any, leaking, anyToolCall *bool) {
	if fieldsHaveToolCalls(fields) {
		*anyToolCall = true
	}
	for _, key := range []string{"reasoning_content", "content"} {
		s, ok := fields[key].(string)
		if !ok || s == "" {
			continue
		}
		if *leaking {
			delete(fields, key)
			continue
		}
		if !hasToolMarkupLeak(s) {
			continue
		}
		*leaking = true
		if i := toolMarkupBlockStart(s); i > 0 {
			fields[key] = s[:i]
		} else {
			delete(fields, key)
		}
	}
}

// guardFrameToolMarkup 对一帧出站 SSE 的所有 choices 过泄漏守卫（delta 与 message 两路同构，
// 规约只有一份，与 Aggregate 共用 guardToolMarkupFields）。
func guardFrameToolMarkup(obj map[string]any, leaking, anyToolCall *bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		if d, ok := c["delta"].(map[string]any); ok {
			guardToolMarkupFields(d, leaking, anyToolCall)
		}
		if m, ok := c["message"].(map[string]any); ok {
			guardToolMarkupFields(m, leaking, anyToolCall)
		}
	}
}

// fieldsHaveToolCalls 报告字段集是否带非空 tool_calls。两种容器类型都要认：流式帧来自 JSON
// 解码（[]any），非流式聚合的 message 是自己构造的（[]map[string]any）。
func fieldsHaveToolCalls(fields map[string]any) bool {
	switch v := fields["tool_calls"].(type) {
	case []any:
		return len(v) > 0
	case []map[string]any:
		return len(v) > 0
	}
	return false
}

// frameHasContent 报告一帧（delta 或 message 形态）是否带非空正文。判定发生在泄漏守卫之后，
// 因此被截断掉的泄漏载荷不计入——它决定 StreamOutcome.ContentText，进而决定能否换号重发。
func frameHasContent(obj map[string]any) bool {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			f, ok := c[key].(map[string]any)
			if !ok {
				continue
			}
			if s, _ := f["content"].(string); s != "" {
				return true
			}
		}
	}
	return false
}

// clearFinishReason 清空一帧里所有 choice 的 finish_reason（配合 normalizeFrame 的
// 「空 finish_reason → null」语义）：换号重发场景下，客户端不该在半途尝试上收尾。
func clearFinishReason(obj map[string]any) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		if c, ok := ci.(map[string]any); ok {
			delete(c, "finish_reason")
		}
	}
}
