// Package config 负责 agnes-hub 的全部持久化状态。
//
// 设计沿用 Python 基线的约定（便于两个版本共用同一个 data 目录、平滑迁移）：
//   - 全 JSON 文件存储，免 SSH 即可备份/迁移；
//   - 所有写操作走「临时文件 + rename」原子写，断电不会写坏配置；
//   - 运行期状态（冷却、在途数、池级校准）**不落盘**，只落有长期价值的字段。
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 官方「实际可执行 RPM」基线
//
// 来源：AgnesAI-Labs/AgnesAI-Models → MODEL_CATALOG.md（限流表更新 2026-06-28）。
// 注意官方限流表按「模型类型」而非单个模型 ID 给出，故 text 池由多个
// 文本模型共用，绝不能按模型名各建一桶。
// ---------------------------------------------------------------------------
var RPMTable = map[string]map[string]float64{
	"free": {
		"text": 10, "image_1k": 20, "image_2k": 10, "image_3k": 1, "image_4k": 1, "video": 1,
	},
	"enterprise": {
		"text": 40, "image_1k": 40, "image_2k": 20, "image_3k": 1, "image_4k": 1, "video": 2,
	},
	"tokenplan": {
		"text": 1000, "image_1k": 100, "image_2k": 80, "image_3k": 1, "image_4k": 1, "video": 5,
	},
	// amd = AMD Radeon Cloud 免费 API（developer.amd.com.cn/radeon/api/v1）。
	// 官方未公开精确 RPM，这里用保守值 20，用户可按实测用 rpm_overrides 上调。
	"amd": {
		"text": 20, "image_1k": 1, "image_2k": 1, "image_3k": 1, "image_4k": 1, "video": 1,
	},
	// openrouter = OpenRouter 聚合网关（openrouter.ai/api/v1）。OpenAI 兼容，单 key 可路由海量模型。
	// 免费档速率随信用额度浮动，这里用保守值 20，用户可按实测用 rpm_overrides 上调。
	"openrouter": {
		"text": 20, "image_1k": 5, "image_2k": 2, "image_3k": 1, "image_4k": 1, "video": 1,
	},
}

// PoolClasses 是全部限流桶。顺序即控制台展示顺序。
var PoolClasses = []string{"text", "image_1k", "image_2k", "image_3k", "image_4k", "video"}

// AccessTypes 支持的账号类型。
var AccessTypes = []string{"free", "enterprise", "tokenplan", "amd", "openrouter"}

// DefaultBaseURL / CNBaseURL 官方两个站点。
const (
	DefaultBaseURL = "https://apihub.agnes-ai.com/v1"
	CNBaseURL      = "https://api.agnes-ai.cn/v1"
)

// DefaultBaseURLByType 各 access_type 的官方默认 Base URL。
// 添加账号时若未填 base_url，则按类型自动套用这里的值。
var DefaultBaseURLByType = map[string]string{
	"free":       "https://apihub.agnes-ai.com/v1",
	"enterprise": "https://apihub.agnes-ai.com/v1",
	"tokenplan":  "https://apihub.agnes-ai.com/v1",
	"amd":        "https://developer.amd.com.cn/radeon/api/v1",
	"openrouter": "https://openrouter.ai/api/v1",
}

// DefaultBaseURLForType 返回某类型的默认 Base URL（未知类型回退官方默认）。
func DefaultBaseURLForType(accessType string) string {
	if u, ok := DefaultBaseURLByType[accessType]; ok && u != "" {
		return u
	}
	return DefaultBaseURL
}

// IsCNHost 判断 base_url 是否属于中国站（agnes-ai.cn）。
func IsCNHost(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "agnes-ai.cn")
}

