// Package hub 是调度核心：候选筛选、粘性路由与溢出、槽位领取、故障熔断、二维自适应校准。
//
// 与 Python 基线相比的四处关键修正（都是会在真实多账号场景下静默失效的缺陷）：
//
//	B1 软粘性 + 溢出。旧实现命中绑定就直接返回该账号，不检查惩罚、不比较负载，
//	   而客户端默认不发会话头 → 同一把下游 Key 的全部请求永久绑到同一个账号，
//	   配了 5 个账号实际只有 1 个在工作；且绑定账号一旦 429，请求会集体睡过
//	   冷却而不是切到健康账号。现在绑定只是「优先候选」，超过溢出阈值即改派并更新绑定。
//	B2 校准因子降维到 (账号 × 池)。旧实现是一个账号级标量被六个池共用 → 视频池
//	   （只有 1 RPM、最容易 429）一次限流会把该账号的文本池也砍掉 20%。
//	B3 并发信号量按模态分离。旧实现账号级一把 → 视频轮询会把文本请求挤住。
//	B4 排队准入提前到领号之前（见 pacer.Reserve）→ 超时的请求不再白烧 RPM 槽位。
package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/pacer"
	"agneshub/internal/pool"
)

// ErrNoCapacity 该池下没有任何可用账号。
var ErrNoCapacity = errors.New("没有可用账号")

// ErrQueueFull / ErrQueueTimeout 真背压。
var (
	ErrQueueFull    = errors.New("排队已满")
	ErrQueueTimeout = errors.New("排队等待超过上限")
)

// Metrics 是运行指标。
type Metrics struct {
	RequestsTotal  atomic.Int64
	RequestsOK     atomic.Int64
	RequestsError  atomic.Int64
	Upstream429    atomic.Int64
	QueuedTotal    atomic.Int64
	QueueTimeout   atomic.Int64
	QueueOverflow  atomic.Int64
	WaitMS         atomic.Int64
	Spillovers     atomic.Int64
	BreakerOpened  atomic.Int64
	BreakerRevived atomic.Int64
	// PanicsTotal 记录被 recover 兜住并已降级（而非进程崩溃）的 panic 次数，
	// 供 /metrics 观测与「挂机健康度」判断（见 P0-2）。
	PanicsTotal atomic.Int64
	// LastSuccessNS 全局最近一次上游成功的 UnixNano 时间戳，
	// 供 /healthz 的 last_success_age_ms（「最近多久有流量真正跑通」）观测。
	LastSuccessNS atomic.Int64
	StartedAt     time.Time
}

// Hub 是调度器。
type Hub struct {
	store *config.Store
	cfg   atomic.Pointer[config.Settings]

	mu            sync.Mutex
	pacers        map[string]*pacer.Pacer
	sems          map[string]chan struct{}
	penaltyUntil  map[string]time.Time
	last429       map[string]time.Time
	inflight      map[string]int
	poolFactors   map[string]float64
	successes     map[string]int
	reviveAt      map[string]time.Time
	pendingFactor map[string]bool

	// Arrivals 到达密度环形缓冲区：记录最近 ARRIVAL_WINDOW 内的请求到达时间戳。
	// 用于控制台展示「到达密度 vs 节拍」，帮助判断宿主是否在并发发请求、
	// 多账号是否真正被吃到。单 goroutine 写入（调度主循环），控制台读取时加锁。
	Arrivals arrivalRing

	// Recent429 记录最近 60s 内真实命中上游 429 的时间戳（有界环形），
	// 供 /healthz 的 upstream_429_rate_per_min（「最近一分钟到底有多少次限流」）。
	Recent429 rate429Ring

	// #22 两级队列：highWaiting 统计「某账号 × 某池」当前处于 Reserve 领槽区间的
	// 非流式（高优先级）请求数。流式（低优先级）在领槽前会限时等它清零，从而让
	// 短促的工具调用（非流式）在文本池里优先于长回答（流式）拿到节拍槽位。
	// 只用 h.mu 保护（短临界区，不嵌套），与 Pacer() 共享同一把锁互斥安全。
	highWaiting map[string]int

	// Metrics 是运行指标（/healthz 与控制台观测）。
	Metrics Metrics
}

// rate429Ring 记录最近 60s 内 429 时间戳，PerMinute() 返回窗口内命中次数。
const RATE429_WINDOW = 60 * time.Second

type rate429Ring struct {
	mu    sync.Mutex
	items [512]time.Time
	n     int
	pos   int
}

// record 记录一次 429。
func (r *rate429Ring) record() {
	r.mu.Lock()
	now := time.Now()
	r.items[r.pos%len(r.items)] = now
	r.pos++
	if r.n < len(r.items) {
		r.n++
	}
	r.mu.Unlock()
}

// PerMinute 返回最近 60s 内 429 命中次数（惰性滑出窗口）。
func (r *rate429Ring) PerMinute() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	c := 0
	for i := 0; i < r.n; i++ {
		idx := (r.pos - r.n + i + len(r.items)) % len(r.items)
		if now.Sub(r.items[idx]) <= RATE429_WINDOW {
			c++
		}
	}
	return c
}

// arrivalRing 是固定容量环形时间戳数组（并发安全）。
const ARRIVAL_WINDOW = 60 * time.Second

type arrivalRing struct {
	mu    sync.Mutex
	items [512]time.Time
	n     int    // 有效条目数（≤ 容量）
	pos   int    // 下一个写入位置
	total int64  // 总计数（含已滑出窗口的）
}

// Add 记录一次请求到达。
func (r *arrivalRing) Add() {
	r.mu.Lock()
	now := time.Now()
	r.items[r.pos%len(r.items)] = now
	r.pos++
	if r.n < len(r.items) {
		r.n++
	}
	r.total++
	r.mu.Unlock()
}

