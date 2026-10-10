package web

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/hub"
	"agneshub/internal/relay"
)

// readGzipAwareBody 读取上游请求体，感知 Content-Encoding: gzip（v1.0.23 起
// 网关默认对上传体做 gzip 压缩）。真实上游 agnes 能解压 gzip，故 mock 桩也须
// 同样解压，否则会把压缩体当明文 Unmarshal 出空 payload、误判 model 为空。
func readGzipAwareBody(r *http.Request) ([]byte, error) {
	lim := io.LimitReader(r.Body, 1<<20)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(lim)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		lim = io.LimitReader(gz, 1<<20)
	}
	return io.ReadAll(lim)
}

// mockAgnes 是一个「会真的限流」的模拟上游。
//
// 刻意复刻真实上游的两个关键行为：
//  1. 相邻请求间隔小于 minInterval 就返回 429（真实约束是「间隔」，不是窗口计数）；
//  2. 模型与端点不匹配时返回 400 且提示「是 chat 模型」——
//     实测真实上游对 model=agnes-auto 发到 /v1/images/generations 就是这个反应，
//     这条断言能证明网关确实做了模型改写，而不是把 auto 原样透传。
type mockAgnes struct {
	mu          sync.Mutex
	minInterval time.Duration
	lastAt      map[string]time.Time
	rejected    atomic.Int64
	accepted    atomic.Int64
	byPath      sync.Map // path -> *atomic.Int64
	lastModel   sync.Map // path -> string
}

func newMockAgnes(minInterval time.Duration) *mockAgnes {
	return &mockAgnes{minInterval: minInterval, lastAt: map[string]time.Time{}}
}

func (m *mockAgnes) count(path string) *atomic.Int64 {
	v, _ := m.byPath.LoadOrStore(path, &atomic.Int64{})
	return v.(*atomic.Int64)
}

func (m *mockAgnes) hits(path string) int64 { return m.count(path).Load() }

func (m *mockAgnes) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		body, _ := readGzipAwareBody(r)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		model, _ := payload["model"].(string)
		m.lastModel.Store(path, model)

		if strings.HasSuffix(path, "/v1/models") {
			writeJSONRaw(w, 200, map[string]any{"object": "list", "data": []map[string]any{
				{"id": "agnes-2.5-flash"}, {"id": "agnes-image-2.5-flash"}, {"id": "agnes-video-2.5-flash"},
			}})
			return
		}

		// 速率闸门：按路径独立计（真实上游文本池与图片池互不影响）
		m.mu.Lock()
		now := time.Now()
		last := m.lastAt[path]
		if !last.IsZero() && now.Sub(last) < m.minInterval {
			m.mu.Unlock()
			m.rejected.Add(1)
			writeJSONRaw(w, 429, map[string]any{"error": map[string]any{
				"message": "您已达到免费用户的 API 速率限制", "code": "rate_limit"}})
			return
		}
		m.lastAt[path] = now
		m.mu.Unlock()
		m.accepted.Add(1)
		m.count(path).Add(1)

		switch {
		case strings.Contains(path, "/v1/chat/completions"):
			if !strings.Contains(model, "flash") || strings.Contains(model, "image") || strings.Contains(model, "video") {
				writeJSONRaw(w, 400, map[string]any{"error": map[string]any{
					"message": "模型 " + model + " 不是 chat 模型", "code": "invalid_request"}})
				return
			}
			writeJSONRaw(w, 200, map[string]any{
				"id": "cmpl-mock", "object": "chat.completion", "created": time.Now().Unix(),
				"model": model,
				"choices": []map[string]any{{"index": 0,
					"message":       map[string]any{"role": "assistant", "content": "pong"},
					"finish_reason": "stop"}},
			})
		case strings.Contains(path, "/v1/images/generations"):
			if !strings.Contains(model, "image") {
				// 复刻真实上游的拒绝方式：把非图片模型判成 chat 模型
				writeJSONRaw(w, 400, map[string]any{"error": map[string]any{
					"message": "模型 " + model + " 是 chat 模型，请使用 /v1/chat/completions",
					"code":    "invalid_request"}})
				return
			}
			if _, ok := payload["prompt"].(string); !ok {
				writeJSONRaw(w, 400, map[string]any{"code": "invalid_request", "message": "prompt 不能为空"})
				return
			}
			writeJSONRaw(w, 200, map[string]any{
				"created": time.Now().Unix(),
				"data":    []map[string]any{{"url": "https://cdn.example.test/mock-1.png", "revised_prompt": "mock"}},
			})
		case strings.Contains(path, "/v1/videos"):
			if _, ok := payload["prompt"].(string); !ok {
				writeJSONRaw(w, 400, map[string]any{"code": "invalid_request", "message": "prompt 不能为空"})
				return
			}
			writeJSONRaw(w, 200, map[string]any{"video_id": "vid-mock-0001", "status": "queued"})
		case strings.Contains(path, "/agnesapi"):
			writeJSONRaw(w, 200, map[string]any{"status": "completed",
				"video_url": "https://cdn.example.test/mock-1.mp4"})
		default:
			writeJSONRaw(w, 404, map[string]any{"error": map[string]any{"message": "not found: " + path}})
		}
	})
}