// stringInSlice 检查字符串是否在切片中。
func StringInSlice(s string, list []string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 数据模型
// ---------------------------------------------------------------------------

// ModelManifest 是账号声明的「真实可调用模型」清单，按模态分组。
// 空数组是**有意义的**：表示该账号明示不支持该模态，调度器会把它排除出对应池。
type ModelManifest struct {
	Text  []string `json:"text"`
	Image []string `json:"image"`
	Video []string `json:"video"`
}

// For 取指定模态的清单。
func (m ModelManifest) For(modality string) []string {
	switch modality {
	case "image":
		return m.Image
	case "video":
		return m.Video
	default:
		return m.Text
	}
}

// Clone 深拷贝，避免控制台改动直接污染内存里正在被调度读取的结构。
//
// 注意这里必须区分 nil 与空切片：`append([]string(nil), empty...)` 会把
// **显式空数组变回 nil**，而本项目的语义里「空数组 = 明示不支持该模态、
// nil = 还没声明（用默认值）」。用 append 实现会让管理端「清空某模态」
// 的操作被静默还原成默认清单 —— 这类 bug 在运行时完全看不出异常，
// 只表现为「配了不生效」。
func (m ModelManifest) Clone() ModelManifest {
	return ModelManifest{
		Text:  cloneSlice(m.Text),
		Image: cloneSlice(m.Image),
		Video: cloneSlice(m.Video),
	}
}

func cloneSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// AccountStats 是长期累计的统计量（落盘）。
type AccountStats struct {
	Requests    int64   `json:"requests"`
	Errors      int64   `json:"errors"`
	RateLimited int64   `json:"rate_limited"`
	LastError   string  `json:"last_error"`
	LastUsedAt  float64 `json:"last_used_at"`
}

// Account 是一个上游账号。运行期状态全部放在 hub 里，这里只留需要落盘的字段。
type Account struct {
	ID                  string             `json:"id"`
	Name                string             `json:"name"`
	APIKey              string             `json:"api_key"`
	BaseURL             string             `json:"base_url"`
	AccessType          string             `json:"access_type"`
	Enabled             bool               `json:"enabled"`
	Group               string             `json:"group,omitempty"`
	ClassesEnabled      []string           `json:"classes_enabled"`
	ModelManifest       ModelManifest      `json:"model_manifest"`
	RPMOverrides        map[string]float64 `json:"rpm_overrides"`
	MaxConcurrency      int                `json:"max_concurrency"`
	LearnedFactor       float64            `json:"learned_factor"`
	PoolFactors         map[string]float64 `json:"pool_factors,omitempty"` // (账号 × 池) 二维校准系数
	LastRateLimited     float64            `json:"last_rate_limited_at"`
	CreatedAt           float64            `json:"created_at"`
	Stats               AccountStats       `json:"stats"`
	// DefaultModel 是该账号的「兜底模型」：当下游传入的模型不在本账号 Manifest 时，
	// 网关会用这个模型名发请求。留空则退回全局 auto 选择。
	DefaultModel        string             `json:"default_model,omitempty"`
	// ConsecutiveFailures 连续失败计数（跨会话保留），用于长期健康追踪。
	ConsecutiveFailures int                `json:"consecutive_failures"`
	// Priority 调用优先级（数字越大越优先）。同优先级内按「首选区域 → 预计等待 → 在途」排序。
	// 默认 0。用于手动把某些账号顶到前面（例如更稳的渠道、或想优先吃满的账号）。
	Priority            int                `json:"priority"`
}

// DownstreamKey 是签发给客户端的中转密钥。
type DownstreamKey struct {
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	Enabled       bool     `json:"enabled"`
	Classes       []string `json:"classes"`
	DailyQuota    int64    `json:"daily_quota"`
	TotalQuota    int64    `json:"total_quota"`
	PinnedAccount string   `json:"pinned_account,omitempty"`
	UsedTotal     int64    `json:"used_total"`
	UsedToday     int64    `json:"used_today"`
	// 令牌（token）用量统计：UsedTokensToday 按天滚动，UsedTokensTotal 累计。
	UsedTokensTotal int64  `json:"used_tokens_total"`
	UsedTokensToday int64  `json:"used_tokens_today"`
	UsedDate      string   `json:"used_date"`
	CreatedAt     float64  `json:"created_at"`
}

// Binding 是「任务 → 账号」的粘性绑定。
type Binding struct {
	AccountID string  `json:"account_id"`
	Updated   float64 `json:"updated"`
}

// VideoJob 是视频任务映射（下游 job_id → 上游 video_id + 承载账号）。
type VideoJob struct {
	JobID     string  `json:"job_id"`
	VideoID   string  `json:"video_id"`
	Model     string  `json:"model"`
	AccountID string  `json:"account_id"`
	Status    string  `json:"status"`
	CreatedAt float64 `json:"created_at"`
	RequestID string  `json:"request_id"`
	Error     string  `json:"error,omitempty"`
}

// ImageJob 是图片任务映射（下游 request_id → 上游 image_id + 产出 URL + 承载账号）。
type ImageJob struct {
	JobID     string  `json:"job_id"`
	ImageID   string  `json:"image_id"`
	Model     string  `json:"model"`
	AccountID string  `json:"account_id"`
	URL       string  `json:"url,omitempty"`
	RevisedURL string `json:"revised_url,omitempty"`
	Size      string  `json:"size,omitempty"`
	Quality   string  `json:"quality,omitempty"`
	Style     string  `json:"style,omitempty"`
	Prompt    string  `json:"prompt"`
	Status    string  `json:"status"`
	Error     string  `json:"error,omitempty"`
	CreatedAt float64 `json:"created_at"`
	RequestID string  `json:"request_id"`
}

// ChatLog 是聊天对话记录（用户问题 + 助手回复），持久化于 chat_logs.json。
type ChatLog struct {
	ID        string  `json:"id"`
	Model     string  `json:"model"`
	Prompt    string  `json:"prompt"`
	Reply     string  `json:"reply"`
	Status    string  `json:"status"`
	CreatedAt float64 `json:"created_at"`
}

// AutoIntentSettings 是 agnes-auto 的判定与适配配置。
type AutoIntentSettings struct {
	ContentScan      bool                `json:"content_scan"`
	MinConfidence    float64             `json:"min_confidence"`
	DefaultImageSize string              `json:"default_image_size"`
	ImageInputField  string              `json:"image_input_field"`
	VideoInputField  string              `json:"video_input_field"`
	VideoWaitSec     int                 `json:"video_wait_sec"`
	PreferredModels  map[string][]string `json:"preferred_models"`
}

// Settings 是全局设置。
type Settings struct {
	AdminPasswordHash    string             `json:"admin_password_hash"`
	AdminPasswordSalt    string             `json:"admin_password_salt"`
	MustChangePassword   bool               `json:"must_change_password"`
	SafetyFactor         float64            `json:"safety_factor"`
	PacingWindowSec      float64            `json:"pacing_window_sec"`
	CalibrationEnabled   bool               `json:"calibration_enabled"`
	PenaltyCooldownSec   float64            `json:"penalty_cooldown_sec"`
	PenaltyFactor        float64            `json:"penalty_factor"`
	MinLearnedFactor     float64            `json:"min_learned_factor"`
	RecoverAfterSec      float64            `json:"recover_after_sec"`
	RecoverSuccesses     int                `json:"recover_successes"`
	QueueMaxWaitMS       int                `json:"queue_max_wait_ms"`
	QueueMaxSize         int                `json:"queue_max_size"`
	KeepaliveMS          int                `json:"keepalive_interval_ms"`
	AffinityMode         string             `json:"affinity_mode"`
	DefaultImageTier     string             `json:"default_image_tier"`
	// RegionPriority 站点优先级：cn_first（国内优先）或 com_first（国际优先）。
	// 401/403 凭据被拒时自动切换到另一站点的 BaseURL 重试同一账号。
	RegionPriority       string             `json:"region_priority,omitempty"`
	ModelAliases         map[string]string  `json:"model_aliases"`
	AutoModelName        string             `json:"auto_model_name"`
	AutoIntent           AutoIntentSettings `json:"auto_intent"`
	ModelManifestDefault ModelManifest      `json:"model_manifest_default"`
	RetryMax             int                `json:"retry_max"`
	RetryBaseBackoffMS   int                `json:"retry_base_backoff_ms"`
	RetryMaxBackoffMS    int                `json:"retry_max_backoff_ms"`
	ImageConcurrency     int                `json:"image_concurrency"`
	VideoMaxInFlight     int                `json:"video_max_inflight"`
	BreakerReviveSec     int                `json:"breaker_revive_sec"`
	VideoPollPath        string             `json:"video_poll_path"`
	VideoPollWithModel   bool               `json:"video_poll_include_model_name"`
	VideoPollIntervalMS  int                `json:"video_poll_interval_ms"`
	LogRetentionDays     int                `json:"log_retention_days"`
	SessionTTLHours      float64            `json:"session_ttl_hours"`
	ProbeModel           string             `json:"probe_model"`
	OptimizationMode     string             `json:"optimization_mode"`
	ImageRecordRetention int                `json:"image_record_retention_days"`
	ImageMaxCapacity     int                `json:"image_max_capacity"`
	VideoMaxCapacity     int                `json:"video_max_capacity"`
	RequestTimeoutMS     int                `json:"request_timeout_ms"` // 单个上游请求超时（ms），0=无限（不推荐）
	// FreeTextRPM 是 agnes 免费档（access_type=free）文本池的标称 RPM，改为可配置项。
	// 默认 10；填 0 或留空则回退默认值。per-account 的 rpm_overrides 仍然优先。
	FreeTextRPM          float64 `json:"free_text_rpm"`
	// StreamIdleTimeoutMS 是流式（SSE）响应的「空闲看门狗」超时（ms）：
	// 只要上游还在持续吐字节，计时就随数据重置，整条流不限时长；
	// 仅当连续超过该时长【没有任何新字节】时才判定为卡死并断开。
	// 非流式请求仍用 RequestTimeoutMS 的整请求墙钟超时，二者互不影响。
	// 默认 60000；填 0 视为 60s。
	StreamIdleTimeoutMS int    `json:"stream_idle_timeout_ms"`
	// UsageLogMaxLines 是 usage.jsonl 保留的最大行数（按最近 N 行滚动截断，
	// 防止长期挂机把磁盘写满）。默认 20000；填 0 表示不限。
	UsageLogMaxLines int `json:"usage_log_max_lines"`
	// UsageRequestLogBytes 是日志里「用户提出的完整请求」保留的最大字节数，
	// 超过即截断（默认 512，排障够用且不撑爆日志）。
	UsageRequestLogBytes int `json:"usage_request_log_bytes"`
	// Scenario 记录当前激活的「情景模式」名（default/code/image/auto/multi/batch）。
	// 情景模式 = 一组网关参数包，一键应用后这些旋钮会被覆写成该情景侧重方向；
	// 账号级 RPMOverrides 永远优先于情景包，不被冲掉。
	Scenario string `json:"scenario,omitempty"`
	// TextTwoLevelQueue 开启文本池「两级队列」：同一池内非流式（工具/短调用）
	// 优先于流式（长生成）出队，只改出队顺序、不破坏该池 RPM 安全节拍。
	// 多任务并行情景默认开启。
	TextTwoLevelQueue bool `json:"text_two_level_queue"`
	ChatPasswordHash     string `json:"chat_password_hash,omitempty"`
	ChatPasswordSalt     string `json:"chat_password_salt,omitempty"`
}

// DefaultSettings 返回出厂设置。
func DefaultSettings() Settings {
	return Settings{
		MustChangePassword: true,
		SafetyFactor:       0.9,
		PacingWindowSec:    60,
		CalibrationEnabled: true,
		PenaltyCooldownSec: 65,
		PenaltyFactor:      0.8,
		MinLearnedFactor:   0.5,
		RecoverAfterSec:    300,
		RecoverSuccesses:   20,
		QueueMaxWaitMS:     120000,
		QueueMaxSize:       200,
		KeepaliveMS:        5000,
		AffinityMode:       "session_then_key",
		DefaultImageTier:   "1k",
		ModelAliases:       map[string]string{},
		AutoModelName:      "agnes-auto",
		AutoIntent: AutoIntentSettings{
			ContentScan:      true,
			MinConfidence:    0.6,
			DefaultImageSize: "1K",
			ImageInputField:  "image",
			VideoInputField:  "image",
			VideoWaitSec:     0,
			PreferredModels: map[string][]string{
				"text":  {"agnes-3.0-flash", "agnes-2.5-flash", "agnes-2.0-flash"},
				"image": {"agnes-image-2.5-flash", "agnes-image-2.1-flash"},
				"video": {"agnes-video-2.5-flash", "agnes-video-v2.0"},
			},
		},
		ModelManifestDefault: ModelManifest{
			Text:  []string{"agnes-3.0-flash", "agnes-2.5-flash", "agnes-2.0-flash"},
			Image: []string{"agnes-image-2.5-flash", "agnes-image-2.1-flash"},
			Video: []string{"agnes-video-2.5-flash", "agnes-video-2.5", "agnes-video-v2.0"},
		},
		RetryMax:            3,
		RetryBaseBackoffMS:  500,
		RetryMaxBackoffMS:   8000,
		ImageConcurrency:    4,
		VideoMaxInFlight:    2,
		BreakerReviveSec:    1800,
		VideoPollPath:       "/agnesapi",
		VideoPollWithModel:  true,
		VideoPollIntervalMS: 10000,
		LogRetentionDays:    7,
		SessionTTLHours:     72,
		ProbeModel:          "agnes-2.5-flash",
		OptimizationMode:    "concurrent_batch",
		ImageRecordRetention: 30,
		ImageMaxCapacity:    500,
		VideoMaxCapacity:    200,
		RegionPriority:      "cn_first",
		RequestTimeoutMS:    30000, // 默认 30s 上游请求超时
		FreeTextRPM:         10,    // agnes 免费档文本池标称 RPM（可配置）
		StreamIdleTimeoutMS: 60000, // 流式空闲看门狗：连续 60s 无新字节才断开
		UsageLogMaxLines:    20000,  // usage.jsonl 滚动保留最大行数
		UsageRequestLogBytes: 512,   // 日志「用户完整请求」保留字节上限
	}
}

// ---------------------------------------------------------------------------
// 情景模式（Scenario）——按网关主要执行方向一键侧重
//
// 设计口径：情景不是新模式，而是「一组网关参数包」。应用某情景即覆写 Settings
// 的若干旋钮（Safety / 文本 RPM / 重试 / 队列 / 并发 / 两级队列 …），随后走
// Hub.Reload() 热重载即时生效、免重启。
//
// 优先级：账号级 RPMOverrides 永远优先于情景包（applyScenario 只写全局 Settings
// 旋钮，不碰账号字段），因此你手工给某号调的 RPM 不会被情景冲掉。
//
// 参数按「8 个免费号」基准设计（池级吞吐 = 单号 RPM × 8 × Safety）。
// ---------------------------------------------------------------------------

// ScenarioPreset 是一个情景的参数包。
type ScenarioPreset struct {
	Name        string   `json:"name"`
	Desc        string   `json:"desc"`
	Safety      float64  `json:"safety_factor"`
	FreeTextRPM float64  `json:"free_text_rpm"`
	RetryMax    int      `json:"retry_max"`
	RetryBaseMS int      `json:"retry_base_backoff_ms"`
	RetryCapMS  int      `json:"retry_max_backoff_ms"`
	QueueSize   int      `json:"queue_max_size"`
	QueueWaitMS int      `json:"queue_max_wait_ms"`
	ImageConc   int      `json:"image_concurrency"`
	VideoInFlt  int      `json:"video_max_inflight"`
	StreamIdle  int      `json:"stream_idle_timeout_ms"`
	TwoLevel    bool     `json:"text_two_level_queue"`
	Region      string   `json:"region_priority"`
}

// ScenarioPresets 返回全部情景参数包（顺序即控制台展示顺序）。
// 8 免费号基准：text 池级 ≈ 10×8×safety，image_1k ≈ 20×8×safety。
func ScenarioPresets() []ScenarioPreset {
	return []ScenarioPreset{
		{
			Name: "default", Desc: "默认均衡：各池按官方节奏跑，不做激进侧重",
			Safety: 0.9, FreeTextRPM: 10, RetryMax: 3, RetryBaseMS: 500, RetryCapMS: 8000,
			QueueSize: 200, QueueWaitMS: 120000, ImageConc: 4, VideoInFlt: 2,
			StreamIdle: 60000, TwoLevel: false, Region: "cn_first",
		},
		{
			Name: "code", Desc: "代码编写：文本高优、首字节延迟优先；重试更快、生图/视频让路",
			Safety: 0.95, FreeTextRPM: 10, RetryMax: 3, RetryBaseMS: 400, RetryCapMS: 4000,
			QueueSize: 200, QueueWaitMS: 60000, ImageConc: 2, VideoInFlt: 1,
			StreamIdle: 60000, TwoLevel: false, Region: "cn_first",
		},
		{
			Name: "image", Desc: "图片生成：拉满 8 号并发生图，文本降档给生图让路",
			Safety: 0.9, FreeTextRPM: 6, RetryMax: 3, RetryBaseMS: 500, RetryCapMS: 8000,
			QueueSize: 200, QueueWaitMS: 120000, ImageConc: 8, VideoInFlt: 1,
			StreamIdle: 60000, TwoLevel: false, Region: "cn_first",
		},
		{
			Name: "auto", Desc: "自动判断：文本/生图/生视频均衡，交给意图判定路由",
			Safety: 0.9, FreeTextRPM: 10, RetryMax: 3, RetryBaseMS: 500, RetryCapMS: 8000,
			QueueSize: 200, QueueWaitMS: 120000, ImageConc: 4, VideoInFlt: 2,
			StreamIdle: 60000, TwoLevel: false, Region: "cn_first",
		},
		{
			Name: "multi", Desc: "多任务并行：多任务横向吃满 8 号，队列更深、429 退避保守、开两级队列（短调用优先）",
			Safety: 0.85, FreeTextRPM: 10, RetryMax: 3, RetryBaseMS: 800, RetryCapMS: 12000,
			QueueSize: 500, QueueWaitMS: 180000, ImageConc: 4, VideoInFlt: 2,
			StreamIdle: 60000, TwoLevel: true, Region: "cn_first",
		},
		{
			Name: "batch", Desc: "挂机批量：无人值守长跑，多等少错、最长空闲容忍，吞吐拉满",
			Safety: 0.85, FreeTextRPM: 10, RetryMax: 3, RetryBaseMS: 1000, RetryCapMS: 15000,
			QueueSize: 500, QueueWaitMS: 300000, ImageConc: 4, VideoInFlt: 2,
			StreamIdle: 120000, TwoLevel: false, Region: "cn_first",
		},
	}
}

// ScenarioNames 返回全部情景 ID（供控制台/校验）。
func ScenarioNames() []string {
	outs := make([]string, 0, len(ScenarioPresets()))
	for _, p := range ScenarioPresets() {
		outs = append(outs, p.Name)
	}
	return outs
}

// FindScenario 按名取情景包；找不到返回 false。
func FindScenario(name string) (ScenarioPreset, bool) {
	for _, p := range ScenarioPresets() {
		if p.Name == name {
			return p, true
		}
	}
	return ScenarioPreset{}, false
}

// ApplyScenario 把某情景的参数包覆写进 Settings（调用方负责落盘 + Hub.Reload）。
// 只改全局旋钮，绝不碰账号级字段（RPMOverrides / 各池 RPM），保证账号级优先。
// 返回 false 表示未知情景（不改任何字段）。
func ApplyScenario(st *Settings, name string) bool {
	p, ok := FindScenario(name)
	if !ok {
		return false
	}
	st.SafetyFactor = p.Safety
	st.FreeTextRPM = p.FreeTextRPM
	st.RetryMax = p.RetryMax
	st.RetryBaseBackoffMS = p.RetryBaseMS
	st.RetryMaxBackoffMS = p.RetryCapMS
	st.QueueMaxSize = p.QueueSize
	st.QueueMaxWaitMS = p.QueueWaitMS
	st.ImageConcurrency = p.ImageConc
	st.VideoMaxInFlight = p.VideoInFlt
	st.StreamIdleTimeoutMS = p.StreamIdle
	st.TextTwoLevelQueue = p.TwoLevel
	st.RegionPriority = p.Region
	st.Scenario = p.Name
	return true
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// Store 是全部持久化状态的唯一真源。
type Store struct {
	mu        sync.RWMutex
	Dir       string
	Settings  Settings
	Accounts  []*Account
	Keys      []*DownstreamKey
	Bindings  map[string]Binding
	Jobs      map[string]*VideoJob
	ImageJobs map[string]*ImageJob
	ChatLogs  map[string]*ChatLog
	// usageLineCount 是 usage.jsonl 的累计行数（进程内估计，启动时从文件读一次，
	// 之后按追加行数增长）。P1-1 用它触发滚动截断，避免长期挂机把磁盘写满。
	usageLineCount atomic.Int64
	// #20 热路径异步落盘：
	//   keysDirty 由 ChargeKey 在改内存后打标，维护循环 FlushKeys 批量写盘（避免每请求
	//   全量 saveKeysLocked）；keys/accounts 的写锁 s.mu 已覆盖 Keys 字段，故此处无需独立锁。
	//   usageBuf/usageMu 收集待落盘的 usage 记录，FlushUsage 批量追加（一次 Open/Write 多行
	//   一次 Close），用独立锁避免与 s.mu（keys/accounts/settings 读多写少）耦合。
	keysDirty atomic.Bool
	usageMu   sync.Mutex
	usageBuf  []map[string]any
	// #20 补漏：bindingsDirty 由热路径 Bind（Pick 每个粘性会话请求都会调）打标，
	// 维护循环 FlushBindings 批量写盘，避免每请求同步 saveBindingsLocked。
	// 与 keysDirty 一样走 s.mu 保护（s.Bindings 在 s.mu 下），无需独立锁。
	bindingsDirty atomic.Bool
}

// NewStore 载入（或初始化）data 目录。
func NewStore(dir string) (*Store, error) {
	s := &Store{
		Dir:      dir,
		Settings: DefaultSettings(),
		Bindings: map[string]Binding{},
		Jobs:     map[string]*VideoJob{},
		ChatLogs: map[string]*ChatLog{},
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	// 处理安装向导传入的初始/覆盖密码（AGNES_ADMIN_PASSWORD 由 cmd/main 在填写了
	// wizard_admin_password 时注入）。只要该变量非空，就覆盖当前管理员密码。
	if adminPW := os.Getenv("AGNES_ADMIN_PASSWORD"); adminPW != "" {
		salt := randHex(8)
		s.Settings.AdminPasswordSalt = salt
		s.Settings.AdminPasswordHash = HashPassword(adminPW, salt)
		s.Settings.MustChangePassword = true
		if err := s.saveSettingsLocked(); err != nil {
			return nil, err
		}
	} else if s.Settings.AdminPasswordHash == "" {
		// 无向导密码且无现有密码：回退到内部默认（不在任何界面展示）
		salt := randHex(8)
		s.Settings.AdminPasswordSalt = salt
		s.Settings.AdminPasswordHash = HashPassword("admin123", salt)
		s.Settings.MustChangePassword = true
		if err := s.saveSettingsLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.Dir, name) }

func (s *Store) load() error {
	snap := DefaultSettings()
	if err := readJSON(s.path("settings.json"), &snap); err != nil {
		return err
	}
	// 向后兼容：旧配置文件缺字段时用默认值补齐
	normalizeSettings(&snap)
	s.Settings = snap

	s.Accounts = nil
	if err := readJSON(s.path("accounts.json"), &s.Accounts); err != nil {
		return err
	}
	s.Keys = nil
	if err := readJSON(s.path("downstream_keys.json"), &s.Keys); err != nil {
		return err
	}
	s.Bindings = map[string]Binding{}
	if err := readJSON(s.path("bindings.json"), &s.Bindings); err != nil {
		return err
	}
	s.Jobs = map[string]*VideoJob{}
	if err := readJSON(s.path("video_jobs.json"), &s.Jobs); err != nil {
		return err
	}
	s.ImageJobs = map[string]*ImageJob{}
	if err := readJSON(s.path("image_jobs.json"), &s.ImageJobs); err != nil {
		return err
	}
	s.ChatLogs = map[string]*ChatLog{}
	if err := readJSON(s.path("chat_logs.json"), &s.ChatLogs); err != nil {
		return err
	}
	for _, a := range s.Accounts {
		normalizeAccount(a, s.Settings)
	}
	// P1-1：启动时从现有 usage.jsonl 读一次行数作为累计基线，
	// 之后每次追加递增；超过阈值时滚动截断，防止挂机写满磁盘。
	s.usageLineCount.Store(countLines(s.path("usage.jsonl")))
	return nil
}

// normalizeSettings 补齐缺失字段（Go 的 json.Unmarshal 不会覆盖已存在字段，
// 但 map/切片为空时需要显式兜底，否则限流会静默失效）。
func normalizeSettings(v *Settings) {
	d := DefaultSettings()
	if v.SafetyFactor <= 0 {
		v.SafetyFactor = d.SafetyFactor
	}
	if v.PacingWindowSec <= 0 {
		v.PacingWindowSec = d.PacingWindowSec
	}
	if v.MinLearnedFactor <= 0 {
		v.MinLearnedFactor = d.MinLearnedFactor
	}
	if v.PenaltyCooldownSec <= 0 {
		v.PenaltyCooldownSec = d.PenaltyCooldownSec
	}
	if v.PenaltyFactor <= 0 {
		v.PenaltyFactor = d.PenaltyFactor
	}
	if v.QueueMaxWaitMS <= 0 {
		v.QueueMaxWaitMS = d.QueueMaxWaitMS
	}
	if v.QueueMaxSize <= 0 {
		v.QueueMaxSize = d.QueueMaxSize
	}
	if v.KeepaliveMS <= 0 {
		v.KeepaliveMS = d.KeepaliveMS
	}
	if v.AffinityMode == "" {
		v.AffinityMode = d.AffinityMode
	}
	if v.DefaultImageTier == "" {
		v.DefaultImageTier = d.DefaultImageTier
	}
	if v.ModelAliases == nil {
		v.ModelAliases = map[string]string{}
	}
	if v.AutoModelName == "" {
		v.AutoModelName = d.AutoModelName
	}
	if v.AutoIntent.PreferredModels == nil {
		v.AutoIntent.PreferredModels = d.AutoIntent.PreferredModels
	}
	if v.AutoIntent.DefaultImageSize == "" {
		v.AutoIntent.DefaultImageSize = d.AutoIntent.DefaultImageSize
	}
	if v.AutoIntent.MinConfidence <= 0 {
		v.AutoIntent.MinConfidence = d.AutoIntent.MinConfidence
	}
	if v.ModelManifestDefault.Text == nil && v.ModelManifestDefault.Image == nil &&
		v.ModelManifestDefault.Video == nil {
		v.ModelManifestDefault = d.ModelManifestDefault
	}
	if v.RetryMax < 0 {
		v.RetryMax = d.RetryMax
	}
	if v.RetryBaseBackoffMS <= 0 {
		v.RetryBaseBackoffMS = d.RetryBaseBackoffMS
	}
	if v.RetryMaxBackoffMS <= 0 {
		v.RetryMaxBackoffMS = d.RetryMaxBackoffMS
	}
	if v.ImageConcurrency <= 0 {
		v.ImageConcurrency = d.ImageConcurrency
	}
	if v.VideoMaxInFlight <= 0 {
		v.VideoMaxInFlight = d.VideoMaxInFlight
	}
	if v.BreakerReviveSec <= 0 {
		v.BreakerReviveSec = d.BreakerReviveSec
	}
	if v.VideoPollPath == "" {
		v.VideoPollPath = d.VideoPollPath
	}
	if v.VideoPollIntervalMS <= 0 {
		v.VideoPollIntervalMS = d.VideoPollIntervalMS
	}
	if v.SessionTTLHours <= 0 {
		v.SessionTTLHours = d.SessionTTLHours
	}
	if v.ProbeModel == "" {
		v.ProbeModel = d.ProbeModel
	}
	if v.OptimizationMode == "" {
		v.OptimizationMode = d.OptimizationMode
	}
	if v.ImageRecordRetention <= 0 {
		v.ImageRecordRetention = d.ImageRecordRetention
	}
	if v.ImageMaxCapacity <= 0 {
		v.ImageMaxCapacity = d.ImageMaxCapacity
	}
	if v.VideoMaxCapacity <= 0 {
		v.VideoMaxCapacity = d.VideoMaxCapacity
	}
	if v.FreeTextRPM <= 0 {
		v.FreeTextRPM = d.FreeTextRPM
	}
	if v.StreamIdleTimeoutMS <= 0 {
		v.StreamIdleTimeoutMS = d.StreamIdleTimeoutMS
	}
	// UsageLogMaxLines：0 视为「沿用默认 20000」（旧配置文件缺该字段时反序列化为 0）；
	// 只有显式负值才表示「不限制」。默认取向是挂机安全——总是带上限，避免写满磁盘。
	if v.UsageLogMaxLines == 0 {
		v.UsageLogMaxLines = d.UsageLogMaxLines
	}
	if v.UsageRequestLogBytes <= 0 {
		v.UsageRequestLogBytes = d.UsageRequestLogBytes
	}
	// 情景模式：旧配置缺该字段时归到 default（不改任何旋钮，仅记录名）。
	if v.Scenario == "" {
		v.Scenario = "default"
	}
}

// normalizeAccount 补齐缺失字段。
//
// model_manifest 的语义：**缺失（nil）→ 用默认清单补齐**（视为「还没声明」）；
// **非 nil 的空切片 → 保留为空**（视为「明示不支持该模态」）。
func normalizeAccount(a *Account, s Settings) {
	if a.AccessType == "" {
		if _, ok := RPMTable[a.AccessType]; !ok {
			a.AccessType = "free"
		}
	}
	if _, ok := RPMTable[a.AccessType]; !ok {
		a.AccessType = "free"
	}
	if a.ClassesEnabled == nil {
		a.ClassesEnabled = append([]string(nil), PoolClasses...)
	}
	if a.RPMOverrides == nil {
		a.RPMOverrides = map[string]float64{}
	}
	if a.MaxConcurrency <= 0 {
		a.MaxConcurrency = 8
	}
	if a.LearnedFactor <= 0 {
		a.LearnedFactor = 1.0
	}
	if a.PoolFactors == nil {
		a.PoolFactors = map[string]float64{}
	}
	if a.BaseURL == "" {
		a.BaseURL = DefaultBaseURL
	}
	if a.ModelManifest.Text == nil {
		a.ModelManifest.Text = append([]string(nil), s.ModelManifestDefault.Text...)
	}
	if a.ModelManifest.Image == nil {
		a.ModelManifest.Image = append([]string(nil), s.ModelManifestDefault.Image...)
	}
	if a.ModelManifest.Video == nil {
		a.ModelManifest.Video = append([]string(nil), s.ModelManifestDefault.Video...)
	}
}

// ---- 原子写 ----

func writeJSON(path string, payload any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, out any) error {
	buf, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(strings.TrimSpace(string(buf))) == 0 {
		return nil
	}
	if err := json.Unmarshal(buf, out); err != nil {
		// 坏文件不应让服务起不来：备份后按默认值继续
		_ = os.WriteFile(path+".broken", buf, 0o644)
		return nil
	}
	return nil
}

func (s *Store) saveSettingsLocked() error { return writeJSON(s.path("settings.json"), s.Settings) }

// SaveSettings 落盘设置。
func (s *Store) SaveSettings() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveSettingsLocked()
}

// SaveAccounts 落盘账号池。
func (s *Store) SaveAccounts() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveAccountsLocked()
}

func (s *Store) saveAccountsLocked() error { return writeJSON(s.path("accounts.json"), s.Accounts) }

// SaveKeys 落盘下游密钥。
func (s *Store) SaveKeys() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveKeysLocked()
}

