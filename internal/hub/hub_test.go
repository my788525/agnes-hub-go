package hub

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"agneshub/internal/config"
)

// newTestHub 构造一个隔离的调度器。
//
// 刻意把安全系数拉到 1.0，让「有效 RPM == 基线 RPM」，否则所有断言都要背着
// 默认的 0.9 去算，测试会变得难以阅读。tweak 用于覆盖单条用例需要的设置，
// 且必须在 New() 之前生效 —— Hub 会把设置快照缓存在 atomic.Pointer 里。
func newTestHub(t *testing.T, tweak func(*config.Settings)) (*Hub, *config.Store) {
	t.Helper()
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败：%v", err)
	}
	if err := store.UpdateSettings(func(s *config.Settings) {
		s.SafetyFactor = 1.0
		s.PacingWindowSec = 60
		if tweak != nil {
			tweak(s)
		}
	}); err != nil {
		t.Fatalf("写入设置失败：%v", err)
	}
	return New(store), store
}

func manifest(text, image, video []string) *config.ModelManifest {
	return &config.ModelManifest{Text: text, Image: image, Video: video}
}

func ids(list []*config.Account) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// 账号 × 模态分工（agnes-auto 落地的关键：一个 key 只买了一种模态的额度）
// ---------------------------------------------------------------------------

// TestCapableRequiresDeclaredModel 未声明（或主动清空）某模态的账号，
// 绝不能被调度到该模态的池 —— 否则生图请求会打到只买了文本额度的 key 上，
// 白白换回一个 403，并让该账号因为累计错误被熔断。
func TestCapableRequiresDeclaredModel(t *testing.T) {
	h, store := newTestHub(t, nil)
	// 显式空数组 = 明示不支持该模态（nil 才是「还没声明，用默认清单」）
	a := store.AddAccount("文本专用", "sk-text-only", "free", "",
		manifest([]string{"agnes-2.5-flash"}, []string{}, []string{}))
	h.Reload()

	if !h.Capable(a, "text") {
		t.Error("已声明文本模型，text 池应可用")
	}
	for _, cls := range []string{"image_1k", "image_2k", "image_3k", "image_4k", "video"} {
		if h.Capable(a, cls) {
			t.Errorf("未声明该模态的账号不应被调度到 %s 池", cls)
		}
	}
	if _, err := h.Pick("", "video", "", "", nil); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("视频池无可用账号时应返回 ErrNoCapacity，实际 %v", err)
	}
	// 空清单必须是「非 nil 空切片」：nil 在控制台 JSON 里会变成 null，
	// 前端拿到 null 再取长度就会直接报错。
	if got := h.DeclaredModels(a, "video"); got == nil {
		t.Error("空清单应返回非 nil 空切片，而不是 nil")
	} else if len(got) != 0 {
		t.Errorf("未声明视频模型的账号不应有视频清单，实际 %v", got)
	}
}

// TestAccountsSplitByModality 多账号按模态分工：生图账号只接生图，文本账号只接文本。
func TestAccountsSplitByModality(t *testing.T) {
	h, store := newTestHub(t, nil)
	img := store.AddAccount("生图账号", "sk-img", "free", "",
		manifest([]string{}, []string{"agnes-image-2.5-flash"}, []string{}))
	txt := store.AddAccount("文本账号", "sk-txt", "free", "",
		manifest([]string{"agnes-2.5-flash"}, []string{}, []string{}))
	h.Reload()

	got := h.Candidates("image_1k", nil, "")
	if len(got) != 1 || got[0].ID != img.ID {
		t.Fatalf("image_1k 候选应只有生图账号，实际 %v", ids(got))
	}
	got = h.Candidates("text", nil, "")
	if len(got) != 1 || got[0].ID != txt.ID {
		t.Fatalf("text 候选应只有文本账号，实际 %v", ids(got))
	}
	got = h.Candidates("video", nil, "")
	if len(got) != 0 {
		t.Fatalf("两个账号都未声明视频模型，video 候选应为空，实际 %v", ids(got))
	}
}

