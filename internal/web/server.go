// Package web 提供对外的 OpenAI 兼容接口与后台控制台。
//
// 对外接口的设计要点
//  1. 客户端永远拿不到上游的 429：请求在服务端排队等槽位，而不是被拒绝。
//  2. 流式请求走「快路径 → 心跳路径」两级：能立刻拿到槽位就直接开上游、
//     返回真实状态码；要等更久时立刻回 200 + SSE 注释帧，边等边喂心跳，
//     避免客户端首字节超时把任务掐断。
//  3. agnes-auto 的三种模态各有归宿：text 走透传、image/video 走改写 + 回译。
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/hub"
	"agneshub/internal/intent"
	"agneshub/internal/pool"
	"agneshub/internal/relay"
	"agneshub/internal/updater"
)

// Server 组装全部路由。
type Server struct {
	Store   *config.Store
	Hub     *hub.Hub
	Client  *http.Client
	mux     *http.ServeMux
	rules   intent.Rules
	Updater *updater.Updater
	// Version 由 main 注入，供 /healthz 与自更新接口显示，避免写死在多处。
	Version string
	// intentCache 缓存 auto 路径的意图判定（body 哈希 + 规则指纹 → 结论）。
	// 仅 handleTextish 命中 auto 模型时读写；显式模型名、媒体端点均不走。
	intentCache *intent.Cache
}

// New 构造服务。
func New(store *config.Store, h *hub.Hub, client *http.Client) *Server {
	s := &Server{Store: store, Hub: h, Client: client, mux: http.NewServeMux(), rules: intent.DefaultRules(),
		intentCache: intent.NewCache(0, 0)}
	s.routes()
	return s
}

// SetClient 替换上游客户端（测试指向本地模拟上游用）。
func (s *Server) SetClient(c *http.Client) { s.Client = c }

// SetUpdater 注入自更新器（启动后调用）。
func (s *Server) SetUpdater(u *updater.Updater) { s.Updater = u }

// Updater 返回当前 updater 实例（测试用）。
func (s *Server) UpdaterInstance() *updater.Updater { return s.Updater }

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.handleHealth)
	m.HandleFunc("GET /v1/models", s.handleModels)
	m.HandleFunc("GET /metrics", s.handleMetrics)

	m.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.handleTextish(w, r, "/v1/chat/completions")
	})
	m.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		s.handleTextish(w, r, "/v1/responses")
	})
	m.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		s.handleTextish(w, r, "/v1/messages")
	})
	m.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		s.handleMedia(w, r, "/v1/images/generations", intent.Image)
	})
	m.HandleFunc("POST /v1/videos", func(w http.ResponseWriter, r *http.Request) {
		s.handleMedia(w, r, "/v1/videos", intent.Video)
	})
	m.HandleFunc("GET /v1/videos/{job_id}", s.handleVideoPoll)
	m.HandleFunc("GET /agnesapi", s.handleAgnesAPI)

	// 干跑判定：不消耗任何配额，便于联调与排障（也能被控制台调用）
	m.HandleFunc("POST /v1/intent/preview", s.handleIntentPreview)

	s.consoleRoutes()
}

// ---------------------------------------------------------------------------
// 通用工具
// ---------------------------------------------------------------------------

type apiError struct {
	Status  int
	Message string
	Type    string
	Code    string
	Headers map[string]string
}

func (e *apiError) Error() string { return e.Message }

func badRequest(msg string) *apiError {
	return &apiError{Status: 400, Message: msg, Type: "invalid_request_error"}
}

func writeErr(w http.ResponseWriter, e *apiError) {
	payload := map[string]any{"error": map[string]any{"message": e.Message, "type": e.Type}}
	if e.Code != "" {
		payload["error"].(map[string]any)["code"] = e.Code
	}
	writeJSON(w, e.Status, payload, e.Headers)
}

