package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agneshub/internal/config"
	"agneshub/internal/hub"
)

func newTestHubForRelay(t *testing.T, tweak func(*config.Settings)) (*hub.Hub, *config.Store) {
	t.Helper()
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败：%v", err)
	}
	if err := store.UpdateSettings(func(s *config.Settings) {
		s.SafetyFactor = 1.0
		s.PacingWindowSec = 60
		s.ContinuationMaxRounds = 2
		s.UpstreamRequestGzip = false // 测试上游不解压，关闭 gzip 便于断言请求体
		if tweak != nil {
			tweak(s)
		}
	}); err != nil {
		t.Fatalf("写入设置失败：%v", err)
	}
	return hub.New(store), store
}

func TestDetectTruncation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
		fmt  string
	}{
		{"openai-length", `{"choices":[{"message":{"content":"x"},"finish_reason":"length"}]}`, true, "openai"},
		{"openai-stop", `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}]}`, false, ""},
		{"gemini-max", `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"MAX_TOKENS"}]}`, true, "gemini"},
		{"anthropic-max", `{"content":[{"type":"text","text":"x"}],"stop_reason":"max_tokens"}`, true, "anthropic"},
		{"bad-json", `not json`, false, ""},
	}
	for _, c := range cases {
		got, format := detectTruncation([]byte(c.body))
		if got != c.want || format != c.fmt {
			t.Errorf("%s: got (%v,%q), want (%v,%q)", c.name, got, format, c.want, c.fmt)
		}
	}
}

func TestAppendContinuationMessages(t *testing.T) {
	base := []byte(`{"model":"m","messages":[{"role":"user","content":"写首诗"}]}`)
	out := appendContinuationMessages(base, "半个回答")
	var req map[string]any
	if json.Unmarshal(out, &req) != nil {
		t.Fatal("输出不是合法 JSON")
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("应追加 assistant+user 两条消息，实际 %d 条", len(msgs))
	}
	if m, _ := msgs[1].(map[string]any); m["role"] != "assistant" || m["content"] != "半个回答" {
		t.Errorf("第 2 条应为 assistant 部分输出，实际 %v", m)
	}
	if m, _ := msgs[2].(map[string]any); m["role"] != "user" {
		t.Errorf("第 3 条应为 user 续写指令，实际 %v", m)
	}
	// 非 messages 格式原样返回
	if out := appendContinuationMessages([]byte(`{"candidates":[]}`), "x"); string(out) != `{"candidates":[]}` {
		t.Errorf("非 OpenAI 格式应原样返回，实际 %s", out)
	}
}