// TestDeclaredModelsNeverEscapeManifest 调度器只能从账号自己声明的清单里挑模型。
// 把账号没买到的模型塞进请求，等于换回一个上游错误。
func TestDeclaredModelsNeverEscapeManifest(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("老额度账号", "sk-old", "free", "",
		manifest([]string{"agnes-2.0-flash"}, []string{}, []string{}))
	h.Reload()

	if !h.Declares(a, "text", "agnes-2.0-flash") {
		t.Error("已声明的模型应被识别")
	}
	if h.Declares(a, "text", "agnes-3.0-flash") {
		t.Error("账号未声明的模型不能被认定为可调用")
	}
	// 配置漏填时退回「声明了该模态」的全集：宁可让上游给出明确错误，
	// 也不要因为一处配置缺失就把请求挡在网关门外。
	if got := h.Candidates("text", nil, "agnes-3.0-flash"); len(got) != 1 {
		t.Fatalf("无人声明该模型时应退回该模态全集，实际 %v", ids(got))
	}
}

// TestRequiredModelPrefersDeclaringAccount 有账号声明了指定模型时，优先只给它。
func TestRequiredModelPrefersDeclaringAccount(t *testing.T) {
	h, store := newTestHub(t, nil)
	store.AddAccount("账号A", "sk-a", "free", "", manifest([]string{"agnes-2.0-flash"}, nil, nil))
	b := store.AddAccount("账号B", "sk-b", "free", "", manifest([]string{"agnes-3.0-flash"}, nil, nil))
	h.Reload()

	got := h.Candidates("text", nil, "agnes-3.0-flash")
	if len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("应只返回声明了该模型的账号，实际 %v", ids(got))
	}
}