// Counts 返回 (窗口内总数, 秒级时间桶切片、每秒均值)。
// 秒桶长度 60，下标 0 = 60s 前、59 = 当前秒，控制台渲染迷你直方图用。
// 滑出窗口的条目从 n 中扣除（惰性，下一次 Counts 才真正清理）。
func (r *arrivalRing) Counts() (total int, perSecond []int, avgPerSec float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	perSecond = make([]int, 60)
	active := 0
	// 遍历有效条目，滑出窗口的标记为 0
	for i := 0; i < r.n; i++ {
		idx := (r.pos - r.n + i + len(r.items)) % len(r.items)
		t := r.items[idx]
		age := now.Sub(t)
		if age > ARRIVAL_WINDOW {
			r.items[idx] = time.Time{} // 清掉避免下次再算
			continue
		}
		sec := int(age.Seconds())
		bucket := 59 - sec
		if bucket < 0 || bucket > 59 {
			continue
		}
		perSecond[bucket]++
		active++
	}
	// 把滑出窗口但还没清掉的条目从 n 中扣除（最多 512 次，无性能问题）
	valid := 0
	for i := 0; i < r.n; i++ {
		idx := (r.pos - r.n + i + len(r.items)) % len(r.items)
		if !r.items[idx].IsZero() && now.Sub(r.items[idx]) <= ARRIVAL_WINDOW {
			valid++
		}
	}
	r.n = valid
	avgPerSec = float64(valid) / 60.0
	return valid, perSecond, avgPerSec
}

// TotalCount 返回到达总数（进程启动以来）。
func (r *arrivalRing) TotalCount() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// New 构造调度器。
func New(store *config.Store) *Hub {
	h := &Hub{
		store:         store,
		pacers:        map[string]*pacer.Pacer{},
		sems:          map[string]chan struct{}{},
		penaltyUntil:  map[string]time.Time{},
		last429:       map[string]time.Time{},
		inflight:      map[string]int{},
		poolFactors:   map[string]float64{},
		successes:     map[string]int{},
		reviveAt:      map[string]time.Time{},
		pendingFactor: map[string]bool{},
		highWaiting:   map[string]int{}, // #22 两级队列：非流式领槽区间计数
	}
	h.Metrics.StartedAt = time.Now()
	h.Reload()
	return h
}

func key(parts ...string) string { return strings.Join(parts, "|") }

// Settings 返回缓存的设置快照（热路径不重复拷贝 map）。
func (h *Hub) Settings() config.Settings {
	if p := h.cfg.Load(); p != nil {
		return *p
	}
	return h.store.SettingsSnapshot()
}

// Reload 重新读取设置与账号配置，重建节拍器与信号量。
//
// 保留已有 Pacer 的排队状态（reconfigure 而非重建），否则调参会清空正在排队的请求。
func (h *Hub) Reload() {
	settings := h.store.SettingsSnapshot()
	h.cfg.Store(&settings)

	h.mu.Lock()
	defer h.mu.Unlock()

	live := map[string]bool{}
	for _, a := range h.store.AccountsSnapshot() {
		// 池因子初始化（首次见到该账号时从落盘值装载）
		for _, cls := range config.PoolClasses {
			k := key(a.ID, cls)
			live[k] = true
			if _, ok := h.poolFactors[k]; !ok {
				if v, ok := a.PoolFactors[cls]; ok && v > 0 {
					h.poolFactors[k] = v
				} else {
					h.poolFactors[k] = 1.0
				}
			}
			rpm := h.effectiveRPMLocked(a, cls, settings)
			if p, ok := h.pacers[k]; ok {
				p.Reconfigure(rpm, settings.PacingWindowSec)
			} else {
				h.pacers[k] = pacer.New(rpm, settings.PacingWindowSec)
			}
		}
		for _, modality := range []string{"text", "image", "video"} {
			sk := key(a.ID, modality)
			live[sk] = true
			if cap := h.concurrencyFor(a, modality, settings); cap > 0 {
				if ch, ok := h.sems[sk]; ok && cap == capOf(ch) {
					continue
				}
				h.sems[sk] = make(chan struct{}, cap)
			}
		}
	}
	for k := range h.pacers {
		if !live[k] {
			delete(h.pacers, k)
		}
	}
	for k := range h.sems {
		if !live[k] {
			delete(h.sems, k)
		}
	}
}

func capOf(ch chan struct{}) int { return cap(ch) }

func (h *Hub) concurrencyFor(a *config.Account, modality string, s config.Settings) int {
	switch modality {
	case "image":
		if s.ImageConcurrency > 0 && s.ImageConcurrency < a.MaxConcurrency {
			return s.ImageConcurrency
		}
		return a.MaxConcurrency
	case "video":
		if s.VideoMaxInFlight > 0 {
			return s.VideoMaxInFlight
		}
		return 1
	default:
		return maxInt(1, a.MaxConcurrency)
	}
}

// ---------------------------------------------------------------------------
// 有效 RPM 与二维校准
// ---------------------------------------------------------------------------

// learnedFactorOf 账号级保守系数：只来自旧数据迁移或管理端手工设置。
//
// 运行期自适应**刻意不再改写它** —— 它乘在全部池上，一旦被单个池的 429 写入，
// 惩罚就会横向传染（见 OnRateLimited 的说明）。自适应一律走 poolFactors。
func (h *Hub) learnedFactorOf(a *config.Account) float64 {
	if a.LearnedFactor <= 0 {
		return 1
	}
	return a.LearnedFactor
}

func (h *Hub) effectiveRPMLocked(a *config.Account, poolClass string, s config.Settings) float64 {
	base := baseRPM(a, poolClass, s.FreeTextRPM)
	factor := h.poolFactors[key(a.ID, poolClass)]
	if factor <= 0 {
		factor = 1
	}
	safety := s.SafetyFactor
	if safety <= 0 {
		safety = 0.9
	}
	return math.Max(0.01, base*safety*h.learnedFactorOf(a)*factor)
}