func writeJSONRaw(w http.ResponseWriter, status int, payload any) {
	buf, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// harness 组装一个指向模拟上游的完整实例。
type harness struct {
	t       *testing.T
	store   *config.Store
	hub     *hub.Hub
	srv     *Server
	ts      *httptest.Server
	up      *httptest.Server
	mock    *mockAgnes
	apiKey  string
	baseURL string
}

func newHarness(t *testing.T, minInterval time.Duration, accountCount int) *harness {
	t.Helper()
	mock := newMockAgnes(minInterval)
	up := httptest.NewServer(mock.handler())

	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("初始化 store 失败：%v", err)
	}
	_ = store.UpdateSettings(func(s *config.Settings) {
		s.PacingWindowSec = 3 // 压缩时间轴，让测试在秒级跑完；生产保持 60
		s.SafetyFactor = 1.0  // 测试里不要额外余量，便于精确断言节拍
		s.MustChangePassword = false
		s.CalibrationEnabled = false // 单独用专门的用例验证校准
		s.QueueMaxWaitMS = 60000
		s.KeepaliveMS = 300
	})

	for i := 0; i < accountCount; i++ {
		acc := store.AddAccount(
			"test-"+string(rune('A'+i)), "key-"+string(rune('a'+i)), "free",
			up.URL+"/v1",
			&config.ModelManifest{
				Text:  []string{"agnes-2.5-flash"},
				Image: []string{"agnes-image-2.5-flash"},
				Video: []string{"agnes-video-2.5-flash"},
			})
		_ = store.MutateAccount(acc.ID, func(a *config.Account) bool {
			a.RPMOverrides = map[string]float64{"text": 18, "image_1k": 18, "video": 1}
			a.MaxConcurrency = 16
			return true
		})
	}

	h := hub.New(store)
	srv := New(store, h, relay.BuildClient())
	ts := httptest.NewServer(srv)

	key := store.AddKey("test-key", []string{"*"}, 0, 0, "")

	harness := &harness{t: t, store: store, hub: h, srv: srv, ts: ts, up: up, mock: mock,
		apiKey: key.Key, baseURL: up.URL + "/v1"}
	t.Cleanup(func() { ts.Close(); up.Close() })
	return harness
}

