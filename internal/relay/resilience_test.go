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

// TestClassifyResponse 锁定「哪些响应算内容不可用」。
// 其中最关键的一条是最后一例：异构结构（视频轮询）必须被判为可用，
// 否则会被当成空内容无限重发。
func TestClassifyResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want respKind
	}{
		{"openai-正常", `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`, respOK},
		{"openai-空内容", `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`, respEmpty},
		{"openai-choices空", `{"choices":[],"usage":{}}`, respEmpty},
		{"openai-工具调用无文本也算可用", `{"choices":[{"message":{"content":"","tool_calls":[{"id":"1","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, respOK},
		{"openai-截断交续写层", `{"choices":[{"message":{"content":"x"},"finish_reason":"length"}]}`, respTruncated},
		{"openai-审查拒绝", `{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}]}`, respBlocked},
		{"gemini-parts有文本", `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`, respOK},
		{"gemini-parts空", `{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`, respEmpty},
		{"gemini-安全拒绝", `{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`, respBlocked},
		{"anthropic-正常", `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`, respOK},
		{"anthropic-空", `{"content":[],"stop_reason":"end_turn"}`, respEmpty},
		{"内嵌error", `{"error":{"message":"internal error"},"choices":[]}`, respErrorish},
		{"非法JSON", `{"choices":[{"message":`, respMalformed},
		{"空体", ``, respEmpty},
		{"视频轮询-不得误判", `{"id":"vid_1","status":"queued","model":"sora"}`, respOK},
		{"图片轮询-不得误判", `{"created":1,"data":[]}`, respOK},
	}
	for _, c := range cases {
		if got := classifyResponse([]byte(c.body)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSoftFailRefiresAnotherAccount：「200 但空内容」不得交给客户端，
// 网关应换号重发，客户端最终拿到的是另一个号的正常回答。
func TestSoftFailRefiresAnotherAccount(t *testing.T) {
	var badHits, goodHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		// 用 Authorization 判断落在哪个号上
		if r.Header.Get("Authorization") == "Bearer sk-bad" {
			badHits++
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{}}`)
			return
		}
		goodHits++
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"真正有用的回答"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, func(s *config.Settings) { s.SoftFailRetryMax = 2 })
	addTestUpstreamWithKey(t, h, store, "坏号", srv.URL, "sk-bad")
	addTestUpstreamWithKey(t, h, store, "好号", srv.URL, "sk-good")

	res, err := Do(context.Background(), h, srv.Client(),
		chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`), false))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	raw := res.ReadAll()

	if !strings.Contains(string(raw), "真正有用的回答") {
		t.Errorf("应换号拿到有效回答，实际：%s", raw)
	}
	if badHits == 0 || goodHits == 0 {
		t.Errorf("应两个号都试过，实际 bad=%d good=%d", badHits, goodHits)
	}
	if h.Metrics.SoftFailRetries.Load() < 1 {
		t.Error("软失败重发次数应被统计")
	}
}

// TestSoftFailRespectsBudget SoftFailRetryMax=0 时保持旧行为（透传，零回归）。
func TestSoftFailRespectsBudget(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, func(s *config.Settings) { s.SoftFailRetryMax = 0 })
	addTestUpstream(t, h, store, srv.URL)

	res, err := Do(context.Background(), h, srv.Client(),
		chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`), false))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	if calls != 1 {
		t.Errorf("预算为 0 时应只请求 1 次并透传，实际 %d 次", calls)
	}
}

// TestSoftFailGiveUpOnBudgetExhausted 软失败预算（soft_fail_retry_max）耗尽后，
// 网关必须把「无内容的 200」换成带原因的 502，而不是伪装成功透传导致客户端静默挂死。
func TestSoftFailGiveUpOnBudgetExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		// 三个号全部返回空内容（无可用产出）
		fmt.Fprint(w, `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, func(s *config.Settings) { s.SoftFailRetryMax = 2 })
	for i := 0; i < 3; i++ {
		addTestUpstream(t, h, store, srv.URL)
	}

	res, err := Do(context.Background(), h, srv.Client(),
		chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`), false))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	raw := res.ReadAll()

	if res.Status != http.StatusBadGateway {
		t.Errorf("预算耗尽应返回 502，实际 %d", res.Status)
	}
	if !strings.Contains(string(raw), "agnes_upstream_content_unavailable") {
		t.Errorf("502 应带可识别错误类型，实际：%s", raw)
	}
	if h.Metrics.SoftFailRetries.Load() != 2 {
		t.Errorf("应换号重发 2 次，实际 %d", h.Metrics.SoftFailRetries.Load())
	}
	if h.Metrics.SoftFailGaveUp.Load() != 1 {
		t.Errorf("放弃续救计数应为 1，实际 %d", h.Metrics.SoftFailGaveUp.Load())
	}
}