func (s *Store) saveKeysLocked() error { return writeJSON(s.path("downstream_keys.json"), s.Keys) }

// SaveBindings 落盘粘性绑定。
func (s *Store) SaveBindings() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveBindingsLocked()
}

func (s *Store) saveBindingsLocked() error { return writeJSON(s.path("bindings.json"), s.Bindings) }

// SaveJobs 落盘视频任务映射。
func (s *Store) SaveJobs() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveJobsLocked()
}

func (s *Store) saveJobsLocked() error { return writeJSON(s.path("video_jobs.json"), s.Jobs) }

// ---- 只读快照 ----

// SettingsSnapshot 返回设置的深拷贝，避免调用方直接改内存对象。
func (s *Store) SettingsSnapshot() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.Settings
	out.ModelAliases = cloneMap(s.Settings.ModelAliases)
	out.AutoIntent.PreferredModels = cloneListMap(s.Settings.AutoIntent.PreferredModels)
	out.ModelManifestDefault = s.Settings.ModelManifestDefault.Clone()
	return out
}

// AccountsSnapshot 返回账号池的深拷贝（含 api_key，仅服务端内部使用）。
func (s *Store) AccountsSnapshot() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.Accounts))
	for _, a := range s.Accounts {
		out = append(out, a.Clone())
	}
	return out
}

