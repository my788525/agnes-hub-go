// Package relay 负责向上游转发：排队、带退避的重试、故障转移、流式透传。
//
// 官方约束（docs/TROUBLESHOOTING.md 与 ERROR_CODES.md）：
//   - 需指数退避的状态码：408 / 429 / 500 / 502 / 503 / 504 / 520 / 522 / 524
//   - 不可重试、必须熔断：401 / 403 / 402
//
// 一处比 Python 基线更严格的地方：**区分幂等与非幂等重试**。
// 旧实现对生图 / 视频提交这类非幂等请求也做「退避后重试」，一旦上游其实已经
// 受理（5xx 发生在受理之后），就会重复扣费、重复建任务 —— 这种错误用户看不见，
// 只会在账单和任务列表里发现。现在的规则是：
//   - 429：上游是「在受理前拒绝」，重试安全，照常重试并换账号；
//   - 408/5xx：仅当调用方声明幂等（GET 轮询，或显式允许）才重试，否则立即返回。
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/hub"
	"agneshub/internal/pool"
)

// RetryableStatus 需退避重试的状态码。
var RetryableStatus = map[int]bool{
	408: true, 429: true, 500: true, 502: true, 503: true, 504: true, 520: true, 522: true, 524: true,
}

// AuthFailStatus 不可重试、必须熔断的状态码。
var AuthFailStatus = map[int]bool{401: true, 403: true, 402: true}

// hopByHop 逐跳首部，不能原样透传。
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailers": true,
	"transfer-encoding": true, "upgrade": true, "content-length": true,
	"content-encoding": true, "host": true,
}

// BuildClient 构造上游 HTTP 客户端。
//
// 不设 Client.Timeout（视频任务提交后响应可能很慢），改为由 ctx 控制；
// http.Client 的原生流式能力让我们无需缓冲即可透传 SSE。
// 账号数未知时用默认连接池（向后兼容测试 / 旧调用）。
func BuildClient() *http.Client {
	return BuildClientForAccounts(0)
}

// BuildClientForAccounts 按当前账号数自适应连接池（P2-3）。
//
// 每个账号通常指向独立的上游 host，连接池 idle 上限按账号数线性放宽，
// 避免多账号并发时 idle 连接被回收重建（TLS 握手成本）；单 host 上限
// 保底 60（与历史默认一致），账号数越多总 idle 池越大。
func BuildClientForAccounts(accountCount int) *http.Client {
	if accountCount < 0 {
		accountCount = 0
	}
	// 总 idle 池：历史默认 200，随账号数放宽（每账号 8 路），下限 200。
	maxIdle := 200
	if need := accountCount * 8; need > maxIdle {
		maxIdle = need
	}
	// 单 host idle：保底 60；账号共用同一 base_url 的场景下随账号数再加。
	maxIdlePerHost := 60
	if need := accountCount; need > 60 {
		maxIdlePerHost = 60 + (need-60)/4 // 多账号共 host 时温和放宽
	}
	transport := &http.Transport{
		MaxIdleConns:        maxIdle,
		MaxIdleConnsPerHost: maxIdlePerHost,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// UpstreamRoot 把账号的 base_url 归一化成「站点根」。
//
// 官方同时存在两种写法（带 /v1 与不带），而端点路径本身带 /v1。
// 若直接拼接，写成 /v1 的用户会得到 /v1/v1/chat/completions ——
// 即官方 ERROR_CODES.md 点名过的「第三方工具重复拼接 /v1」404 坑。
func UpstreamRoot(a *config.Account) string {
	base := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	if base == "" {
		base = config.DefaultBaseURL
	}
	if strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	return strings.TrimRight(base, "/")
}

// UpstreamURL 拼接上游完整 URL。
func UpstreamURL(a *config.Account, path string) string {
	root := UpstreamRoot(a)
	if strings.HasPrefix(path, "/") {
		return root + path
	}
	return root + "/" + path
}

// ClientHeaders 构造上游请求头。
func ClientHeaders(a *config.Account, extra map[string]string, anthropic bool) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+a.APIKey)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "baiPiao-hub/1.0")
	if anthropic {
		// 官方文档：/v1/messages 走 Anthropic 兼容通道、以 x-api-key 鉴权。
		// 同时带上两种鉴权头以兼容。
		h.Set("x-api-key", a.APIKey)
		h.Set("anthropic-version", "2023-06-01")
	}
	for k, v := range extra {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		h.Set(k, v)
	}
	return h
}