// TestSoftFailGiveUpOnAllExcluded 账号数 ≤ 重发预算、全部返回空内容时，Pick 会
// 因全部被 exclude 而失败；此时若最后一次正是软失败，仍应转成 502，而非把 200 空内容透传。
func TestSoftFailGiveUpOnAllExcluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		fmt.Fprint(w, `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, func(s *config.Settings) { s.SoftFailRetryMax = 2 })
	addTestUpstream(t, h, store, srv.URL)
	addTestUpstream(t, h, store, srv.URL)

	res, err := Do(context.Background(), h, srv.Client(),
		chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`), false))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	raw := res.ReadAll()

	if res.Status != http.StatusBadGateway {
		t.Errorf("全 exclude 后也应返回 502，实际 %d", res.Status)
	}
	if !strings.Contains(string(raw), "agnes_upstream_content_unavailable") {
		t.Errorf("502 应带可识别错误类型，实际：%s", raw)
	}
	if h.Metrics.SoftFailGaveUp.Load() != 1 {
		t.Errorf("放弃续救计数应为 1，实际 %d", h.Metrics.SoftFailGaveUp.Load())
	}
}

// TestVideoPollShapeNotRefired 视频轮询这类异构响应绝不能被当空内容重发。
func TestVideoPollShapeNotRefired(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"id":"vid_1","object":"video","status":"queued"}`)
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, func(s *config.Settings) { s.SoftFailRetryMax = 2 })
	addTestUpstream(t, h, store, srv.URL)

	opts := Options{
		PoolClass: "video", Method: http.MethodGet, Path: "/v1/videos/vid_1",
		Idempotent: true, // 轮询是幂等的，正是最容易误伤的一类
	}
	res, err := Do(context.Background(), h, srv.Client(), opts)
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	res.ReadAll()
	if calls != 1 {
		t.Errorf("视频轮询状态响应应只请求 1 次，实际 %d 次", calls)
	}
}

// TestStreamImplicitTruncationResume 上游「没有任何 finish_reason 就断流」——
// 这是 agnes 最常见的隐式截断，客户端原本会直接卡死/报错，现在网关续写。
func TestStreamImplicitTruncationResume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		raw, _ := io.ReadAll(r.Body)
		if strings.Count(string(raw), `"role":"user"`) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"半截回答\"}}]}\n\n")
			// 注意：没有 finish chunk、没有 [DONE]，写完就直接断开 —— 隐式截断
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"，我接着说完。\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
		f.Flush()
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, nil)
	addTestUpstream(t, h, store, srv.URL)

	res, err := Do(context.Background(), h, srv.Client(),
		chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`), true))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	out := string(mustReadAll(t, res.Stream))

	if !strings.Contains(out, "半截回答") || !strings.Contains(out, "，我接着说完。") {
		t.Errorf("隐式截断应自动续写，实际输出：\n%s", out)
	}
	if strings.Count(out, "[DONE]") != 1 {
		t.Errorf("[DONE] 应恰好 1 次，实际 %d 次：\n%s", strings.Count(out, "[DONE]"), out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("收尾必须有规范 finish 帧，否则客户端 SDK 会判为未完成：\n%s", out)
	}
	if h.Metrics.ResumedStreams.Load() < 1 {
		t.Error("auto-resume 次数应被统计")
	}
}

