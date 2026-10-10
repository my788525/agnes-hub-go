package hub

import (
	"testing"

	"agneshub/internal/config"
)

func TestHistorySampleAndExport(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("账号A", "sk-a", "free", "", nil)
	b := store.AddAccount("账号B", "sk-b", "free", "", nil)
	h.Reload()

	h.hist = &History{lastAcc: map[string]int64{}}

	// 模拟若干秒：制造一些完成计数，让 done_delta 与 per-account 差分生效
	for i := 0; i < 5; i++ {
		h.Metrics.RequestsOK.Add(3)
		store.MutateAccount(a.ID, func(acc *config.Account) bool { acc.Stats.Requests += 2; return true })
		store.MutateAccount(b.ID, func(acc *config.Account) bool { acc.Stats.Requests += 1; return true })
		h.hist.sample(h)
	}

	got := h.MetricsHistory("fine")
	pts, ok := got["points"].([]map[string]any)
	if !ok || len(pts) != 5 {
		t.Fatalf("fine 应返回 5 个点，实际 %d", len(pts))
	}
	// 每点应含三块图所需字段
	for _, p := range pts {
		for _, k := range []string{"ts", "load_pct", "arrive_rps", "done_rps", "queue_depth", "inflight", "r429_pm", "events", "acc"} {
			if _, exists := p[k]; !exists {
				t.Fatalf("采样点缺少字段 %s", k)
			}
		}
		acc, _ := p["acc"].([]map[string]any)
		if len(acc) != 2 {
			t.Fatalf("每点应含 2 个账号，实际 %d", len(acc))
		}
	}

	// 图例元数据：按 id 排序且颜色稳定
	metas, _ := got["accounts"].([]AccMeta)
	if len(metas) != 2 {
		t.Fatalf("图例应含 2 个账号，实际 %d", len(metas))
	}
	if metas[0].ID > metas[1].ID {
		t.Fatal("图例未按 id 排序")
	}
	// 颜色稳定性：相同 id 永远同色
	if accColor(a.ID) != accColor(a.ID) {
		t.Fatal("accColor 不稳定")
	}
}

// 首个采样点只建基线：账号 Stats.Requests 是持久化的历史累计值，
// 若不特殊处理，重启后第一帧会把全部历史当成「这一秒完成」，图表出现巨大尖峰。
func TestHistoryFirstSampleIsBaselineOnly(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("老账号", "sk-old", "free", "", nil)
	h.Reload()
	// 模拟「重启前已累计 5000 次」
	store.MutateAccount(a.ID, func(acc *config.Account) bool { acc.Stats.Requests = 5000; return true })

	h.hist = &History{lastAcc: map[string]int64{}}
	h.hist.sample(h) // 首点：只建基线

	got := h.MetricsHistory("fine")
	pts := got["points"].([]map[string]any)
	first := pts[len(pts)-1]
	acc0 := first["acc"].([]map[string]any)
	if len(acc0) != 1 {
		t.Fatalf("应含 1 个账号，实际 %d", len(acc0))
	}
	if d := acc0[0]["done_delta"]; d != int64(0) {
		t.Fatalf("首点 done_delta 应为 0（只建基线），实际 %v", d)
	}

	// 第二点：真实增量应被正确计入
	store.MutateAccount(a.ID, func(acc *config.Account) bool { acc.Stats.Requests += 3; return true })
	h.hist.sample(h)
	pts = h.MetricsHistory("fine")["points"].([]map[string]any)
	second := pts[len(pts)-1]["acc"].([]map[string]any)
	if d := second[0]["done_delta"]; d != int64(3) {
		t.Fatalf("第二点 done_delta 应为 3，实际 %v", d)
	}
}

func TestHistoryCapacityLoadPct(t *testing.T) {
	h, store := newTestHub(t, nil)
	store.AddAccount("文本账号", "sk-t", "free", "", nil)
	h.Reload()
	h.hist = &History{lastAcc: map[string]int64{}}
	// 给文本池一个固定 RPM（默认 free 文本基线 10 → 容量 10/60 ≈ 0.167 req/s）
	// 当前秒到达 1 次 → load_pct ≈ 600%
	h.Arrivals.Add()
	h.hist.sample(h)
	got := h.MetricsHistory("fine")
	pts := got["points"].([]map[string]any)
	p := pts[len(pts)-1]
	load, _ := p["load_pct"].(float64)
	if load <= 0 {
		t.Fatalf("存在到达时 load_pct 应 > 0，实际 %v", p["load_pct"])
	}
}