func TestMergeChatResponseOpenAI(t *testing.T) {
	base := []byte(`{"choices":[{"message":{"role":"assistant","content":"part1"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	cont := []byte(`{"choices":[{"message":{"role":"assistant","content":"part2"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":7,"total_tokens":37}}`)
	merged, ok := mergeChatResponse(base, cont, "openai")
	if !ok {
		t.Fatal("合并失败")
	}
	var m map[string]any
	if json.Unmarshal(merged, &m) != nil {
		t.Fatal("合并结果不是合法 JSON")
	}
	ch := m["choices"].([]any)[0].(map[string]any)
	cmsg, _ := ch["message"].(map[string]any)
	if cmsg == nil || cmsg["content"] != "part1part2" {
		t.Errorf("内容应拼接进 message.content，实际 %v", ch)
	}
	if ch["finish_reason"] != "stop" {
		t.Errorf("finish_reason 应沿用续写轮 stop，实际 %v", ch["finish_reason"])
	}
	u := m["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(40) || u["completion_tokens"] != float64(12) || u["total_tokens"] != float64(52) {
		t.Errorf("usage 应累加，实际 %v", u)
	}
}

// TestSSEFilterSwallowsFinishAndDone 过滤器吞掉 finish chunk 与 [DONE]，并累积 delta 文本。
func TestSSEFilterSwallowsFinishAndDone(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"He"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"llo"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n") + "\n"
	f := newSSEFilter(strings.NewReader(input))
	out, _ := io.ReadAll(f)
	if strings.Contains(string(out), "finish_reason") || strings.Contains(string(out), "[DONE]") {
		t.Errorf("finish chunk 与 [DONE] 应被吞掉，实际输出：%s", out)
	}
	if !strings.Contains(string(out), `"content":"He"`) || !strings.Contains(string(out), `"content":"llo"`) {
		t.Errorf("内容 delta 应原样下发，实际输出：%s", out)
	}
	if f.partial.String() != "Hello" {
		t.Errorf("partial 应累积为 Hello，实际 %q", f.partial.String())
	}
	if f.finishReason != "length" {
		t.Errorf("finishReason 应记录 length，实际 %q", f.finishReason)
	}
}

// 搭一个指向 httptest 上游的账号。
func addTestUpstream(t *testing.T, h *hub.Hub, store *config.Store, url string) {
	t.Helper()
	a := store.AddAccount("测试号", "sk-test", "free", "",
		&config.ModelManifest{Text: []string{"agnes-2.5-flash"}})
	store.MutateAccount(a.ID, func(acc *config.Account) bool {
		acc.BaseURL = url
		acc.Enabled = true
		return true
	})
	h.Reload()
}

func chatOptions(body []byte, stream bool) Options {
	return Options{
		PoolClass: "text", RequiredModel: "agnes-2.5-flash",
		Path: "/v1/chat/completions", Body: body, Idempotent: true,
		Stream: stream, Continuable: true,
	}
}

// TestContinuationNonStream 端到端：第一轮截断（finish_reason=length），
// 网关自动追加续写消息重新请求，最终把两段拼成一个 finish_reason=stop 的响应。
func TestContinuationNonStream(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if len(bodies) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"part1"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"part2"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":7,"total_tokens":37}}`)
		}
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, nil)
	addTestUpstream(t, h, store, srv.URL)

	res, err := Do(context.Background(), h, srv.Client(), chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`), false))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	body := res.ReadAll()
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		t.Fatalf("响应不是合法 JSON：%s", body)
	}
	ch := m["choices"].([]any)[0].(map[string]any)
	msg, _ := ch["message"].(map[string]any)
	if msg == nil || msg["content"] != "part1part2" {
		t.Errorf("内容应拼接进 message.content，实际 %v", ch)
	}
	if ch["finish_reason"] != "stop" {
		t.Errorf("最终 finish_reason 应为 stop，实际 %v", ch["finish_reason"])
	}
	if len(bodies) != 2 {
		t.Fatalf("应发出 2 轮上游请求，实际 %d", len(bodies))
	}
	// 第二轮请求必须带上第一轮的部分输出 + 续写指令
	var req2 map[string]any
	if json.Unmarshal([]byte(bodies[1]), &req2) != nil {
		t.Fatalf("第二轮请求体不合法：%s", bodies[1])
	}
	msgs, _ := req2["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("第二轮请求应有 3 条消息，实际 %d", len(msgs))
	}
	if h.Metrics.Continuations.Load() < 1 {
		t.Error("续写轮数应被统计")
	}
}

// TestContinuationStream 端到端流式：第一段 SSE 截断，网关在同一流上自动
// 续推第二段；客户端视角只看到一个 finish chunk 与一个 [DONE]。
func TestContinuationStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		{
			raw, _ := io.ReadAll(r.Body)
			if strings.Count(string(raw), `"role":"user"`) == 1 {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			} else {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"! more\"}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}
		}
		f.Flush()
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, nil)
	addTestUpstream(t, h, store, srv.URL)

	res, err := Do(context.Background(), h, srv.Client(), chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`), true))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	raw, _ := io.ReadAll(res.Stream)
	out := string(raw)

	if !strings.Contains(out, "Hello") || !strings.Contains(out, " world") || !strings.Contains(out, "! more") {
		t.Errorf("三段内容都应出现在同一流里，实际：\n%s", out)
	}
	if strings.Count(out, "[DONE]") != 1 {
		t.Errorf("[DONE] 应恰好出现 1 次（由网关收尾补发），实际 %d 次：\n%s", strings.Count(out, "[DONE]"), out)
	}
	if strings.Count(out, `"finish_reason"`) != 1 {
		t.Errorf("finish chunk 应恰好出现 1 次（中间截断的被吞），实际 %d 次：\n%s", strings.Count(out, `"finish_reason"`), out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("最终 finish 应为 stop：\n%s", out)
	}
	if !strings.Contains(out, ": agnes-hub continuation round 1") {
		t.Errorf("续写处应有 SSE 注释帧：\n%s", out)
	}
	if h.Metrics.Continuations.Load() < 1 {
		t.Error("续写轮数应被统计")
	}
}

// TestContinuationDisabled 轮数为 0 时截断原样透传（零回归）。
func TestContinuationDisabled(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"choices":[{"message":{"content":"part1"},"finish_reason":"length"}]}`)
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, func(s *config.Settings) { s.ContinuationMaxRounds = 0 })
	addTestUpstream(t, h, store, srv.URL)

	res, err := Do(context.Background(), h, srv.Client(), chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`), false))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	body := res.ReadAll()
	if calls != 1 {
		t.Fatalf("禁用续写时只应请求 1 次，实际 %d", calls)
	}
	if !strings.Contains(string(body), `"finish_reason":"length"`) {
		t.Errorf("禁用续写时应原样透传截断响应，实际 %s", body)
	}
}