// TestStreamErrorFrameSwallowed 上游中途塞 error 帧：不得泄漏给客户端，
// 网关吞掉后续写。
func TestStreamErrorFrameSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		raw, _ := io.ReadAll(r.Body)
		if strings.Count(string(raw), `"role":"user"`) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"写了两句\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"upstream blew up\",\"code\":500}}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"然后写完了\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
		f.Flush()
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, nil)
	addTestUpstream(t, h, store, srv.URL)

	res, err := Do(context.Background(), h, srv.Client(),
		chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`), true))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	out := string(mustReadAll(t, res.Stream))

	if strings.Contains(out, "upstream blew up") {
		t.Errorf("error 帧不得泄漏给客户端（客户端一收到就停）：\n%s", out)
	}
	if !strings.Contains(out, "然后写完了") {
		t.Errorf("应自动续写完：\n%s", out)
	}
	if strings.Count(out, "[DONE]") != 1 || !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("必须有且仅有一个规范收尾：\n%s", out)
	}
}

// TestStreamAlwaysTerminates 即使续写轮也失败，网关也必须以规范 finish + [DONE]
// 收尾——绝不能让客户端「既没收到结束标记又收到断开」（SDK 会抛错中断任务）。
func TestStreamAlwaysTerminates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		raw, _ := io.ReadAll(r.Body)
		if strings.Count(string(raw), `"role":"user"`) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"前言\"}}]}\n\n")
			// 无 finish、无 [DONE]，直接断
		} else {
			w.WriteHeader(http.StatusInternalServerError) // 续写轮彻底失败
		}
		f.Flush()
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, nil)
	addTestUpstream(t, h, store, srv.URL)

	res, err := Do(context.Background(), h, srv.Client(),
		chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`), true))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	out := string(mustReadAll(t, res.Stream))

	if !strings.Contains(out, "前言") {
		t.Errorf("已生成的部分必须保留：\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, "[DONE]") {
		t.Errorf("续写失败也必须补发规范收尾帧，实际：\n%s", out)
	}
}

// TestPaymentRequiredSwitchesAccount 402 额度耗尽可能只针对某个号：
// 必须换号再试，而不是直接把 402 甩给客户端。
func TestPaymentRequiredSwitchesAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-poor" {
			w.WriteHeader(http.StatusPaymentRequired)
			fmt.Fprint(w, `{"error":{"message":"insufficient balance"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"用另一个号成功了"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, nil)
	addTestUpstreamWithKey(t, h, store, "没钱号", srv.URL, "sk-poor")
	addTestUpstreamWithKey(t, h, store, "有钱号", srv.URL, "sk-rich")

	res, err := Do(context.Background(), h, srv.Client(),
		chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`), false))
	if err != nil {
		t.Fatalf("Do 失败：%v", err)
	}
	defer res.Close()
	raw := res.ReadAll()
	if strings.Contains(string(raw), "insufficient balance") {
		t.Errorf("还有别的号有额度时不应把 402 透传给客户端，实际：%s", raw)
	}
	if !strings.Contains(string(raw), "用另一个号成功了") {
		t.Errorf("应换号重试成功，实际：%s", raw)
	}
}

// TestExhaustedReturnsUpstreamTruth 所有号都不可用时，交还最后那次真实的上游
// 结果（含 429 + Retry-After 语义），而不是毫无信息量的「无可用账号」。
func TestExhaustedReturnsUpstreamTruth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"upstream down"}}`)
	}))
	defer srv.Close()

	h, store := newTestHubForRelay(t, func(s *config.Settings) { s.RetryMax = 3 })
	addTestUpstream(t, h, store, srv.URL)

	res, err := Do(context.Background(), h, srv.Client(),
		chatOptions([]byte(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`), false))
	if err != nil {
		t.Fatalf("不应返回 transport 级错误：%v", err)
	}
	defer res.Close()
	if res.Status != http.StatusServiceUnavailable {
		t.Errorf("应保留上游真实状态码 503，实际 %d", res.Status)
	}
	if strings.Contains(string(res.ReadAll()), "可用账号") {
		t.Errorf("不应把真实原因替换成笼统的「无可用账号」")
	}
}

// ---- helpers ----

func addTestUpstreamWithKey(t *testing.T, h *hub.Hub, store *config.Store, name, url, key string) {
	t.Helper()
	a := store.AddAccount(name, key, "free", "",
		&config.ModelManifest{Text: []string{"agnes-2.5-flash"}})
	store.MutateAccount(a.ID, func(acc *config.Account) bool {
		acc.BaseURL = url
		acc.Enabled = true
		return true
	})
	h.Reload()
}

func mustReadAll(t *testing.T, r io.ReadCloser) []byte {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读取流向失败：%v", err)
	}
	return raw
}

var _ = json.Marshal
