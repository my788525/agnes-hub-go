package hub

import (
	"context"
	"log"
	"sync"
	"time"
)

// 动态图表历史序列（任务管理器风格实时图的数据源）。
//
// 采样器每秒拍一个快照：负载率 / 到达 / 完成 / 排队 / 在途 / 429 / 每账号完成数，
// 存进细窗口（fine，1s 一格，10 分钟）与粗窗口（coarse，10s 一格，1 小时）两个环形缓冲。
// 控制台「总览第二屏」的 canvas 滚动图从 /api/metrics-history 拉取并自绘，无需刷新页面。

const (
	histFineCap    = 600 // 10 分钟 @1s
	histCoarseCap  = 360 // 1 小时 @10s
	histCoarseStep = 10  // 每 10 个 fine 样本聚成一个 coarse
)

// AccSample 是某账号在某秒的采样。
type AccSample struct {
	ID        string `json:"id"`
	Inflight  int    `json:"inflight"`
	DoneDelta int64  `json:"done_delta"` // 该秒完成的请求数（按账号 Stats.Requests 差分）
}

// HistorySample 是某秒的采样点。
type HistorySample struct {
	TS         int64       `json:"ts"`
	LoadPct    float64     `json:"load_pct"`   // 到达速率 ÷ text 池容量 × 100
	ArriveRPS  float64     `json:"arrive_rps"` // 当前秒到达
	DoneRPS    float64     `json:"done_rps"`   // 完成 req/s（差分）
	QueueDepth int         `json:"queue_depth"`
	Inflight   int         `json:"inflight"`
	// Pending 是「已进网关但还没走完」的转发请求数（读 body / 解析 / 意图判定 /
	// 排队 / 上游往返全算在内）。它是「低频大请求」场景下唯一能看出网关在干活的
	// 指标——负载百分比在这种场景恒接近 0，极易被误读成「网关没收到请求」。
	Pending int         `json:"pending"`
	R429PM     int         `json:"r429_pm"`
	Events     int         `json:"events"` // 位掩码：1=429 事件, 2=熔断打开
	Acc        []AccSample `json:"acc"`
}

// AccMeta 是账号图例元数据（颜色稳定，按 id 排序输出）。
type AccMeta struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// History 是双环形缓冲 + 采样器。
type History struct {
	mu       sync.Mutex
	fine     []HistorySample
	coarse   []HistorySample
	lastOK   int64
	lastAcc  map[string]int64
	last429  int64
	lastBO   int64
	primed   bool  // 首个采样点只建基线、不记增量（避免把历史累计当成本秒完成数）
	coarseN int64 // 已累计的 fine 样本数（用于聚合 coarse）
	coarseAcc struct {
		n         int
		sum       HistorySample
		firstTS   int64
		accDone   map[string]int64 // 聚合期间各账号 DoneDelta 累加
	}
}

// StartHistory 启动每秒采样器（须在 Hub 初始化之后调用，与 StartMaintenance 并列）。
func (h *Hub) StartHistory(ctx context.Context) {
	hist := &History{
		fine:    make([]HistorySample, 0, histFineCap),
		coarse:  make([]HistorySample, 0, histCoarseCap),
		lastAcc: map[string]int64{},
	}
	h.hist = hist
	go func() {
		defer func() {
			if e := recover(); e != nil {
				h.Metrics.PanicsTotal.Add(1)
				log.Printf("[panic-recovered] history sampler: %v", e)
			}
		}()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				func() {
					defer func() {
						if e := recover(); e != nil {
							h.Metrics.PanicsTotal.Add(1)
							log.Printf("[panic-recovered] history tick: %v", e)
						}
					}()
					hist.sample(h)
				}()
			}
		}
	}()
}