// SanitizeHeaders 剔除逐跳首部与无法用 latin-1 表达的值。
//
// 后者是硬约束：HTTP 头只能承载 latin-1 字节，而账号名常含中文，
// 直接把中文写进响应头会让整个响应变成 500。
func SanitizeHeaders(in http.Header) http.Header {
	out := http.Header{}
	for k, values := range in {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range values {
			if !isLatin1(v) {
				continue
			}
			out.Add(k, v)
		}
	}
	return out
}

func isLatin1(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return false
		}
	}
	return true
}

// Options 是一次转发的完整入参。
type Options struct {
	SessionKey    string
	PoolClass     string
	Pinned        string
	RequiredModel string
	Method        string
	Path          string
	Body          []byte
	// BodyFor 允许按「最终选中的账号」生成请求体
	// （agnes-auto 需要按该账号声明的清单改写 model 字段）。
	BodyFor      func(a *config.Account) ([]byte, string)
	ExtraHeaders map[string]string
	Anthropic    bool
	// Idempotent 声明该请求可否安全重试。GET 轮询与纯文本对话为 true，
	// 生图 / 视频提交为 false（重试会造成重复扣费与重复建任务）。
	Idempotent bool
	// Stream 标记这是一次流式（SSE）请求。为 true 时，请求超时时钟改由
	// 「空闲看门狗」驱动（只要上游还在持续吐字节就不断开，仅在连续
	// StreamIdleTimeoutMS 无任何新字节时才断开），避免长文本回答被固定的
	// 整请求墙钟超时中途掐断。非流式请求忽略此字段，仍走 RequestTimeoutMS。
	Stream bool
}

// Result 是一次转发的产物。
type Result struct {
	Status    int
	Header    http.Header
	Body      []byte
	Stream    io.ReadCloser // 非 nil 表示上游仍在流式输出，调用方负责消费并关闭
	Account   *config.Account
	ModelUsed string
	WaitMS    int64
	Attempts  int
}

// Close 释放流。
func (r *Result) Close() {
	if r != nil && r.Stream != nil {
		_ = r.Stream.Close()
		r.Stream = nil
	}
}

