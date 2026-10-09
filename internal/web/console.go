package web

import (
	"context"
	_ "embed"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/intent"
	"agneshub/internal/pool"
	"agneshub/internal/relay"
)

//go:embed static/console.html
var consoleHTML []byte

//go:embed static/chat.html
var chatHTML []byte

//go:embed static/chat.main.js
var chatMainJS []byte

//go:embed static/logo.png
var logoPNG []byte

const cookieName = "baipiao_hub_session"
const chatPasswordCookie = "baipiao_chat_password"

func (s *Server) consoleRoutes() {
	m := s.mux
	m.HandleFunc("GET /console", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprint(len(consoleHTML)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(consoleHTML)
	})
	m.HandleFunc("GET /logo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", fmt.Sprint(len(logoPNG)))
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(logoPNG)
	})

	m.HandleFunc("POST /api/login", s.apiLogin)
	m.HandleFunc("POST /api/logout", s.apiLogout)
	m.HandleFunc("GET /api/session", s.apiSession)
	m.HandleFunc("POST /api/password", s.apiPassword)

	m.HandleFunc("GET /api/accounts", s.apiListAccounts)
	m.HandleFunc("POST /api/accounts", s.apiCreateAccount)
	m.HandleFunc("PATCH /api/accounts/{id}", s.apiUpdateAccount)
	m.HandleFunc("DELETE /api/accounts/{id}", s.apiDeleteAccount)
	m.HandleFunc("POST /api/accounts/{id}/test", s.apiTestAccount)
	m.HandleFunc("POST /api/accounts/{id}/reset-factors", s.apiResetFactors)
	m.HandleFunc("POST /api/accounts/bulk", s.apiBulkImport)
	m.HandleFunc("POST /api/accounts/bulk-update", s.apiBulkUpdate)

	m.HandleFunc("GET /api/keys", s.apiListKeys)
	m.HandleFunc("POST /api/keys", s.apiCreateKey)
	m.HandleFunc("PATCH /api/keys", s.apiUpdateKey)
	m.HandleFunc("DELETE /api/keys", s.apiDeleteKey)

	m.HandleFunc("GET /api/stats", s.apiStats)
	m.HandleFunc("GET /api/queue", s.apiQueue)
	m.HandleFunc("GET /api/logs", s.apiLogs)
	m.HandleFunc("GET /api/bindings", s.apiBindings)
	m.HandleFunc("POST /api/bindings/clear", s.apiClearBindings)
	m.HandleFunc("GET /api/video-jobs", s.apiVideoJobs)
	m.HandleFunc("GET /api/image-jobs", s.apiImageJobs)
	m.HandleFunc("DELETE /api/video-jobs/{id}", s.apiDeleteVideoJob)
	m.HandleFunc("DELETE /api/image-jobs/{id}", s.apiDeleteImageJob)
	m.HandleFunc("POST /api/video-jobs/clear", s.apiClearVideoJobs)
	m.HandleFunc("POST /api/image-jobs/clear", s.apiClearImageJobs)
	m.HandleFunc("GET /api/chat-logs", s.apiChatLogs)
	m.HandleFunc("POST /api/chat-logs", s.apiCreateChatLog)
	m.HandleFunc("DELETE /api/chat-logs/{id}", s.apiDeleteChatLog)
	m.HandleFunc("POST /api/chat-logs/clear", s.apiClearChatLogs)

	m.HandleFunc("GET /api/settings", s.apiGetSettings)
	m.HandleFunc("POST /api/settings", s.apiSetSettings)
	m.HandleFunc("GET /api/scenarios", s.apiListScenarios)
	m.HandleFunc("GET /api/export", s.apiExport)
	m.HandleFunc("POST /api/import", s.apiImport)

	m.HandleFunc("POST /api/intent/preview", s.apiIntentPreview)
	m.HandleFunc("POST /api/probe", s.apiProbe)
	m.HandleFunc("GET /api/rpm-table", s.apiRPMTable)

	m.HandleFunc("GET /api/update/status", s.apiUpdateStatus)
	m.HandleFunc("GET /api/update/check", s.apiUpdateCheck)
	m.HandleFunc("POST /api/update/apply", s.apiUpdateApply)

	// 网页端：聊天 / 生图 / 生视频（免第三方 AI Coding 积分）
	m.HandleFunc("GET /chat", s.handleChat)
	m.HandleFunc("GET /chat.main.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Content-Length", fmt.Sprint(len(chatMainJS)))
		_, _ = w.Write(chatMainJS)
	})
	m.HandleFunc("POST /api/chat/login", s.apiChatLogin)
	m.HandleFunc("GET /api/chat/session", s.apiChatSession)
	m.HandleFunc("GET /api/models", s.apiChatModels)
	// Chat 代理端点：免下游密钥，自动走账号池 + RPM 限制
	m.HandleFunc("POST /api/chat/v1/chat/completions", s.handleChatProxy)
	m.HandleFunc("POST /api/chat/v1/images/generations", s.handleChatMediaProxy)
	m.HandleFunc("POST /api/chat/v1/videos", s.handleChatMediaProxy)
	m.HandleFunc("GET /api/chat/v1/videos/{job_id}", s.handleChatMediaProxy)
}

// ---------------------------------------------------------------------------
// 会话
// ---------------------------------------------------------------------------

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return c.Value == s.Store.SessionToken()
}

// chatAuthed 检查 chat 页面密码是否验证通过（cookie 或无密码设置）。
func (s *Server) chatAuthed(r *http.Request) bool {
	// 先检查是否有 chat 密码 cookie
	c, err := r.Cookie(chatPasswordCookie)
	if err == nil && c.Value != "" {
		return true
	}
	// 未设置密码则允许访问
	settings := s.Store.SettingsSnapshot()
	return settings.ChatPasswordHash == ""
}

// authedOrChat 同时接受管理员会话或 Chat 密码会话（图片/视频库接口在两种入口下都要可用）。
func (s *Server) authedOrChat(r *http.Request) bool {
	return s.authed(r) || s.chatAuthed(r)
}

func (s *Server) deny(w http.ResponseWriter) {
	writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "未登录或会话已失效"}}, nil)
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if !s.Store.VerifyPassword(asStr(body["password"])) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "管理员密码错误"}}, nil)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Store.SessionToken(),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 86400})
	writeJSON(w, 200, map[string]any{"ok": true,
		"must_change_password": s.Store.SettingsSnapshot().MustChangePassword}, nil)
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"logged_in":            s.authed(r),
		"must_change_password": s.Store.SettingsSnapshot().MustChangePassword,
	}, nil)
}