func baseRPM(a *config.Account, poolClass string, freeTextRPM float64) float64 {
	if v, ok := a.RPMOverrides[poolClass]; ok && v > 0 {
		return v
	}
	// agnes 免费档文本池的 RPM 改为可配置项（FreeTextRPM）；0 表示用档位表。
	if a.AccessType == "free" && poolClass == "text" && freeTextRPM > 0 {
		return freeTextRPM
	}
	table, ok := config.RPMTable[a.AccessType]
	if !ok {
		table = config.RPMTable["free"]
	}
	if v, ok := table[poolClass]; ok {
		return v
	}
	return 1
}

// EffectiveRPM 对外暴露的有效 RPM。
func (h *Hub) EffectiveRPM(a *config.Account, poolClass string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.effectiveRPMLocked(a, poolClass, h.Settings())
}

// Pacer 取（或惰性创建）某账号某池的节拍器。
func (h *Hub) Pacer(a *config.Account, poolClass string) *pacer.Pacer {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := key(a.ID, poolClass)
	if p, ok := h.pacers[k]; ok {
		return p
	}
	p := pacer.New(h.effectiveRPMLocked(a, poolClass, h.Settings()), h.Settings().PacingWindowSec)
	h.pacers[k] = p
	return p
}

// Semaphore 取（或惰性创建）某账号某模态的并发信号量。
func (h *Hub) Semaphore(a *config.Account, modality string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := key(a.ID, modality)
	if ch, ok := h.sems[k]; ok {
		return ch
	}
	ch := make(chan struct{}, maxInt(1, h.concurrencyFor(a, modality, h.Settings())))
	h.sems[k] = ch
	return ch
}

// ReloadPacer 只收紧/放宽「单个账号 × 单个池」的节拍器，避免 429 高频路径上做
// 全量快照 Reload()（O(账号数 × 池数) 的重新初始化，429 风暴时每个请求都拖一次）。
// 调用前必须已更新完 poolFactors（本方法只读当前因子重算 effectiveRPM）。
func (h *Hub) ReloadPacer(a *config.Account, poolClass string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.Settings()
	k := key(a.ID, poolClass)
	rpm := h.effectiveRPMLocked(a, poolClass, s)
	if p, ok := h.pacers[k]; ok {
		p.Reconfigure(rpm, s.PacingWindowSec)
	} else {
		h.pacers[k] = pacer.New(rpm, s.PacingWindowSec)
	}
}

// ---------------------------------------------------------------------------
// 候选筛选
// ---------------------------------------------------------------------------

// DeclaredModels 账号在 model_manifest 里声明的、属于该池的模型。
//
// 刻意返回非 nil 空切片而不是 nil：这个结果会直接进控制台 JSON，
// nil 会序列化成 `null`，前端拿到 null 再做 `.length` / 迭代就会炸。
func (h *Hub) DeclaredModels(a *config.Account, poolClass string) []string {
	modality := pool.ModalityOfPool(poolClass)
	manifest := pool.ManifestOf(a, h.Settings())
	out := []string{}
	for _, m := range manifest.For(modality) {
		if strings.TrimSpace(m) != "" {
			out = append(out, strings.TrimSpace(m))
		}
	}
	return out
}

// Capable 账号能否承载该池：既要在 classes_enabled 里，也要声明了对应模态的模型。
//
// 第二条是「多账号按模态分工」的落点：未声明（或主动清空）某模态的账号不再被
// 调度到该模态的池，避免把生图请求打到只买了文本额度的账号上白白触发 403。
func (h *Hub) Capable(a *config.Account, poolClass string) bool {
	enabled := false
	for _, c := range a.ClassesEnabled {
		if c == poolClass || c == "*" {
			enabled = true
			break
		}
	}
	if len(a.ClassesEnabled) == 0 {
		enabled = true
	}
	if !enabled {
		return false
	}
	return len(h.DeclaredModels(a, poolClass)) > 0
}

// Declares 账号是否声明了某个具体模型。
func (h *Hub) Declares(a *config.Account, poolClass, model string) bool {
	if model == "" {
		return false
	}
	for _, m := range h.DeclaredModels(a, poolClass) {
		if m == model {
			return true
		}
	}
	return false
}

// Candidates 候选账号。requiredModel 非空时优先返回声明了该模型的账号；
// 若无人声明则退回「声明了该模态」的全集 —— 宁可让上游给出明确错误，
// 也不要因为配置漏填就把请求挡在门外。
func (h *Hub) Candidates(poolClass string, exclude map[string]bool, requiredModel string) []*config.Account {
	h.mu.Lock()
	now := time.Now()
	// 熔断到期自动复活（被动触发，避免依赖后台定时器）
	var revive []string
	for id, at := range h.reviveAt {
		if !at.IsZero() && now.After(at) {
			revive = append(revive, id)
		}
	}
	for _, id := range revive {
		delete(h.reviveAt, id)
	}
	h.mu.Unlock()
	for _, id := range revive {
		h.tryRevive(id)
	}

	var base []*config.Account
	for _, a := range h.store.AccountsSnapshot() {
		if !a.Enabled || strings.TrimSpace(a.APIKey) == "" {
			continue
		}
		if exclude != nil && exclude[a.ID] {
			continue
		}
		if !h.Capable(a, poolClass) {
			continue
		}
		base = append(base, a)
	}
	if requiredModel != "" && len(base) > 0 {
		var declared []*config.Account
		for _, a := range base {
			if h.Declares(a, poolClass, requiredModel) {
				declared = append(declared, a)
			}
		}
		if len(declared) > 0 {
			return declared
		}
	}
	return base
}