// ReadAll 读取完整响应体（用于非流式路径）。
func (r *Result) ReadAll() []byte {
	if r == nil {
		return []byte("{}")
	}
	if r.Stream == nil {
		if len(r.Body) == 0 {
			return []byte("{}")
		}
		return r.Body
	}
	raw, _ := io.ReadAll(r.Stream)
	_ = r.Stream.Close()
	r.Stream = nil
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

// Do 带排队 / 重试 / 换账号的转发入口。
func Do(ctx context.Context, h *hub.Hub, client *http.Client, opts Options) (*Result, error) {
	if opts.Method == "" {
		opts.Method = http.MethodPost
	}
	s := h.Settings()
	retryMax := s.RetryMax
	if retryMax < 0 {
		retryMax = 0
	}
	// #25：429（限流/达到最大额度）走一条独立的「网关内长预算」重试路径。
	// 它是「受理前拒绝、重试安全」，网关会换账号 + 重新排队 + 感知 Retry-After 退避地
	// 自动重试，直到成功或耗尽预算，期间不透传 429 给客户端，从而不中断在途任务。
	// RateLimitRetryMax=0 沿用 RetryMax；RateLimitWaitBudgetMS=0 表示不限等待预算。
	rateLimitRetries := s.RateLimitRetryMax
	if rateLimitRetries == 0 {
		rateLimitRetries = retryMax
	}
	rateLimitBudgetMS := int64(s.RateLimitWaitBudgetMS)

	exclude := map[string]bool{}
	var totalWait int64
	var last *Result
	var rl429Used int    // 已用 429 内部重试次数
	var rl429WaitMS int64 // 429 内部重试累计退避等待（ms），受预算约束
	// 记录一次到达，供控制台「到达密度 vs 节拍」观测。
	h.Arrivals.Add()
	// #24：记录一次负载画像（模态 + 是否流式），供接入方自动检测匹配情景。
	h.NoteRequest(opts.PoolClass, opts.Stream)

	for attempt := 0; ; attempt++ {
		picked, err := h.Pick(opts.SessionKey, opts.PoolClass, opts.Pinned, opts.RequiredModel, exclude)
		if err != nil {
			return nil, err
		}
		account := picked.Account

		wait, err := h.Acquire(ctx, account, opts.PoolClass, opts.Stream) // #22：流式=低优先级让位非流式
		if err != nil {
			return nil, err
		}
		totalWait += wait.Milliseconds()

		body := opts.Body
		modelUsed := opts.RequiredModel
		if opts.BodyFor != nil {
			generated, model := opts.BodyFor(account)
			body = generated
			if model != "" {
				modelUsed = model
			}
		}

		h.Metrics.RequestsTotal.Add(1)
		h.TrackInflight(account.ID, 1)
		result, retry, err := attemptOnce(ctx, h, client, account, opts, body, modelUsed, totalWait, attempt+1)
		h.TrackInflight(account.ID, -1)
		if err != nil {
			return nil, err
		}
		last = result

		if !retry {
			if result.Status < 400 {
				h.NoteSuccess(account)
				h.OnSuccess(account, opts.PoolClass)
			}
			h.Metrics.WaitMS.Add(result.WaitMS)
			return result, nil
		}

		// 非幂等请求在「非 429」的失败上必须立即返回：429 是受理前拒绝（重试安全），
		// 而 5xx 可能发生在上游已受理之后，重试会造成重复副作用。
		if !opts.Idempotent && result.Status != http.StatusTooManyRequests {
			h.Metrics.WaitMS.Add(result.WaitMS)
			return result, nil
		}

		exclude[account.ID] = true

		// #25：429（限流/达到最大额度）走「网关内长预算自动重试」——换账号 + 重新排队 +
		// 感知 Retry-After 退避，直到成功或预算耗尽，期间不透传 429 给客户端，
		// 从而不中断客户端在途任务。预算耗尽才把最后一次 429 透传（由 server 补 Retry-After）。
		if result.Status == http.StatusTooManyRequests {
			budgetGone := rateLimitBudgetMS > 0 && rl429WaitMS >= rateLimitBudgetMS
			if rl429Used >= rateLimitRetries || budgetGone {
				h.Metrics.RequestsError.Add(1)
				h.Metrics.WaitMS.Add(result.WaitMS)
				return result, nil
			}
			w := sleep429(ctx, h, s, opts.PoolClass, result.Header, rl429Used)
			rl429WaitMS += w.Milliseconds()
			rl429Used++
			continue
		}

		// 非 429 的可重试失败（408/5xx/401/403）：走普通 retryMax 预算。
		if attempt < retryMax {
			sleepBackoff(ctx, s, attempt)
			continue
		}
		h.Metrics.RequestsError.Add(1)
		h.Metrics.WaitMS.Add(result.WaitMS)
		return result, nil
	}
	if last == nil {
		return nil, errors.New("没有可用账号完成该请求")
	}
	return last, nil
}

// attemptOnce 单次尝试。返回 (结果, 是否应重试, 致命错误)。
func attemptOnce(ctx context.Context, h *hub.Hub, client *http.Client, account *config.Account,
	opts Options, body []byte, modelUsed string, totalWaitMS int64, attemptNo int) (*Result, bool, error) {

	url := UpstreamURL(account, opts.Path)

	// P0: 请求超时保护。
	//   - 非流式：整请求墙钟超时（RequestTimeoutMS），上游 hang 住会耗尽连接池，必须限时；
	//   - 流式：改为「空闲看门狗」——从请求发起起计时，只要 StreamIdleTimeoutMS 内没有
	//     任何新字节（含首字节）就断开；持续吐字则不断开。避免长文本回答被固定 30s
	//     墙钟超时在途掐断（这是挂机场景下「任务不断流」的关键）。
	s := h.Settings()
	timeoutMS := s.RequestTimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = 30000 // 默认 30s
	}
	idleMS := s.StreamIdleTimeoutMS
	if idleMS <= 0 {
		idleMS = 60000 // 默认 60s 空闲才断开
	}

	var (
		reqCtx context.Context
		cancel context.CancelFunc
	)
	if opts.Stream {
		reqCtx, cancel = context.WithCancel(ctx)
	} else {
		reqCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	}

	req, err := http.NewRequestWithContext(reqCtx, opts.Method, url, bytes.NewReader(body))
	if err != nil {
		cancel() // P2：request 构造失败时也取消 context，避免泄漏（vet 提示）。
		return nil, false, err
	}
	for k, values := range ClientHeaders(account, opts.ExtraHeaders, opts.Anthropic) {
		req.Header[k] = values
	}

	// 单账号并发上限（按模态分离，避免视频轮询挤占文本请求）
	sem := h.Semaphore(account, pool.ModalityOfPool(opts.PoolClass))
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		cancel() // 取消请求 context（ctx 已被外部取消，显式释放派生 context 避免泄漏）
		return nil, false, ctx.Err()
	}

	// 流式请求：启动空闲看门狗（覆盖首字节等待）。client.Do 阻塞在响应头阶段，
	// 若上游迟迟不回响应头 / 连接建立后无数据，看门狗会在 idleMS 后 cancel 断开。
	var wd *streamWatchdog
	if opts.Stream {
		wd = newStreamWatchdog(cancel, time.Duration(idleMS)*time.Millisecond)
		wd.arm()
	}

	resp, err := client.Do(req)
	if err != nil {
		<-sem
		if wd != nil {
			wd.stop()
		}
		cancel()
		h.NoteError(account, fmt.Sprintf("%T: %v", err, err))
		h.Metrics.RequestsError.Add(1)
		return &Result{
			Status:  http.StatusBadGateway,
			Header:  http.Header{"Content-Type": []string{"application/json"}},
			Body:    errorBody(fmt.Sprintf("上游连接失败：%v", err), "upstream_error"),
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, true, nil
	}

	status := resp.StatusCode
	if wd != nil {
		// 响应头已到达：重置空闲计时，进入「流式体」阶段继续看门狗。
		wd.arm()
	}

	if status == 402 {
		// 402 Payment Required：额度耗尽，不熔断（等待复活即可），但记录错误不重试
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if wd != nil {
			wd.stop()
		}
		<-sem
		cancel()
		h.NoteError(account, fmt.Sprintf("HTTP %d: %s", status, string(raw)))
		h.Metrics.RequestsError.Add(1)
		return &Result{
			Status: status, Header: SanitizeHeaders(resp.Header), Body: raw,
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, false, nil // 402 不重试
	}

	if AuthFailStatus[status] { // 401 / 403
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if wd != nil {
			wd.stop()
		}
		<-sem
		cancel()
		h.OnAuthFailure(account, fmt.Sprintf("HTTP %d: %s", status, string(raw)))
		return &Result{
			Status: status, Header: SanitizeHeaders(resp.Header), Body: raw,
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, true, nil
	}

	if status == http.StatusTooManyRequests {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if wd != nil {
			wd.stop()
		}
		<-sem
		cancel()
		h.OnRateLimited(account, opts.PoolClass)
		return &Result{
			Status: status, Header: SanitizeHeaders(resp.Header), Body: raw,
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, true, nil
	}

	if RetryableStatus[status] {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if wd != nil {
			wd.stop()
		}
		<-sem
		cancel()
		h.NoteError(account, fmt.Sprintf("HTTP %d: %s", status, string(raw)))
		return &Result{
			Status: status, Header: SanitizeHeaders(resp.Header), Body: raw,
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, true, nil
	}

	// 交给调用方消费：包一层以便在关闭时释放并发信号量并取消 context。
	// 注意：成功路径【绝不】在此提前关闭 body 或取消 reqCtx，否则调用方读取 body 时
	// 连接已关闭 / context 已取消，会读到空体（表现为网关回 {}）。
	// 直到调用方读完 body 触发 Close()，才在此 release 里 cancel()。
	var streamBody io.ReadCloser = resp.Body
	if opts.Stream {
		// 流式：body 上套 idleStreamBody（读到字节就 wd.arm() 重置看门狗），
		// 与前面 client.Do 阶段启动的看门狗衔接，连续 idleMS 无任何字节才断开。
		streamBody = &idleStreamBody{ReadCloser: resp.Body, wd: wd}
	}
	wrapped := &releaseOnClose{ReadCloser: streamBody, release: func() {
		cancel() // body 读取完毕后才取消 context
		select {
		case <-sem:
		default:
		}
	}}
	return &Result{
		Status: status, Header: SanitizeHeaders(resp.Header), Stream: wrapped,
		Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
	}, false, nil
}

type releaseOnClose struct {
	io.ReadCloser
	release func()
	once    bool
}

func (s *releaseOnClose) Close() error {
	err := s.ReadCloser.Close()
	if !s.once {
		s.once = true
		s.release()
	}
	return err
}

// streamWatchdog 是流式请求的「空闲看门狗」：从请求发起起计时，只要 interval 内没有
// 任何字节（包括首字节/响应头迟迟未到）就调用 cancel 断开上游；arm() 在「有字节到达」
// 时重置计时。它是挂机场景下「长回答不断流」的关键——非流式请求用固定墙钟超时，
// 流式请求改用它：上游只要还在正常吐字，整条 SSE 流可持续任意久，不会被 30s 墙钟在途掐断。
type streamWatchdog struct {
	cancel   context.CancelFunc
	interval time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	stopped  bool
}

func newStreamWatchdog(cancel context.CancelFunc, interval time.Duration) *streamWatchdog {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &streamWatchdog{cancel: cancel, interval: interval}
}

// arm 启动或重置一次空闲计时（从此刻起 interval 内无事件则 cancel）。
func (g *streamWatchdog) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return
	}
	if g.timer != nil {
		g.timer.Stop()
	}
	g.timer = time.AfterFunc(g.interval, g.cancel)
}

// stop 永久停止看门狗（请求结束 / 出错时）。幂等。
func (g *streamWatchdog) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return
	}
	g.stopped = true
	if g.timer != nil {
		g.timer.Stop()
	}
}