// TestCandidatesSkipDisabledAndKeyless 停用或没有 key 的账号不参与调度。
func TestCandidatesSkipDisabledAndKeyless(t *testing.T) {
	h, store := newTestHub(t, nil)
	disabled := store.AddAccount("已停用", "sk-x", "free", "", nil)
	store.AddAccount("没有key", "   ", "free", "", nil)
	store.MutateAccount(disabled.ID, func(a *config.Account) bool {
		a.Enabled = false
		return true
	})
	h.Reload()

	if got := h.Candidates("text", nil, ""); len(got) != 0 {
		t.Fatalf("停用与无 key 的账号都不该成为候选，实际 %v", ids(got))
	}
	if _, err := h.Pick("", "text", "", "", nil); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("无可用账号应返回 ErrNoCapacity，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 软粘性 + 溢出（B1）
// ---------------------------------------------------------------------------

// TestStickyBindingKeptWhenHealthy 绑定账号健康时沿用绑定，保证同任务的多轮
// 对话落在同一个账号上（上下文一致 + 不浪费新建连接）。
func TestStickyBindingKeptWhenHealthy(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("账号A", "sk-a", "free", "",
		manifest([]string{"agnes-2.5-flash"}, []string{}, []string{}))
	store.AddAccount("账号B", "sk-b", "free", "",
		manifest([]string{"agnes-2.5-flash"}, []string{}, []string{}))
	h.Reload()

	const session = "ses:task-1"
	store.Bind(session, a.ID)
	res, err := h.Pick(session, "text", "", "", nil)
	if err != nil {
		t.Fatalf("选号失败：%v", err)
	}
	if res.Spilled {
		t.Fatal("绑定账号健康时不应发生溢出")
	}
	if res.Account.ID != a.ID {
		t.Fatalf("应沿用绑定账号，实际 %s", res.Account.Name)
	}
}

// TestNewSessionPrefersUnboundAccount 任务分散：两个条件完全相同的账号，其中一个
// 已被任务 1 绑定，新任务 2 进来时应优先选「没人绑定」的那个号，避免并发任务的
// 压力全部挤在同一个号的通道上（这正是「不同任务走不同的号」的调度语义）。
func TestNewSessionPrefersUnboundAccount(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("账号A", "sk-a", "free", "",
		manifest([]string{"agnes-2.5-flash"}, []string{}, []string{}))
	b := store.AddAccount("账号B", "sk-b", "free", "",
		manifest([]string{"agnes-2.5-flash"}, []string{}, []string{}))
	h.Reload()

	store.Bind("ses:task-1", a.ID) // 任务 1 已绑定 A

	res, err := h.Pick("ses:task-2", "text", "", "", nil) // 新任务 2 尚无绑定
	if err != nil {
		t.Fatalf("选号失败：%v", err)
	}
	if res.Account.ID != b.ID {
		t.Fatalf("新任务应分散到未绑定的账号B，实际选中 %s（绑定数 A=%d B=%d）",
			res.Account.Name,
			store.BindingsCountByAccount()[a.ID], store.BindingsCountByAccount()[b.ID])
	}
	// 且选中后立即建立新绑定，后续请求保持粘性
	if cnt := store.BindingsCountByAccount(); cnt[b.ID] != 1 {
		t.Fatalf("新任务选中 B 后应建立绑定，实际 B 的绑定数 %d", cnt[b.ID])
	}
}

// TestStickyBindingSpilloverOverloaded Account 是 B1 的核心修正。
//
// 旧实现命中绑定就直接返回该账号，既不比较负载也不看惩罚。而客户端默认不发会话头，
// 于是「同一把下游 Key 的全部请求永久绑到同一个账号」—— 配了 5 个账号实际只有 1 个
// 在工作，且绑定账号一旦拥堵，请求会集体睡过去而不是切到健康账号。
func TestStickyBindingSpilloverOverloadedAccount(t *testing.T) {
	h, store := newTestHub(t, nil)
	slow := store.AddAccount("拥堵账号", "sk-slow", "free", "",
		manifest([]string{"agnes-2.5-flash"}, []string{}, []string{}))
	fast := store.AddAccount("空闲账号", "sk-fast", "free", "",
		manifest([]string{"agnes-2.5-flash"}, []string{}, []string{}))
	store.MutateAccount(slow.ID, func(a *config.Account) bool {
		a.RPMOverrides["text"] = 1 // 间隔 60s
		return true
	})
	store.MutateAccount(fast.ID, func(a *config.Account) bool {
		a.RPMOverrides["text"] = 600 // 间隔 100ms
		return true
	})
	h.Reload()

	const session = "ses:task-2"
	store.Bind(session, slow.ID)

	// 把拥堵账号唯一的槽位用掉，让它的预计等待涨到约 60s
	if _, err := h.Pacer(slow, "text").Reserve(context.Background(), 0, 0); err != nil {
		t.Fatalf("占位失败：%v", err)
	}
	if w := h.Pacer(slow, "text").ProjectedWait(); w <= spillPenalty {
		t.Fatalf("拥堵账号预计等待应超过溢出阈值 %s，实际 %s（测试前提不成立）", spillPenalty, w)
	}

	res, err := h.Pick(session, "text", "", "", nil)
	if err != nil {
		t.Fatalf("选号失败：%v", err)
	}
	if !res.Spilled {
		t.Fatal("绑定账号拥堵时应当发生溢出改派，否则多账号形同虚设")
	}
	if res.Account.ID != fast.ID {
		t.Fatalf("应改派到空闲账号，实际 %s", res.Account.Name)
	}
	if res.Bound == nil || res.Bound.ID != slow.ID {
		t.Error("应保留原绑定账号，便于排查「请求为什么换了账号」")
	}
	if b, ok := store.BindingsGet(session); !ok || b.AccountID != fast.ID {
		t.Errorf("溢出后绑定必须就地更新，否则每次请求都要重走一遍溢出判断（实际 %+v）", b)
	}
	if h.Metrics.Spillovers.Load() == 0 {
		t.Error("溢出次数应被统计")
	}
}

// TestPickWaitsInsteadOfFailingWhenAllCooling 全员冷却时不退化为报错。
//
// 无人值守场景下「等一下」远好于「任务断掉」——这也是本网关存在的意义。
func TestPickWaitsInsteadOfFailingWhenAllCooling(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("唯一账号", "sk-a", "free", "", nil)
	h.Reload()

	h.OnRateLimited(a, "text")

	res, err := h.Pick("", "text", "", "", nil)
	if err != nil {
		t.Fatalf("全员冷却时不应直接失败，而应返回最早解冻的账号：%v", err)
	}
	if res.Account == nil || res.Penalty <= 0 {
		t.Fatalf("应返回冷却剩余时间，实际 %+v", res)
	}
}

// ---------------------------------------------------------------------------
// 二维自适应校准（B2）
// ---------------------------------------------------------------------------

// TestCalibrationIsPerPoolNotPerAccount 一次 429 只能下调出问题的那一个池。
//
// effectiveRPM = 基线 × 安全系数 × 账号级系数 × 池因子。视频池只有 1 RPM、
// 最容易 429，如果惩罚被写进账号级系数，文本池会跟着一起变慢 ——
// 这正是「配了多账号，文本却一起变慢」的根因。
func TestCalibrationIsPerPoolNotPerAccount(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("双模态账号", "sk-a", "free", "", nil) // nil → 默认三模态清单
	h.Reload()

	beforeText := h.EffectiveRPM(a, "text")
	beforeVideo := h.EffectiveRPM(a, "video")
	if math.Abs(beforeText-10) > 1e-9 {
		t.Fatalf("free 档文本池基线应为 10 RPM，实际 %.3f", beforeText)
	}
	if math.Abs(beforeVideo-1) > 1e-9 {
		t.Fatalf("free 档视频池基线应为 1 RPM，实际 %.3f", beforeVideo)
	}

	h.OnRateLimited(a, "video")

	if f := h.FactorOf(a.ID, "video"); f >= 1 {
		t.Errorf("视频池被限流后其因子应下调，实际 %.3f", f)
	}
	if f := h.FactorOf(a.ID, "text"); f != 1 {
		t.Errorf("视频池的 429 不得拖累文本池因子，实际 %.3f", f)
	}
	if got := h.EffectiveRPM(a, "text"); math.Abs(got-beforeText) > 1e-9 {
		t.Errorf("文本池有效 RPM 不应被视频池限流改动：%.3f → %.3f", beforeText, got)
	}
	if got := h.EffectiveRPM(a, "video"); got >= beforeVideo {
		t.Errorf("视频池有效 RPM 应下调：%.3f → %.3f", beforeVideo, got)
	}
	if got := h.EffectiveRPM(a, "video"); math.Abs(got-0.8) > 1e-9 {
		t.Errorf("1 RPM × 0.8 惩罚因子应为 0.8，实际 %.3f", got)
	}
	if h.PenaltyRemaining(a.ID) <= 0 {
		t.Error("被限流后该账号应进入冷却")
	}
	// 落盘的必须是池级因子，而不是账号级系数
	if saved := store.AccountByID(a.ID); saved.LearnedFactor != 1 {
		t.Errorf("429 不得写入账号级系数（会把惩罚传染到全部池），实际 %.3f", saved.LearnedFactor)
	}
	if saved := store.AccountByID(a.ID); math.Abs(saved.PoolFactors["video"]-0.8) > 1e-9 {
		t.Errorf("池级因子应立即落盘以保证重启不丢，实际 %.3f", saved.PoolFactors["video"])
	}
}

// TestFactorRecoversAfterSuccesses 校准因子按「成功次数」回升。
//
// 按墙钟回升在低流量账号上永远等不到，表现为「校准砍下去就再也回不来」。
func TestFactorRecoversAfterSuccesses(t *testing.T) {
	h, store := newTestHub(t, func(s *config.Settings) { s.RecoverSuccesses = 3 })
	a := store.AddAccount("账号", "sk-a", "free", "", nil)
	h.Reload()

	h.OnRateLimited(a, "text")
	if got := h.FactorOf(a.ID, "text"); math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("首次 429 后因子应为 0.8，实际 %.3f", got)
	}

	// 未达阈值前不得回升
	h.OnSuccess(a, "text")
	h.OnSuccess(a, "text")
	if got := h.FactorOf(a.ID, "text"); math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("成功 2 次（阈值 3）不应回升，实际 %.3f", got)
	}
	// 第 3 次成功触发 +0.05
	h.OnSuccess(a, "text")
	if got := h.FactorOf(a.ID, "text"); math.Abs(got-0.85) > 1e-9 {
		t.Fatalf("达到阈值后应回升 0.05 至 0.85，实际 %.3f", got)
	}
	// 回升只作用于该池
	if got := h.FactorOf(a.ID, "video"); got != 1 {
		t.Errorf("回升不得波及其它池，实际 %.3f", got)
	}
	// 下降时不会穿过下限
	for i := 0; i < 20; i++ {
		h.OnRateLimited(a, "text")
	}
	if got := h.FactorOf(a.ID, "text"); got < 0.5-1e-9 {
		t.Errorf("因子不得低于校准下限 0.5，实际 %.3f", got)
	}
}