func (s *Server) apiPassword(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	pw := asStr(body["new_password"])
	if len([]rune(pw)) < 6 {
		writeErr(w, badRequest("新密码至少 6 位"))
		return
	}
	if err := s.Store.SetPassword(pw); err != nil {
		writeErr(w, &apiError{Status: 500, Type: "internal_error", Message: err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Store.SessionToken(),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 86400})
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// ---------------------------------------------------------------------------
// Chat 页面密码
// ---------------------------------------------------------------------------

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	// 检查 chat 密码验证
	if !s.chatAuthed(r) {
		// 需要密码，重定向到带错误参数的登录页
		http.Redirect(w, r, "/chat?need_password=1", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(chatHTML)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(chatHTML)
}

func (s *Server) apiChatLogin(w http.ResponseWriter, r *http.Request) {
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	pw := asStr(body["password"])
	if !s.Store.VerifyChatPassword(pw) {
		writeErr(w, &apiError{Status: 401, Type: "authentication_error", Message: "密码错误"})
		return
	}
	// 设置 chat 密码 cookie（7天过期）
	http.SetCookie(w, &http.Cookie{Name: chatPasswordCookie, Value: "authenticated",
		Path: "/chat", HttpOnly: false, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 86400})
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiChatSession(w http.ResponseWriter, r *http.Request) {
	hasPassword := s.Store.SettingsSnapshot().ChatPasswordHash != ""
	isAuthed := s.chatAuthed(r)
	writeJSON(w, 200, map[string]any{
		"requires_password": hasPassword,
		"authenticated":     isAuthed,
	}, nil)
}

// apiChatModels 返回聊天页（对话/生图/生视频）可用的模型列表。
//
// 除了内置的 agnes 模型与用户别名，还把每个已启用账号在其清单里声明的模型合并进来，
// 这样 chat 下拉框才能列出 AMD / OpenRouter 等非 agnes 渠道的真实模型，便于直接测试连通性。
func (s *Server) apiChatModels(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	settings := s.Store.SettingsSnapshot()
	base := pool.KnownModelNamesByModality(settings.ModelAliases)
	models := map[string][]string{"text": {}, "image": {}, "video": {}}
	seen := map[string]map[string]bool{"text": {}, "image": {}, "video": {}}
	add := func(mod, name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[mod][name] {
			return
		}
		seen[mod][name] = true
		models[mod] = append(models[mod], name)
	}
	for _, mod := range []string{"text", "image", "video"} {
		for _, m := range base[mod] {
			add(mod, m)
		}
	}
	for _, a := range s.Store.AccountsSnapshot() {
		if !a.Enabled || strings.TrimSpace(a.APIKey) == "" {
			continue
		}
		mf := pool.ManifestOf(a, settings)
		for _, m := range mf.Text {
			add("text", m)
		}
		for _, m := range mf.Image {
			add("image", m)
		}
		for _, m := range mf.Video {
			add("video", m)
		}
	}
	for _, mod := range []string{"text", "image", "video"} {
		sort.Strings(models[mod])
	}
	writeJSON(w, 200, map[string]any{"models": models}, nil)
}

// ---------------------------------------------------------------------------
// 账号池
// ---------------------------------------------------------------------------

func mask(key string) string {
	if len(key) <= 10 {
		return strings.Repeat("*", len(key))
	}
	return key[:6] + "..." + key[len(key)-4:]
}

func (s *Server) apiListAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	settings := s.Store.SettingsSnapshot()
	out := []map[string]any{}
	for _, a := range s.Store.AccountsSnapshot() {
		base := map[string]any{}
		for _, cls := range config.PoolClasses {
			base[cls] = round2c(baseRPMOf(a, cls))
		}
		eff := map[string]any{}
		declared := map[string]any{}
		for _, cls := range config.PoolClasses {
			eff[cls] = round2c(s.Hub.EffectiveRPM(a, cls))
			declared[cls] = s.Hub.DeclaredModels(a, cls)
		}
		out = append(out, map[string]any{
			"id": a.ID, "name": a.Name, "group": a.Group,
			"base_url": a.BaseURL, "access_type": a.AccessType,
			"enabled": a.Enabled, "class_enabled": a.ClassesEnabled,
			"model_manifest": a.ModelManifest,
			"api_key_masked": mask(a.APIKey),
			"rpm_base":       base, "rpm_effective": eff, "declared": declared,
			"max_concurrency":      a.MaxConcurrency,
			"learned_factor":       round3c(a.LearnedFactor),
			"pool_factors":         a.PoolFactors,
			"penalty_remaining_ms": s.Hub.PenaltyRemaining(a.ID).Milliseconds(),
			"inflight":             s.Hub.Inflight(a.ID),
			"stats":                a.Stats,
			"consecutive_failures": a.ConsecutiveFailures,
			"default_model":        a.DefaultModel,
			"priority":             a.Priority,
			"rpm_overrides":        a.RPMOverrides,
		})
	}
	writeJSON(w, 200, map[string]any{
		"accounts": out, "pool_classes": config.PoolClasses,
		"access_types":     config.AccessTypes,
		"default_manifest": settings.ModelManifestDefault,
	}, nil)
}

func baseRPMOf(a *config.Account, poolClass string) float64 {
	table, ok := config.RPMTable[a.AccessType]
	if !ok {
		table = config.RPMTable["free"]
	}
	if v, ok := a.RPMOverrides[poolClass]; ok && v > 0 {
		return v
	}
	if v, ok := table[poolClass]; ok {
		return v
	}
	return 1
}

func (s *Server) apiCreateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	apiKey := strings.TrimSpace(asStr(body["api_key"]))
	if apiKey == "" {
		writeErr(w, badRequest("api_key 不能为空"))
		return
	}
	var manifest *config.ModelManifest
	if raw, ok := body["model_manifest"].(map[string]any); ok {
		manifest = manifestFromAny(raw)
	}
	account := s.Store.AddAccount(asStr(body["name"]), apiKey,
		asStr(body["access_type"]), asStr(body["base_url"]), manifest)
	if v, ok := body["priority"]; ok {
		if n := asInt(v); n > 0 {
			s.Store.MutateAccount(account.ID, func(a *config.Account) bool { a.Priority = n; return true })
		}
	}
	if group := strings.TrimSpace(asStr(body["group"])); group != "" {
		s.Store.MutateAccount(account.ID, func(a *config.Account) bool { a.Group = group; return true })
	}
	if v := asInt(body["max_concurrency"]); v > 0 {
		s.Store.MutateAccount(account.ID, func(a *config.Account) bool { a.MaxConcurrency = v; return true })
	}
	if dm := strings.TrimSpace(asStr(body["default_model"])); dm != "" {
		s.Store.MutateAccount(account.ID, func(a *config.Account) bool { a.DefaultModel = dm; return true })
	}
	if overrides, ok := body["rpm_overrides"].(map[string]any); ok {
		s.Store.MutateAccount(account.ID, func(a *config.Account) bool {
			for k, v := range overrides {
				switch n := v.(type) {
				case float64:
					a.RPMOverrides[k] = n
				case string:
					a.RPMOverrides[k] = parseFloat(n)
				}
			}
			return true
		})
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true, "id": account.ID}, nil)
}

func manifestFromAny(raw map[string]any) *config.ModelManifest {
	m := &config.ModelManifest{}
	if v, ok := raw["text"]; ok {
		m.Text = stringList(v)
	}
	if v, ok := raw["image"]; ok {
		m.Image = stringList(v)
	}
	if v, ok := raw["video"]; ok {
		m.Video = stringList(v)
	}
	return m
}

func (s *Server) apiUpdateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	id := r.PathValue("id")
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	found := s.Store.MutateAccount(id, func(a *config.Account) bool {
		changed := false
		setStr := func(key string, dst *string) {
			if v, ok := body[key]; ok {
				*dst, changed = asStr(v), true
			}
		}
		setBool := func(key string, dst *bool) {
			if v, ok := body[key]; ok {
				*dst, changed = truthy(v), true
			}
		}
		setStr("name", &a.Name)
		setStr("group", &a.Group)
		setStr("base_url", &a.BaseURL)
		setStr("access_type", &a.AccessType)
		setStr("default_model", &a.DefaultModel)
		setBool("enabled", &a.Enabled)
		if v, ok := body["priority"]; ok {
			n := asInt(v)
			if n < 0 {
				n = 0
			}
			a.Priority, changed = n, true
		}
		if v, ok := body["api_key"]; ok && strings.TrimSpace(asStr(v)) != "" {
			a.APIKey, changed = strings.TrimSpace(asStr(v)), true
		}
		if v, ok := body["max_concurrency"]; ok {
			if n := asInt(v); n > 0 {
				a.MaxConcurrency, changed = n, true
			}
		}
		if v, ok := body["class_enabled"]; ok {
			a.ClassesEnabled, changed = stringList(v), true
		}
		if v, ok := body["model_manifest"].(map[string]any); ok {
			a.ModelManifest, changed = *manifestFromAny(v), true
		}
		if v, ok := body["rpm_overrides"].(map[string]any); ok {
			next := map[string]float64{}
			for k, raw := range v {
				next[k] = parseFloat(asStr(raw))
			}
			a.RPMOverrides, changed = next, true
		}
		if a.BaseURL != "" {
			a.BaseURL = strings.TrimRight(a.BaseURL, "/")
		}
		return changed
	})
	if !found {
		writeErr(w, &apiError{Status: 404, Type: "invalid_request_error", Message: "账号不存在"})
		return
	}
	if truthy(body["reset_calibration"]) {
		s.Store.MutateAccount(id, func(a *config.Account) bool {
			a.LearnedFactor = 1
			a.LastRateLimited = 0
			return true
		})
		s.Hub.ResetFactors(id)
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiResetFactors(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	id := r.PathValue("id")
	s.Store.MutateAccount(id, func(a *config.Account) bool {
		a.LearnedFactor = 1
		return true
	})
	s.Hub.ResetFactors(id)
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	ok := s.Store.DeleteAccount(r.PathValue("id"))
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": ok}, nil)
}

// apiTestAccount 连通性 + 模型权限探测。
func (s *Server) apiTestAccount(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	account := s.Store.AccountByID(r.PathValue("id"))
	if account == nil {
		writeErr(w, &apiError{Status: 404, Type: "invalid_request_error", Message: "账号不存在"})
		return
	}
	settings := s.Store.SettingsSnapshot()
	client := s.Client
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	started := time.Now()
	var models []string
	planned := []map[string]any{}

	// 1) 拉取上游真实模型清单（零配额，且能直接看出清单是否过期）
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		relay.UpstreamURL(account, "/v1/models"), nil); err == nil {
		for k, v := range relay.ClientHeaders(account, nil, false) {
			req.Header[k] = v
		}
		if resp, err := client.Do(req); err == nil {
			raw := readAllLimited(resp.Body, 1<<20)
			_ = resp.Body.Close()
			if resp.StatusCode < 400 {
				data := decodeMap(raw)
				models = stringList(data["data"])
				if len(models) == 0 {
					for _, m := range mapList(data["data"]) {
						if id := asStr(m["id"]); id != "" {
							models = append(models, id)
						}
					}
				}
			}
		}
	}

	// 2) 用一个最小文本请求验证 Key / 网络 / 模型权限。
	//    探测模型优先用该账号自己声明的文本模型（AMD / OpenRouter 等渠道没有官方 ProbeModel），
	//    拿不到再退回全局 ProbeModel。
	probeModel := settings.ProbeModel
	if ms := pool.ManifestOf(account, settings).Text; len(ms) > 0 {
		probeModel = ms[0]
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      probeModel,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens": 4, "stream": false,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		relay.UpstreamURL(account, "/v1/chat/completions"), strings.NewReader(string(payload)))
	for k, v := range relay.ClientHeaders(account, nil, false) {
		req.Header[k] = v
	}
	status := 0
	bodyText := ""
	if resp, err := client.Do(req); err == nil {
		raw := readAllLimited(resp.Body, 1<<20)
		_ = resp.Body.Close()
		status = resp.StatusCode
		bodyText = truncateStr(string(raw), 400)
	} else {
		bodyText = err.Error()
	}

	// 3) 清单与真实可用模型做差集，直接指出配置漂移
	declared := pool.ManifestOf(account, settings)
	known := map[string]bool{}
	for _, m := range models {
		known[m] = true
	}
	if len(models) > 0 {
		for name, list := range map[string][]string{
			"text": declared.Text, "image": declared.Image, "video": declared.Video,
		} {
			for _, m := range list {
				if !known[m] {
					planned = append(planned, map[string]any{
						"modality": name, "model": m,
						"issue": "上游模型清单里没有它（可能已下线或拼写错误）",
					})
				}
			}
		}
	}

	writeJSON(w, 200, map[string]any{
		"ok": status > 0 && status < 400, "status": status,
		"latency_ms":  time.Since(started).Milliseconds(),
		"probe_model": probeModel,
		"models":      models,
		"drift":       planned,
		"hint":        hintForStatus(status),
		"body":        bodyText,
	}, nil)
}

func hintForStatus(status int) string {
	switch status {
	case 200:
		return "正常"
	case 0:
		return "网络层失败：无法连接上游（检查代理 / DNS / Base URL）"
	case 400:
		return "请求参数问题（检查模型名与请求体）"
	case 401:
		return "API Key 无效 / 格式错误 / 账号状态异常"
	case 402:
		return "余额或配额不足"
	case 403:
		return "无该模型权限 / 网络被策略拦截 / 地区受限"
	case 404:
		return "路径或模型名错误（注意 Base URL 不要重复拼接 /v1）"
	case 429:
		return "已触发 RPM 限流（说明当前有效 RPM 设得过高）"
	case 500, 502, 503:
		return "上游服务异常或波动，可重试"
	}
	return "未知状态码，参考官方 ERROR_CODES.md"
}

// apiBulkImport 批量导入账号：每行 `名称,key[,base_url[,access_type[,group]]]`。
//
// 这是「多账号」真正可用与否的分水岭：注册到 10 个账号后，手工点 50 次表单
// 是不可接受的。导入会**跳过硬校验失败的整行**并逐行回报原因，而不是整体失败。
func (s *Server) apiBulkImport(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	text := asStr(body["text"])
	if strings.TrimSpace(text) == "" {
		writeErr(w, badRequest("text 不能为空"))
		return
	}
	defaultManifest := s.Store.SettingsSnapshot().ModelManifestDefault
	added := []map[string]any{}
	failed := []map[string]any{}

	for idx, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		if len(parts) < 2 || parts[1] == "" {
			failed = append(failed, map[string]any{"line": idx + 1, "text": line,
				"reason": "格式应为「名称,key[,base_url[,access_type[,group]]]」"})
			continue
		}
		name, apiKey := parts[0], parts[1]
		accessType := "free"
		if len(parts) > 3 && parts[3] != "" {
			accessType = parts[3]
		}
		baseURL := config.DefaultBaseURLForType(accessType)
		if len(parts) > 2 && parts[2] != "" {
			baseURL = parts[2]
		}
		group := ""
		if len(parts) > 4 {
			group = parts[4]
		}
		if _, ok := config.RPMTable[accessType]; !ok {
			failed = append(failed, map[string]any{"line": idx + 1, "text": line,
				"reason": "未知 access_type：" + accessType})
			continue
		}
		if strings.Contains(apiKey, "sk-") == false && len(apiKey) < 8 {
			failed = append(failed, map[string]any{"line": idx + 1, "text": line, "reason": "key 看起来不合法"})
			continue
		}
		m := defaultManifest.Clone()
		acc := s.Store.AddAccount(name, apiKey, accessType, baseURL, &m)
		if group != "" {
			s.Store.MutateAccount(acc.ID, func(a *config.Account) bool { a.Group = group; return true })
		}
		added = append(added, map[string]any{"id": acc.ID, "name": acc.Name, "group": group})
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true, "added": added, "failed": failed,
		"added_count": len(added), "failed_count": len(failed)}, nil)
}