func writeJSON(w http.ResponseWriter, status int, payload any, headers map[string]string) {
	buf, err := json.Marshal(payload)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"响应序列化失败","type":"internal_error"}}`))
		return
	}
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// downstreamKey 校验并返回下游密钥。
func (s *Server) downstreamKey(r *http.Request) (*config.DownstreamKey, *apiError) {
	auth := r.Header.Get("Authorization")
	token := ""
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		token = strings.TrimSpace(auth[7:])
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if token == "" {
		return nil, &apiError{Status: 401, Type: "invalid_request_error",
			Message: "缺少 API Key（请用 Authorization: Bearer <key>）"}
	}
	item := s.Store.KeyByValue(token)
	if item == nil || !item.Enabled {
		return nil, &apiError{Status: 401, Type: "invalid_request_error", Message: "API Key 无效或已停用"}
	}
	return item, nil
}

func readBody(r *http.Request) (map[string]any, []byte, *apiError) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return nil, nil, badRequest("读取请求体失败：" + err.Error())
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, raw, nil
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&body); err != nil {
		return nil, nil, badRequest("请求体不是合法 JSON")
	}
	return body, raw, nil
}

func (s *Server) gate(item *config.DownstreamKey, poolClass string) *apiError {
	if msg := s.Store.QuotaExceeded(item); msg != "" {
		return &apiError{Status: 429, Message: msg, Type: "rate_limit_error", Code: "quota_exceeded"}
	}
	if !config.KeyAllowsClass(item, poolClass) {
		return &apiError{Status: 403, Type: "permission_error",
			Message: fmt.Sprintf("该密钥不允许调用 %s 池", poolClass)}
	}
	return nil
}

func headerMap(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.Header {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

// autoIntentConfig 从全局设置导出判定配置。
func autoIntentConfig(s config.Settings) intent.Config {
	return intent.Config{
		ContentScan:      s.AutoIntent.ContentScan,
		MinConfidence:    s.AutoIntent.MinConfidence,
		DefaultImageSize: s.AutoIntent.DefaultImageSize,
		ImageInputField:  s.AutoIntent.ImageInputField,
		VideoInputField:  s.AutoIntent.VideoInputField,
		VideoWaitSec:     s.AutoIntent.VideoWaitSec,
		PreferredModels:  s.AutoIntent.PreferredModels,
	}
}

// autoModelUnion 汇总所有可用账号在指定模态下声明的模型（跨账号并集）。
func (s *Server) autoModelUnion(settings config.Settings, modality string) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range s.Store.AccountsSnapshot() {
		if !a.Enabled || strings.TrimSpace(a.APIKey) == "" {
			continue
		}
		manifest := pool.ManifestOf(a, settings)
		if len(a.ClassesEnabled) > 0 {
			capable := false
			for _, cls := range config.PoolClasses {
				if pool.ModalityOfPool(cls) != modality {
					continue
				}
				for _, c := range a.ClassesEnabled {
					if c == cls || c == "*" {
						capable = true
					}
				}
			}
			if !capable {
				continue
			}
		}
		for _, m := range manifest.For(modality) {
			m = strings.TrimSpace(m)
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// resolveAutoModel 为 auto 请求选定具体模型（跨账号并集 + 偏好排序）。
func (s *Server) resolveAutoModel(settings config.Settings, modality string) (string, *apiError) {
	union := s.autoModelUnion(settings, modality)
	if len(union) == 0 {
		return "", &apiError{Status: 503, Type: "upstream_error", Code: "no_capacity",
			Message: fmt.Sprintf("没有任何账号声明支持 %s 模态，请到控制台为账号填写模型清单", modality)}
	}
	pref := intent.PreferredModels(autoIntentConfig(settings), modality)
	if model := intent.ChooseModel(union, pref); model != "" {
		return model, nil
	}
	if fb := pool.FallbackModel[modality]; fb != "" {
		return fb, nil
	}
	return union[0], nil
}

// bodyForAccount 生成「按最终选中账号」改写的请求体，并返回实际使用的模型。
//
// 这是异构账号池能真正协力的关键：账号 A 有 image-2.5，账号 B 只有 image-2.1 时，
// 每个账号都拿到自己清单里最合适的那个模型，而不是被一把全局模型名卡住。
//
// P3 增强：如果下游传入的模型名不在本账号 Manifest 中，则使用账号级别的 DefaultModel
// 作为兜底，而非退回全局选择。这样下游可以用任意别名映射到各账号的默认模型。
func (s *Server) bodyForAccount(base map[string]any, poolClass, modality string, settings config.Settings) func(*config.Account) ([]byte, string) {
	pref := intent.PreferredModels(autoIntentConfig(settings), modality)
	return func(a *config.Account) ([]byte, string) {
		declared := s.Hub.DeclaredModels(a, poolClass)
		model := intent.ChooseModel(declared, pref)
		if model == "" {
			if fb := pool.FallbackModel[modality]; fb != "" {
				model = fb
			}
		}
		// 如果选出的模型不在本账号 Manifest 中，用账号级默认模型兜底
		if model != "" && !config.StringInSlice(model, declared) {
			if a.DefaultModel != "" && config.StringInSlice(a.DefaultModel, declared) {
				model = a.DefaultModel
			}
		}
		clone := make(map[string]any, len(base))
		for k, v := range base {
			clone[k] = v
		}
		if model != "" {
			clone["model"] = model
		}
		buf, _ := json.Marshal(clone)
		return buf, model
	}
}

// ---------------------------------------------------------------------------
// 基础端点
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	enabled := 0
	for _, a := range s.Store.AccountsSnapshot() {
		if a.Enabled {
			enabled++
		}
	}
	ver := s.Version
	if ver == "" {
		ver = "dev"
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "accounts": enabled,
		"uptime_sec": int(time.Since(s.Hub.Metrics.StartedAt).Seconds()),
		"version":    ver,
	}, nil)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, e := s.downstreamKey(r); e != nil {
		writeErr(w, e)
		return
	}
	settings := s.Store.SettingsSnapshot()
	created := time.Now().Unix()
	data := []map[string]any{{
		"id": settings.AutoModelName, "object": "model", "created": created,
		"owned_by": "agnes-hub",
		"metadata": map[string]any{
			"pool_class":  "auto",
			"description": "统一入口：网关自动判定文本/生图/生视频，无需分别指定模型",
		},
	}}
	declared := map[string]bool{}
	for _, a := range s.Store.AccountsSnapshot() {
		for _, m := range pool.ManifestOf(a, settings).Text {
			declared[m] = true
		}
		for _, m := range pool.ManifestOf(a, settings).Image {
			declared[m] = true
		}
		for _, m := range pool.ManifestOf(a, settings).Video {
			declared[m] = true
		}
	}
	for _, name := range pool.KnownModelNames(settings.ModelAliases) {
		cls := pool.Classify(name, nil, settings.ModelAliases, settings.DefaultImageTier)
		data = append(data, map[string]any{
			"id": name, "object": "model", "created": created, "owned_by": "agnes-hub",
			"metadata": map[string]any{
				"pool_class": cls,
				"modality":   pool.ModalityOfModel(name, settings.ModelAliases),
				"declared":   declared[name],
			},
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data}, nil)
}

// ---------------------------------------------------------------------------
// Prometheus metrics
// ---------------------------------------------------------------------------

// handleMetrics 返回 Prometheus 格式的网关指标。
//
// 设计要点：
//   - agenes-hub 本身不是长期运行的监控目标，指标用于人工排障与短期观察。
//     因此用原生文本格式而非 JSON，Prometheus 可直接 scrap。
//   - 只暴露最重要的计数器和 gauge：请求量、成功/错误、队列、熔断状态、
//     每账号的冷却剩余时间、到达密度等。
//   - 不使用 prometheus.Client 依赖（保持零外部依赖），自己拼行。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	h := s.Hub
	m := h.Metrics
	startedAt := h.Metrics.StartedAt.Unix()
	settings := h.Settings()

	now := time.Now()
	// 每账号 gauge：冷却剩余秒数、连续失败数
	type acctGauge struct {
		id                   string
		name                 string
		enabled              bool
		penaltyRemainingSec  float64
		consecutiveFailures  int
	}
	var gauges []acctGauge
	for _, a := range s.Store.AccountsSnapshot() {
		gauges = append(gauges, acctGauge{
			id:                    a.ID,
			name:                  a.Name,
			enabled:               a.Enabled,
			penaltyRemainingSec:   h.PenaltyRemaining(a.ID).Seconds(),
			consecutiveFailures:   a.ConsecutiveFailures,
		})
	}

	// 计算总 RPM（所有账号 text 池之和）
	totalTextRPM := 0.0
	for _, a := range s.Store.AccountsSnapshot() {
		if !a.Enabled || strings.TrimSpace(a.APIKey) == "" {
			continue
		}
		totalTextRPM += h.EffectiveRPM(a, "text")
	}

	var sb strings.Builder
	sb.WriteString("# HELP baiPiao_hub_requests_total Total requests served\n")
	sb.WriteString("# TYPE baiPiao_hub_requests_total counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_requests_total %d\n", m.RequestsTotal.Load()))

	sb.WriteString("# HELP baiPiao_hub_requests_ok Successful requests\n")
	sb.WriteString("# TYPE baiPiao_hub_requests_ok counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_requests_ok %d\n", m.RequestsOK.Load()))

	sb.WriteString("# HELP baiPiao_hub_requests_error Failed requests\n")
	sb.WriteString("# TYPE baiPiao_hub_requests_error counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_requests_error %d\n", m.RequestsError.Load()))

	sb.WriteString("# HELP baiPiao_hub_upstream_429 Upstream 429s received\n")
	sb.WriteString("# TYPE baiPiao_hub_upstream_429 counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_upstream_429 %d\n", m.Upstream429.Load()))

	sb.WriteString("# HELP baiPiao_hub_queued_total Requests that had to wait in queue\n")
	sb.WriteString("# TYPE baiPiao_hub_queued_total counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_queued_total %d\n", m.QueuedTotal.Load()))

	sb.WriteString("# HELP baiPiao_hub_queue_timeout Requests that timed out waiting\n")
	sb.WriteString("# TYPE baiPiao_hub_queue_timeout counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_queue_timeout %d\n", m.QueueTimeout.Load()))

	sb.WriteString("# HELP baiPiao_hub_queue_overflow Requests dropped because queue full\n")
	sb.WriteString("# TYPE baiPiao_hub_queue_overflow counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_queue_overflow %d\n", m.QueueOverflow.Load()))

	sb.WriteString("# HELP baiPiao_hub_spillovers Soft-affinity spillovers\n")
	sb.WriteString("# TYPE baiPiao_hub_spillovers counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_spillovers %d\n", m.Spillovers.Load()))

	sb.WriteString("# HELP baiPiao_hub_breaker_opened Breakers opened (401/403/402)\n")
	sb.WriteString("# TYPE baiPiao_hub_breaker_opened counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_breaker_opened %d\n", m.BreakerOpened.Load()))

	sb.WriteString("# HELP baiPiao_hub_breaker_revived Breakers revived after cooldown\n")
	sb.WriteString("# TYPE baiPiao_hub_breaker_revived counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_breaker_revived %d\n", m.BreakerRevived.Load()))

	sb.WriteString("# HELP baiPiao_hub_wait_ms_total Total wait time in milliseconds\n")
	sb.WriteString("# TYPE baiPiao_hub_wait_ms_total counter\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_wait_ms_total %d\n", m.WaitMS.Load()))

	sb.WriteString("# HELP baiPiao_hub_uptime_seconds Seconds since startup\n")
	sb.WriteString("# TYPE baiPiao_hub_uptime_seconds gauge\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_uptime_seconds %.0f\n", now.Sub(h.Metrics.StartedAt).Seconds()))

	sb.WriteString("# HELP baiPiao_hub_started_at_seconds Unix timestamp when hub started\n")
	sb.WriteString("# TYPE baiPiao_hub_started_at_seconds gauge\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_started_at_seconds %.0f\n", float64(startedAt)))

	sb.WriteString("# HELP baiPiao_hub_total_text_rpm Current effective text RPM sum across all accounts\n")
	sb.WriteString("# TYPE baiPiao_hub_total_text_rpm gauge\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_total_text_rpm %.2f\n", totalTextRPM))

	sb.WriteString("# HELP baiPiao_hub_account_penalty_remaining_sec Seconds until account unblocked\n")
	sb.WriteString("# TYPE baiPiao_hub_account_penalty_remaining_sec gauge\n")
	sb.WriteString("# LABELS account_id,account_name,enabled\n")
	for _, g := range gauges {
		enabledLabel := "0"
		if g.enabled {
			enabledLabel = "1"
		}
		sb.WriteString(fmt.Sprintf("baiPiao_hub_account_penalty_remaining_sec{account_id=\"%s\",account_name=\"%s\",enabled=\"%s\"} %.2f\n",
			g.id, g.name, enabledLabel, g.penaltyRemainingSec))
	}

	sb.WriteString("# HELP baiPiao_hub_account_consecutive_failures Consecutive error count per account\n")
	sb.WriteString("# TYPE baiPiao_hub_account_consecutive_failures gauge\n")
	sb.WriteString("# LABELS account_id,account_name\n")
	for _, g := range gauges {
		sb.WriteString(fmt.Sprintf("baiPiao_hub_account_consecutive_failures{account_id=\"%s\",account_name=\"%s\"} %d\n",
			g.id, g.name, g.consecutiveFailures))
	}

	sb.WriteString("# HELP baiPiao_hub_request_timeout_ms Configured request timeout\n")
	sb.WriteString("# TYPE baiPiao_hub_request_timeout_ms gauge\n")
	sb.WriteString(fmt.Sprintf("baiPiao_hub_request_timeout_ms %d\n", settings.RequestTimeoutMS))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(sb.String()))
}

// ---------------------------------------------------------------------------
// 文本类端点（chat / responses / messages）—— 含 agnes-auto 判定
// ---------------------------------------------------------------------------

// decideCached 走 LRU 缓存的意图判定（仅 handleTextish 的 auto 路径用）。
//
// 命中条件（全部满足才用缓存）：
//   - 规则集未变（指纹在 Key() 里，变了自动 miss，无需手动清）
//   - body + requestedModel 哈希命中
//   - 未超过 TTL（10 分钟）
//
// 命中时 Source 标记为 "cache"，Reason 写 "命中意图判定缓存"，
// 下游观测端能直接区分「缓存判定」与「现场判定」。
func (s *Server) decideCached(path string, body map[string]any, requested string, settings config.Settings) intent.Result {
	icfg := autoIntentConfig(settings)
	key := s.intentCache.Key(body, s.rules, icfg.MinConfidence, icfg.ContentScan, requested)
	if cached := s.intentCache.Get(key); cached != nil {
		out := *cached
		if out.Source != "cache" {
			out.Source = "cache"
			out.Reason = "命中意图判定缓存"
		}
		return out
	}
	res := intent.Decide(path, body, requested, icfg, s.rules,
		settings.ModelAliases, pool.ModalityOfModel, "")
	s.intentCache.Put(key, res)
	return res
}

func (s *Server) handleTextish(w http.ResponseWriter, r *http.Request, path string) {
	item, e := s.downstreamKey(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	settings := s.Store.SettingsSnapshot()
	requested := strings.TrimSpace(asStr(body["model"]))
	wantsStream := truthy(body["stream"])

	var decision intent.Result
	if intent.IsAutoModel(requested) || requested == "" {
		decision = s.decideCached(path, body, requested, settings)
	} else {
		decision = intent.Result{
			Modality:       pool.ModalityOfModel(requested, settings.ModelAliases),
			Source:         "model",
			Score:          1,
			Reason:         fmt.Sprintf("模型名 %s 已明确模态", requested),
			ModelRequested: requested,
			Prompt:         intent.ExtractPrompt(body),
		}
		if decision.Modality == "" {
			decision.Modality = intent.Text
		}
	}

	switch decision.Modality {
	case intent.Image:
		s.serveAutoImage(w, r, item, body, decision, settings, true, wantsStream)
		return
	case intent.Video:
		s.serveAutoVideo(w, r, item, body, decision, settings, true, wantsStream)
		return
	}

	// ---- 文本 ----
	poolClass := "text"
	if !intent.IsAutoModel(requested) {
		poolClass = pool.Classify(requested, body, settings.ModelAliases, settings.DefaultImageTier)
	}
	if e := s.gate(item, poolClass); e != nil {
		writeErr(w, e)
		return
	}

	var requiredModel string
	bodyFor := func(a *config.Account) ([]byte, string) { return nil, "" }
	if intent.IsAutoModel(requested) || requested == "" {
		model, e := s.resolveAutoModel(settings, intent.Text)
		if e != nil {
			writeErr(w, e)
			return
		}
		requiredModel = model
		decision.ModelUsed = model
		// 每个账号用它自己清单里的最佳模型
		bodyFor = s.bodyForAccount(body, poolClass, intent.Text, settings)
	} else {
		resolved := pool.ResolveModel(requested, settings.ModelAliases)
		clone := cloneBody(body)
		clone["model"] = resolved
		buf, _ := json.Marshal(clone)
		requiredModel = resolved
		decision.ModelUsed = resolved
		bodyFor = func(*config.Account) ([]byte, string) { return buf, resolved }
	}

	sessionKey := s.Hub.SessionKey(headerMap(r), item.Key)
	opts := relay.Options{
		SessionKey: sessionKey, PoolClass: poolClass, Pinned: item.PinnedAccount,
		RequiredModel: requiredModel, Method: http.MethodPost, Path: path,
		BodyFor: bodyFor, Anthropic: path == "/v1/messages",
		Idempotent: true, // 文本对话可安全重试
	}
	s.logUsage(item, decision, poolClass, r, path, wantsStream)
	s.proxy(w, r, item, opts, decision, wantsStream)
}

// ---------------------------------------------------------------------------
// 媒体端点（images / videos）—— 显式端点即意图
// ---------------------------------------------------------------------------

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request, path, modality string) {
	item, e := s.downstreamKey(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	settings := s.Store.SettingsSnapshot()
	requested := strings.TrimSpace(asStr(body["model"]))
	wantsStream := truthy(body["stream"])

	decision := intent.Decide(path, body, firstNonEmpty(requested, settings.AutoModelName),
		autoIntentConfig(settings), s.rules, settings.ModelAliases, pool.ModalityOfModel, modality)

	// 显式端点即意图：/v1/videos 一律走视频提交路径，/v1/images/* 一律走生图路径。
	//
	// 响应形态按调用方形态决定，两种都要支持：
	//   - 官方形态（裸 prompt，无 messages 且非流式）→ 原样返回上游结构，
	//     保证直接用官方 SDK / curl 的程序化客户端不受影响；
	//   - chat 形态（带 messages，或 stream=true）→ 回译成 chat 响应，
	//     让只会说 chat 的客户端零改动也能生图 / 生视频。
	_, hasMessages := body["messages"]
	chatShape := hasMessages || wantsStream
	if modality == intent.Video {
		s.serveAutoVideo(w, r, item, body, decision, settings, chatShape, wantsStream)
		return
	}
	s.serveAutoImage(w, r, item, body, decision, settings, chatShape, wantsStream)
}

// pickMediaModel 选定媒体模态使用的模型。
//
// 客户端**显式**给了具体模型名时必须尊重，绝不能被自动偏好覆盖 ——
// 否则「我明明指定了 image-2.1，系统却用 2.5 生图」会变成无法解释的行为。
func (s *Server) pickMediaModel(settings config.Settings, decision intent.Result, modality string) (string, *apiError) {
	requested := strings.TrimSpace(decision.ModelRequested)
	if requested != "" && !intent.IsAutoModel(requested) {
		return pool.ResolveModel(requested, settings.ModelAliases), nil
	}
	return s.resolveAutoModel(settings, modality)
}

// mediaBodyFor 生成按账号改写的请求体。显式模型时不做任何替换。
func (s *Server) mediaBodyFor(base map[string]any, settings config.Settings, decision intent.Result,
	poolClass, modality, fallbackModel string) func(*config.Account) ([]byte, string) {

	explicit := strings.TrimSpace(decision.ModelRequested) != "" && !intent.IsAutoModel(decision.ModelRequested)
	if explicit {
		model := pool.ResolveModel(decision.ModelRequested, settings.ModelAliases)
		clone := make(map[string]any, len(base))
		for k, v := range base {
			clone[k] = v
		}
		clone["model"] = model
		buf, _ := json.Marshal(clone)
		return func(*config.Account) ([]byte, string) { return buf, model }
	}
	pref := intent.PreferredModels(autoIntentConfig(settings), modality)
	return func(a *config.Account) ([]byte, string) {
		declared := s.Hub.DeclaredModels(a, poolClass)
		chosen := intent.ChooseModel(declared, pref)
		if chosen == "" {
			chosen = fallbackModel
		}
		clone := make(map[string]any, len(base))
		for k, v := range base {
			clone[k] = v
		}
		if chosen != "" {
			clone["model"] = chosen
		}
		buf, _ := json.Marshal(clone)
		return buf, chosen
	}
}

// ---------------------------------------------------------------------------
// agnes-auto → 生图
// ---------------------------------------------------------------------------

func (s *Server) serveAutoImage(w http.ResponseWriter, r *http.Request, item *config.DownstreamKey,
	body map[string]any, decision intent.Result, settings config.Settings, chatShape, stream bool) {

	// 先按分辨率定池，再选模型
	preliminary := pool.PoolForModality(intent.Image, body, settings.DefaultImageTier)
	if msg := s.Store.QuotaExceeded(item); msg != "" {
		writeErr(w, &apiError{Status: 429, Message: msg, Type: "rate_limit_error", Code: "quota_exceeded"})
		return
	}
	if !config.KeyAllowsClass(item, preliminary) {
		writeErr(w, &apiError{Status: 403, Type: "permission_error",
			Message: fmt.Sprintf("该密钥不允许调用 %s 池", preliminary)})
		return
	}

	model, e := s.pickMediaModel(settings, decision, intent.Image)
	if e != nil {
		writeErr(w, e)
		return
	}
	decision.ModelUsed = model
	cfg := autoIntentConfig(settings)
	upstreamBody := intent.BuildImageBody(decision, model, cfg, body)
	decision.DroppedFields = intent.DroppedFields(body, intent.ImageFieldWhitelist)

	poolClass := pool.PoolForModality(intent.Image, upstreamBody, settings.DefaultImageTier)

	sessionKey := s.Hub.SessionKey(headerMap(r), item.Key)
	bodyFor := s.mediaBodyFor(upstreamBody, settings, decision, poolClass, intent.Image, model)

	result, err := relay.Do(r.Context(), s.Hub, s.Client, relay.Options{
		SessionKey: sessionKey, PoolClass: poolClass, Pinned: item.PinnedAccount,
		RequiredModel: model, Method: http.MethodPost, Path: "/v1/images/generations",
		BodyFor: bodyFor, Idempotent: false, // 生图非幂等：不重试，避免重复扣费
	})
	if err != nil {
		writeErr(w, relayError(err))
		return
	}
	raw := result.ReadAll()
	decision.ModelUsed = result.ModelUsed
	s.Store.ChargeKey(item.Key, extractUsage(raw))
	s.logUsageFull(item, decision, poolClass, "/v1/images/generations", chatShape, result.Account, result.WaitMS, result.Attempts, mustJSON(body))

	// 记录图片生成结果（便于控制台查看历史）
	requestID := config.NewID("img")
	prompt := decision.Prompt.Text
	if prompt == "" {
		if rawMsg, ok := body["prompt"].(string); ok {
			prompt = rawMsg
		}
	}
	size := asStr(body["size"])
	if size == "" {
		if w, ok := body["width"]; ok {
			if h, ok2 := body["height"]; ok2 {
				size = fmt.Sprintf("%dx%d", int(intToFloat64(w)), int(intToFloat64(h)))
			}
		}
	}
	imgJob := &config.ImageJob{
		JobID:     requestID,
		Model:     model,
		AccountID: result.Account.ID,
		Prompt:    prompt,
		Size:      size,
		Status:    "completed",
		CreatedAt: float64(time.Now().UnixNano()) / 1e9,
		RequestID: requestID,
	}
	// 解析响应中的 URL
	data := decodeMap(raw)
	items := mapList(data["data"])
	for _, item := range items {
		if url, ok := item["url"].(string); ok && url != "" {
			imgJob.URL = url
			break
		}
		if b64, ok := item["b64_json"].(string); ok && b64 != "" {
			imgJob.URL = "data:image/png;base64," + b64
			break
		}
	}
	s.Store.PutImageJob(imgJob)

	extra := decision.Headers()
	extra["X-Agnes-Hub-Account"] = safeHeader(result.Account.Name)
	extra["X-Agnes-Hub-Account-Id"] = result.Account.ID
	extra["X-Agnes-Hub-Wait-Ms"] = strconv.FormatInt(result.WaitMS, 10)
	extra["X-Agnes-Hub-Attempts"] = strconv.Itoa(result.Attempts)

	if result.Status >= 400 {
		writeJSON(w, result.Status, decodeOrRaw(raw), extra)
		return
	}

	// 官方形态：原样返回上游结构，程序化客户端零影响
	if !chatShape {
		writeJSON(w, result.Status, decodeOrRaw(raw), extra)
		return
	}

	content, images := intent.ImageContent(items, decision.Prompt.Text)
	if len(decision.DroppedFields) > 0 {
		content += "\n\n> 说明：字段 " + strings.Join(decision.DroppedFields, ", ") + " 未被生图端点接受，已忽略。"
	}
	envelope := intent.ChatEnvelope(decision, result.ModelUsed, content,
		map[string]any{"data": data["data"], "images": images})
	if stream {
		s.writeSyntheticSSE(w, envelope, extra)
		return
	}
	writeJSON(w, 200, envelope, extra)
}

// ---------------------------------------------------------------------------
// agnes-auto → 生视频
// ---------------------------------------------------------------------------

func (s *Server) serveAutoVideo(w http.ResponseWriter, r *http.Request, item *config.DownstreamKey,
	body map[string]any, decision intent.Result, settings config.Settings, chatShape, stream bool) {

	poolClass := "video"
	if msg := s.Store.QuotaExceeded(item); msg != "" {
		writeErr(w, &apiError{Status: 429, Message: msg, Type: "rate_limit_error", Code: "quota_exceeded"})
		return
	}
	if !config.KeyAllowsClass(item, poolClass) {
		writeErr(w, &apiError{Status: 403, Type: "permission_error", Message: "该密钥不允许调用 video 池"})
		return
	}
	model, e := s.pickMediaModel(settings, decision, intent.Video)
	if e != nil {
		writeErr(w, e)
		return
	}
	cfg := autoIntentConfig(settings)
	upstreamBody := intent.BuildVideoBody(decision, model, cfg, body)
	decision.DroppedFields = intent.DroppedFields(body, intent.VideoFieldWhitelist)

	sessionKey := s.Hub.SessionKey(headerMap(r), item.Key)
	bodyFor := s.mediaBodyFor(upstreamBody, settings, decision, poolClass, intent.Video, model)

	result, err := relay.Do(r.Context(), s.Hub, s.Client, relay.Options{
		SessionKey: sessionKey, PoolClass: poolClass, Pinned: item.PinnedAccount,
		RequiredModel: model, Method: http.MethodPost, Path: "/v1/videos",
		BodyFor: bodyFor, Idempotent: false, // 视频提交非幂等：重试会重复建任务
	})
	if err != nil {
		writeErr(w, relayError(err))
		return
	}
	raw := result.ReadAll()
	decision.ModelUsed = result.ModelUsed
	s.logUsageFull(item, decision, poolClass, "/v1/videos", chatShape, result.Account, result.WaitMS, result.Attempts, mustJSON(body))

	extra := decision.Headers()
	extra["X-Agnes-Hub-Account"] = safeHeader(result.Account.Name)
	extra["X-Agnes-Hub-Account-Id"] = result.Account.ID
	extra["X-Agnes-Hub-Wait-Ms"] = strconv.FormatInt(result.WaitMS, 10)

	if result.Status >= 400 {
		writeJSON(w, result.Status, decodeOrRaw(raw), extra)
		return
	}

	payload := decodeMap(raw)
	videoID := extractVideoID(payload)
	job := &config.VideoJob{
		JobID:     config.NewID("job"),
		VideoID:   videoID,
		Model:     result.ModelUsed,
		AccountID: result.Account.ID,
		Status:    "submitted",
		CreatedAt: float64(time.Now().UnixNano()) / 1e9,
	}
	s.Store.PutJob(job)
	s.Store.ChargeKey(item.Key, extractUsage(raw))
	extra["X-Agnes-Hub-Job-Id"] = job.JobID

	jobInfo := map[string]any{
		"job_id": job.JobID, "video_id": videoID,
		"poll_url": "/v1/videos/" + job.JobID,
	}

	// 官方形态：补上 job_id/poll_url 后原样返回上游结构，程序化客户端零影响。
	if !chatShape {
		payload["job_id"] = job.JobID
		if videoID != "" {
			payload["video_id"] = videoID
		}
		payload["poll_url"] = jobInfo["poll_url"]
		writeJSON(w, result.Status, payload, extra)
		return
	}

	// 可选：阻塞等待视频完成（默认 0 = 不等待）。视频是异步的，
	// 对「用 chat 客户端发一句话要视频」的用户，等待能让体验完整；
	// 但等待会占住一条连接，所以默认关闭、由控制台决定。
	videoURL := ""
	if cfg.VideoWaitSec > 0 && videoID != "" {
		videoURL = s.waitForVideo(r.Context(), result.Account, job, cfg.VideoWaitSec)
	}

	envelope := intent.ChatEnvelope(decision, result.ModelUsed,
		intent.VideoContent(result.ModelUsed, jobInfo, videoURL),
		map[string]any{"job_id": job.JobID, "video_id": videoID, "poll_url": jobInfo["poll_url"]})
	// 同时在 agnes_hub 元信息里暴露任务信息，便于程序化客户端一处取全
	if meta, ok := envelope["agnes_hub"].(map[string]any); ok {
		meta["video"] = jobInfo
		meta["job_id"] = job.JobID
		meta["video_id"] = videoID
	}
	if stream {
		s.writeSyntheticSSE(w, envelope, extra)
		return
	}
	writeJSON(w, 200, envelope, extra)
}

// waitForVideo 轮询等待视频完成，返回 video_url 或空串。
func (s *Server) waitForVideo(ctx context.Context, account *config.Account, job *config.VideoJob, maxSec int) string {
	settings := s.Store.SettingsSnapshot()
	deadline := time.Now().Add(time.Duration(maxSec) * time.Second)
	interval := time.Duration(settings.VideoPollIntervalMS) * time.Millisecond
	if interval < time.Second {
		interval = 5 * time.Second
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(interval):
		}
		body := s.pollUpstream(ctx, account, job)
		if body == nil {
			continue
		}
		if url := extractVideoURL(body); url != "" {
			return url
		}
	}
	return ""
}

func (s *Server) pollUpstream(ctx context.Context, account *config.Account, job *config.VideoJob) map[string]any {
	settings := s.Store.SettingsSnapshot()
	path := settings.VideoPollPath
	if path == "" {
		path = "/agnesapi"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relay.UpstreamURL(account, path), nil)
	if err != nil {
		return nil
	}
	q := req.URL.Query()
	q.Set("video_id", job.VideoID)
	if settings.VideoPollWithModel && job.Model != "" {
		q.Set("model_name", job.Model)
	}
	req.URL.RawQuery = q.Encode()
	for k, v := range relay.ClientHeaders(account, nil, false) {
		req.Header[k] = v
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil
	}
	return decodeMap(raw)
}

// ---------------------------------------------------------------------------
// 视频轮询（下游 job_id → 上游 video_id）
// ---------------------------------------------------------------------------

func (s *Server) handleVideoPoll(w http.ResponseWriter, r *http.Request) {
	if _, e := s.downstreamKey(r); e != nil {
		writeErr(w, e)
		return
	}
	jobID := r.PathValue("job_id")
	job, ok := s.Store.JobByID(jobID)
	if !ok {
		writeErr(w, &apiError{Status: 404, Type: "invalid_request_error", Message: "未知的 job_id：" + jobID})
		return
	}
	account := s.Store.AccountByID(job.AccountID)
	if account == nil {
		writeErr(w, &apiError{Status: 410, Type: "upstream_error", Message: "创建该任务的上游账号已不存在"})
		return
	}
	data := s.pollUpstream(r.Context(), account, job)
	if data == nil {
		writeErr(w, &apiError{Status: 502, Type: "upstream_error", Message: "轮询上游失败"})
		return
	}
	data["job_id"] = job.JobID
	data["video_id"] = job.VideoID
	writeJSON(w, 200, data, nil)
}

func (s *Server) handleAgnesAPI(w http.ResponseWriter, r *http.Request) {
	if _, e := s.downstreamKey(r); e != nil {
		writeErr(w, e)
		return
	}
	videoID := strings.TrimSpace(r.URL.Query().Get("video_id"))
	if videoID == "" {
		writeErr(w, badRequest("缺少 video_id 参数"))
		return
	}
	var account *config.Account
	if job, ok := s.Store.JobByVideoID(videoID); ok {
		account = s.Store.AccountByID(job.AccountID)
	}
	if account == nil {
		for _, a := range s.Store.AccountsSnapshot() {
			if a.Enabled && strings.TrimSpace(a.APIKey) != "" && len(s.Hub.DeclaredModels(a, "video")) > 0 {
				account = a
				break
			}
		}
	}
	if account == nil {
		writeErr(w, &apiError{Status: 503, Type: "upstream_error", Message: "没有可用账号"})
		return
	}
	settings := s.Store.SettingsSnapshot()
	path := settings.VideoPollPath
	if path == "" {
		path = "/agnesapi"
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, relay.UpstreamURL(account, path), nil)
	if err != nil {
		writeErr(w, badRequest(err.Error()))
		return
	}
	req.URL.RawQuery = r.URL.RawQuery
	for k, v := range relay.ClientHeaders(account, nil, false) {
		req.Header[k] = v
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		writeErr(w, &apiError{Status: 502, Type: "upstream_error", Message: "轮询上游失败：" + err.Error()})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	writeJSON(w, resp.StatusCode, decodeOrRaw(raw), nil)
}

// ---------------------------------------------------------------------------
// 干跑判定（零配额消耗）
// ---------------------------------------------------------------------------

func (s *Server) handleIntentPreview(w http.ResponseWriter, r *http.Request) {
	if _, e := s.downstreamKey(r); e != nil {
		writeErr(w, e)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	settings := s.Store.SettingsSnapshot()
	path := strings.TrimSpace(asStr(body["_path"]))
	if path == "" {
		path = "/v1/chat/completions"
	}
	delete(body, "_path")
	forced := strings.TrimSpace(asStr(body["_force_modality"]))
	delete(body, "_force_modality")
	requested := asStr(body["model"])
	if requested == "" {
		requested = settings.AutoModelName
	}
	decision := intent.Decide(path, body, requested, autoIntentConfig(settings), s.rules,
		settings.ModelAliases, pool.ModalityOfModel, forced)
	resolved := ""
	if m, e := s.resolveAutoModel(settings, decision.Modality); e == nil {
		resolved = m
	}
	writeJSON(w, 200, map[string]any{
		"intent":                        decision.Modality,
		"intent_by":                     decision.Source,
		"intent_label":                  decision.SourceLabel(),
		"intent_score":                  decision.Score,
		"intent_reason":                 decision.Reason,
		"prompt":                        decision.Prompt.Text,
		"input_images":                  len(decision.Prompt.Images),
		"model_resolved":                resolved,
		"available_models_for_modality": s.autoModelUnion(settings, decision.Modality),
	}, nil)
}

// ---------------------------------------------------------------------------
// 转发：非流式 / 流式（含排队心跳）
// ---------------------------------------------------------------------------

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, item *config.DownstreamKey,
	opts relay.Options, decision intent.Result, wantsStream bool) {

	ctx := r.Context()
	if !wantsStream {
		result, err := relay.Do(ctx, s.Hub, s.Client, opts)
		if err != nil {
			writeErr(w, relayError(err))
			return
		}
		raw := result.ReadAll()
		if result.Status < 400 {
			s.Store.ChargeKey(item.Key, extractUsage(raw))
		}
		s.logUsageFull(item, decision, opts.PoolClass, opts.Path, false, result.Account, result.WaitMS, result.Attempts, string(opts.Body))
		headers := passthroughHeaders(result.Header)
		for k, v := range decision.Headers() {
			headers[k] = v
		}
		headers["X-Agnes-Hub-Model"] = result.ModelUsed
		if result.Account != nil {
			headers["X-Agnes-Hub-Account"] = safeHeader(result.Account.Name)
			headers["X-Agnes-Hub-Account-Id"] = result.Account.ID
		}
		headers["X-Agnes-Hub-Wait-Ms"] = strconv.FormatInt(result.WaitMS, 10)
		headers["X-Agnes-Hub-Attempts"] = strconv.Itoa(result.Attempts)
		if result.Status >= 400 {
			writeJSON(w, result.Status, decodeOrRaw(raw), headers)
			return
		}
		writeJSON(w, result.Status, decodeOrRaw(raw), headers)
		return
	}

	// 流式：快路径预等，超时则转入 SSE 心跳
	settings := s.Store.SettingsSnapshot()
	keepalive := time.Duration(settings.KeepaliveMS) * time.Millisecond
	if keepalive <= 0 {
		keepalive = 5 * time.Second
	}
	type outcome struct {
		result *relay.Result
		err    error
	}
	ch := make(chan outcome, 1)
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		result, err := relay.Do(ctx2, s.Hub, s.Client, opts)
		ch <- outcome{result: result, err: err}
	}()

	select {
	case out := <-ch:
		s.finishStream(w, item, out.result, out.err, decision, opts, true)
		return
	case <-time.After(keepalive):
	case <-ctx.Done():
		return
	}

	// --- 心跳路径 ---
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	for k, v := range decision.Headers() {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEComment(w, flusher, "agnes-hub waiting for rate-limit slot")

	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()
	for {
		select {
		case out := <-ch:
			if out.err != nil {
				writeSSEFrame(w, flusher, map[string]any{
					"error": map[string]any{"message": out.err.Error(), "type": "baipiao_hub_queue"}})
				return
			}
			result := out.result
			if result.Status >= 400 {
				raw := result.ReadAll()
				var payload any
				if json.Unmarshal(raw, &payload) != nil {
					payload = map[string]any{"error": map[string]any{
						"message": string(raw), "type": "upstream_error"}}
				}
			writeSSEFrame(w, flusher, payload)
			s.logUsageFull(item, decision, opts.PoolClass, opts.Path, true, result.Account, result.WaitMS, result.Attempts, string(opts.Body))
			return
		}
		writeSSEComment(w, flusher, fmt.Sprintf("baiPiao-hub account=%s wait_ms=%d attempts=%d model=%s",
			safeHeader(result.Account.Name), result.WaitMS, result.Attempts, result.ModelUsed))
		tail := &sseTail{w: &flushWriter{w: w, f: flusher}, cap: 1 << 16}
		_, _ = io.Copy(tail, result.Stream)
		result.Close()
		s.Store.ChargeKey(item.Key, tail.usage())
		s.logUsageFull(item, decision, opts.PoolClass, opts.Path, true, result.Account, result.WaitMS, result.Attempts, string(opts.Body))
		return
		case <-ticker.C:
			writeSSEComment(w, flusher, "agnes-hub waiting for rate-limit slot")
		case <-ctx.Done():
			return
		}
	}
}

// finishStream 已拿到上游响应，按真实状态码回给客户端。
func (s *Server) finishStream(w http.ResponseWriter, item *config.DownstreamKey, result *relay.Result,
	err error, decision intent.Result, opts relay.Options, streamMode bool) {

	if err != nil {
		writeErr(w, relayError(err))
		return
	}
	if result.Status >= 400 {
		raw := result.ReadAll()
		headers := passthroughHeaders(result.Header)
		for k, v := range decision.Headers() {
			headers[k] = v
		}
		writeJSON(w, result.Status, decodeOrRaw(raw), headers)
		return
	}
	headers := passthroughHeaders(result.Header)
	for k, v := range decision.Headers() {
		headers[k] = v
	}
	headers["X-Agnes-Hub-Model"] = result.ModelUsed
	headers["X-Agnes-Hub-Account"] = safeHeader(result.Account.Name)
	headers["X-Agnes-Hub-Account-Id"] = result.Account.ID
	headers["X-Agnes-Hub-Wait-Ms"] = strconv.FormatInt(result.WaitMS, 10)
	headers["X-Agnes-Hub-Attempts"] = strconv.Itoa(result.Attempts)
	headers["Content-Type"] = "text/event-stream"
	headers["Cache-Control"] = "no-cache"
	delete(headers, "Content-Length")
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	tail := &sseTail{w: &flushWriter{w: w, f: flusher}, cap: 1 << 16}
	_, _ = io.Copy(tail, result.Stream)
	result.Close()
	s.Store.ChargeKey(item.Key, tail.usage())
	s.logUsageFull(item, decision, opts.PoolClass, opts.Path, true, result.Account, result.WaitMS, result.Attempts, string(opts.Body))
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

func writeSSEComment(w io.Writer, f http.Flusher, text string) {
	_, _ = w.Write([]byte(": " + text + "\n\n"))
	if f != nil {
		f.Flush()
	}
}

func writeSSEFrame(w io.Writer, f http.Flusher, payload any) {
	buf, _ := json.Marshal(payload)
	_, _ = w.Write(append(append([]byte("data: "), buf...), '\n', '\n'))
	if f != nil {
		f.Flush()
	}
}

// writeSyntheticSSE 把非流式结果合成 SSE（客户端要 stream=true 时用）。
func (s *Server) writeSyntheticSSE(w http.ResponseWriter, envelope map[string]any, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, frame := range intent.SSEFromChat(envelope) {
		_, _ = w.Write(frame)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// 日志与错误
// ---------------------------------------------------------------------------

func (s *Server) logUsage(item *config.DownstreamKey, decision intent.Result, poolClass string,
	r *http.Request, path string, stream bool) {
	// 流式请求在拿到最终结果前先记一条「受理」记录，便于排障时看到排队行为
	s.Store.AppendUsage(map[string]any{
		"ts": time.Now().Format("2006-01-02 15:04:05"), "phase": "accepted",
		"endpoint": path, "pool_class": poolClass, "intent": decision.Modality,
		"intent_by": decision.Source, "key_name": item.Name, "stream": stream,
	})
}

func (s *Server) logUsageFull(item *config.DownstreamKey, decision intent.Result, poolClass,
	path string, stream bool, account *config.Account, waitMS int64, attempts int,
	userRequest string) {
	acctName, acctID := "", ""
	if account != nil {
		acctName, acctID = account.Name, account.ID
	}
	// 用户提出的完整请求：截断到 8KB，避免图文/视频 base64 把日志撑爆。
	ur := requestLog(userRequest)
	s.Store.AppendUsage(map[string]any{
		"ts": time.Now().Format("2006-01-02 15:04:05"), "phase": "done",
		"endpoint": path, "pool_class": poolClass,
		"intent": decision.Modality, "intent_by": decision.Source,
		"intent_score": decision.Score, "intent_reason": decision.Reason,
		"model_requested": decision.ModelRequested, "model_used": decision.ModelUsed,
		"key_name": item.Name, "account": acctName, "account_id": acctID,
		"wait_ms": waitMS, "attempts": attempts, "stream": stream,
		"user_request": ur,
	})
}

// requestLog 把用户完整请求收敛到安全长度（长请求含 base64，只保留前 8KB）。
func requestLog(s string) string {
	const max = 8192
	if len(s) > max {
		return s[:max] + " …(truncated)"
	}
	return s
}

// mustJSON 把任意值编码为 JSON 字符串（用于把解码后的请求体重新序列化进日志）。
func mustJSON(v any) string {
	if v == nil {
		return ""
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(buf)
}

// extractUsage 从 OpenAI 格式响应体解析 token 用量（total_tokens）；解析不到返回 0。
func extractUsage(raw []byte) int64 {
	var probe struct {
		Usage struct {
			TotalTokens      int64 `json:"total_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &probe) == nil {
		if probe.Usage.TotalTokens > 0 {
			return probe.Usage.TotalTokens
		}
		// 某些上游只给 prompt/completion 不给 total：相加兜底。
		if probe.Usage.PromptTokens > 0 || probe.Usage.CompletionTokens > 0 {
			return probe.Usage.PromptTokens + probe.Usage.CompletionTokens
		}
	}
	return 0
}