func (h *Hub) tryRevive(id string) {
	h.store.MutateAccount(id, func(a *config.Account) bool {
		if a.Enabled {
			return false
		}
		// 自动复活：置回启用，但**不做清零**——保留已下调的校准系数，
		// 让该账号以更慢的节拍试探恢复，而不是复活瞬间再撞一次 429。
		a.Enabled = true
		a.Stats.LastError = "熔断冷却结束，已自动复活（低速试探）"
		return true
	})
	h.Metrics.BreakerRevived.Add(1)
	h.Reload()
}

// StartMaintenance 启动后台维护：熔断复活 + 到期绑定清理 + 因子落盘。
func (h *Hub) StartMaintenance(ctx context.Context) {
	go func() {
		// P0-2：维护循环是「挂机无人值守」的关键后台任务，任何一次 panic 都不得让
		// 它退出（否则 30s 的熔断复活 / 因子落盘全停）。每轮单独 recover。
		defer func() {
			if e := recover(); e != nil {
				h.Metrics.PanicsTotal.Add(1)
				log.Printf("[panic-recovered] maintenance loop: %v\n%s", e, debug.Stack())
			}
		}()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				h.flushFactors()
				return
			case <-ticker.C:
				func() {
					defer func() {
						if e := recover(); e != nil {
							h.Metrics.PanicsTotal.Add(1)
							log.Printf("[panic-recovered] maintenance tick: %v\n%s", e, debug.Stack())
						}
					}()
					h.flushFactors()
				}()
			}
		}
	}()
}

// flushFactors 把二维校准因子合并落盘（降写放大：30 秒一次批量写，而不是每次 429 都写）。
// 同时把 429/成功/熔断 路径里改过但尚未写盘的账号统计一并落盘。
func (h *Hub) flushFactors() {
	h.mu.Lock()
	dirty := make([]string, 0, len(h.pendingFactor))
	for k := range h.pendingFactor {
		dirty = append(dirty, k)
	}
	h.pendingFactor = map[string]bool{}
	values := make(map[string]float64, len(dirty))
	for _, k := range dirty {
		values[k] = h.poolFactors[k]
	}
	h.mu.Unlock()
	if len(dirty) > 0 {
		h.store.MutateAccountMap(values, dirty)
	}
	// 兜底：把统计字段（RateLimited / LastError / 熔断状态等）批量写一次，
	// 热路径里改这些字段的代码一律走 MutateAccountNoSave，这里统一落盘。
	h.store.FlushAccounts()
	// #20 热路径异步落盘：批量写下游密钥（ChargeKey 只改内存）+ 批量追加 usage 日志
	// （AppendUsage 只入内存缓冲）。与 429 不写盘同口径，挂机丢 ≤30s 可接受。
	h.store.FlushKeys()
	h.store.FlushUsage()
}

// BindFactor 立即把某个 (账号 × 池) 因子落盘（控制台重置时用）。
func (h *Hub) BindFactor(accountID, poolClass string, value float64) {
	h.mu.Lock()
	h.poolFactors[key(accountID, poolClass)] = value
	h.pendingFactor[key(accountID, poolClass)] = true
	h.mu.Unlock()
	h.flushFactors()
}

// FactorOf 读取二维校准因子。
func (h *Hub) FactorOf(accountID, poolClass string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.poolFactors[key(accountID, poolClass)]
	if v <= 0 {
		return 1
	}
	return v
}

// ResetFactors 把某账号全部池的校准因子复位为 1。
func (h *Hub) ResetFactors(accountID string) {
	h.mu.Lock()
	for _, cls := range config.PoolClasses {
		h.poolFactors[key(accountID, cls)] = 1.0
		h.pendingFactor[key(accountID, cls)] = true
	}
	h.mu.Unlock()
	h.flushFactors()
}

// ---------------------------------------------------------------------------
// 粘性会话
// ---------------------------------------------------------------------------

// SessionKey 计算粘性键（口径 C：先看会话头，无则回落下游 Key）。
func (h *Hub) SessionKey(headers map[string]string, downstreamKey string) string {
	mode := strings.ToLower(h.Settings().AffinityMode)
	if mode == "none" {
		return ""
	}
	session := strings.TrimSpace(headers["x-baipiao-session"])
	if session == "" {
		session = strings.TrimSpace(headers["x-session-id"])
	}
	if session == "" {
		session = strings.TrimSpace(headers["x-task-id"])
	}
	switch mode {
	case "key":
		return "key:" + downstreamKey
	case "session":
		if session != "" {
			return "ses:" + session
		}
		return ""
	default: // session_then_key
		if session != "" {
			return "ses:" + session
		}
		return "key:" + downstreamKey
	}
}

// ---------------------------------------------------------------------------
// 选账号（软粘性 + 溢出）
// ---------------------------------------------------------------------------

const spillPenalty = 3 * time.Second // 绑定账号预计等待超过此值就让位

// PickResult 是选号结果。
type PickResult struct {
	Account *config.Account
	Penalty time.Duration
	Spilled bool // 本次是否从粘性绑定溢出了出去
	Bound   *config.Account
}

// pickWithPriority 选择账号：优先级（数字越大越优先）为第一排序键，
// 同优先级内按「首选区域 → 预计等待 → 在途」排序。这样手动调高的账号会稳定顶到前面，
// 而区域优先级退化为同优先级内的次级偏好（与「cn_first / com_first」语义一致）。
func (h *Hub) pickWithPriority(poolClass string, exclude map[string]bool, requiredModel string) *config.Account {
	s := h.Settings()
	primaryCN := s.RegionPriority != "com_first" // 默认 cn_first

	bestAcc := (*config.Account)(nil)
	bestScore := pickScore{}
	init := false
	for _, a := range h.Candidates(poolClass, exclude, requiredModel) {
		sc := pickScore{
			priority:    a.Priority,
			regionMatch: config.IsCNHost(a.BaseURL) == primaryCN,
			wait:        h.Pacer(a, poolClass).ProjectedWait(),
			inflight:    h.Inflight(a.ID),
		}
		if !init || sc.betterThan(bestScore) {
			bestAcc, bestScore, init = a, sc, true
		}
	}
	return bestAcc
}