// Clone 深拷贝账号。
func (a *Account) Clone() *Account {
	cp := *a
	cp.ClassesEnabled = cloneSlice(a.ClassesEnabled)
	cp.ModelManifest = a.ModelManifest.Clone()
	cp.RPMOverrides = cloneMap(a.RPMOverrides)
	cp.PoolFactors = cloneMap(a.PoolFactors)
	return &cp
}

func cloneMap[V any](in map[string]V) map[string]V {
	if in == nil {
		return map[string]V{}
	}
	out := make(map[string]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneListMap(in map[string][]string) map[string][]string {
	if in == nil {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// AccountByID 按 ID 取账号（返回内部指针，改动后需自行落盘）。
func (s *Store) AccountByID(id string) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// MutateAccount 在写锁内修改账号并落盘，避免控制台与调度器读改竞争。
func (s *Store) MutateAccount(id string, fn func(*Account) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.Accounts {
		if a.ID == id {
			if fn(a) {
				_ = s.saveAccountsLocked()
			}
			return true
		}
	}
	return false
}

// TouchStatsDirty 在写锁内改账号统计但**不**落盘，仅由调度器 30s 维护循环批量 flush。
// 用途：429 风暴 / 高频成功路径避免每次请求都同步写 accounts.json。
func (s *Store) TouchStatsDirty(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.Accounts {
		if a.ID == id {
			return // 数据已在内存，落盘交给定期 flush（见 hub.flushFactors）
		}
	}
}

// FlushAccounts 把内存中所有账号的状态写一次 accounts.json。
// 用于维护循环的批量落盘（替代热路径里的同步写盘）。
func (s *Store) FlushAccounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.saveAccountsLocked()
}

// MutateAccountNoSave 在写锁内改账号但不落盘（由调用方负责标 dirty / 定时落盘）。
func (s *Store) MutateAccountNoSave(id string, fn func(*Account) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.Accounts {
		if a.ID == id {
			fn(a)
			return true
		}
	}
	return false
}

// MutateAccountMap 批量写回 (账号 × 池) 二维校准因子。
//
// 入参 key 形如 "<account_id>|<pool_class>"。做成批量接口是为了降低写放大：
// 高频 429 时若每次都重写整个 accounts.json，磁盘与 CPU 都会被无谓消耗。
func (s *Store) MutateAccountMap(values map[string]float64, keys []string) {
	type pair struct{ id, cls string }
	pairs := make([]pair, 0, len(keys))
	for _, k := range keys {
		parts := strings.SplitN(k, "|", 2)
		if len(parts) == 2 {
			pairs = append(pairs, pair{parts[0], parts[1]})
		}
	}
	if len(pairs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.Accounts {
		for _, p := range pairs {
			if a.ID != p.id {
				continue
			}
			if a.PoolFactors == nil {
				a.PoolFactors = map[string]float64{}
			}
			a.PoolFactors[p.cls] = values[p.id+"|"+p.cls]
		}
	}
	_ = s.saveAccountsLocked()
}

// AddAccount 新增账号。
func (s *Store) AddAccount(name, apiKey, accessType, baseURL string, manifest *ModelManifest) *Account {
	if _, ok := RPMTable[accessType]; !ok {
		accessType = "free"
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURLForType(accessType)
	}
	a := &Account{
		ID:             NewID("acc"),
		Name:           orDefault(name, "未命名账号"),
		APIKey:         strings.TrimSpace(apiKey),
		BaseURL:        strings.TrimRight(baseURL, "/"),
		AccessType:     accessType,
		Enabled:        true,
		ClassesEnabled: append([]string(nil), PoolClasses...),
		RPMOverrides:   map[string]float64{},
		MaxConcurrency: 8,
		LearnedFactor:  1,
		PoolFactors:    map[string]float64{},
		CreatedAt:      float64(time.Now().UnixNano()) / 1e9,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if manifest != nil {
		a.ModelManifest = manifest.Clone()
	}
	normalizeAccount(a, s.Settings)
	s.Accounts = append(s.Accounts, a)
	_ = s.saveAccountsLocked()
	return a
}

// DeleteAccount 删除账号。
func (s *Store) DeleteAccount(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.Accounts {
		if a.ID == id {
			s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
			_ = s.saveAccountsLocked()
			return true
		}
	}
	return false
}

// RPMFor 取该账号在该池的基线 RPM（先看覆盖，再看档位表）。
func (s *Store) RPMFor(a *Account, poolClass string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return rpmFor(a, poolClass, s.Settings.FreeTextRPM)
}

func rpmFor(a *Account, poolClass string, freeTextRPM float64) float64 {
	if v, ok := a.RPMOverrides[poolClass]; ok && v > 0 {
		return v
	}
	// agnes 免费档文本池的 RPM 改为可配置项（FreeTextRPM）；0 表示用档位表。
	if a.AccessType == "free" && poolClass == "text" && freeTextRPM > 0 {
		return freeTextRPM
	}
	table, ok := RPMTable[a.AccessType]
	if !ok {
		table = RPMTable["free"]
	}
	if v, ok := table[poolClass]; ok {
		return v
	}
	return 1
}

// ---- 下游密钥 ----

// KeyByValue 按密钥值查找（常量时间比较）。
func (s *Store) KeyByValue(value string) *DownstreamKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.Keys {
		if subtleEqual(k.Key, value) {
			return k
		}
	}
	return nil
}

// AddKey 签发下游密钥。
func (s *Store) AddKey(name string, classes []string, daily, total int64, pinned string) *DownstreamKey {
	if len(classes) == 0 {
		classes = []string{"*"}
	}
	item := &DownstreamKey{
		Key:           "sk-agnes-" + randHex(24),
		Name:          orDefault(name, "未命名密钥"),
		Enabled:       true,
		Classes:       classes,
		DailyQuota:    daily,
		TotalQuota:    total,
		PinnedAccount: pinned,
		UsedDate:      time.Now().Format("2006-01-02"),
		CreatedAt:     float64(time.Now().UnixNano()) / 1e9,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Keys = append(s.Keys, item)
	_ = s.saveKeysLocked()
	return item
}

// DeleteKey 删除密钥。
func (s *Store) DeleteKey(value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.Keys {
		if k.Key == value {
			s.Keys = append(s.Keys[:i], s.Keys[i+1:]...)
			_ = s.saveKeysLocked()
			return true
		}
	}
	return false
}

// MutateKey 修改密钥并落盘。
func (s *Store) MutateKey(value string, fn func(*DownstreamKey) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.Keys {
		if k.Key == value {
			if fn(k) {
				_ = s.saveKeysLocked()
			}
			return true
		}
	}
	return false
}

// FlushKeys 批量落盘下游密钥（#20：维护循环周期调用）。
// 仅当 ChargeKey 在内存改过（keysDirty=true）才真正写一次盘，把「每请求全量
// saveKeysLocked」降为「30s 一次」；与 FlushAccounts 的兜底写盘同口径。
func (s *Store) FlushKeys() {
	if !s.keysDirty.CompareAndSwap(true, false) {
		return // 无脏数据
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.saveKeysLocked()
}

// KeysSnapshot 返回密钥列表深拷贝。
func (s *Store) KeysSnapshot() []*DownstreamKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*DownstreamKey, 0, len(s.Keys))
	for _, k := range s.Keys {
		cp := *k
		cp.Classes = append([]string(nil), k.Classes...)
		out = append(out, &cp)
	}
	return out
}

// KeyAllowsClass 判断密钥是否允许调用该池。
func KeyAllowsClass(k *DownstreamKey, poolClass string) bool {
	if k == nil {
		return false
	}
	if len(k.Classes) == 0 {
		return true
	}
	for _, c := range k.Classes {
		if c == "*" || c == poolClass {
			return true
		}
	}
	return false
}

// QuotaExceeded 返回超限原因（空串表示未超限）。
func (s *Store) QuotaExceeded(k *DownstreamKey) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	today := time.Now().Format("2006-01-02")
	usedToday := int64(0)
	if k.UsedDate == today {
		usedToday = k.UsedToday
	}
	if k.DailyQuota > 0 && usedToday >= k.DailyQuota {
		return fmt.Sprintf("下游密钥已达每日额度上限（%d）", k.DailyQuota)
	}
	if k.TotalQuota > 0 && k.UsedTotal >= k.TotalQuota {
		return fmt.Sprintf("下游密钥已达总额度上限（%d）", k.TotalQuota)
	}
	return ""
}

// ChargeKey 记账（按天滚动）。tokens 为该次请求消耗的 token 总量（解析不到则为 0）。
// #20：热路径不再同步全量落盘（原 saveKeysLocked 每请求一次 Open/Write/Close），
// 改为内存累加 + 标 keysDirty，由维护循环 FlushKeys 周期批量写盘。
func (s *Store) ChargeKey(value string, tokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	for _, k := range s.Keys {
		if k.Key == value {
			if k.UsedDate != today {
				k.UsedDate = today
				k.UsedToday = 0
				k.UsedTokensToday = 0
			}
			k.UsedTotal++
			k.UsedToday++
			k.UsedTokensTotal += tokens
			k.UsedTokensToday += tokens
			s.keysDirty.Store(true)
			return
		}
	}
}

// ---- 粘性绑定 ----

// Bind 写入绑定。
// #20 补漏：热路径（Pick 每个粘性会话请求都会调）不再同步 saveBindingsLocked，
// 改为内存写 + 标 bindingsDirty，由维护循环 FlushBindings 30s 批量落盘。
func (s *Store) Bind(sessionKey, accountID string) {
	if sessionKey == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Bindings == nil {
		s.Bindings = map[string]Binding{}
	}
	s.Bindings[sessionKey] = Binding{AccountID: accountID, Updated: float64(time.Now().UnixNano()) / 1e9}
	s.gcBindingsLocked()
	s.bindingsDirty.Store(true)
}

// FlushBindings 批量落盘粘性绑定（#20 补漏：维护循环周期调用）。
// 仅当热路径 Bind 改过（bindingsDirty=true）才真正写一次盘；与 FlushKeys 同口径。
func (s *Store) FlushBindings() {
	if !s.bindingsDirty.CompareAndSwap(true, false) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.saveBindingsLocked()
}

// BindingsGet 读取绑定。
func (s *Store) BindingsGet(sessionKey string) (Binding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.Bindings[sessionKey]
	return b, ok
}

func (s *Store) gcBindingsLocked() {
	ttl := s.Settings.SessionTTLHours * 3600
	if ttl <= 0 {
		return
	}
	cutoff := float64(time.Now().UnixNano())/1e9 - ttl
	for k, v := range s.Bindings {
		if v.Updated < cutoff {
			delete(s.Bindings, k)
		}
	}
}

// BindingsSnapshot 返回绑定列表（供控制台展示）。
func (s *Store) BindingsSnapshot() map[string]Binding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.Bindings)
}

// ClearBindings 清空全部绑定。
func (s *Store) ClearBindings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Bindings = map[string]Binding{}
	_ = s.saveBindingsLocked()
}