func (h *harness) post(path string, payload map[string]any) (*http.Response, map[string]any) {
	h.t.Helper()
	buf, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+path, strings.NewReader(string(buf)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("请求 %s 失败：%v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func chatBody(text string) map[string]any {
	return map[string]any{
		"model": "agnes-auto",
		"messages": []map[string]any{
			{"role": "user", "content": text},
		},
	}
}

// ---------------------------------------------------------------------------
// 一、永远不把 429 透传给客户端
// ---------------------------------------------------------------------------

func TestNeverLeaks429UnderBurst(t *testing.T) {
	h := newHarness(t, 140*time.Millisecond, 1) // 上游相邻间隔 < 140ms 就限流

	const concurrency = 12
	var wg sync.WaitGroup
	statuses := make([]int, concurrency)
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			resp, _ := h.post("/v1/chat/completions", chatBody("hello"))
			statuses[idx] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()

	for i, code := range statuses {
		if code != 200 {
			t.Errorf("第 %d 个请求拿到 HTTP %d，期望 200 —— 客户端不应看到 429", i, code)
		}
	}
	if got := h.mock.rejected.Load(); got != 0 {
		t.Errorf("模拟上游拒绝了 %d 次请求，说明节拍没压住真实边界（应恒为 0）", got)
	}
	if got := h.mock.accepted.Load(); got != concurrency {
		t.Errorf("上游实际受理 %d 次，期望 %d", got, concurrency)
	}
}

func TestUpstreamDoesRejectWhenPacingDisabled(t *testing.T) {
	// 对照组：把有效 RPM 调到远超上游边界，证明「这个模拟上游真的会限流」。
	// 否则上面那个测试可能只是恒真而毫无价值。
	h := newHarness(t, 140*time.Millisecond, 1)
	acc := h.store.AccountsSnapshot()[0]
	h.store.MutateAccount(acc.ID, func(a *config.Account) bool {
		a.RPMOverrides = map[string]float64{"text": 6000}
		return true
	})
	h.hub.Reload()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.post("/v1/chat/completions", chatBody("hello")) }()
	}
	wg.Wait()
	if h.mock.rejected.Load() == 0 {
		t.Fatal("模拟上游在超速时没有限流 —— 说明它不具备限流能力，前一个测试的结论不成立")
	}
}

// ---------------------------------------------------------------------------
// 二、agnes-auto 的三种模态
// ---------------------------------------------------------------------------

func TestAutoRoutesText(t *testing.T) {
	h := newHarness(t, 0, 1)
	resp, body := h.post("/v1/chat/completions", chatBody("解释一下什么是向量数据库"))
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %v", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Agnes-Hub-Intent"); got != "text" {
		t.Errorf("意图应为 text，实际 %q", got)
	}
	if got := h.mock.lastModelOf("/v1/chat/completions"); got != "agnes-2.5-flash" {
		t.Errorf("上游收到的模型应为 agnes-2.5-flash，实际 %q", got)
	}
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("响应缺少 choices：%v", body)
	}
}

func TestAutoRoutesImageFromContent(t *testing.T) {
	h := newHarness(t, 0, 1)
	resp, body := h.post("/v1/chat/completions", chatBody("帮我画一张赛博朋克风格的城市夜景"))
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %v", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Agnes-Hub-Intent"); got != "image" {
		t.Fatalf("意图应为 image，实际 %q（判定依据 %s）", got, resp.Header.Get("X-Agnes-Hub-Intent-By"))
	}
	if got := resp.Header.Get("X-Agnes-Hub-Intent-By"); got != "content" {
		t.Errorf("判定依据应为 content，实际 %q", got)
	}
	// 关键断言：上游收到的模型必须被改写成图片模型。
	// 若把 agnes-auto 原样透传，模拟上游会回 400「是 chat 模型」。
	if got := h.mock.lastModelOf("/v1/images/generations"); got != "agnes-image-2.5-flash" {
		t.Errorf("上游图片端点收到的模型应为 agnes-image-2.5-flash，实际 %q", got)
	}
	// 且响应必须被回译成 chat 形态
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("响应缺少 choices：%v", body)
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "https://cdn.example.test/mock-1.png") {
		t.Errorf("chat 内容应包含图片链接，实际 %q", content)
	}
	if !strings.Contains(content, "![") {
		t.Errorf("chat 内容应为 Markdown 图片语法，实际 %q", content)
	}
	meta, _ := body["agnes_hub"].(map[string]any)
	if meta == nil || meta["intent"] != "image" {
		t.Errorf("响应缺少 agnes_hub 元信息：%v", body["agnes_hub"])
	}
}