// pickScore 是候选账号的排序评分；betterThan 越大越优。
type pickScore struct {
	priority    int
	regionMatch bool
	wait        time.Duration
	inflight    int
}

func (s pickScore) betterThan(o pickScore) bool {
	if s.priority != o.priority {
		return s.priority > o.priority
	}
	if s.regionMatch != o.regionMatch {
		return s.regionMatch // 同优先级内首选区域更优
	}
	if s.wait != o.wait {
		return s.wait < o.wait
	}
	return s.inflight < o.inflight
}

// Pick 选择账号。
//
// 优先级：钉死账号 > 粘性绑定（未溢出）> 预计等待最短（按区域优先级）。
// 全部账号处于冷却时**不退化为报错**，而是等最早解冻的那个 ——
// 无人值守场景下「等一下」远好于「任务断掉」。
func (h *Hub) Pick(sessionKey, poolClass, pinned, requiredModel string, exclude map[string]bool) (PickResult, error) {
	if pinned != "" && (exclude == nil || !exclude[pinned]) {
		if a := h.store.AccountByID(pinned); a != nil && a.Enabled && strings.TrimSpace(a.APIKey) != "" &&
			h.Capable(a, poolClass) && (exclude == nil || !exclude[pinned]) {
			return PickResult{Account: a, Penalty: h.PenaltyRemaining(a.ID)}, nil
		}
	}

	var bound *config.Account
	if sessionKey != "" {
		if b, ok := h.store.BindingsGet(sessionKey); ok {
			if a := h.store.AccountByID(b.AccountID); a != nil && a.Enabled &&
				strings.TrimSpace(a.APIKey) != "" && h.Capable(a, poolClass) &&
				(exclude == nil || !exclude[a.ID]) {
				// 软粘性绑定只在该账号能承载本次请求的「具体模型」时才生效。
				// 否则（例如同一会话先用了 AMD 模型、后切到 agnes 模型）应放弃旧绑定，
				// 让调度器按 requiredModel 重新选号，避免把 agnes 请求错发给 AMD 账号。
				if requiredModel == "" || h.Declares(a, poolClass, requiredModel) {
					bound = a
				}
			}
		}
	}

	healthy := func(list []*config.Account) []*config.Account {
		var out []*config.Account
		for _, a := range list {
			if h.PenaltyRemaining(a.ID) <= 0 {
				out = append(out, a)
			}
		}
		return out
	}

	candidates := h.Candidates(poolClass, exclude, requiredModel)
	poolCandidates := healthy(candidates)

	// --- 软粘性：绑定账号健康且不拥堵时沿用，否则溢出改派 ---
	if bound != nil && h.PenaltyRemaining(bound.ID) <= 0 {
		wait := h.Pacer(bound, poolClass).ProjectedWait()
		alt := bestOf(h, poolCandidates, poolClass, bound.ID)
		if wait <= spillPenalty || alt == nil {
			return PickResult{Account: bound, Penalty: 0, Bound: bound}, nil
		}
		h.Metrics.Spillovers.Add(1)
		h.store.Bind(sessionKey, alt.ID)
		return PickResult{Account: alt, Penalty: h.PenaltyRemaining(alt.ID), Spilled: true, Bound: bound}, nil
	}

	// 按区域优先级选择：先试首选区域，再 fallback
	chosen := h.pickWithPriority(poolClass, exclude, requiredModel)
	if chosen == nil {
		// 兜底：忽略 exclude 再查一轮（全员冷却时等最早解冻）
		chosen = h.pickWithPriority(poolClass, nil, requiredModel)
		if chosen == nil {
			all := h.Candidates(poolClass, nil, requiredModel)
			if len(all) == 0 {
				return PickResult{}, fmt.Errorf("%w：池分类 %s 下没有可用账号（未配置 / 已停用 / 未声明该模态）",
					ErrNoCapacity, poolClass)
			}
			var bestAcc *config.Account
			bestWait := time.Duration(0)
			for _, a := range all {
				w := h.PenaltyRemaining(a.ID)
				if bestAcc == nil || a.Priority > bestAcc.Priority || (a.Priority == bestAcc.Priority && w < bestWait) {
					bestAcc, bestWait = a, w
				}
			}
			return PickResult{Account: bestAcc, Penalty: bestWait}, nil
		}
	}

	if sessionKey != "" {
		h.store.Bind(sessionKey, chosen.ID)
	}
	return PickResult{Account: chosen, Penalty: h.PenaltyRemaining(chosen.ID)}, nil
}

// bestOf 在候选里挑预计等待最短者；同等待时比在途请求数。
func bestOf(h *Hub, candidates []*config.Account, poolClass, skipID string) *config.Account {
	var best *config.Account
	var bestWait time.Duration
	bestInflight := 0
	for _, a := range candidates {
		if a.ID == skipID {
			continue
		}
		w := h.Pacer(a, poolClass).ProjectedWait()
		inflight := h.Inflight(a.ID)
		if best == nil || w < bestWait || (w == bestWait && inflight < bestInflight) {
			best, bestWait, bestInflight = a, w, inflight
		}
	}
	return best
}

// PenaltyRemaining 该账号的冷却剩余时长。
func (h *Hub) PenaltyRemaining(accountID string) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.penaltyUntil[accountID]
	if !ok {
		return 0
	}
	if d := time.Until(until); d > 0 {
		return d
	}
	return 0
}

// Inflight 该账号当前在途请求数。
func (h *Hub) Inflight(accountID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inflight[accountID]
}

// TrackInflight 调整在途计数。
func (h *Hub) TrackInflight(accountID string, delta int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inflight[accountID] = maxInt(0, h.inflight[accountID]+delta)
}

