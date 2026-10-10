package relay

// 韧性层（resilience）：把「HTTP 200 但内容不可用」的响应识别出来并重发换号。
//
// 背景：能导致客户端任务中断的不只有 HTTP 错误码。上游在下面这些情况下依然
// 返回 200，但客户端拿到后无法继续工作（写代码场景表现为空回复或被过滤）：
//   - 响应里根本没有任何内容（choices 为空 / content 为空串 / Gemini parts 为空）
//   - 200 + body 内嵌 error（部分网关这样返回）
//   - 响应体不是合法 JSON（被截断 / 连接池复用串包）
//   - finish_reason 为审查类（content_filter / SAFETY / RECITATION ...），
//     这类在【另一个账号】上往往完全正常
//
// 这些都不该透传给客户端——客户端一收到就停。网关应当把它们当成一次「软失败」，
// 换一个号重新排队再发一轮；只有确实无号可用时才把最后的结果交出去。
//
// 边界（防误伤，这几条很重要）：
//   - 只认得 OpenAI / Gemini / Anthropic 三种文本对话结构；
//     **认不出来的结构一律视为可用**——视频轮询返回 {status:"queued"} 没有
//     choices，若误判为「空内容」会导致无限重发。
//   - max_tokens 截断（finish_reason=length）不算软失败：它由 continuation
//     层的「自动续写」接管（重试同一请求还会同样截断，必须改成续写）。
//   - 已产出 tool_calls 的响应算可用（工具调用没有文本内容是正确的）。

import (
	"bytes"
	"encoding/json"
	"strings"
)

// respKind 是一次成功响应的「内容可用性」分类。
type respKind int

const (
	respOK respKind = iota // 内容可用（含「认不出来」的异构结构）
	respTruncated          // max_tokens 截断 → 交给续写层，不重发
	respBlocked            // 安全/审查策略拒绝 → 换号重发可能成功
	respEmpty              // 没有任何产出
	respErrorish           // body 内嵌 error
	respMalformed          // 非合法 JSON
)

func (k respKind) String() string {
	switch k {
	case respTruncated:
		return "truncated"
	case respBlocked:
		return "blocked"
	case respEmpty:
		return "empty"
	case respErrorish:
		return "upstream_error_body"
	case respMalformed:
		return "malformed_json"
	default:
		return "ok"
	}
}

// blocksContent 是「上游主动中止生成」的 finish/stop reason。
// 它不是截断（不缺 token），换个号常常就能正常输出，所以值得重发。
var blocksContent = map[string]bool{
	"content_filter":     true, // OpenAI
	"SAFETY":             true, // Gemini
	"RECITATION":         true,
	"BLOCKLIST":          true,
	"PROHIBITED_CONTENT": true,
	"OTHER":              true,
	"SPII":               true,
	"MALFORMED_FUNCTION_CALL": true,
	"refusal":            true, // Anthropic
	"sensitive":          true,
}

// needRefire 判定该软失败是否值得「换号重发一轮」。
func (k respKind) needRefire() bool {
	return k == respBlocked || k == respEmpty || k == respErrorish || k == respMalformed
}

// classifyResponse 判定一次「HTTP < 400」响应的内容可用性。
// 只处理三种可识别的文本对话结构；认不出来的一律返回 respOK（绝不误伤）。
func classifyResponse(body []byte) respKind {
	if len(bytes.TrimSpace(body)) == 0 {
		return respEmpty
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return respMalformed
	}
	// 200 + 内嵌 error：部分上游/网关把错误放在 200 的 body 里。
	if ev, ok := m["error"]; ok && ev != nil {
		if s, isStr := ev.(string); !isStr || strings.TrimSpace(s) != "" {
			return respErrorish
		}
	}
	switch {
	case m["choices"] != nil:
		return classifyOpenAIResp(m)
	case m["candidates"] != nil:
		return classifyGeminiResp(m)
	case m["stop_reason"] != nil:
		return classifyAnthropicResp(m)
	}
	return respOK // 认不出来：保持原样透传
}

func classifyOpenAIResp(m map[string]any) respKind {
	choices, ok := m["choices"].([]any)
	if !ok {
		return respOK
	}
	if len(choices) == 0 {
		return respEmpty
	}
	c, ok := choices[0].(map[string]any)
	if !ok {
		return respOK
	}
	if fr, _ := c["finish_reason"].(string); fr != "" {
		if fr == "length" {
			return respTruncated
		}
		if blocksContent[fr] {
			return respBlocked
		}
	}
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		// 流式 chunk 形态（delta）不做内容判定，避免误判中间帧
		return respOK
	}
	if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
		return respOK // 工具调用没有文本内容是正确的
	}
	if strings.TrimSpace(anyContentToString(msg["content"])) == "" {
		return respEmpty
	}
	return respOK
}

func classifyGeminiResp(m map[string]any) respKind {
	cands, ok := m["candidates"].([]any)
	if !ok {
		return respOK
	}
	if len(cands) == 0 {
		return respEmpty
	}
	c, ok := cands[0].(map[string]any)
	if !ok {
		return respOK
	}
	if fr, _ := c["finishReason"].(string); fr != "" {
		if fr == "MAX_TOKENS" {
			return respTruncated
		}
		if blocksContent[fr] {
			return respBlocked
		}
	}
	if strings.TrimSpace(geminiText(c["content"])) == "" {
		return respEmpty
	}
	return respOK
}

func classifyAnthropicResp(m map[string]any) respKind {
	if sr, _ := m["stop_reason"].(string); sr != "" {
		if sr == "max_tokens" {
			return respTruncated
		}
		if blocksContent[sr] {
			return respBlocked
		}
	}
	if strings.TrimSpace(anyContentToString(m["content"])) == "" {
		return respEmpty
	}
	return respOK
}

// geminiText 提取 Gemini content 里的文本。
// 结构是 {"role":"model","parts":[{"text":"..."}]}，与 OpenAI 的
// content 字段不同形，必须单独处理——否则 Gemini 路径会一直是「空内容」。
func geminiText(v any) string {
	cm, ok := v.(map[string]any)
	if !ok {
		return anyContentToString(v)
	}
	parts, ok := cm["parts"].([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			if tx, _ := pm["text"].(string); tx != "" {
				sb.WriteString(tx)
			}
		}
	}
	return sb.String()
}