func TestAutoRoutesVideoFromContent(t *testing.T) {
	h := newHarness(t, 0, 1)
	resp, body := h.post("/v1/chat/completions", chatBody("生成一段海边日落的海浪视频"))
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %v", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Agnes-Hub-Intent"); got != "video" {
		t.Fatalf("意图应为 video，实际 %q", got)
	}
	if got := h.mock.hits("/v1/videos"); got != 1 {
		t.Errorf("视频端点应被调用 1 次，实际 %d 次", got)
	}
	if got := h.mock.lastModelOf("/v1/videos"); got != "agnes-video-2.5-flash" {
		t.Errorf("上游视频端点收到的模型应为 agnes-video-2.5-flash，实际 %q", got)
	}
	meta, _ := body["agnes_hub"].(map[string]any)
	if meta == nil {
		t.Fatalf("响应缺少 agnes_hub：%v", body)
	}
	jobID, _ := meta["job_id"].(string)
	if !strings.HasPrefix(jobID, "job_") {
		t.Errorf("应签发 job_id，实际 %q", jobID)
	}
	if _, ok := h.store.JobByID(jobID); !ok {
		t.Errorf("job_id %s 应已登记到任务表", jobID)
	}
}

func TestAutoFallsBackToTextOnMetaQuestion(t *testing.T) {
	// 「how do I draw a cat in matplotlib」含 draw + cat，是典型的内容误判陷阱。
	h := newHarness(t, 0, 1)
	resp, _ := h.post("/v1/chat/completions", chatBody("how do I draw a cat in matplotlib?"))
	if got := resp.Header.Get("X-Agnes-Hub-Intent"); got != "text" {
		t.Fatalf("教学/疑问句应回落 text，实际 %q", got)
	}
	if h.mock.hits("/v1/images/generations") != 0 {
		t.Error("生图端点不应被调用")
	}
}

func TestAgentRequestNeverRoutedToImage(t *testing.T) {
	h := newHarness(t, 0, 1)
	payload := chatBody("画一张图")
	payload["tools"] = []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}}
	resp, _ := h.post("/v1/chat/completions", payload)
	if got := resp.Header.Get("X-Agnes-Hub-Intent"); got != "text" {
		t.Fatalf("带 tools 的请求必须走 text，实际 %q", got)
	}
	if h.mock.hits("/v1/images/generations") != 0 {
		t.Error("agent 请求不应被改道到生图")
	}
}