// sample 采集一秒并推入缓冲。
func (hi *History) sample(h *Hub) {
	// 到达密度（复用现有逻辑）
	arr := h.arrivalDensity()
	arriveRPS := 0.0
	if ps, ok := arr["per_second"].([]int); ok && len(ps) > 0 {
		arriveRPS = float64(ps[len(ps)-1]) // 末位 = 当前秒
	}
	// text 池容量（req/s）= Σ 启用账号 text RPM / 60
	capacity := 0.0
	h.mu.Lock()
	for k, p := range h.pacers {
		if len(k) > 5 && k[len(k)-5:] == "|text" && p.RPM() > 0 {
			capacity += p.RPM() / 60.0
		}
	}
	h.mu.Unlock()
	loadPct := 0.0
	if capacity > 0 {
		loadPct = arriveRPS / capacity * 100.0
	}

	// primed = 首个采样点。账号 Stats.Requests 是「持久化累计值」，基线从 0 起算会把
	// 重启前的历史总量当成第一秒的完成数，导致「多账号分配」图第一帧出现巨大尖峰。
	// 首个点只建立基线、不记增量。
	primed := hi.primed

	// 完成速率（全局差分）
	okNow := h.Metrics.RequestsOK.Load()
	doneRPS := float64(okNow - hi.lastOK)
	if doneRPS < 0 || !primed {
		doneRPS = 0 // 计数器回绕/重启，或首点只建基线
	}
	hi.lastOK = okNow

	// 429 / 熔断事件边沿检测
	r429Now := h.Metrics.Upstream429.Load()
	boNow := h.Metrics.BreakerOpened.Load()
	events := 0
	if primed && r429Now > hi.last429 {
		events |= 1
	}
	if primed && boNow > hi.lastBO {
		events |= 2
	}
	hi.last429 = r429Now
	hi.lastBO = boNow

	// 每账号 完成数差分
	acc := make([]AccSample, 0)
	for _, a := range h.store.AccountsSnapshot() {
		cur := a.Stats.Requests
		delta := cur - hi.lastAcc[a.ID]
		if delta < 0 || !primed {
			delta = 0
		}
		hi.lastAcc[a.ID] = cur
		acc = append(acc, AccSample{ID: a.ID, Inflight: h.Inflight(a.ID), DoneDelta: delta})
	}
	hi.primed = true

	now := time.Now().Unix()
	s := HistorySample{
		TS:         now,
		LoadPct:    round2(loadPct),
		ArriveRPS:  round2(arriveRPS),
		DoneRPS:    round2(doneRPS),
		QueueDepth: h.QueueDepth(),
		Inflight:   h.InflightTotal(),
		Pending:    int(h.Metrics.Pending.Load()),
		R429PM:     h.Upstream429RatePerMin(),
		Events:     events,
		Acc:        acc,
	}

	hi.mu.Lock()
	// fine 环形
	hi.fine = append(hi.fine, s)
	if len(hi.fine) > histFineCap {
		hi.fine = hi.fine[len(hi.fine)-histFineCap:]
	}
	// coarse 聚合
	ca := hi.coarseAcc
	ca.n++
	ca.sum.LoadPct += s.LoadPct
	ca.sum.ArriveRPS += s.ArriveRPS
	ca.sum.DoneRPS += s.DoneRPS
	ca.sum.QueueDepth += s.QueueDepth
	ca.sum.Inflight += s.Inflight
	ca.sum.R429PM += s.R429PM
	ca.sum.Events |= s.Events
	// pending 是「有没有请求卡在网关里」的信号，取窗口峰值而非平均值：
	// 平均会把一次短促的卡顿稀释成 0.1 再取整抹掉，正是我们要抓的东西。
	if s.Pending > ca.sum.Pending {
		ca.sum.Pending = s.Pending
	}
	if ca.accDone == nil {
		ca.accDone = map[string]int64{}
	}
	for _, a := range s.Acc {
		ca.accDone[a.ID] += a.DoneDelta
	}
	if ca.firstTS == 0 {
		ca.firstTS = s.TS
	}
	if ca.n >= histCoarseStep {
		div := float64(ca.n)
		agg := HistorySample{
			TS:         ca.firstTS,
			LoadPct:    round2(ca.sum.LoadPct / div),
			ArriveRPS:  round2(ca.sum.ArriveRPS / div),
			DoneRPS:    round2(ca.sum.DoneRPS / div),
			QueueDepth: ca.sum.QueueDepth / ca.n,
			Inflight:   ca.sum.Inflight / ca.n,
			Pending:    ca.sum.Pending, // 峰值，不平均
			R429PM:     int(float64(ca.sum.R429PM) / div),
			Events:     ca.sum.Events,
			Acc:        make([]AccSample, 0, len(ca.accDone)),
		}
		for _, a := range h.store.AccountsSnapshot() {
			agg.Acc = append(agg.Acc, AccSample{ID: a.ID, Inflight: h.Inflight(a.ID), DoneDelta: ca.accDone[a.ID]})
		}
		hi.coarse = append(hi.coarse, agg)
		if len(hi.coarse) > histCoarseCap {
			hi.coarse = hi.coarse[len(hi.coarse)-histCoarseCap:]
		}
		hi.coarseAcc = struct {
			n       int
			sum     HistorySample
			firstTS int64
			accDone map[string]int64
		}{}
	}
	hi.mu.Unlock()
}