// ---- 视频任务 ----

// PutJob 记录视频任务。
func (s *Store) PutJob(job *VideoJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Jobs == nil {
		s.Jobs = map[string]*VideoJob{}
	}
	s.Jobs[job.JobID] = job
	s.evictVideoJobsLocked()
	_ = s.saveJobsLocked()
}

// evictVideoJobsLocked 在持锁状态下按容量上限淘汰最旧记录（调用前必须已加写锁）。
func (s *Store) evictVideoJobsLocked() {
	cap := s.Settings.VideoMaxCapacity
	if cap <= 0 || len(s.Jobs) <= cap {
		return
	}
	ids := make([]*VideoJob, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		ids = append(ids, j)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].CreatedAt < ids[j].CreatedAt })
	for i := 0; i < len(ids)-cap; i++ {
		delete(s.Jobs, ids[i].JobID)
	}
}

// DeleteVideoJob 删除单条视频记录。
func (s *Store) DeleteVideoJob(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Jobs[id]; !ok {
		return false
	}
	delete(s.Jobs, id)
	_ = s.saveJobsLocked()
	return true
}

// ClearVideoJobs 清空全部视频记录。
func (s *Store) ClearVideoJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Jobs = map[string]*VideoJob{}
	_ = s.saveJobsLocked()
}