// InflightTotal 全部账号当前在途请求总数（优雅停机据此判断「等待在途完成」）。
func (h *Hub) InflightTotal() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	total := 0
	for _, n := range h.inflight {
		total += n
	}
	return total
}

// ---------------------------------------------------------------------------
// 富健康度观测（P1-4：/healthz 不仅报 ok，还报「网关到底能不能干活」）
// ---------------------------------------------------------------------------

// QueueDepth 当前排队的请求总数（所有池的 pacer.Waiting() 之和）。
func (h *Hub) QueueDepth() int {
	h.mu.Lock()
	total := 0
	for _, p := range h.pacers {
		total += p.Waiting()
	}
	h.mu.Unlock()
	return total
}

// BreakerOpenCount 当前处于熔断（已禁用、等待复活）中的账号数。
func (h *Hub) BreakerOpenCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	n := 0
	for _, at := range h.reviveAt {
		if now.Before(at) {
			n++
		}
	}
	return n
}

// LastSuccessAgeMS 距全局最近一次上游成功的毫秒数；从未成功则 -1。
func (h *Hub) LastSuccessAgeMS() int64 {
	ns := h.Metrics.LastSuccessNS.Load()
	if ns == 0 {
		return -1
	}
	return time.Since(time.Unix(0, ns)).Milliseconds()
}

// Upstream429RatePerMin 最近 60s 真实命中上游 429 的次数（富健康度信号）。
func (h *Hub) Upstream429RatePerMin() int {
	return h.Recent429.PerMinute()
}

// PoolMaxProjectedWaitMS 取某池所有「启用」账号 pacer 的最大投影等待（毫秒）。
// #21：供 /healthz 的 text_projected_wait_ms（排队可观测）与上游 429 兜底
// Retry-After（按池深度算，而非写死 5s）复用。无启用账号或该池无流量则返回 0。
func (h *Hub) PoolMaxProjectedWaitMS(poolClass string) int64 {
	max := int64(0)
	for _, a := range h.store.AccountsSnapshot() {
		if !a.Enabled {
			continue
		}
		if w := h.Pacer(a, poolClass).ProjectedWait().Milliseconds(); w > max {
			max = w
		}
	}
	return max
}

// PanicsTotal 已被 recover 兜住的 panic 次数。
func (h *Hub) PanicsTotal() int64 {
	return h.Metrics.PanicsTotal.Load()
}

// ---------------------------------------------------------------------------
// 领槽位
// ---------------------------------------------------------------------------

// Acquire 等待并领取一个发送槽位，返回排队等待时长。
//
// lowPriority：#22 两级队列。文本池内「非流式（工具/短调用）= 高优先级、流式（长生成）
// = 低优先级」。开启 TextTwoLevelQueue 且池为 text 时，流式请求（lowPriority=true）在
// 领槽前会限时等本账号非流式请求的领槽区间（highWaiting）清零，让短促调用优先拿到
// 节拍槽位；领到的仍是下一个空闲槽（绝不超车已领槽位），RPM 安全节拍不变。
func (h *Hub) Acquire(ctx context.Context, a *config.Account, poolClass string, lowPriority bool) (time.Duration, error) {
	s := h.Settings()
	maxWait := time.Duration(s.QueueMaxWaitMS) * time.Millisecond
	limitMS := s.QueueMaxWaitMS

	started := time.Now()
	if penalty := h.PenaltyRemaining(a.ID); penalty > 0 {
		select {
		case <-time.After(penalty):
		case <-ctx.Done():
			return time.Since(started), ctx.Err()
		}
	}
	// #22 两级队列：仅文本池 + 开启开关 + 本请求为低优先级（流式）时，先限时让位。
	// 轮询 highWaiting[k]==0（尊重 ctx 与剩余预算），避免高优先级（非流式）还在领槽时
	// 流式请求插队抢占节拍。等待本身计入总排队时长。
	if lowPriority && s.TextTwoLevelQueue && poolClass == "text" {
		k := key(a.ID, poolClass)
		highWaitingClear := func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.highWaiting[k] == 0
		}
		if !highWaitingClear() {
			deadline := started.Add(maxWait)
			for !highWaitingClear() {
				remain := time.Until(deadline)
				if remain <= 0 {
					break // 预算耗尽，不再让位（继续按剩余 0 预算领槽，由 Reserve 判定）
				}
				select {
				case <-time.After(min(remain, 50*time.Millisecond)):
				case <-ctx.Done():
					return time.Since(started), ctx.Err()
				}
			}
		}
	}
	// 高优先级（非流式）在领槽区间内标记 highWaiting，让后到的低优先级让位。
	k := key(a.ID, poolClass)
	if !lowPriority {
		h.mu.Lock()
		h.highWaiting[k]++
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			if h.highWaiting[k] > 0 {
				h.highWaiting[k]--
			}
			h.mu.Unlock()
		}()
	}
	waited, err := h.Pacer(a, poolClass).Reserve(ctx, maxWait-time.Since(started), s.QueueMaxSize)
	if err != nil {
		switch {
		case errors.Is(err, pacer.ErrQueueFull):
			h.Metrics.QueueOverflow.Add(1)
			return time.Since(started), fmt.Errorf("%w（上限 %d）", ErrQueueFull, s.QueueMaxSize)
		case errors.Is(err, pacer.ErrTooLong):
			h.Metrics.QueueTimeout.Add(1)
			return time.Since(started), fmt.Errorf("%w %dms（实际需等待 %s）", ErrQueueTimeout, limitMS, waited)
		default:
			return time.Since(started), err
		}
	}
	total := time.Since(started)
	if total > maxWait {
		h.Metrics.QueueTimeout.Add(1)
		return total, fmt.Errorf("%w %dms（实际 %s）", ErrQueueTimeout, limitMS, total)
	}
	if total > 1500*time.Millisecond {
		h.Metrics.QueuedTotal.Add(1)
	}
	_ = waited
	return total, nil
}