// idleStreamBody 把「读到字节就重置看门狗」包进 io.ReadCloser，交给 releaseOnClose。
type idleStreamBody struct {
	io.ReadCloser
	wd *streamWatchdog
}

func (s *idleStreamBody) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if n > 0 {
		s.wd.arm()
	}
	return n, err
}

func (s *idleStreamBody) Close() error {
	if s.wd != nil {
		s.wd.stop()
	}
	return s.ReadCloser.Close()
}

func sleepBackoff(ctx context.Context, s config.Settings, attempt int) {
	base := time.Duration(s.RetryBaseBackoffMS) * time.Millisecond
	capDelay := time.Duration(s.RetryMaxBackoffMS) * time.Millisecond
	delay := base * time.Duration(1<<uint(attempt))
	if delay > capDelay {
		delay = capDelay
	}
	jitter := 0.7 + rand.Float64()*0.6
	select {
	case <-time.After(time.Duration(float64(delay) * jitter)):
	case <-ctx.Done():
	}
}

// parseRetryAfter 解析上游 429 的 Retry-After 头（秒数或 HTTP 日期），返回建议等待秒数；
// 未带该头（免费号 agnes 常不带）时返回 0，由调用方用池投影等待兜底。
func parseRetryAfter(header http.Header) float64 {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if sec, err := strconv.ParseFloat(raw, 64); err == nil {
		if sec < 0 {
			return 0
		}
		return sec
	}
	if t, err := http.ParseTime(raw); err == nil {
		sec := t.Sub(time.Now()).Seconds()
		if sec < 0 {
			sec = 0
		}
		return sec
	}
	return 0
}

