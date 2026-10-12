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
	"strings"
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
func BuildClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 60,
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
	rateLimitRetryMax := s.RateLimitRetryMax
	if rateLimitRetryMax < 0 {
		rateLimitRetryMax = 0
	}
	// 429 走独立的、更大的重试预算：限流往往只是瞬时拥挤，网关内排队换号重试
	// 通常能消化，让客户端几乎永远拿不到 429，从而不中断任务。非 429 失败
	//（5xx / 凭据被拒）仍受通用 retryMax 约束。
	// 注意：这里绝不动用任何空闲看门狗 / 流式看门狗——那是后期版本导致
	// 「思考中卡死」的元凶，1.0.40 刻意不引入。
	maxAttempt := retryMax
	if rateLimitRetryMax > maxAttempt {
		maxAttempt = rateLimitRetryMax
	}

	exclude := map[string]bool{}
	var totalWait int64
	var last *Result
	rlAttempts := 0 // 已发生的 429 重试次数
	// 记录一次到达，供控制台「到达密度 vs 节拍」观测。
	h.Arrivals.Add()

	for attempt := 0; attempt <= maxAttempt; attempt++ {
		picked, err := h.Pick(opts.SessionKey, opts.PoolClass, opts.Pinned, opts.RequiredModel, exclude)
		if err != nil {
			return nil, err
		}
		account := picked.Account

		wait, err := h.Acquire(ctx, account, opts.PoolClass)
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
		if result.Status == http.StatusTooManyRequests {
			// 429 用专享预算 rateLimitRetryMax（默认 8），而非通用 retryMax。
			rlAttempts++
			if rlAttempts < rateLimitRetryMax {
				sleepBackoff(ctx, s, attempt)
				continue
			}
		} else if attempt < retryMax {
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

	// P0: 请求超时保护。为本次请求创建带超时的子 context，防止上游 hang 住耗尽连接池。
	s := h.Settings()
	timeoutMS := s.RequestTimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = 30000 // 默认 30s
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)

	req, err := http.NewRequestWithContext(reqCtx, opts.Method, url, bytes.NewReader(body))
	if err != nil {
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
		return nil, false, ctx.Err()
	}

	resp, err := client.Do(req)
	if err != nil {
		<-sem
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

	if status == 402 {
		// 402 Payment Required：额度耗尽，不熔断（等待复活即可），但记录错误不重试
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
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
	wrapped := &releaseOnClose{ReadCloser: resp.Body, release: func() {
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

func errorBody(message, etype string) []byte {
	buf, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": etype},
	})
	return buf
}