// sseTail 在转发 SSE 流给客户端的同时，保留末尾片段用于解析流末的 usage。
type sseTail struct {
	w   io.Writer
	buf []byte
	cap int
}

func (t *sseTail) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		return n, err
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		t.buf = t.buf[len(t.buf)-t.cap:]
	}
	return n, nil
}

// usage 逐 data: 行解析 SSE，取最后一个成功解析出 usage 的 total_tokens。
func (t *sseTail) usage() int64 {
	var total int64
	for _, line := range strings.Split(string(t.buf), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var probe struct {
			Usage struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &probe) == nil && probe.Usage.TotalTokens > 0 {
			total = probe.Usage.TotalTokens
		}
	}
	return total
}

func relayError(err error) *apiError {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), hub.ErrQueueFull.Error()):
		return &apiError{Status: 429, Type: "rate_limit_error", Code: "queue_full",
			Message: err.Error(), Headers: map[string]string{"Retry-After": "5"}}
	case strings.Contains(err.Error(), hub.ErrQueueTimeout.Error()):
		return &apiError{Status: 429, Type: "rate_limit_error", Code: "queue_timeout",
			Message: err.Error(), Headers: map[string]string{"Retry-After": "5"}}
	case strings.Contains(err.Error(), hub.ErrNoCapacity.Error()):
		return &apiError{Status: 503, Type: "upstream_error", Code: "no_capacity", Message: err.Error()}
	default:
		return &apiError{Status: 502, Type: "upstream_error", Message: err.Error()}
	}
}