// sleep429 是一次 429 内部重试前的等待：优先用上游 Retry-After（钳到 [0,120]s），
// 否则用该池当前投影等待（下一空闲槽）+ 一轮指数退避兜底；全程可被 ctx 取消。
// 返回实际等待的时长（用于累计 429 等待预算）。
func sleep429(ctx context.Context, h *hub.Hub, s config.Settings, poolClass string, header http.Header, attempt int) time.Duration {
	waitSec := parseRetryAfter(header)
	if waitSec <= 0 {
		// 免费号常不带 Retry-After：用池投影等待（下一空闲槽）作为下限，避免拍满节拍。
		projMS := h.PoolMaxProjectedWaitMS(poolClass)
		if float64(projMS)/1000.0 > waitSec {
			waitSec = float64(projMS) / 1000.0
		}
		// 再叠一轮温和指数退避（base * 2^attempt，钳到 cap），避免全员同节拍重试。
		exp := int(attempt)
		mult := 1 << uint(exp)
		backoffMS := float64(s.RetryBaseBackoffMS) * float64(mult)
		if capMS := float64(s.RetryMaxBackoffMS); backoffMS > capMS {
			backoffMS = capMS
		}
		waitSec += backoffMS / 1000.0
	}
	if waitSec > 120 {
		waitSec = 120 // 单次等待上限，防止 Retry-After 异常大值挂死
	}
	jitter := 0.85 + rand.Float64()*0.3
	d := time.Duration(float64(waitSec) * jitter * 1000) * time.Millisecond
	if d <= 0 {
		return 0
	}
	start := time.Now()
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
	return time.Since(start)
}

func errorBody(message, etype string) []byte {
	buf, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": etype},
	})
	return buf
}