// ---------------------------------------------------------------------------
// 反馈：429 / 成功 / 鉴权失败
// ---------------------------------------------------------------------------

// OnRateLimited 上游真的返回了 429 —— 我们高估了边界，触发该 (账号 × 池) 的自适应下调。
//
// 关键：只下调出问题的那一个池，且**绝不改写账号级系数**。
// effectiveRPM = 基线 × 安全系数 × 账号级系数 × 池因子，只要账号级系数被写一次，
// 惩罚就会横向传染到该账号的全部池：视频池只有 1 RPM、最容易 429，一次视频限流
// 会连带把文本池砍掉 20% —— 这正是「配了多账号、文本却一起变慢」的根因。
// 因此这里只动 pool_factors[池]。
func (h *Hub) OnRateLimited(a *config.Account, poolClass string) {
	s := h.Settings()
	h.Metrics.Upstream429.Add(1)
	h.Recent429.record() // P1-4：富健康度——最近 60s 429 速率

	floor := s.MinLearnedFactor
	if floor <= 0 {
		floor = 0.5
	}
	penalty := s.PenaltyFactor
	if penalty <= 0 {
		penalty = 0.8
	}
	cooldown := time.Duration(s.PenaltyCooldownSec * float64(time.Second))

	h.mu.Lock()
	k := key(a.ID, poolClass)
	factor := h.poolFactors[k]
	if factor <= 0 {
		factor = 1
	}
	if s.CalibrationEnabled {
		factor = math.Max(floor, factor*penalty)
		h.poolFactors[k] = factor
		h.pendingFactor[k] = true
	}
	h.last429[k] = time.Now()
	h.successes[k] = 0
	h.penaltyUntil[a.ID] = time.Now().Add(cooldown)
	h.mu.Unlock()

	// 429 路径不写盘：统计与池因子只在内存改，由 30s 维护循环（flushFactors →
	// store.FlushAccounts）批量落盘，避免 429 风暴时每个请求都阻塞一次磁盘 I/O
	// 拖慢整个换号重试循环。重启最坏丢 30s 内累计的 429 统计，可接受。
	h.store.MutateAccountNoSave(a.ID, func(acc *config.Account) bool {
		acc.Stats.RateLimited++
		acc.LastRateLimited = float64(time.Now().UnixNano()) / 1e9
		if s.CalibrationEnabled {
			if acc.PoolFactors == nil {
				acc.PoolFactors = map[string]float64{}
			}
			acc.PoolFactors[poolClass] = factor
		}
		return true
	})
	// P1-3/P2-4：429 高频路径只收紧受影响的「单个账号 × 单个池」节拍器，
	// 不再做全量快照 Reload()（O(账号×池) 重新初始化，429 风暴时拖慢换号循环）。
	// 账号集 / 设置在此路径不会变，仅这一个池因子变了，单 pacer reload 即可覆盖。
	h.ReloadPacer(a, poolClass)
}

// OnSuccess 按「成功次数」回升校准因子（而非按墙钟 —— 墙钟回升在低流量账号上
// 永远等不到，表现为「校准砍下去就再也回不来」）。
func (h *Hub) OnSuccess(a *config.Account, poolClass string) {
	s := h.Settings()
	if !s.CalibrationEnabled {
		return
	}
	need := s.RecoverSuccesses
	if need <= 0 {
		need = 20
	}
	h.mu.Lock()
	k := key(a.ID, poolClass)
	h.successes[k]++
	current := h.poolFactors[k]
	if current <= 0 {
		current = 1
	}
	recovered := false
	if h.successes[k] >= need && current < 1 {
		step := 0.05
		h.poolFactors[k] = math.Min(1, current+step)
		h.successes[k] = 0
		h.pendingFactor[k] = true
		recovered = true
	}
	h.mu.Unlock()
	if recovered {
		p := h.Pacer(a, poolClass)
		p.Reconfigure(h.EffectiveRPM(a, poolClass), s.PacingWindowSec)
	}
}

// OnAuthFailure 401/403/402 不可重试：立即熔断并安排自动复活。
//
// P2 增强：连续失败超过阈值时延长熔断冷却，让账号有更长的恢复窗口。
func (h *Hub) OnAuthFailure(a *config.Account, reason string) {
	s := h.Settings()
	baseReviveAfter := time.Duration(s.BreakerReviveSec) * time.Second
	if baseReviveAfter <= 0 {
		baseReviveAfter = 30 * time.Minute
	}
	// 连续失败超过阈值时，冷却时间指数增长（上限 2 倍）
	extended := baseReviveAfter
	if a.ConsecutiveFailures >= 5 {
		extended = time.Duration(float64(baseReviveAfter) * 1.5)
	}
	if a.ConsecutiveFailures >= 10 {
		extended = time.Duration(float64(baseReviveAfter) * 2.0)
	}
	h.mu.Lock()
	h.reviveAt[a.ID] = time.Now().Add(extended)
	h.mu.Unlock()
	h.Metrics.BreakerOpened.Add(1)

	_ = h.store.MutateAccountNoSave(a.ID, func(acc *config.Account) bool {
		acc.Enabled = false
		acc.Stats.Errors++
		acc.Stats.LastError = truncate(reason, 200) +
			fmt.Sprintf("（已熔断，%s 后自动复活低速试探）", extended)
		return true
	})
	h.Reload()
}

// NoteError 记录一次性错误（不熔断），并增加连续失败计数用于长期健康追踪。
func (h *Hub) NoteError(a *config.Account, reason string) {
	_ = h.store.MutateAccountNoSave(a.ID, func(acc *config.Account) bool {
		acc.Stats.Errors++
		acc.Stats.LastError = truncate(reason, 200)
		acc.ConsecutiveFailures++
		return true
	})
}

