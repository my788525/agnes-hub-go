package relay

// 截断自动续写（MAX_TOKENS continuation）。
//
// 问题：上游偶发以 finish_reason=length / MAX_TOKENS 截断输出（OpenAI 兼容格式
// 的 finish_reason="length"，Gemini 原生 finishReason="MAX_TOKENS"，Anthropic
// stop_reason="max_tokens"）。这不是可重试错误——重试同一请求还是同样截断——
// 但直接透传会让客户端任务中途停止（写代码场景截断的代码无法继续）。
//
// 方案：网关内自动续写。检测到截断后，把「已生成的部分输出」作为 assistant
// 消息追加回请求，再追加一条 user 续写指令，重新走完整的排队 / 选号 / 节拍
// 流程发起新一轮上游请求（天然满足「自动排队重试」），把续写内容与已有内容
// 拼接，直到 finish_reason 正常或轮数用尽。客户端全程只看到一次连续响应。
//
// 范围（v1）：
//   - 仅对 Continuable=true 的文本 chat 路径启用（生图/视频轮询不适用）。
//   - 非流式：OpenAI / Gemini / Anthropic 三种格式的检测与拼接。
//   - 流式：OpenAI chunk 格式的 SSE 过滤（吞掉中间的 finish chunk 与 [DONE]，
//     由网关在真正结束时统一补发）；其他 SSE 格式原样透传，零回归。
//   - 工具调用截断（tool_calls 参数被截断）不续写——提取不到部分文本时原样透传。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agneshub/internal/config"
	"agneshub/internal/hub"
)

const continuationPrompt = "Continue exactly where you left off. Do not repeat any previously emitted content, and do not restate the task."

// noteContinuation 计数一次续写轮。
func noteContinuation(h *hub.Hub) {
	if h != nil {
		h.Metrics.Continuations.Add(1)
	}
}

// ---- 检测 ----

// detectTruncation 判断成功响应体是否因达到 max_tokens 截断。
// 返回 (是否截断, 格式)；格式用于选择对应的拼接器。
func detectTruncation(body []byte) (bool, string) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return false, ""
	}
	// OpenAI chat completions
	if choices, ok := m["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if fr, _ := c["finish_reason"].(string); fr == "length" {
				return true, "openai"
			}
		}
	}
	// Gemini 原生
	if cands, ok := m["candidates"].([]any); ok && len(cands) > 0 {
		if c, ok := cands[0].(map[string]any); ok {
			if fr, _ := c["finishReason"].(string); fr == "MAX_TOKENS" {
				return true, "gemini"
			}
		}
	}
	// Anthropic messages
	if sr, _ := m["stop_reason"].(string); sr == "max_tokens" {
		return true, "anthropic"
	}
	return false, ""
}