// apiBulkUpdate 按分组 / ID 列表批量改配置（启用、停用、清单、并发、RPM 覆盖）。
func (s *Server) apiBulkUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	group := asStr(body["group"])
	ids := stringList(body["ids"])
	var targets []string
	for _, a := range s.Store.AccountsSnapshot() {
		if len(ids) > 0 {
			for _, id := range ids {
				if id == a.ID {
					targets = append(targets, a.ID)
				}
			}
			continue
		}
		if group == "" || a.Group == group {
			targets = append(targets, a.ID)
		}
	}
	changed := 0
	for _, id := range targets {
		if s.Store.MutateAccount(id, func(a *config.Account) bool {
			dirty := false
			if v, ok := body["enabled"]; ok {
				a.Enabled, dirty = truthy(v), true
			}
			if v, ok := body["max_concurrency"]; ok {
				if n := asInt(v); n > 0 {
					a.MaxConcurrency, dirty = n, true
				}
			}
			if v, ok := body["model_manifest"].(map[string]any); ok {
				a.ModelManifest, dirty = *manifestFromAny(v), true
			}
			if v, ok := body["class_enabled"]; ok {
				a.ClassesEnabled, dirty = stringList(v), true
			}
			return dirty
		}) {
			changed++
		}
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true, "updated": changed, "matched": len(targets)}, nil)
}