// TestResetFactors 控制台一键重置应把该账号全部池复位。
func TestResetFactors(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("账号", "sk-a", "free", "", nil)
	h.Reload()

	h.OnRateLimited(a, "video")
	h.ResetFactors(a.ID)
	for _, cls := range config.PoolClasses {
		if got := h.FactorOf(a.ID, cls); math.Abs(got-1) > 1e-9 {
			t.Errorf("%s 池因子应被复位为 1，实际 %.3f", cls, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 熔断与自动复活
// ---------------------------------------------------------------------------

// TestBreakerDisablesAccountAndAutoRevives 401/403/402 不可重试：立即熔断停用，
// 冷却结束后自动拉回（被动触发，不依赖后台定时器存活）。
func TestBreakerDisablesAccountAndAutoRevives(t *testing.T) {
	h, store := newTestHub(t, func(s *config.Settings) { s.BreakerReviveSec = 1 })
	a := store.AddAccount("会失效的账号", "sk-a", "free", "", nil)
	h.Reload()

	h.OnAuthFailure(a, "401 invalid api key")
	if store.AccountByID(a.ID).Enabled {
		t.Fatal("鉴权失败后账号应立即熔断停用")
	}
	if got := h.Candidates("text", nil, ""); len(got) != 0 {
		t.Fatalf("熔断期间不应作为候选，实际 %v", ids(got))
	}
	if h.Metrics.BreakerOpened.Load() == 0 {
		t.Error("熔断次数应被统计")
	}

	time.Sleep(1200 * time.Millisecond)

	// 候选筛选时会自动复活到期账号
	got := h.Candidates("text", nil, "")
	if len(got) != 1 {
		t.Fatalf("冷却结束后应自动复活，实际候选 %v", ids(got))
	}
	if !store.AccountByID(a.ID).Enabled {
		t.Fatal("账号应被置回启用状态")
	}
	if h.Metrics.BreakerRevived.Load() == 0 {
		t.Error("复活次数应被统计")
	}
}

// TestNoteErrorDoesNotBreaker 普通错误只记录，不熔断 ——
// 一次 500 不代表凭证失效，不该让账号下线 30 分钟。
func TestNoteErrorDoesNotBreaker(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("账号", "sk-a", "free", "", nil)
	h.Reload()

	h.NoteError(a, "502 bad gateway")
	if !store.AccountByID(a.ID).Enabled {
		t.Fatal("一次性错误不应熔断账号")
	}
	if h.PenaltyRemaining(a.ID) > 0 {
		t.Error("一次性错误不应让账号进入冷却")
	}
}

// ---------------------------------------------------------------------------
// 排队（B3 / B4）
// ---------------------------------------------------------------------------

// TestAcquireRejectsWhenWaitExceedsLimit 预计等待超过上限时立刻拒绝，
// 并把原因映射成 ErrQueueTimeout（而不是让客户端悬着）。
func TestAcquireRejectsWhenWaitExceedsLimit(t *testing.T) {
	h, store := newTestHub(t, func(s *config.Settings) {
		s.QueueMaxWaitMS = 200
		s.QueueMaxSize = 10
	})
	a := store.AddAccount("慢账号", "sk-slow", "free", "",
		manifest([]string{"agnes-2.5-flash"}, []string{}, []string{}))
	store.MutateAccount(a.ID, func(acc *config.Account) bool {
		acc.RPMOverrides["text"] = 1 // 间隔 60s
		return true
	})
	h.Reload()

	if _, err := h.Acquire(context.Background(), a, "text", false); err != nil {
		t.Fatalf("首次领槽应立刻成功：%v", err)
	}
	if _, err := h.Acquire(context.Background(), a, "text", false); !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("预计等待 60s 远超上限 200ms，应返回 ErrQueueTimeout，实际 %v", err)
	}
	if h.Metrics.QueueTimeout.Load() == 0 {
		t.Error("排队超时应被统计")
	}
	// 被拒的请求不得占用槽位：下一次仍应是「等待 60s」而不是 120s
	if w := h.Pacer(a, "text").ProjectedWait(); w > 70*time.Second {
		t.Fatalf("被拒的请求消耗了槽位（预计等待膨胀到 %s）", w)
	}
}

// TestConcurrencySplitsByModality 并发信号量按模态分离（B3）。
//
// 旧实现是账号级一把信号量，视频轮询会长期占着它，把文本请求挤住 ——
// 表现为「一提交视频，聊天就卡住」。
func TestConcurrencySplitsByModality(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("账号", "sk-a", "free", "", nil)
	h.Reload()

	text := h.Semaphore(a, "text")
	image := h.Semaphore(a, "image")
	video := h.Semaphore(a, "video")
	if text == image || image == video || text == video {
		t.Fatal("不同模态必须使用不同的信号量，否则会互相挤占")
	}
	if cap(text) != 8 {
		t.Errorf("文本并发应等于账号上限 8，实际 %d", cap(text))
	}
	if cap(image) != 4 {
		t.Errorf("生图并发应受 ImageConcurrency=4 限制，实际 %d", cap(image))
	}
	if cap(video) != 2 {
		t.Errorf("视频并发应受 VideoMaxInFlight=2 限制，实际 %d", cap(video))
	}
}

// TestEffectiveRPMAppliesSafetyAndFactor 有效 RPM 的构成：
// 基线 × 安全系数 × 账号级系数 × 池因子。
func TestEffectiveRPMAppliesSafetyAndFactor(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("账号", "sk-a", "free", "", nil)
	h.Reload()

	if got := h.EffectiveRPM(a, "text"); math.Abs(got-10) > 1e-9 {
		t.Fatalf("free 档文本池 baseline×安全系数(1.0) 应为 10，实际 %.3f", got)
	}
	h.BindFactor(a.ID, "text", 0.5)
	if got := h.EffectiveRPM(a, "text"); math.Abs(got-5) > 1e-9 {
		t.Fatalf("池因子 0.5 应使有效 RPM 降到 5，实际 %.3f", got)
	}
}

// TestAccountRPMOverrideWinsOverTable 账号级 RPM 覆盖优先于档位表 ——
// 这是「某个账号实际额度与官方文档不符」时的唯一兜底手段。
func TestAccountRPMOverrideWinsOverTable(t *testing.T) {
	h, store := newTestHub(t, nil)
	a := store.AddAccount("实测更快的账号", "sk-a", "free", "", nil)
	store.MutateAccount(a.ID, func(acc *config.Account) bool {
		acc.RPMOverrides["text"] = 22 // 实测 22 RPM 可通过、30 RPM 被限
		return true
	})
	h.Reload()

	if got := h.EffectiveRPM(a, "text"); math.Abs(got-22) > 1e-9 {
		t.Fatalf("账号级覆盖应优先于档位表，实际 %.3f", got)
	}
}

// ---------------------------------------------------------------------------
// 粘性键
// ---------------------------------------------------------------------------

func TestSessionKeyModes(t *testing.T) {
	h, store := newTestHub(t, nil)

	tweakSettings(t, h, store, func(s *config.Settings) { s.AffinityMode = "session_then_key" })
	if got := h.SessionKey(map[string]string{"x-agnes-session": "abc"}, "sk-1"); got != "ses:abc" {
		t.Errorf("有会话头时应按会话绑定，实际 %q", got)
	}
	if got := h.SessionKey(map[string]string{}, "sk-1"); got != "key:sk-1" {
		t.Errorf("无会话头时应回落到下游 Key，实际 %q", got)
	}

	// 只按下游 Key 绑定：同一把 key 的全部请求固定落在同一账号
	tweakSettings(t, h, store, func(s *config.Settings) { s.AffinityMode = "key" })
	if got := h.SessionKey(map[string]string{"x-agnes-session": "abc"}, "sk-1"); got != "key:sk-1" {
		t.Errorf("key 模式应忽略会话头，实际 %q", got)
	}

	// 只按会话绑定：无会话头时不绑定（把并发尽量摊到多账号上）
	tweakSettings(t, h, store, func(s *config.Settings) { s.AffinityMode = "session" })
	if got := h.SessionKey(map[string]string{}, "sk-1"); got != "" {
		t.Errorf("session 模式下无会话头时不应绑定，实际 %q", got)
	}

	tweakSettings(t, h, store, func(s *config.Settings) { s.AffinityMode = "none" })
	if got := h.SessionKey(map[string]string{"x-agnes-session": "abc"}, "sk-1"); got != "" {
		t.Errorf("关闭粘性时不应返回绑定键，实际 %q", got)
	}
}

// tweakSettings 在 Hub 已构造之后改设置并刷新它的缓存快照。
func tweakSettings(t *testing.T, h *Hub, store *config.Store, fn func(*config.Settings)) {
	t.Helper()
	if err := store.UpdateSettings(fn); err != nil {
		t.Fatalf("写入设置失败：%v", err)
	}
	h.Reload()
}

// ---------------------------------------------------------------------------
// #24 负载画像遥测（NoteRequest → LoadProfile → RecommendScenario）
// ---------------------------------------------------------------------------

// TestNoteRequestFeedsLoadProfile 每请求埋点必须真实累加到 LoadProfile：
// 这是「接入方自动检测」能跑起来的前提（此前 NoteRequest 未被调用，画像恒为空）。
func TestNoteRequestFeedsLoadProfile(t *testing.T) {
	h, _ := newTestHub(t, nil)
	h.NoteRequest("text", true)
	h.NoteRequest("text", false)
	h.NoteRequest("image", false)

	p := h.LoadProfile()
	if p.TotalRecent != 3 {
		t.Fatalf("TotalRecent 应为 3，实际 %d", p.TotalRecent)
	}
	if math.Abs(p.TextRatio-2.0/3.0) > 1e-9 {
		t.Errorf("TextRatio 应为 2/3，实际 %v", p.TextRatio)
	}
	if math.Abs(p.ImageRatio-1.0/3.0) > 1e-9 {
		t.Errorf("ImageRatio 应为 1/3，实际 %v", p.ImageRatio)
	}
	// 流式比只对文本请求有意义：2 条文本里 1 条流式 → 0.5
	if math.Abs(p.StreamRatio-0.5) > 1e-9 {
		t.Errorf("StreamRatio 应为 0.5，实际 %v", p.StreamRatio)
	}
}

// TestRecommendScenarioRoutesByProfile RecommendScenario 是纯函数，必须按画像
// 稳定映射到对应情景（高 429→batch、生图高→image、视频高→auto、
// 文本多并发→workbuddy、纯文本低并发→code、空负载→default）。
func TestRecommendScenarioRoutesByProfile(t *testing.T) {
	cases := []struct {
		name string
		in   config.LoadProfile
		want string
	}{
		{"空负载回落 default", config.LoadProfile{}, "default"},
		{"高 429 → batch", config.LoadProfile{TotalRecent: 10, Upstream429Rate: 5}, "batch"},
		{"生图占比高 → image", config.LoadProfile{TotalRecent: 10, ImageRatio: 0.5}, "image"},
		{"视频占比高 → auto", config.LoadProfile{TotalRecent: 10, VideoRatio: 0.5}, "auto"},
		{"文本为主+流式 → workbuddy", config.LoadProfile{TotalRecent: 10, TextRatio: 0.9, StreamRatio: 0.6}, "workbuddy"},
		{"纯文本低并发 → code", config.LoadProfile{TotalRecent: 10, TextRatio: 1.0}, "code"},
		{"不偏向 → default", config.LoadProfile{TotalRecent: 10, TextRatio: 0.5, ImageRatio: 0.3, VideoRatio: 0.2}, "default"},
	}
	for _, c := range cases {
		preset, reason := config.RecommendScenario(c.in)
		if preset.Name != c.want {
			t.Errorf("[%s] 期望情景 %s，实际 %s（reason=%s）", c.name, c.want, preset.Name, reason)
		}
	}
}