func TestExplicitEndpointForcesModality(t *testing.T) {
	h := newHarness(t, 0, 1)
	resp, body := h.post("/v1/images/generations", map[string]any{
		"model": "agnes-auto", "prompt": "a red apple",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %v", resp.StatusCode, body)
	}
	// 官方形态：裸 prompt 调用应返回上游原始结构，而不是 chat 信封
	if _, ok := body["data"]; !ok {
		t.Fatalf("裸 prompt 调用应返回官方 data 结构：%v", body)
	}
	if _, ok := body["choices"]; ok {
		t.Error("裸 prompt 调用不应被包成 chat 信封")
	}
	if got := h.mock.lastModelOf("/v1/images/generations"); got != "agnes-image-2.5-flash" {
		t.Errorf("模型应被改写为图片模型，实际 %q", got)
	}
}

func TestExplicitModelIsRespected(t *testing.T) {
	h := newHarness(t, 0, 1)
	_, _ = h.post("/v1/images/generations", map[string]any{
		"model": "agnes-image-2.1-flash", "prompt": "a red apple",
	})
	if got := h.mock.lastModelOf("/v1/images/generations"); got != "agnes-image-2.1-flash" {
		t.Errorf("客户端显式指定的模型必须被尊重，实际上游收到 %q", got)
	}
}

func TestIntentPreviewIsFree(t *testing.T) {
	h := newHarness(t, 0, 1)
	_, body := h.post("/v1/intent/preview", chatBody("生成一张产品海报"))
	if body["intent"] != "image" {
		t.Errorf("干跑判定应为 image，实际 %v（%v）", body["intent"], body["intent_reason"])
	}
	if h.mock.hits("/v1/images/generations") != 0 || h.mock.hits("/v1/chat/completions") != 0 {
		t.Error("干跑判定不应产生任何上游请求")
	}
}

// ---------------------------------------------------------------------------
// 三、多账号调配
// ---------------------------------------------------------------------------

func TestAccountsSpecialisedByModality(t *testing.T) {
	h := newHarness(t, 0, 2)
	accounts := h.store.AccountsSnapshot()
	// A 只做文本，B 只做图 —— 验证「按模态分工」真的生效
	h.store.MutateAccount(accounts[0].ID, func(a *config.Account) bool {
		a.ModelManifest = config.ModelManifest{Text: []string{"agnes-2.5-flash"},
			Image: []string{}, Video: []string{}}
		return true
	})
	h.store.MutateAccount(accounts[1].ID, func(a *config.Account) bool {
		a.ModelManifest = config.ModelManifest{Text: []string{},
			Image: []string{"agnes-image-2.5-flash"}, Video: []string{}}
		return true
	})
	h.hub.Reload()

	_, _ = h.post("/v1/chat/completions", chatBody("你好"))
	_, _ = h.post("/v1/chat/completions", chatBody("画一张猫的图片"))

	if h.mock.hits("/v1/chat/completions") == 0 {
		t.Error("应有一个账号承载了文本请求")
	}
	if h.mock.hits("/v1/images/generations") == 0 {
		t.Error("应有一个账号承载了生图请求")
	}
}

func TestModelFallbackWhenUnevenManifests(t *testing.T) {
	// 账号 A 有 image-2.5，账号 B 只有 image-2.1。
	// 无论请求落到谁身上，都必须用「该账号自己声明的模型」，而不是全局硬编码。
	h := newHarness(t, 0, 2)
	accounts := h.store.AccountsSnapshot()
	h.store.MutateAccount(accounts[0].ID, func(a *config.Account) bool {
		a.ModelManifest = config.ModelManifest{Text: []string{}, Image: []string{"agnes-image-2.1-flash"}, Video: []string{}}
		return true
	})
	h.store.MutateAccount(accounts[1].ID, func(a *config.Account) bool {
		a.Enabled = false
		return true
	})
	h.hub.Reload()

	_, _ = h.post("/v1/chat/completions", chatBody("画一张猫的图片"))
	if got := h.mock.lastModelOf("/v1/images/generations"); got != "agnes-image-2.1-flash" {
		t.Errorf("应采用该账号声明的 agnes-image-2.1-flash，实际 %q", got)
	}
}

func TestNoAccountDeclaresModality(t *testing.T) {
	h := newHarness(t, 0, 1)
	acc := h.store.AccountsSnapshot()[0]
	h.store.MutateAccount(acc.ID, func(a *config.Account) bool {
		a.ModelManifest = config.ModelManifest{Text: []string{"agnes-2.5-flash"},
			Image: []string{}, Video: []string{}}
		return true
	})
	h.hub.Reload()

	resp, body := h.post("/v1/chat/completions", chatBody("画一张猫的图片"))
	if resp.StatusCode != 503 {
		t.Fatalf("无人支持该模态时应回 503，实际 %d：%v", resp.StatusCode, body)
	}
	if h.mock.hits("/v1/images/generations") != 0 {
		t.Error("不应向上游发起生图请求")
	}
}

func TestUserFacingErrorsAreReadable(t *testing.T) {
	mock := newMockAgnes(0)
	up := httptest.NewServer(mock.handler())
	defer up.Close()
	store, _ := config.NewStore(t.TempDir())
	h := hub.New(store)
	srv := New(store, h, relay.BuildClient())
	srv.SetClient(&http.Client{Timeout: 5 * time.Second})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	key := store.AddKey("k", []string{"*"}, 0, 0, "")

	// 无账号：应为可读的 503，而不是 panic 或裸 500
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"agnes-auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("无账号时应回 503，实际 %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "模型清单") {
		t.Errorf("错误信息应给出可操作指引，实际 %s", string(raw))
	}
}

func TestChineseAccountNameDoesNotBreakHeaders(t *testing.T) {
	// HTTP 头只能承载 latin-1；账号名含中文时必须转义而不是让响应 500。
	h := newHarness(t, 0, 1)
	acc := h.store.AccountsSnapshot()[0]
	h.store.MutateAccount(acc.ID, func(a *config.Account) bool { a.Name = "中国站-免费账号1"; return true })
	h.hub.Reload()

	resp, _ := h.post("/v1/chat/completions", chatBody("你好"))
	if resp.StatusCode != 200 {
		t.Fatalf("含中文账号名不应导致响应异常，实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Agnes-Hub-Account"); got == "" {
		t.Error("账号名响应头不应丢失")
	}
}

func (m *mockAgnes) lastModelOf(path string) string {
	if v, ok := m.lastModel.Load(path); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