// NoteSuccess 记一次成功（统计），并重置连续失败计数。
func (h *Hub) NoteSuccess(a *config.Account) {
	_ = h.store.MutateAccountNoSave(a.ID, func(acc *config.Account) bool {
		acc.Stats.Requests++
		acc.Stats.LastUsedAt = float64(time.Now().UnixNano()) / 1e9
		acc.ConsecutiveFailures = 0
		return true
	})
	h.Metrics.RequestsOK.Add(1)
	// P1-4：全局最近成功时间戳（/healthz last_success_age_ms）。
	h.Metrics.LastSuccessNS.Store(time.Now().UnixNano())
}

// ---------------------------------------------------------------------------
// 观测
// ---------------------------------------------------------------------------

// Snapshot 生成控制台总览。
func (h *Hub) Snapshot() map[string]any {
	s := h.Settings()
	accounts := make([]map[string]any, 0)
	for _, a := range h.store.AccountsSnapshot() {
		buckets := map[string]any{}
		for _, cls := range config.PoolClasses {
			p := h.Pacer(a, cls)
			buckets[cls] = map[string]any{
				"rpm":               round2(p.RPM()),
				"waiting":           p.Waiting(),
				"projected_wait_ms": p.ProjectedWait().Milliseconds(),
				"factor":            round3(h.FactorOf(a.ID, cls)),
				"declared":          h.DeclaredModels(a, cls),
			}
		}
		accounts = append(accounts, map[string]any{
			"id":                   a.ID,
			"name":                 a.Name,
			"group":                a.Group,
			"enabled":              a.Enabled,
			"access_type":          a.AccessType,
			"classes_enabled":      a.ClassesEnabled,
			"model_manifest":       a.ModelManifest,
			"learned_factor":       round3(a.LearnedFactor),
			"penalty_remaining_ms": h.PenaltyRemaining(a.ID).Milliseconds(),
			"inflight":             h.Inflight(a.ID),
			"stats":                a.Stats,
			"buckets":              buckets,
		})
	}
	total := h.Metrics.RequestsTotal.Load()
	if total == 0 {
		total = 1
	}
	return map[string]any{
		"metrics": map[string]any{
			"started_at":      h.Metrics.StartedAt.Format("2006-01-02 15:04:05"),
			"uptime_sec":      int(time.Since(h.Metrics.StartedAt).Seconds()),
			"requests_total":  h.Metrics.RequestsTotal.Load(),
			"requests_ok":     h.Metrics.RequestsOK.Load(),
			"requests_error":  h.Metrics.RequestsError.Load(),
			"upstream_429":    h.Metrics.Upstream429.Load(),
			"queued_total":    h.Metrics.QueuedTotal.Load(),
			"queue_timeout":   h.Metrics.QueueTimeout.Load(),
			"queue_overflow":  h.Metrics.QueueOverflow.Load(),
			"spillovers":      h.Metrics.Spillovers.Load(),
			"breaker_opened":  h.Metrics.BreakerOpened.Load(),
			"breaker_revived": h.Metrics.BreakerRevived.Load(),
			"wait_ms_total":   h.Metrics.WaitMS.Load(),
		},
		"avg_wait_ms":  h.Metrics.WaitMS.Load() / total,
		"accounts":     accounts,
		"bindings":     len(h.store.BindingsSnapshot()),
		"keys":         len(h.store.KeysSnapshot()),
		"pool_classes": config.PoolClasses,
		// 到达密度 vs 节拍（多账号是否真被吃到的核心观测）
		"arrival": h.arrivalDensity(),
		"auto_routing": map[string]any{
			"enabled":        true,
			"model_name":     s.AutoModelName,
			"content_scan":   s.AutoIntent.ContentScan,
			"min_confidence": s.AutoIntent.MinConfidence,
		},
	}
}

// arrivalDensity 计算最近 60s 的到达密度，并给出文本池节拍的比值。
// 比值 >1 表示到达比节拍更密，多账号可线性扩容；<1 表示到达稀疏，
// 账号数不是瓶颈。
func (h *Hub) arrivalDensity() map[string]any {
	active, perSecond, avgPerSec := h.Arrivals.Counts()
	// 取任一账号的 text 池 RPM 作为基准间隔
	textRPM := 0.0
	h.mu.Lock()
	for k, p := range h.pacers {
		if strings.HasSuffix(k, "|text") && p.RPM() > 0 {
			textRPM = p.RPM()
			break
		}
	}
	h.mu.Unlock()
	textIntervalSec := 0.0
	if textRPM > 0 {
		textIntervalSec = 60.0 / textRPM
	}
	ratio := 0.0
	if textIntervalSec > 0 {
		ratio = avgPerSec * textIntervalSec
	}
	return map[string]any{
		"window_sec":       int(ARRIVAL_WINDOW.Seconds()),
		"active_60s":       active,
		"avg_per_sec":      round2(avgPerSec),
		"text_rpm":         round2(textRPM),
		"text_interval_ms": int(textIntervalSec * 1000),
		// ratio > 1 表示到达更密于节拍 → 多账号真正吃到
		"ratio": round2(ratio),
		"per_second": perSecond,
	}
}

// QueueView 队列视图。
func (h *Hub) QueueView() []map[string]any {
	var out []map[string]any
	for _, a := range h.store.AccountsSnapshot() {
		for _, cls := range config.PoolClasses {
			p := h.Pacer(a, cls)
			if p.Waiting() == 0 {
				continue
			}
			out = append(out, map[string]any{
				"account":           a.Name,
				"account_id":        a.ID,
				"pool_class":        cls,
				"waiting":           p.Waiting(),
				"rpm":               round2(p.RPM()),
				"projected_wait_ms": p.ProjectedWait().Milliseconds(),
			})
		}
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