// ---------------------------------------------------------------------------
// 下游密钥
// ---------------------------------------------------------------------------

func (s *Server) apiListKeys(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"keys": s.Store.KeysSnapshot()}, nil)
}

func (s *Server) apiCreateKey(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	item := s.Store.AddKey(asStr(body["name"]), stringList(body["classes"]),
		int64(asInt(body["daily_quota"])), int64(asInt(body["total_quota"])), asStr(body["pinned_account"]))
	writeJSON(w, 200, map[string]any{"ok": true, "key": item}, nil)
}

func (s *Server) apiUpdateKey(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	target := asStr(body["key"])
	ok := s.Store.MutateKey(target, func(k *config.DownstreamKey) bool {
		if v, exists := body["name"]; exists {
			k.Name = asStr(v)
		}
		if v, exists := body["enabled"]; exists {
			k.Enabled = truthy(v)
		}
		if v, exists := body["classes"]; exists {
			k.Classes = stringList(v)
		}
		if v, exists := body["daily_quota"]; exists {
			k.DailyQuota = int64(asInt(v))
		}
		if v, exists := body["total_quota"]; exists {
			k.TotalQuota = int64(asInt(v))
		}
		if v, exists := body["pinned_account"]; exists {
			k.PinnedAccount = asStr(v)
		}
		if truthy(body["reset_usage"]) {
			k.UsedTotal, k.UsedToday = 0, 0
			k.UsedDate = time.Now().Format("2006-01-02")
		}
		return true
	})
	writeJSON(w, 200, map[string]any{"ok": ok}, nil)
}

func (s *Server) apiDeleteKey(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": s.Store.DeleteKey(r.URL.Query().Get("key"))}, nil)
}

// ---------------------------------------------------------------------------
// 观测
// ---------------------------------------------------------------------------

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	snapshot := s.Hub.Snapshot()
	snapshot["settings"] = s.Store.SettingsSnapshot()
	writeJSON(w, 200, snapshot, nil)
}

func (s *Server) apiQueue(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"queue": s.Hub.QueueView()}, nil)
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	limit := asInt(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	writeJSON(w, 200, map[string]any{"logs": s.Store.TailUsage(limit)}, nil)
}

func (s *Server) apiBindings(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	rows := []map[string]any{}
	for session, b := range s.Store.BindingsSnapshot() {
		name := "已删除"
		if a := s.Store.AccountByID(b.AccountID); a != nil {
			name = a.Name
		}
		rows = append(rows, map[string]any{"session": session, "account": name, "updated": b.Updated})
	}
	writeJSON(w, 200, map[string]any{"bindings": rows}, nil)
}

func (s *Server) apiClearBindings(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	s.Store.ClearBindings()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiVideoJobs(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"jobs": s.Store.JobsSnapshot()}, nil)
}

func (s *Server) apiImageJobs(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"jobs": s.Store.ImageJobsSnapshot()}, nil)
}