// JobByID 取视频任务。
func (s *Store) JobByID(id string) (*VideoJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.Jobs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	return &cp, true
}

// JobByVideoID 按上游 video_id 反查任务。
func (s *Store) JobByVideoID(videoID string) (*VideoJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.Jobs {
		if j.VideoID == videoID {
			cp := *j
			return &cp, true
		}
	}
	return nil, false
}

// JobsSnapshot 返回全部任务。
func (s *Store) JobsSnapshot() []*VideoJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*VideoJob, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		cp := *j
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// ---- 图片任务 ----

// PutImageJob 记录图片任务，并在超过容量上限时淘汰最旧记录。
func (s *Store) PutImageJob(job *ImageJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ImageJobs == nil {
		s.ImageJobs = map[string]*ImageJob{}
	}
	s.ImageJobs[job.JobID] = job
	s.evictImageJobsLocked()
	_ = s.saveImageJobsLocked()
}

// evictImageJobsLocked 在持锁状态下按容量上限淘汰最旧记录（调用前必须已加写锁）。
func (s *Store) evictImageJobsLocked() {
	cap := s.Settings.ImageMaxCapacity
	if cap <= 0 || len(s.ImageJobs) <= cap {
		return
	}
	// 按创建时间升序排序，删掉最旧的若干条
	ids := make([]*ImageJob, 0, len(s.ImageJobs))
	for _, j := range s.ImageJobs {
		ids = append(ids, j)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].CreatedAt < ids[j].CreatedAt })
	for i := 0; i < len(ids)-cap; i++ {
		delete(s.ImageJobs, ids[i].JobID)
	}
}