// anyContentToString 把 content（string 或多模态 blocks 数组）摊平成纯文本。
// 含非文本块时只取文本部分（不编造）。
func anyContentToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, blk := range t {
			if bm, ok := blk.(map[string]any); ok {
				if bt, _ := bm["type"].(string); bt == "text" {
					if tx, _ := bm["text"].(string); tx != "" {
						sb.WriteString(tx)
					}
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// partialText 提取截断响应里「已生成的部分输出」，作为续写上下文。
// 提取不到（例如截断发生在 tool_calls 参数上）返回 ""，调用方应放弃续写。
func partialText(body []byte, format string) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	switch format {
	case "openai":
		if choices, ok := m["choices"].([]any); ok && len(choices) > 0 {
			if c, ok := choices[0].(map[string]any); ok {
				if msg, ok := c["message"].(map[string]any); ok {
					return anyContentToString(msg["content"])
				}
			}
		}
	case "gemini":
		if cands, ok := m["candidates"].([]any); ok && len(cands) > 0 {
			if c, ok := cands[0].(map[string]any); ok {
				return anyContentToString(c["content"])
			}
		}
	case "anthropic":
		return anyContentToString(m["content"])
	}
	return ""
}

// ---- 请求体改写 ----

// appendContinuationMessages 把部分输出 + 续写指令追加到请求 messages。
// 只处理 OpenAI 格式（messages 字段）；其余格式原样返回（不续写）。
func appendContinuationMessages(base []byte, partial string) []byte {
	var req map[string]any
	if json.Unmarshal(base, &req) != nil {
		return base
	}
	msgs, ok := req["messages"].([]any)
	if !ok {
		return base
	}
	next := make([]any, len(msgs), len(msgs)+2)
	copy(next, msgs)
	next = append(next,
		map[string]any{"role": "assistant", "content": partial},
		map[string]any{"role": "user", "content": continuationPrompt},
	)
	req["messages"] = next
	out, err := json.Marshal(req)
	if err != nil {
		return base
	}
	return out
}

// ---- 响应拼接 ----

func numField(m map[string]any, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

// mergeChatResponse 把续写轮响应拼进前段响应（OpenAI / Gemini / Anthropic）。
// 内容合并、finish_reason 沿用续写轮（它是更靠后的真实状态）、usage 累加。
func mergeChatResponse(base []byte, cont []byte, format string) ([]byte, bool) {
	var b, c map[string]any
	if json.Unmarshal(base, &b) != nil || json.Unmarshal(cont, &c) != nil {
		return nil, false
	}
	switch format {
	case "openai":
		bc, ok1 := b["choices"].([]any)
		cc, ok2 := c["choices"].([]any)
		if !ok1 || !ok2 || len(bc) == 0 || len(cc) == 0 {
			return nil, false
		}
		bm, _ := bc[0].(map[string]any)
		cm, _ := cc[0].(map[string]any)
		// 内容在 choices[0].message.content（非流式），不是 choice 顶层
		bMsg, _ := bm["message"].(map[string]any)
		cMsg, _ := cm["message"].(map[string]any)
		if bMsg == nil || cMsg == nil {
			return nil, false
		}
		bMsg["content"] = anyContentToString(bMsg["content"]) + anyContentToString(cMsg["content"])
		if fr, ok := cm["finish_reason"]; ok {
			bm["finish_reason"] = fr
		}
		if bu, ok := b["usage"].(map[string]any); ok {
			if cu, ok := c["usage"].(map[string]any); ok {
				for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
					if pv, ok := numField(bu, k); ok {
						if cv, ok := numField(cu, k); ok {
							bu[k] = int64(pv + cv)
						}
					}
				}
			}
		}
	case "gemini":
		bc, ok1 := b["candidates"].([]any)
		cc, ok2 := c["candidates"].([]any)
		if !ok1 || !ok2 || len(bc) == 0 || len(cc) == 0 {
			return nil, false
		}
		bm, _ := bc[0].(map[string]any)
		cm, _ := cc[0].(map[string]any)
		bm["content"] = anyContentToString(bm["content"]) + anyContentToString(cm["content"])
		if fr, ok := cm["finishReason"]; ok {
			bm["finishReason"] = fr
		}
		if bu, ok := b["usageMetadata"].(map[string]any); ok {
			if cu, ok := c["usageMetadata"].(map[string]any); ok {
				for _, k := range []string{"promptTokenCount", "candidatesTokenCount", "totalTokenCount"} {
					if pv, ok := numField(bu, k); ok {
						if cv, ok := numField(cu, k); ok {
							bu[k] = int64(pv + cv)
						}
					}
				}
			}
		}
	case "anthropic":
		bm, ok1 := b["content"].([]any)
		cm, ok2 := c["content"].([]any)
		if !ok1 || !ok2 {
			return nil, false
		}
		b["content"] = append(bm, cm...)
		if sr, ok := c["stop_reason"]; ok {
			b["stop_reason"] = sr
		}
		if bu, ok := b["usage"].(map[string]any); ok {
			if cu, ok := c["usage"].(map[string]any); ok {
				for _, k := range []string{"input_tokens", "output_tokens"} {
					if pv, ok := numField(bu, k); ok {
						if cv, ok := numField(cu, k); ok {
							bu[k] = int64(pv + cv)
						}
					}
				}
			}
		}
	default:
		return nil, false
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, false
	}
	return out, true
}

// ---- 非流式续写主循环 ----

// continuationResult 对非流式成功响应执行截断续写循环。
// 失败或不可续写时返回已得的最好结果（绝不因续写失败而报错给客户端）。
func continuationResult(ctx context.Context, h *hub.Hub, client *http.Client, opts Options,
	first *Result, firstBody []byte) *Result {

	body := firstBody
	truncated, format := detectTruncation(body)
	if !truncated {
		first.Body = body
		first.Stream = nil
		return first
	}
	noteContinuation(h)

	origBodyFor := opts.BodyFor
	if origBodyFor == nil {
		b := opts.Body
		origBodyFor = func(*config.Account) ([]byte, string) { return b, "" }
	}
	rounds := h.Settings().ContinuationMaxRounds
	res := first
	for r := 0; r < rounds; r++ {
		partial := partialText(body, format)
		if strings.TrimSpace(partial) == "" {
			break // 提取不到部分输出（如 tool_calls 截断），不盲目续写
		}
		next, err := doOnceWithContinuation(ctx, h, client, opts, origBodyFor, partial)
		if err != nil || next == nil || next.Status >= 400 {
			if next != nil {
				next.Close() // 续写轮失败也要释放上游连接
			}
			break
		}
		nb := next.ReadAll()
		merged, ok := mergeChatResponse(body, nb, format)
		next.Close()
		if !ok {
			break
		}
		body = merged
		res = next
		noteContinuation(h)
		if t, _ := detectTruncation(body); !t {
			break // 续写完成
		}
	}
	res.Body = body
	res.Stream = nil
	return res
}

// doOnceWithContinuation 以「追加续写消息后的 body」重新走一轮完整的
// 排队 / 选号 / 节拍 / 上游请求（复用 doOnce 的全部语义，含 429 内重试）。
func doOnceWithContinuation(ctx context.Context, h *hub.Hub, client *http.Client,
	opts Options, origBodyFor func(*config.Account) ([]byte, string), partial string) (*Result, error) {
	wrapped := Options(opts)
	wrapped.BodyFor = func(a *config.Account) ([]byte, string) {
		base, model := origBodyFor(a)
		return appendContinuationMessages(base, partial), model
	}
	return doOnce(ctx, h, client, wrapped)
}

// ---- 流式续写 ----

// sseFilter 是 SSE 逐行过滤器：记录 finish_reason、累积 delta 文本、
// 吞掉 finish chunk 与 data: [DONE]（由 continuationStream 在真正结束时补发）。
// 非 OpenAI chunk 格式自动切换原样透传。
type sseFilter struct {
	sc           *bufio.Scanner
	pending      []byte // 已过滤待读字节
	eof          bool
	pass         bool   // 非 OpenAI 格式，原样透传
	finish       []byte // 缓存的 finish chunk 行（原样字节，含 \n）
	finishReason string
	partial      strings.Builder
	sawOpenAI    bool
}

func newSSEFilter(r io.Reader) *sseFilter {
	f := &sseFilter{}
	f.sc = bufio.NewScanner(r)
	f.sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return f
}

func (f *sseFilter) emit(line string) {
	f.pending = append(f.pending, line...)
	f.pending = append(f.pending, '\n')
}

// fill 从 scanner 尽量填充 pending，返回是否有了新数据。
func (f *sseFilter) fill() bool {
	for f.sc.Scan() {
		line := f.sc.Text()
		if f.pass {
			f.emit(line)
			return true
		}
		if !strings.HasPrefix(line, "data:") {
			f.emit(line)
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue // 吞掉，结尾由 continuationStream 补发
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			f.emit(line)
			continue
		}
		choices, ok := chunk["choices"].([]any)
		if !ok {
			// usage-only 收尾帧（stream_options.include_usage）等：原样下发
			f.sawOpenAI = true
			f.emit(line)
			continue
		}
		f.sawOpenAI = true
		if len(choices) > 0 {
			if c, ok := choices[0].(map[string]any); ok {
				if d, ok := c["delta"].(map[string]any); ok {
					f.partial.WriteString(anyContentToString(d["content"]))
				}
				if fr, _ := c["finish_reason"].(string); fr != "" {
					f.finishReason = fr
					f.finish = append([]byte(line), '\n')
					continue // 吞掉 finish chunk，收尾时决定是否补发
				}
			}
		}
		f.emit(line)
		return true
	}
	f.eof = true
	return false
}

func (f *sseFilter) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		if f.eof {
			return 0, io.EOF
		}
		if !f.fill() {
			return 0, io.EOF
		}
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

// continuationStream 把「多轮续写的 SSE 流」封装成一个 ReadCloser：
// 当前段读到 EOF 时，若上一段 finish_reason=length 且还有轮数，就自动重新
// 排队发起新一轮请求继续推；轮数用尽或正常结束时补发 finish chunk + [DONE]。
// 对 server 层完全透明（它只管 io.Copy）。
type continuationStream struct {
	ctx         context.Context
	h           *hub.Hub
	client      *http.Client
	opts        Options
	origBodyFor func(*config.Account) ([]byte, string)
	rounds      int

	round  int
	cur    *Result
	f      *sseFilter
	closed bool
}

func newContinuationStream(ctx context.Context, h *hub.Hub, client *http.Client,
	opts Options, first *Result, origBodyFor func(*config.Account) ([]byte, string), rounds int) *continuationStream {
	return &continuationStream{
		ctx: ctx, h: h, client: client, opts: opts,
		origBodyFor: origBodyFor, rounds: rounds,
		cur: first, f: newSSEFilter(first.Stream),
	}
}

// advance 结束当前段并切换到续写段。返回 false 表示无法继续（应收尾）。
func (c *continuationStream) advance() bool {
	partial := c.f.partial.String()
	if strings.TrimSpace(partial) == "" {
		return false
	}
	if c.round >= c.rounds {
		return false
	}
	prev := c.cur
	c.round++
	noteContinuation(c.h)
	next, err := doOnceWithContinuation(c.ctx, c.h, c.client, c.opts, c.origBodyFor, partial)
	if err != nil || next == nil || next.Status >= 400 || next.Stream == nil {
		if next != nil {
			next.Close()
		}
		return false
	}
	c.cur = next
	c.f = newSSEFilter(next.Stream)
	if prev != nil {
		prev.Close()
	}
	return true
}

func (c *continuationStream) Read(p []byte) (int, error) {
	for {
		if c.closed {
			return 0, io.EOF
		}
		n, err := c.f.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == nil {
			continue
		}
		// 当前段 EOF：决定续写或收尾
		if c.f.finishReason == "length" && c.advance() {
			// SSE 注释帧：对客户端不可见，但让连接保持活跃并便于抓包观察
			comment := fmt.Sprintf(": agnes-hub continuation round %d\n\n", c.round)
			if len(p) >= len(comment) {
				copy(p, comment)
				return len(comment), nil
			}
			continue
		}
		// 收尾：补发缓存的 finish chunk（stop / length 都要给客户端）+ [DONE]
		var tail []byte
		if c.f.sawOpenAI {
			tail = append(tail, c.f.finish...)
			tail = append(tail, []byte("data: [DONE]\n\n")...)
		}
		c.closeCurrent()
		if len(tail) == 0 {
			return 0, io.EOF
		}
		if len(p) < len(tail) {
			// p 太小（io.Copy 的 p 足够大，不会发生）：兜底塞 pending
			c.f.pending = append(c.f.pending, tail...)
			continue
		}
		copy(p, tail)
		return len(tail), io.EOF
	}
}

func (c *continuationStream) closeCurrent() {
	if c.cur != nil {
		c.cur.Close()
		c.cur = nil
	}
}

func (c *continuationStream) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.closeCurrent()
	return nil
}