// apiDeleteImageJob 删除单条图片记录。
func (s *Server) apiDeleteImageJob(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, &apiError{Status: 400, Type: "bad_request", Message: "missing id"})
		return
	}
	if !s.Store.DeleteImageJob(id) {
		writeErr(w, &apiError{Status: 404, Type: "not_found", Message: "image job not found"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// apiClearImageJobs 清空全部图片记录。
func (s *Server) apiClearImageJobs(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	s.Store.ClearImageJobs()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// apiDeleteVideoJob 删除单条视频记录。
func (s *Server) apiDeleteVideoJob(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, &apiError{Status: 400, Type: "bad_request", Message: "missing id"})
		return
	}
	if !s.Store.DeleteVideoJob(id) {
		writeErr(w, &apiError{Status: 404, Type: "not_found", Message: "video job not found"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// apiClearVideoJobs 清空全部视频记录。
func (s *Server) apiClearVideoJobs(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	s.Store.ClearVideoJobs()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// ---- 聊天记录 ----

func (s *Server) apiChatLogs(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"logs": s.Store.ChatLogsSnapshot()}, nil)
}

func (s *Server) apiCreateChatLog(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	var body struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Reply  string `json:"reply"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, &apiError{Status: 400, Type: "bad_request", Message: "invalid body"})
		return
	}
	if body.Prompt == "" {
		writeErr(w, &apiError{Status: 400, Type: "bad_request", Message: "prompt required"})
		return
	}
	if body.Status == "" {
		body.Status = "completed"
	}
	id := "cl_" + randHex(12)
	log := &config.ChatLog{
		ID:        id,
		Model:     body.Model,
		Prompt:    body.Prompt,
		Reply:     body.Reply,
		Status:    body.Status,
		CreatedAt: float64(time.Now().Unix()),
	}
	s.Store.AddChatLog(log)
	writeJSON(w, 200, map[string]any{"id": id, "ok": true}, nil)
}

func (s *Server) apiDeleteChatLog(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, &apiError{Status: 400, Type: "bad_request", Message: "missing id"})
		return
	}
	if !s.Store.DeleteChatLog(id) {
		writeErr(w, &apiError{Status: 404, Type: "not_found", Message: "chat log not found"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiClearChatLogs(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		s.deny(w)
		return
	}
	s.Store.ClearChatLogs()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// randHex 生成 n 字节的十六进制随机串，用于聊天记录等实体的唯一 ID。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// 设置
// ---------------------------------------------------------------------------

// apiListScenarios 返回全部情景参数包（供控制台「运行情景」卡片渲染）。
func (s *Server) apiListScenarios(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{
		"current": s.Store.SettingsSnapshot().Scenario,
		"scenarios": config.ScenarioPresets(),
	}, nil)
}

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	settings := s.Store.SettingsSnapshot()
	settings.AdminPasswordHash = ""
	settings.AdminPasswordSalt = ""
	settings.ChatPasswordHash = ""
	settings.ChatPasswordSalt = ""
	writeJSON(w, 200, settings, nil)
}

func (s *Server) apiSetSettings(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	err := s.Store.UpdateSettings(func(st *config.Settings) { applySettings(st, body) })
	if err != nil {
		writeErr(w, &apiError{Status: 500, Type: "internal_error", Message: err.Error()})
		return
	}

	// 单独处理 chat_password（需要在锁外调用 Store 方法）
	if pw, ok := body["chat_password"]; ok {
		pwd := strings.TrimSpace(asStr(pw))
		if len(pwd) >= 4 {
			if err := s.Store.SetChatPassword(pwd); err != nil {
				writeErr(w, &apiError{Status: 500, Type: "internal_error", Message: err.Error()})
				return
			}
		} else if pwd == "" {
			// 清空密码
			if err := s.Store.SetChatPassword(""); err != nil {
				writeErr(w, &apiError{Status: 500, Type: "internal_error", Message: err.Error()})
				return
			}
		}
	}

	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// applySettings 把控制台提交的字段写入设置（只认白名单，忽略未知键）。
// 注意：此函数在 UpdateSettings 锁内调用，不能访问 s。
func applySettings(st *config.Settings, p map[string]any) {
	// 情景模式打底：先按 scenario 覆写整套旋钮，随后下方单项白名单再逐个覆盖
	// （实现「一键应用情景 + 单项可改回」）。未知情景 ApplyScenario 返回 false、不改字段。
	if n := strings.TrimSpace(asStr(p["scenario"])); n != "" {
		_ = config.ApplyScenario(st, n)
	}
	f := func(key string, dst *float64) {
		if v, ok := p[key]; ok {
			*dst = parseFloat(asStr(v))
		}
	}
	i := func(key string, dst *int) {
		if v, ok := p[key]; ok {
			*dst = asInt(v)
		}
	}
	b := func(key string, dst *bool) {
		if v, ok := p[key]; ok {
			*dst = truthy(v)
		}
	}
	s2 := func(key string, dst *string) {
		if v, ok := p[key]; ok {
			*dst = asStr(v)
		}
	}
	f("safety_factor", &st.SafetyFactor)
	f("pacing_window_sec", &st.PacingWindowSec)
	b("calibration_enabled", &st.CalibrationEnabled)
	f("penalty_cooldown_sec", &st.PenaltyCooldownSec)
	f("penalty_factor", &st.PenaltyFactor)
	f("min_learned_factor", &st.MinLearnedFactor)
	f("recover_after_sec", &st.RecoverAfterSec)
	i("recover_successes", &st.RecoverSuccesses)
	i("queue_max_wait_ms", &st.QueueMaxWaitMS)
	i("queue_max_size", &st.QueueMaxSize)
	i("keepalive_interval_ms", &st.KeepaliveMS)
	s2("affinity_mode", &st.AffinityMode)
	s2("region_priority", &st.RegionPriority)
	i("request_timeout_ms", &st.RequestTimeoutMS)
	s2("default_image_tier", &st.DefaultImageTier)
	s2("optimization_mode", &st.OptimizationMode)
	i("image_record_retention_days", &st.ImageRecordRetention)
	i("image_max_capacity", &st.ImageMaxCapacity)
	i("video_max_capacity", &st.VideoMaxCapacity)

	// chat_password is handled separately in apiSetSettings to avoid accessing s here
	i("retry_max", &st.RetryMax)
	i("retry_base_backoff_ms", &st.RetryBaseBackoffMS)
	i("retry_max_backoff_ms", &st.RetryMaxBackoffMS)
	i("image_concurrency", &st.ImageConcurrency)
	i("video_max_inflight", &st.VideoMaxInFlight)
	i("breaker_revive_sec", &st.BreakerReviveSec)
	s2("video_poll_path", &st.VideoPollPath)
	b("video_poll_include_model_name", &st.VideoPollWithModel)
	f("free_text_rpm", &st.FreeTextRPM)
	i("video_poll_interval_ms", &st.VideoPollIntervalMS)
	i("log_retention_days", &st.LogRetentionDays)
	f("session_ttl_hours", &st.SessionTTLHours)
	s2("probe_model", &st.ProbeModel)
	// 两级队列单项开关（不随情景改也能独立打开/关闭）
	if v, ok := p["text_two_level_queue"]; ok {
		st.TextTwoLevelQueue = truthy(v)
	}

	if v, ok := p["model_aliases"].(map[string]any); ok {
		next := map[string]string{}
		for k, raw := range v {
			next[k] = asStr(raw)
		}
		st.ModelAliases = next
	}
	if v, ok := p["auto_model_name"]; ok && strings.TrimSpace(asStr(v)) != "" {
		st.AutoModelName = asStr(v)
	}
	if v, ok := p["model_manifest_default"].(map[string]any); ok {
		st.ModelManifestDefault = *manifestFromAny(v)
	}
	if v, ok := p["auto_intent"].(map[string]any); ok {
		ai := st.AutoIntent
		if x, ok := v["content_scan"]; ok {
			ai.ContentScan = truthy(x)
		}
		if x, ok := v["min_confidence"]; ok {
			ai.MinConfidence = parseFloat(asStr(x))
		}
		if x, ok := v["default_image_size"]; ok && asStr(x) != "" {
			ai.DefaultImageSize = asStr(x)
		}
		if x, ok := v["image_input_field"]; ok {
			ai.ImageInputField = asStr(x)
		}
		if x, ok := v["video_input_field"]; ok {
			ai.VideoInputField = asStr(x)
		}
		if x, ok := v["video_wait_sec"]; ok {
			ai.VideoWaitSec = asInt(x)
		}
		if x, ok := v["preferred_models"].(map[string]any); ok {
			next := map[string][]string{}
			for k, raw := range x {
				next[k] = stringList(raw)
			}
			ai.PreferredModels = next
		}
		st.AutoIntent = ai
	}
}

// ---------------------------------------------------------------------------
// 迁移
// ---------------------------------------------------------------------------

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	settings := s.Store.SettingsSnapshot()
	settings.AdminPasswordHash = ""
	settings.AdminPasswordSalt = ""
	writeJSON(w, 200, map[string]any{
		"version":     2,
		"exported_at": time.Now().Format("2006-01-02 15:04:05"),
		"accounts":    s.Store.AccountsSnapshot(),
		"keys":        s.Store.KeysSnapshot(),
		"settings":    settings,
	}, nil)
}

func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	mode := asStr(body["mode"])
	if mode == "" {
		mode = "replace"
	}
	added := 0
	if raw, ok := body["accounts"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			apiKey := strings.TrimSpace(asStr(m["api_key"]))
			if apiKey == "" || strings.Contains(apiKey, "...") {
				continue
			}
			acc := s.Store.AddAccount(asStr(m["name"]), apiKey, asStr(m["access_type"]),
				asStr(m["base_url"]), nil)
			s.Store.MutateAccount(acc.ID, func(a *config.Account) bool {
				if v, ok := m["model_manifest"].(map[string]any); ok {
					a.ModelManifest = *manifestFromAny(v)
				}
				if v, ok := m["group"]; ok {
					a.Group = asStr(v)
				}
				if v, ok := m["max_concurrency"]; ok {
					if n := asInt(v); n > 0 {
						a.MaxConcurrency = n
					}
				}
				if v, ok := m["enabled"]; ok {
					a.Enabled = truthy(v)
				}
				return true
			})
			added++
		}
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true, "accounts_imported": added}, nil)
}

// ---------------------------------------------------------------------------
// 意图干跑 与 一键实测
// ---------------------------------------------------------------------------

func (s *Server) apiIntentPreview(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	s.previewIntent(w, body)
}

func (s *Server) previewIntent(w http.ResponseWriter, body map[string]any) {
	settings := s.Store.SettingsSnapshot()
	path := strings.TrimSpace(asStr(body["_path"]))
	if path == "" {
		path = "/v1/chat/completions"
	}
	forced := strings.TrimSpace(asStr(body["_force_modality"]))
	cases := []map[string]any{}
	samples := body["_samples"]
	if list, ok := samples.([]any); ok {
		for _, raw := range list {
			text := asStr(raw)
			payload := map[string]any{
				"model": settings.AutoModelName,
				"messages": []map[string]any{
					{"role": "user", "content": text},
				},
			}
			decision := intent.Decide(path, payload, settings.AutoModelName,
				autoIntentConfig(settings), s.rules, settings.ModelAliases, pool.ModalityOfModel, forced)
			cases = append(cases, map[string]any{
				"input": text, "intent": decision.Modality, "by": decision.Source,
				"label": decision.SourceLabel(), "score": round3c(decision.Score),
				"reason": decision.Reason,
			})
		}
		writeJSON(w, 200, map[string]any{"cases": cases}, nil)
		return
	}

	decision := intent.Decide(path, body, firstNonEmpty(asStr(body["model"]), settings.AutoModelName),
		autoIntentConfig(settings), s.rules, settings.ModelAliases, pool.ModalityOfModel, forced)
	resolved, _ := s.resolveAutoModel(settings, decision.Modality)
	writeJSON(w, 200, map[string]any{
		"intent": decision.Modality, "intent_by": decision.Source,
		"intent_label": decision.SourceLabel(), "intent_score": round3c(decision.Score),
		"intent_reason": decision.Reason, "prompt": decision.Prompt.Text,
		"input_images":                  len(decision.Prompt.Images),
		"model_resolved":                resolved,
		"available_models_for_modality": s.autoModelUnion(settings, decision.Modality),
	}, nil)
}

// apiProbe 对一个账号做 RPM 阶梯实测，返回「零 429 的最高速率」。
//
// 这是唯一会消耗真实配额的接口，因此**必须显式带 confirm:true**。
// 用途：上游悄悄调限额后自动跟上，而不是等任务断了才发现。
func (s *Server) apiProbe(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if !truthy(body["confirm"]) {
		writeErr(w, &apiError{Status: 400, Type: "invalid_request_error",
			Message: "该操作会消耗真实上游配额，请显式提交 confirm: true"})
		return
	}
	account := s.Store.AccountByID(asStr(body["account_id"]))
	if account == nil {
		writeErr(w, badRequest("账号不存在"))
		return
	}
	modality := firstNonEmpty(asStr(body["modality"]), "text")
	rates := floatList(body["rates"])
	if len(rates) == 0 {
		rates = []float64{18, 22, 30}
	}
	perRate := asInt(body["per_rate"])
	if perRate <= 0 {
		perRate = 7
	}
	results, err := s.probeRates(r.Context(), account, modality, rates, perRate)
	if err != nil {
		writeErr(w, &apiError{Status: 502, Type: "upstream_error", Message: err.Error()})
		return
	}
	best := 0.0
	for _, row := range results {
		if row["rate_limited"] == 0 && row["other_error"] == 0 {
			if rpm := row["target_rpm"].(float64); rpm > best {
				best = rpm
			}
		}
	}
	suggest := map[string]any{}
	if best > 0 {
		suggest["safe_rpm"] = int(best * 0.9)
		suggest["note"] = "已按 10% 安全余量给出建议值，可写入该账号的 rpm_overrides"
	}
	writeJSON(w, 200, map[string]any{"ok": true, "modality": modality,
		"results": results, "highest_clean_rpm": best, "suggest": suggest}, nil)
}

func (s *Server) probeRates(ctx context.Context, account *config.Account, modality string,
	rates []float64, perRate int) ([]map[string]any, error) {

	settings := s.Store.SettingsSnapshot()
	client := s.Client
	var results []map[string]any

	for _, rpm := range rates {
		interval := time.Duration(float64(time.Second) * 60.0 / rpm)
		ok, limited, other := 0, 0, 0
		started := time.Now()
		for i := 0; i < perRate; i++ {
			if i > 0 {
				select {
				case <-time.After(interval):
				case <-ctx.Done():
					return results, ctx.Err()
				}
			}
			path, payload := probePayload(modality, account, settings)
			raw, _ := json.Marshal(payload)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				relay.UpstreamURL(account, path), strings.NewReader(string(raw)))
			if err != nil {
				other++
				continue
			}
			for k, v := range relay.ClientHeaders(account, nil, false) {
				req.Header[k] = v
			}
			resp, err := client.Do(req)
			if err != nil {
				other++
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode == 200:
				ok++
			case resp.StatusCode == 429:
				limited++
			default:
				other++
			}
		}
		elapsed := time.Since(started).Seconds()
		observed := 0.0
		if elapsed > 0 {
			observed = float64(ok) * 60 / elapsed
		}
		results = append(results, map[string]any{
			"target_rpm": rpm, "sent": perRate, "ok": ok,
			"rate_limited": limited, "other_error": other,
			"elapsed_sec": round2c(elapsed), "effective_rpm_observed": round2c(observed),
		})
	}
	return results, nil
}

func probePayload(modality string, account *config.Account, settings config.Settings) (string, map[string]any) {
	manifest := pool.ManifestOf(account, settings)
	pick := func(list []string, fallback string) string {
		if len(list) > 0 {
			return list[0]
		}
		return fallback
	}
	switch modality {
	case "image":
		return "/v1/images/generations", map[string]any{
			"model":  pick(manifest.Image, pool.FallbackModel["image"]),
			"prompt": "a small blue circle icon", "size": "1K",
		}
	case "video":
		return "/v1/videos", map[string]any{
			"model":  pick(manifest.Video, pool.FallbackModel["video"]),
			"prompt": "a one second clip of a blue circle",
		}
	default:
		return "/v1/chat/completions", map[string]any{
			"model":      pick(manifest.Text, settings.ProbeModel),
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
			"max_tokens": 1, "stream": false,
		}
	}
}

func (s *Server) apiRPMTable(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{
		"table": config.RPMTable, "pool_classes": config.PoolClasses,
		"access_types": config.AccessTypes,
		"note":         "官方限流表按「模型类型」而非单个模型 ID 给出；标称值与实际可执行值不一致，故默认留安全余量",
	}, nil)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func readAllLimited(rc interface{ Read([]byte) (int, error) }, limit int64) []byte {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	var total int64
	for total < limit {
		n, err := rc.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			total += int64(n)
		}
		if err != nil {
			break
		}
	}
	return buf
}

func stringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			switch x := item.(type) {
			case string:
				out = append(out, x)
			case map[string]any:
				if id := asStr(x["id"]); id != "" {
					out = append(out, id)
				}
			default:
				if s := asStr(item); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func floatList(v any) []float64 {
	list := stringList(v)
	out := make([]float64, 0, len(list))
	for _, s := range list {
		out = append(out, parseFloat(s))
	}
	return out
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case string:
		return int(parseFloat(t))
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func parseFloat(s string) float64 {
	var f float64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	if err != nil {
		return 0
	}
	return f
}

func round2c(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
func round3c(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------------------
// 自更新
// ---------------------------------------------------------------------------

// updateStatus 汇总当前版本与最近一次检查结果，供控制台展示。
func (s *Server) updateStatus() map[string]any {
	cur := s.Version
	if s.Updater != nil {
		cur = s.Updater.Version()
	}
	out := map[string]any{
		"current_version": cur,
		"enabled":         s.Updater != nil,
	}
	if s.Updater == nil {
		return out
	}
	if last := s.Updater.LastCheck(); last != nil {
		out["last_check"] = last
	}
	return out
}

// apiUpdateStatus 返回当前版本与最近一次检查结果（不主动联网）。
func (s *Server) apiUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, s.updateStatus(), nil)
}

// apiUpdateCheck 立即向 GitHub 查一次最新版本。
func (s *Server) apiUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	if s.Updater == nil {
		writeJSON(w, 200, map[string]any{
			"current_version": s.Version,
			"enabled":         false,
			"error":           "自更新未启用",
		}, nil)
		return
	}
	result, err := s.Updater.Check(r.Context())
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"current_version": s.Updater.Version(),
			"error":           err.Error(),
		}, nil)
		return
	}
	writeJSON(w, 200, result, nil)
}

// apiUpdateApply 下载并替换二进制。
//
// 必须有管理员会话：这是唯一会改动磁盘上可执行文件的接口。
// 替换成功后置重启标志，主循环监听到就优雅退出 ——
// Windows 上随后由助手进程完成替换并重启，类 Unix 上由调用方重启。
func (s *Server) apiUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	if s.Updater == nil {
		writeJSON(w, 200, map[string]any{
			"success": false,
			"error":   "自更新未启用",
		}, nil)
		return
	}
	result, err := s.Updater.Apply(r.Context())
	if err != nil {
		writeJSON(w, 200, map[string]any{"success": false, "error": err.Error()}, nil)
		return
	}
	if result.Success {
		s.Updater.RequestRestart()
	}
	writeJSON(w, 200, result, nil)
}

// ---------------------------------------------------------------------------
// Chat 代理端点（免下游密钥，自动走账号池）
// ---------------------------------------------------------------------------

// handleChatProxy 代理文本对话请求到账号池，无需下游 API Key。
func (s *Server) handleChatProxy(w http.ResponseWriter, r *http.Request) {
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
		decision = intent.Decide("/v1/chat/completions", body, requested,
			autoIntentConfig(settings), s.rules, settings.ModelAliases,
			pool.ModalityOfModel, "")
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

	// 非文本模态（图片/视频意图）转发到媒体代理
	if decision.Modality != intent.Text {
		s.handleChatMediaProxy(w, r)
		return
	}

	// 文本路径：走账号池，无需下游密钥
	poolClass := "text"
	if !intent.IsAutoModel(requested) {
		poolClass = pool.Classify(requested, body, settings.ModelAliases, settings.DefaultImageTier)
	}

	model, modelErr := s.resolveAutoModel(settings, intent.Text)
	if modelErr != nil {
		writeErr(w, modelErr)
		return
	}
	decision.ModelUsed = model
	// 显式指定了具体模型（如 AMD 的 MiniCPM5-2B、OpenRouter 的某模型）时，
	// RequiredModel 必须用「用户请求的模型」而不是 auto 解析出的兜底模型，
	// 否则调度器会去找声明了兜底模型的账号，把请求错发给不认识该模型的渠道。
	requiredModel := model
	if !intent.IsAutoModel(requested) && requested != "" {
		requiredModel = requested
		decision.ModelUsed = requested
	}

	bodyFor := func(a *config.Account) ([]byte, string) {
		if intent.IsAutoModel(requested) || requested == "" {
			manifest := pool.ManifestOf(a, settings)
			chosen := intent.ChooseModel(manifest.Text, autoIntentConfig(settings).PreferredModels["text"])
			if chosen == "" {
				chosen = pool.FallbackModel["text"]
			}
			clone := make(map[string]any, len(body))
			for k, v := range body {
				clone[k] = v
			}
			clone["model"] = chosen
			buf, _ := json.Marshal(clone)
			return buf, chosen
		}
		resolved := pool.ResolveModel(requested, settings.ModelAliases)
		clone := cloneBody(body)
		clone["model"] = resolved
		buf, _ := json.Marshal(clone)
		return buf, resolved
	}

	sessionKey := s.Hub.SessionKey(headerMap(r), "")
	opts := relay.Options{
		SessionKey: sessionKey, PoolClass: poolClass,
		RequiredModel: requiredModel, Method: http.MethodPost, Path: "/v1/chat/completions",
		BodyFor: bodyFor, Idempotent: true,
	}

	ctx := r.Context()
	if !wantsStream {
		result, err := relay.Do(ctx, s.Hub, s.Client, opts)
		if err != nil {
			writeErr(w, relayError(err))
			return
		}
		raw := result.ReadAll()
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

	// 流式路径
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
			s.finishChatStream(w, out.result, out.err, decision, opts)
			return
	case <-time.After(time.Duration(settings.KeepaliveMS) * time.Millisecond):
	}

	// 心跳路径
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	for k, v := range decision.Headers() {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for {
		select {
		case out := <-ch:
			if out.err != nil {
				writeSSEComment(w, flusher, "error: "+out.err.Error())
				return
			}
			raw := out.result.ReadAll()
			if out.result.Status >= 400 {
				writeSSEComment(w, flusher, "error: HTTP "+strconv.Itoa(out.result.Status))
				return
			}
			// 转发上游流
			_ = json.NewEncoder(&flushSSE{w: w, f: flusher}).Encode(decodeOrRaw(raw))
			_, _ = io.Copy(&flushWriter{w: w, f: flusher}, out.result.Stream)
			out.result.Close()
			return
		case <-time.After(time.Duration(settings.KeepaliveMS) * time.Millisecond):
			writeSSEComment(w, flusher, "baipiao-hub chat proxy keepalive")
		case <-ctx2.Done():
			return
		}
	}
}

// flushSSE 是一个 io.Writer，将 JSON 编码为 SSE data: 帧。
type flushSSE struct {
	w io.Writer
	f http.Flusher
}

func (f *flushSSE) Write(p []byte) (int, error) {
	_, err := f.w.Write([]byte("data: "))
	if err != nil {
		return 0, err
	}
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	_, err = f.w.Write([]byte("\n\n"))
	if f.f != nil {
		f.f.Flush()
	}
	return n, err
}

// finishChatStream 处理流式结果并写入响应。
func (s *Server) finishChatStream(w http.ResponseWriter, result *relay.Result, err error,
	decision intent.Result, opts relay.Options) {
	if err != nil {
		writeErr(w, relayError(err))
		return
	}
	raw := result.ReadAll()
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
	// stream=true 时，上游返回的 raw 本身就是 SSE 帧序列；直接原样写回并声明 event-stream。
	headers["Content-Type"] = "text/event-stream"
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(result.Status)
	_, _ = w.Write(raw)
}

// handleChatMediaProxy 代理图片/视频请求到账号池，无需下游 API Key。
func (s *Server) handleChatMediaProxy(w http.ResponseWriter, r *http.Request) {
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	settings := s.Store.SettingsSnapshot()
	requested := strings.TrimSpace(asStr(body["model"]))

	var modality string
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "images/generations"):
		modality = intent.Image
	case strings.HasSuffix(path, "videos"), strings.Contains(path, "videos/"):
		modality = intent.Video
	default:
		modality = intent.Text
	}

	decision := intent.Decide(path, body, firstNonEmpty(requested, settings.AutoModelName),
		autoIntentConfig(settings), s.rules, settings.ModelAliases, pool.ModalityOfModel, modality)

	poolClass := ""
	switch decision.Modality {
	case intent.Image:
		poolClass = pool.PoolForModality(intent.Image, body, settings.DefaultImageTier)
	case intent.Video:
		poolClass = "video"
	default:
		poolClass = "text"
	}

	model, modelErr := s.pickMediaModel(settings, decision, decision.Modality)
	if modelErr != nil {
		writeErr(w, modelErr)
		return
	}
	decision.ModelUsed = model

	cfg := autoIntentConfig(settings)
	var upstreamBody map[string]any
	var bodyFor func(*config.Account) ([]byte, string)

	if decision.Modality == intent.Image {
		upstreamBody = intent.BuildImageBody(decision, model, cfg, body)
		decision.DroppedFields = intent.DroppedFields(body, intent.ImageFieldWhitelist)
		bodyFor = s.mediaBodyFor(upstreamBody, settings, decision, poolClass, intent.Image, model)
	} else if decision.Modality == intent.Video {
		upstreamBody = intent.BuildVideoBody(decision, model, cfg, body)
		decision.DroppedFields = intent.DroppedFields(body, intent.VideoFieldWhitelist)
		bodyFor = s.mediaBodyFor(upstreamBody, settings, decision, poolClass, intent.Video, model)
	} else {
		// 兜底：作为文本处理
		bodyFor = func(a *config.Account) ([]byte, string) {
			if intent.IsAutoModel(requested) || requested == "" {
				manifest := pool.ManifestOf(a, settings)
				chosen := intent.ChooseModel(manifest.Text, autoIntentConfig(settings).PreferredModels["text"])
				if chosen == "" {
					chosen = pool.FallbackModel["text"]
				}
				clone := make(map[string]any, len(body))
				for k, v := range body {
					clone[k] = v
				}
				clone["model"] = chosen
				buf, _ := json.Marshal(clone)
				return buf, chosen
			}
			resolved := pool.ResolveModel(requested, settings.ModelAliases)
			clone := cloneBody(body)
			clone["model"] = resolved
			buf, _ := json.Marshal(clone)
			return buf, resolved
		}
	}

	sessionKey := s.Hub.SessionKey(headerMap(r), "")
	isIdempotent := strings.HasPrefix(path, "GET /v1/videos/") || strings.Contains(path, "videos/")
	opts := relay.Options{
		SessionKey: sessionKey, PoolClass: poolClass,
		RequiredModel: model, Method: http.MethodPost, Path: path,
		BodyFor: bodyFor, Idempotent: isIdempotent,
	}

	result, err := relay.Do(r.Context(), s.Hub, s.Client, opts)
	if err != nil {
		writeErr(w, relayError(err))
		return
	}
	raw := result.ReadAll()
	decision.ModelUsed = result.ModelUsed
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
}