// passthroughHeaders 只保留安全的透传头。
func passthroughHeaders(in http.Header) map[string]string {
	out := map[string]string{}
	if in == nil {
		return out
	}
	for _, k := range []string{"Content-Type", "Retry-After"} {
		if v := in.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

func safeHeader(v string) string {
	ok := true
	for i := 0; i < len(v); i++ {
		if v[i] > 0x7F {
			ok = false
			break
		}
	}
	if ok {
		return v
	}
	// HTTP 头只能承载 latin-1：非 ASCII 一律百分号转义，
	// 否则写头时整个响应会变成 500（Python 基线上真实踩过）。
	var sb strings.Builder
	for _, b := range []byte(v) {
		if b >= 0x20 && b < 0x7F {
			sb.WriteByte(b)
		} else {
			sb.WriteString(fmt.Sprintf("%%%02X", b))
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// 解码小工具
// ---------------------------------------------------------------------------

func asStr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case json.Number:
		return s.String()
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(s)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func intToFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	}
	return false
}

func decodeMap(raw []byte) map[string]any {
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func decodeOrRaw(raw []byte) any {
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"error": map[string]any{
			"message": strings.TrimSpace(string(raw)), "type": "upstream_error"}}
	}
	return out
}

func mapList(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func cloneBody(body map[string]any) map[string]any {
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func extractVideoID(payload map[string]any) string {
	keys := []string{"video_id", "videoid", "id", "task_id", "taskId"}
	for _, k := range keys {
		if v := asStr(payload[k]); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if data, ok := payload["data"].(map[string]any); ok {
		for _, k := range keys {
			if v := asStr(data[k]); strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func extractVideoURL(payload map[string]any) string {
	for _, k := range []string{"video_url", "url", "download_url", "video"} {
		if v := asStr(payload[k]); strings.HasPrefix(v, "http") {
			return v
		}
	}
	if data, ok := payload["data"].(map[string]any); ok {
		return extractVideoURL(data)
	}
	return ""
}