// DeleteImageJob 删除单条图片记录。
func (s *Store) DeleteImageJob(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ImageJobs[id]; !ok {
		return false
	}
	delete(s.ImageJobs, id)
	_ = s.saveImageJobsLocked()
	return true
}

// ClearImageJobs 清空全部图片记录。
func (s *Store) ClearImageJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ImageJobs = map[string]*ImageJob{}
	_ = s.saveImageJobsLocked()
}

// ImageJobByID 取图片任务。
func (s *Store) ImageJobByID(id string) (*ImageJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.ImageJobs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	return &cp, true
}

// ImageJobsSnapshot 返回全部图片任务（倒序，按创建时间）。
func (s *Store) ImageJobsSnapshot() []*ImageJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ImageJob, 0, len(s.ImageJobs))
	for _, j := range s.ImageJobs {
		cp := *j
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func (s *Store) saveImageJobsLocked() error { return writeJSON(s.path("image_jobs.json"), s.ImageJobs) }

// ---- 聊天记录 ----

// AddChatLog 追加一条聊天对话记录并落盘。
func (s *Store) AddChatLog(log *ChatLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ChatLogs == nil {
		s.ChatLogs = map[string]*ChatLog{}
	}
	s.ChatLogs[log.ID] = log
	_ = s.saveChatLogsLocked()
}

// DeleteChatLog 删除单条聊天记录。
func (s *Store) DeleteChatLog(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ChatLogs[id]; !ok {
		return false
	}
	delete(s.ChatLogs, id)
	_ = s.saveChatLogsLocked()
	return true
}

// ClearChatLogs 清空全部聊天记录。
func (s *Store) ClearChatLogs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ChatLogs = map[string]*ChatLog{}
	_ = s.saveChatLogsLocked()
}