// MetricsHistory 返回指定窗口的采样点 + 账号图例元数据。
func (h *Hub) MetricsHistory(win string) map[string]any {
	hi := h.hist
	if hi == nil {
		return map[string]any{"points": []any{}, "accounts": []any{}}
	}
	hi.mu.Lock()
	var pts []HistorySample
	if win == "coarse" {
		pts = append([]HistorySample(nil), hi.coarse...)
	} else {
		pts = append([]HistorySample(nil), hi.fine...)
	}
	hi.mu.Unlock()

	// 图例：按 id 排序，颜色稳定
	accs := h.store.AccountsSnapshot()
	metas := make([]AccMeta, 0, len(accs))
	for _, a := range accs {
		metas = append(metas, AccMeta{ID: a.ID, Name: a.Name, Color: accColor(a.ID)})
	}
	sortAccMeta(metas)

	out := make([]map[string]any, 0, len(pts))
	for _, p := range pts {
		accOut := make([]map[string]any, 0, len(p.Acc))
		for _, a := range p.Acc {
			accOut = append(accOut, map[string]any{
				"id": a.ID, "inflight": a.Inflight, "done_delta": a.DoneDelta,
			})
		}
		out = append(out, map[string]any{
			"ts": p.TS, "load_pct": p.LoadPct, "arrive_rps": p.ArriveRPS,
			"done_rps": p.DoneRPS, "queue_depth": p.QueueDepth, "inflight": p.Inflight,
			"pending": p.Pending,
			"r429_pm": p.R429PM, "events": p.Events, "acc": accOut,
		})
	}
	return map[string]any{"points": out, "accounts": metas}
}

// accColor 按 id 稳定地分配一个颜色（相同 id 永远同色）。
func accColor(id string) string {
	palette := []string{
		"#2f6fd6", "#2f9e6f", "#e08a1e", "#7a52c7", "#d64545",
		"#1aa3b5", "#c0457f", "#6b8e23", "#9467bd", "#17a2b8",
		"#e0a800", "#5b6573", "#9c6644", "#588157", "#b5179e",
	}
	sum := 0
	for _, b := range []byte(id) {
		sum = (sum*31 + int(b)) & 0x7fffffff
	}
	return palette[sum%len(palette)]
}

func sortAccMeta(m []AccMeta) {
	for i := 1; i < len(m); i++ {
		for j := i; j > 0 && m[j-1].ID > m[j].ID; j-- {
			m[j-1], m[j] = m[j], m[j-1]
		}
	}
}