// ChatLogByID 取单条聊天记录。
func (s *Store) ChatLogByID(id string) (*ChatLog, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.ChatLogs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	return &cp, true
}

// ChatLogsSnapshot 返回全部聊天记录（按创建时间倒序）。
func (s *Store) ChatLogsSnapshot() []*ChatLog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ChatLog, 0, len(s.ChatLogs))
	for _, j := range s.ChatLogs {
		cp := *j
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func (s *Store) saveChatLogsLocked() error { return writeJSON(s.path("chat_logs.json"), s.ChatLogs) }

// ---- 用量日志 ----

// AppendUsage 记录一行 usage（#20：热路径不再每请求 Open/Write/Close）。
// 改为入内存缓冲 usageBuf，由维护循环 FlushUsage 批量落盘（一次 Open/Write 多行
// 一次 Close）。usageLineCount 仍按记录数原子自增，用于触发滚动截断。
func (s *Store) AppendUsage(record map[string]any) {
	if record == nil {
		return
	}
	s.usageMu.Lock()
	s.usageBuf = append(s.usageBuf, record)
	s.usageMu.Unlock()

	// 行数计数只作「是否该截断」的触发器，真正截断在 FlushUsage 批量落盘后
	// 于磁盘态上执行（此处文件里还没有这批缓冲行，不能据此截断）。
	s.usageLineCount.Add(1)
}

// FlushUsage 批量把 usageBuf 里的记录一次性追加到 usage.jsonl（#20：维护循环周期调用）。
// 一次 Open/Write 多行 + 一次 Close，把「每请求一次磁盘往返」降为「30s 一次批量写」。
// 与 429 不写盘同口径：挂机期间若进程被杀，最多丢最近一个 flush 周期（≤30s）的日志。
func (s *Store) FlushUsage() {
	s.usageMu.Lock()
	if len(s.usageBuf) == 0 {
		s.usageMu.Unlock()
		return
	}
	batch := s.usageBuf
	s.usageBuf = nil
	s.usageMu.Unlock()

	f, err := os.OpenFile(s.path("usage.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// 写盘失败：记录放回缓冲头部，下轮重试（不丢数据）。
		s.usageMu.Lock()
		s.usageBuf = append(batch, s.usageBuf...)
		s.usageMu.Unlock()
		return
	}
	var sb strings.Builder
	for _, rec := range batch {
		line, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		sb.Write(line)
		sb.WriteByte('\n')
	}
	if sb.Len() > 0 {
		_, _ = f.WriteString(sb.String())
	}
	_ = f.Close()

	// 批量落盘完成后，基于磁盘态做一次截断（P1-1 滚动日志防磁盘写满）。
	maxLines := s.Settings.UsageLogMaxLines
	if maxLines > 0 && s.usageLineCount.Load()%int64(maxLines) == 0 {
		s.truncateUsageLog(maxLines)
	}
}

// truncateUsageLog 把 usage.jsonl 截断为最近 keep 行，并更新计数。
func (s *Store) truncateUsageLog(keep int) {
	path := s.path("usage.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if out != "" {
		out += "\n"
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
	s.usageLineCount.Store(countLines(path))
}

// countLines 统计文件的换行数（作为行数的近似基线）。
func countLines(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return int64(strings.Count(string(data), "\n"))
}

// TailUsage 读取最近 n 条日志（倒序，只读磁盘）。
func (s *Store) TailUsage(n int) []map[string]any {
	buf, err := os.ReadFile(s.path("usage.jsonl"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]map[string]any, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// TailUsageLive 控制台日志页专用（#20 修复）：把「尚未落盘的内存缓冲」也算进最近
// 记录，避免异步批量落盘后，刚发完请求的 user_request（用户输入）要等 30s 才在
// 日志页可见。pending 里的记录尚未上盘，与磁盘内容不重叠；结果倒序（最新在前），
// 取最近 n 条。
func (s *Store) TailUsageLive(n int) []map[string]any {
	if n <= 0 {
		n = 200
	}
	// 1) 内存未落盘缓冲（append 顺序：旧→新）
	s.usageMu.Lock()
	pending := make([]map[string]any, len(s.usageBuf))
	copy(pending, s.usageBuf)
	pendingCount := len(pending)
	s.usageMu.Unlock()

	// 2) 磁盘尾部：多读 pending 的份数，保证合并后仍有 n 条可选
	disk := s.TailUsage(n + pendingCount)

	out := make([]map[string]any, 0, n)
	// pending 倒序遍历即「新→旧」，先取它（比磁盘里更近）
	for i := pendingCount - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, pending[i])
	}
	for _, rec := range disk {
		if len(out) >= n {
			break
		}
		out = append(out, rec)
	}
	return out
}

// ---- 密码 ----

// HashPassword 口令散列（与 Python 基线一致：sha256(salt+password)）。
func HashPassword(password, salt string) string {
	sum := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(sum[:])
}

// VerifyPassword 校验口令。
func (s *Store) VerifyPassword(password string) bool {
	s.mu.RLock()
	hash, salt := s.Settings.AdminPasswordHash, s.Settings.AdminPasswordSalt
	s.mu.RUnlock()
	if hash == "" {
		return false
	}
	return subtleEqual(hash, HashPassword(password, salt))
}

// SetPassword 改密。
func (s *Store) SetPassword(password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	salt := randHex(8)
	s.Settings.AdminPasswordSalt = salt
	s.Settings.AdminPasswordHash = HashPassword(password, salt)
	s.Settings.MustChangePassword = false
	return s.saveSettingsLocked()
}

// VerifyChatPassword 校验 chat 页面密码。
func (s *Store) VerifyChatPassword(password string) bool {
	s.mu.RLock()
	hash, salt := s.Settings.ChatPasswordHash, s.Settings.ChatPasswordSalt
	s.mu.RUnlock()
	if hash == "" {
		return true // 未设置密码，允许访问
	}
	return subtleEqual(hash, HashPassword(password, salt))
}

// SetChatPassword 设置/更新 chat 页面密码。空密码表示禁用。
func (s *Store) SetChatPassword(password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(password) == "" {
		s.Settings.ChatPasswordHash = ""
		s.Settings.ChatPasswordSalt = ""
	} else {
		salt := randHex(8)
		s.Settings.ChatPasswordSalt = salt
		s.Settings.ChatPasswordHash = HashPassword(password, salt)
	}
	return s.saveSettingsLocked()
}

// SessionToken 派生会话令牌（改密即全端失效）。
func (s *Store) SessionToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sum := sha256.Sum256([]byte(s.Settings.AdminPasswordSalt + ":" + s.Settings.AdminPasswordHash + ":baiPiao-hub"))
	return hex.EncodeToString(sum[:])
}

// ---- 设置写入 ----

// UpdateSettings 用回调在写锁内修改设置并落盘。
func (s *Store) UpdateSettings(fn func(*Settings)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.Settings)
	normalizeSettings(&s.Settings)
	return s.saveSettingsLocked()
}

// ---- 小工具 ----

// NewID 生成带前缀的随机 ID。
func NewID(prefix string) string { return prefix + "_" + randHex(6) }

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// subtleEqual 常量时间字符串比较，避免密钥被时序侧信道推断。
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
