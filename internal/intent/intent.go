// Package intent 实现多平台自动意图判定与请求/响应适配。
//
// 为什么必须由网关做这件事
// ------------------------
// 实测：把 model=agnes-auto 直接发给 /v1/images/generations，上游回
// `模型 agnes-auto 是 chat 模型，请使用 /v1/chat/completions`。
// 也就是说「一个模型名走全模态」在上游不存在，必须由网关判定模态、
// 换成真实模型、必要时改写端点与请求体，再把响应翻译回 chat 形态。
//
// 判定信号（可信度从高到低）
//  1. endpoint —— 打的是 /v1/images/generations 或 /v1/videos。端点即意图。
//  2. model    —— 客户端给了具体模型名（非 auto），按模型名归类，绝不覆盖。
//  3. params   —— size/aspect_ratio（图）或 num_frames/frame_rate/duration（视频）。
//  4. content  —— 从用户那句话里读祈使式生成意图。
//  5. default  —— 保守回落到 text。
//
// 第 4 条是唯一有误判风险的一条，因此：只在端点不明确时启用；用「动词+名词」
// 结构匹配而非单词匹配；元讨论/疑问/代码块/教学措辞一律扣分；agent 类请求
// （含 tools/tool_calls/response_format）直接禁用；必须越过阈值才切换。
//
// 误判代价不对称：把提问当生图，用户会收到一张莫名其妙的图。所以阈值偏高，
// 且每次判定的 source/score/reason 都写进响应头与日志，便于事后追查。
package intent

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 模态常量。
const (
	Text  = "text"
	Image = "image"
	Video = "video"
)

// Modalities 全部模态。
var Modalities = []string{Text, Image, Video}

// AutoModelNames 客户端可用的统一模型名（等价）。
var AutoModelNames = []string{"auto", "auto-all"}

// SourceLabels 判定来源的中文说明（只进日志与响应体，绝不进响应头）。
var SourceLabels = map[string]string{
	"endpoint": "端点",
	"model":    "模型名",
	"params":   "请求参数",
	"content":  "内容意图",
	"default":  "默认回落",
	"forced":   "管理端强制",
}

const (
	cnVerb    = `(?:请|帮我|帮忙|给我|麻烦|替我|想要|想)?\s*(?:画|绘制|描画|生成|做|出|来|搞|弄)\s*(?:一|两|三|四|五|几|个|张|幅|份|段|条)?\s*`
	cnImgNoun = `(?:图片|图像|插画|插图|照片|图画|海报|封面|头像|图标|logo|壁纸|背景图|配图|示意图|宣传图|效果图|图)`
	cnVidNoun = `(?:视频|短片|动画|影片|视频片段|动图|微电影)`
)

// Rules 是可被控制台覆写的判定规则。
type Rules struct {
	ImagePatterns    []*regexp.Regexp
	VideoPatterns    []*regexp.Regexp
	NegativePatterns []*regexp.Regexp
	MinConfidence    float64
	ContentScan      bool
}

// DefaultRules 返回出厂规则。
func DefaultRules() Rules {
	return Rules{
		ImagePatterns: compileAll([]string{
			// 触发词 + 画/绘制（「帮我画…」「请绘制…」）
			`(?:帮我|帮忙|请|给我|麻烦|替我|想要|想)\s*(?:画|绘制|描画)`,
			// 画/绘制 + 量词 + 内容（「画一张赛博朋克城市夜景」——名词可能在很远的地方甚至没有）
			`(?:画|绘制|描画)\s*(?:一|两|三|四|五|几)?\s*(?:张|幅|个|只)\s*\S`,
			// 生成/做/出 + 图片量词 + 内容（「生成一张产品海报」）
			`(?:生成|做|出|来|搞|弄)\s*(?:一|两|三|几)?\s*(?:张|幅|份)\s*\S`,
			`(?:出图|作图|配图|画图|文生图|图生图|以图生图)`,
			// 生成动词 + 40 字内的图片类名词
			`(?:画|绘制|描画|生成|制作|设计|做)\s*[^。！？!?\n]{0,40}?(?:图片|图像|插画|插图|照片|图画|海报|封面|头像|图标|logo|壁纸|背景图|配图|示意图|宣传图|效果图|表情包|图)`,
			`(?i)\b(?:generate|create|make|draw|paint|render|produce|design)\b[^.\n]{0,60}?\b(?:image|picture|photo|illustration|artwork|poster|logo|icon|avatar|wallpaper|portrait)\b`,
			`(?i)\b(?:draw|paint|imagine)\s+(?:me\s+)?(?:a|an|the)\b`,
			`(?i)^\s*/(?:imagine|image|draw)\b`,
			`(?i)^\s*!(?:draw|image)\b`,
		}),
		VideoPatterns: compileAll([]string{
			`(?:文生视频|图生视频|视频生成|生成视频|做成视频|动起来|生成动画|做成动画|做个视频|做视频|拍一段)`,
			`(?:帮我|帮忙|请|给我|麻烦|替我|想要|想)\s*(?:生成|做|来|制作|搞|出)\s*(?:一|两|三|几)?\s*(?:段|个|条)?\s*(?:视频|短片|动画|影片)`,
			`(?:生成|做|来|制作)\s*(?:一|两|三|几)?\s*(?:段|个|条)\s*[^。！？!?\n]{0,30}?(?:视频|短片|动画|影片|微电影)`,
			`(?i)\b(?:generate|create|make|produce|render)\b[^.\n]{0,60}?\b(?:video|animation|animated\s+clip|movie|short\s+film|clip)\b`,
			`(?i)\b(?:animate|morph)\b[^.\n]{0,40}\b(?:image|photo|picture|it)\b`,
		}),
		NegativePatterns: compileAll([]string{
			`(?:什么是|是什么|如何|怎么|怎样|教程|区别|为什么|介绍一下|解释一下|讲解|原理|对比|优缺点|是否支持|能不能|可以吗|支持吗)`,
			`(?i)\b(?:how\s+(?:do|does|to|can|would|should)|what\s+(?:is|are|does)|why\b|explain|tutorial|guide|difference|versus|vs\.?|documentation|docs?|reference)\b`,
			"```",
			`(?:报错|错误码|error\s*code|stack\s*trace|traceback)`,
		}),
		MinConfidence: 0.6,
		ContentScan:   true,
	}
}

func compileAll(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if re, err := regexp.Compile(p); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// Config 是判定所需的设置快照。
type Config struct {
	ContentScan      bool
	MinConfidence    float64
	DefaultImageSize string
	ImageInputField  string
	VideoInputField  string
	VideoWaitSec     int
	PreferredModels  map[string][]string
}

// 端点与参数信号
var (
	imagePathHints = []string{"/images", "/image/", "/edits"}
	videoPathHints = []string{"/videos", "/video/"}

	imageParamStrong = []string{"size", "aspect_ratio", "aspectratio", "image_size"}
	videoParamStrong = []string{"num_frames", "frame_rate", "frames", "fps", "duration",
		"first_frame_image", "last_frame_image", "image_tail"}

	agentGuards = []string{"tools", "tool_choice", "functions", "function_call", "response_format"}
)

// ImageFieldWhitelist / VideoFieldWhitelist 是允许透传给上游的字段。
var (
	ImageFieldWhitelist = []string{
		"size", "response_format", "quality", "style", "seed", "negative_prompt",
		"image", "images", "image_url", "strength", "aspect_ratio", "width", "height",
		"prompt_extend", "watermark",
	}
	VideoFieldWhitelist = []string{
		"height", "width", "num_frames", "frame_rate", "duration", "seed",
		"negative_prompt", "resolution", "ratio", "image", "images", "image_url",
		"first_frame_image", "last_frame_image",
	}
	// 这些键即便不在白名单里也不算「被丢弃」——它们是 chat 形态的固有字段
	ignorableKeys = map[string]bool{
		"model": true, "messages": true, "prompt": true, "stream": true,
		"tools": true, "tool_choice": true, "functions": true, "response_format": true,
		"temperature": true, "top_p": true, "user": true, "metadata": true,
		"max_tokens": true, "max_completion_tokens": true, "system": true, "stop": true,
		"frequency_penalty": true, "presence_penalty": true, "n": true, "logprobs": true,
		"top_logprobs": true, "parallel_tool_calls": true, "service_tier": true, "store": true,
	}
)

// IsAutoModel 判断是否「让网关决定」的模型名。
//
// 重要规则：自定义 auto 名称（如 "my-auto"）如果与上游已知模型名重合，
// 必须视为非 auto —— 否则网关会错误地路由到 auto 模态判定逻辑，
// 而不是把请求当作显式模型调用直接透传。
// 例如：用户把 agnes-auto 改名为 "agnes-image-2.5-flash"，这应该走显式 image 路由。
func IsAutoModel(name string) bool {
	low := strings.ToLower(strings.TrimSpace(name))
	if low == "" {
		return false
	}
	// 内置 auto 名称集合
	autoNames := map[string]bool{
		"auto": true, "auto-all": true,
	}
	if autoNames[low] {
		return true
	}
	// auto 前缀匹配
	if strings.HasPrefix(low, "auto") {
		return true
	}
	// 已知上游模型名集合（与 pool.TextModels/ImageModels/VideoModels 同步）
	knownModels := map[string]bool{
		// text models
		"agnes-2.5-flash": true, "agnes-2.0-flash": true, "agnes-1.5-flash": true,
		"agnes-3.0-flash": true, "agnes-2.5-pro": true,
		"agnes-2.5-pro-alpha": true, "agnes-2.5-pro-beta": true,
		// image models
		"agnes-image-2.0-flash": true, "agnes-image-2.1-flash": true, "agnes-image-2.5-flash": true,
		// video models
		"agnes-video-v2.0": true, "agnes-video-2.5": true, "agnes-video-2.5-flash": true,
		// built-in aliases
		"gpt-4o": true, "gpt-4o-mini": true, "gpt-4-turbo": true, "gpt-3.5-turbo": true,
		"claude-3-5-sonnet": true, "claude-sonnet-4": true, "dall-e-3": true, "gpt-image-1": true,
	}
	if knownModels[low] {
		return false
	}
	// 模糊匹配：含 image/video 关键字的可能是未知模型，不阻断 auto
	lowNoDash := strings.ReplaceAll(low, "-", "")
	if strings.Contains(lowNoDash, "image") || strings.Contains(lowNoDash, "video") {
		// 可能是自定义别名，不强制阻断，但也不视为标准 auto
		return false
	}
	return false
}

// ModalityFromPath 端点信号。
func ModalityFromPath(path string) string {
	low := strings.ToLower(path)
	for _, h := range imagePathHints {
		if strings.Contains(low, h) {
			return Image
		}
	}
	for _, h := range videoPathHints {
		if strings.Contains(low, h) {
			return Video
		}
	}
	return ""
}

// ModalityFromParams 参数信号。
func ModalityFromParams(body map[string]any) string {
	for _, k := range videoParamStrong {
		if v, ok := body[k]; ok && v != nil {
			return Video
		}
	}
	hasMessages := len(asMessages(body["messages"])) > 0
	for _, k := range imageParamStrong {
		if v, ok := body[k]; ok && v != nil {
			return Image
		}
	}
	if !hasMessages {
		for _, k := range []string{"prompt", "image", "image_url"} {
			if _, ok := body[k]; ok {
				return Image
			}
		}
	}
	return ""
}

// asMessages 把 messages 字段归一化成 []map[string]any。
//
// 必须同时接受 []any（JSON 解码产物）与 []map[string]any（服务端内部构造，
// 例如控制台干跑用的 payload）。只做 `.([]any)` 断言时，后者会**静默失败**：
// 提示词提取为空 → 一律回落 text；agent 守卫看不见 tool_calls → 可能把 agent
// 请求改道去生图。两类后果都不会报错，只是「配了不生效」。
func asMessages(v any) []map[string]any {
	switch list := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return list
	}
	return nil
}

// Prompt 是从请求体里提取出的「用户这句话」与随附输入图。
type Prompt struct {
	Text   string
	Images []string
}

// ExtractPrompt 提取提示词与输入图（后者的用户是图生图 / 图生视频）。
func ExtractPrompt(body map[string]any) Prompt {
	var texts, images []string
	if msgs := asMessages(body["messages"]); len(msgs) > 0 {
		for i := len(msgs) - 1; i >= 0; i-- {
			msg := msgs[i]
			if asString(msg["role"]) != "user" {
				continue
			}
			switch content := msg["content"].(type) {
			case string:
				texts = append(texts, content)
			case []any:
				for _, rawPart := range content {
					part, ok := rawPart.(map[string]any)
					if !ok {
						continue
					}
					switch asString(part["type"]) {
					case "text", "input_text":
						if t := asString(part["text"]); t != "" {
							texts = append(texts, t)
						}
					case "image_url", "input_image":
						switch v := part["image_url"].(type) {
						case string:
							images = append(images, strings.TrimSpace(v))
						case map[string]any:
							if u := strings.TrimSpace(asString(v["url"])); u != "" {
								images = append(images, u)
							}
						}
					}
				}
			}
			break
		}
	}
	if len(texts) == 0 {
		for _, k := range []string{"prompt", "input", "text", "query", "q"} {
			if t := strings.TrimSpace(asString(body[k])); t != "" {
				texts = append(texts, t)
				break
			}
		}
	}
	for _, k := range []string{"image", "image_url"} {
		switch v := body[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				images = append(images, s)
			}
		case []any:
			for _, item := range v {
				if s := strings.TrimSpace(asString(item)); s != "" {
					images = append(images, s)
				}
			}
		}
	}
	return Prompt{Text: strings.TrimSpace(strings.Join(texts, "\n")), Images: images}
}

// CleanUserMessage 从一条 user 消息原文中剥离 WorkBuddy 等客户端注入的系统开销，
// 只保留用户真正输入的部分，用于「查看请求」日志展示。
//
// 行为：
//  1. 若内含 <user_query>...</user_query> 包裹（WorkBuddy 的常规形态），取其内部文本——
//     这是最精确的用户输入，已天然排除了包裹在外层的 <system-reminder> 等注入。
//  2. 否则去掉所有 <system-reminder ...>...</system-reminder> 块后返回剩余文本。
//  3. 普通 chat 请求（user 消息本身即纯文本、无上述包裹）原样透传，清理为零副作用。
func CleanUserMessage(raw string) string {
	if raw == "" {
		return ""
	}
	if m := userQueryRe.FindStringSubmatch(raw); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(systemReminderRe.ReplaceAllString(raw, ""))
}

var (
	userQueryRe      = regexp.MustCompile(`(?s)<user_query>(.*?)</user_query>`)
	systemReminderRe = regexp.MustCompile(`(?s)<system-reminder[^>]*>.*?</system-reminder>`)
)

// videoNounRe / imageNounRe / generateVerbRe 用于组合信号判定。
//
// 单靠「出现『视频』二字」会误伤「视频压缩工具推荐」「视频号怎么开」这类
// 与生成无关的提问，因此要求同时出现生成动词才认定为视频意图。
//
// imageNounRe 另有一个用途：判定「最终产物」到底是谁。
var (
	videoNounRe    = regexp.MustCompile(`(?:视频|短片|动画|影片|微电影)`)
	imageNounRe    = regexp.MustCompile(`(?:` + cnImgNoun + `)`)
	generateVerbRe = regexp.MustCompile(`(?:画|绘制|描画|生成|做|出|来|制作|设计|渲染|搞|弄|拍)`)
)

// lastNounIndex 返回名词最后一次出现的位置（按 rune 计），未出现返回 -1。
func lastNounIndex(text string, re *regexp.Regexp) int {
	locs := re.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return -1
	}
	last := locs[len(locs)-1][1]
	return len([]rune(text[:last]))
}

// imageIsProduct 判断用户真正要拿到的东西是不是图片。
//
// 依据：中文里「产物」几乎总在句末。谁的模态名词出现在更靠后的位置，
// 谁就是产物；另一个词只是修饰或输入来源。这条规则解决的是最容易误判的
// 两种混合句式：
//
//	「生成一段视频里的关键帧图片」→ 视频是来源，产物是图片 → image
//	「把这张照片做成视频」        → 照片是输入，产物是视频 → video
func imageIsProduct(text string) bool {
	lastImage := lastNounIndex(text, imageNounRe)
	lastVideo := lastNounIndex(text, videoNounRe)
	if lastImage < 0 {
		return false
	}
	return lastVideo < 0 || lastImage > lastVideo
}

// ContentScore 对用户这句话打出「这是生图/生视频意图」的置信度。
func ContentScore(text string, hasInputImages bool, rules Rules) (string, float64, string) {
	if strings.TrimSpace(text) == "" {
		return "", 0, "没有可判定的文本"
	}
	imageHits := matchAll(text, rules.ImagePatterns)
	videoHits := matchAll(text, rules.VideoPatterns)
	negativeHits := matchAll(text, rules.NegativePatterns)

	// 「产物归属」信号：产物名词落在句末时，它比另一个模态的命中更有决定性。
	// 例「生成一段视频里的关键帧图片」——含「视频名词+生成动词」组合信号，
	// 但用户要的是图片，因此该组合信号在此被抑制，不参与比分。
	productIsImage := imageIsProduct(text)

	// 视频名词 + 生成动词 → 视频意图（组合信号，避免单词误伤）
	if !productIsImage && videoNounRe.MatchString(text) && generateVerbRe.MatchString(text) {
		videoHits = append(videoHits, videoNounRe.FindString(text)+"+生成动词")
	}
	if len(imageHits) == 0 && len(videoHits) == 0 {
		return "", 0, "未命中生成类措辞"
	}

	modality := Image
	score := 0.55 + 0.15*float64(minInt(len(imageHits), 2))
	if productIsImage && len(videoHits) > 0 {
		score += 0.10 // 名词在句末，产物明确，压过「视频只是来源」的干扰
	}
	if len(videoHits) > 0 && !productIsImage {
		videoScore := 0.62 + 0.15*float64(minInt(len(videoHits), 2))
		if videoScore >= score {
			modality, score = Video, videoScore
		}
	}

	var notes []string
	if modality == Image && productIsImage && len(videoHits) > 0 {
		notes = append(notes, "含视频词，但产物名词在句末，判定为图片")
	}
	if modality == Image && hasInputImages {
		score += 0.10
		notes = append(notes, "带输入图（图生图）")
	}
	if len(negativeHits) > 0 {
		score -= 0.35
		notes = append(notes, fmt.Sprintf("含元讨论/疑问措辞「%s」", truncate(negativeHits[0], 40)))
	}
	if len([]rune(text)) > 1500 {
		score -= 0.20
		notes = append(notes, "文本过长，更像文档而非生成指令")
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	hits := imageHits
	if modality == Video {
		hits = videoHits
	}
	reason := fmt.Sprintf("命中「%s」", truncate(hits[0], 40))
	if len(notes) > 0 {
		reason += "，" + strings.Join(notes, "；")
	}
	return modality, score, reason
}

// AgentRequest 识别 agent / 结构化调用 —— 这类请求绝不能改道去生图。
func AgentRequest(body map[string]any) string {
	for _, k := range agentGuards {
		if v, ok := body[k]; ok && v != nil {
			if arr, isArr := v.([]any); isArr && len(arr) == 0 {
				continue
			}
			return fmt.Sprintf("含 %s 字段（agent/结构化调用）", k)
		}
	}
	for _, msg := range asMessages(body["messages"]) {
		if _, has := msg["tool_calls"]; has {
			return "多轮含 tool_calls（agent 循环）"
		}
		if asString(msg["role"]) == "tool" {
			return "多轮含 tool 消息（agent 循环）"
		}
	}
	return ""
}

// Result 是一次意图判定的完整结论。
type Result struct {
	Modality       string
	Source         string
	Score          float64
	Reason         string
	Prompt         Prompt
	ModelRequested string
	ModelUsed      string
	DroppedFields  []string
}

// SourceLabel 判定来源的中文说明。
func (r Result) SourceLabel() string { return SourceLabels[r.Source] }

// Headers 生成响应头（只放 latin-1 安全的值，reason 可能是中文绝不进头）。
func (r Result) Headers() map[string]string {
	out := map[string]string{
		"X-Agnes-Hub-Intent":       r.Modality,
		"X-Agnes-Hub-Intent-By":    r.Source,
		"X-Agnes-Hub-Intent-Score": fmt.Sprintf("%.2f", r.Score),
	}
	if r.ModelUsed != "" {
		out["X-Agnes-Hub-Model"] = r.ModelUsed
	}
	if r.ModelRequested != "" {
		out["X-Agnes-Hub-Model-Requested"] = r.ModelRequested
	}
	return out
}

// Meta 给下游程序化消费的元信息（可含中文）。
func (r Result) Meta() map[string]any {
	return map[string]any{
		"intent":          r.Modality,
		"intent_by":       r.Source,
		"intent_label":    r.SourceLabel(),
		"intent_score":    round3(r.Score),
		"intent_reason":   r.Reason,
		"model_requested": r.ModelRequested,
		"model_used":      r.ModelUsed,
		"input_images":    len(r.Prompt.Images),
	}
}

// Decide 主判定。任何分支都不应 panic —— 判定失败回落 text。
//
// forced 非空时直接采用（管理端「强制模态」用）。
func Decide(path string, body map[string]any, requestedModel string, cfg Config, rules Rules,
	aliases map[string]string, modalityOfModel func(string, map[string]string) string,
	forced string) Result {

	if rules.MinConfidence <= 0 {
		rules.MinConfidence = cfg.MinConfidence
	}
	if rules.MinConfidence <= 0 {
		rules.MinConfidence = 0.6
	}

	res := Result{ModelRequested: requestedModel, Modality: Text, Source: "default", Score: 0}
	locked := false

	switch {
	case containsString(Modalities, forced):
		res.Modality, res.Source, res.Score, res.Reason = forced, "forced", 1, "管理端指定模态"
		locked = true
	case ModalityFromPath(path) != "":
		res.Modality = ModalityFromPath(path)
		res.Source, res.Score, res.Reason = "endpoint", 1, fmt.Sprintf("端点 %s 已明确模态", path)
		locked = true
	case !IsAutoModel(requestedModel):
		if m := modalityOfModel(requestedModel, aliases); m != "" {
			res.Modality, res.Source, res.Score = m, "model", 1
			res.Reason = fmt.Sprintf("模型名 %s 已明确模态", requestedModel)
			locked = true
		}
	}

	res.Prompt = ExtractPrompt(body)
	if locked {
		return res
	}

	if m := ModalityFromParams(body); m != "" {
		res.Modality, res.Source = m, "params"
		res.Score = 0.85
		if m == Video {
			res.Score = 0.90
		}
		res.Reason = fmt.Sprintf("请求参数含 %s 专属字段", m)
		return res
	}

	if !cfg.ContentScan {
		res.Source, res.Reason = "default", "内容判定已在控制台关闭"
		return res
	}
	if guard := AgentRequest(body); guard != "" {
		res.Source, res.Reason = "default", "内容判定已禁用："+guard
		return res
	}
	modality, score, reason := ContentScore(res.Prompt.Text, len(res.Prompt.Images) > 0, rules)
	res.Score = score
	if modality != "" && score >= rules.MinConfidence {
		res.Modality, res.Source, res.Reason = modality, "content", reason
		return res
	}
	res.Modality = Text
	res.Source = "default"
	if reason != "" {
		res.Reason = reason
	} else {
		res.Reason = "无生成类意图，按文本对话处理"
	}
	return res
}

// PreferredModels 取某模态的模型偏好顺序。
func PreferredModels(cfg Config, modality string) []string {
	if cfg.PreferredModels == nil {
		return nil
	}
	var out []string
	for _, v := range cfg.PreferredModels[modality] {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ChooseModel 在「账号真实声明的模型」里挑一个。
// 偏好顺序只用于排序，**绝不越界使用账号未声明的模型**。
func ChooseModel(declared, preference []string) string {
	var clean []string
	for _, d := range declared {
		if s := strings.TrimSpace(d); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	for _, want := range preference {
		for _, have := range clean {
			if want == have {
				return want
			}
		}
	}
	return clean[0]
}

// ---------------------------------------------------------------------------
// 请求体改写
// ---------------------------------------------------------------------------

var tierOnly = regexp.MustCompile(`(?i)^\s*([1-4])\s*k\s*$`)

func normalizeSize(v any, fallback string) string {
	if s := strings.TrimSpace(asString(v)); s != "" {
		if m := tierOnly.FindStringSubmatch(s); m != nil {
			return m[1] + "K" // 上游只接受 1K/2K/3K/4K 大写
		}
		return s
	}
	if fallback == "" {
		return "1K"
	}
	return fallback
}

// BuildImageBody 把 chat 形态的意图翻成上游 /v1/images/generations 请求体。
func BuildImageBody(res Result, model string, cfg Config, source map[string]any) map[string]any {
	out := map[string]any{"model": model, "prompt": res.Prompt.Text}
	for _, k := range ImageFieldWhitelist {
		if v, ok := source[k]; ok && v != nil && asString(v) != "" {
			out[k] = v
		}
	}
	out["size"] = normalizeSize(out["size"], cfg.DefaultImageSize)
	if len(res.Prompt.Images) > 0 && cfg.ImageInputField != "" {
		if _, exists := out[cfg.ImageInputField]; !exists {
			if len(res.Prompt.Images) == 1 {
				out[cfg.ImageInputField] = res.Prompt.Images[0]
			} else {
				out[cfg.ImageInputField] = res.Prompt.Images
			}
		}
	}
	return out
}

// BuildVideoBody 把意图翻成上游 /v1/videos 请求体。
//
// 刻意不注入 height/width/num_frames/frame_rate 默认值：这些字段直接决定视频
// 时长与算力消耗，凭空注入等于替用户花钱。
func BuildVideoBody(res Result, model string, cfg Config, source map[string]any) map[string]any {
	out := map[string]any{"model": model, "prompt": res.Prompt.Text}
	for _, k := range VideoFieldWhitelist {
		if v, ok := source[k]; ok && v != nil && asString(v) != "" {
			out[k] = v
		}
	}
	if len(res.Prompt.Images) > 0 && cfg.VideoInputField != "" {
		if _, exists := out[cfg.VideoInputField]; !exists {
			if len(res.Prompt.Images) == 1 {
				out[cfg.VideoInputField] = res.Prompt.Images[0]
			} else {
				out[cfg.VideoInputField] = res.Prompt.Images
			}
		}
	}
	return out
}

// DroppedFields 找出被丢弃的字段（透明化：告诉用户哪些字段没被上游端点接受）。
func DroppedFields(source map[string]any, whitelist []string) []string {
	allowed := map[string]bool{"model": true, "messages": true, "prompt": true, "stream": true}
	for _, k := range whitelist {
		allowed[k] = true
	}
	var out []string
	for k := range source {
		if allowed[k] || ignorableKeys[k] {
			continue
		}
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// ---------------------------------------------------------------------------
// 响应翻译：把生图 / 生视频结果包成 chat 形态，让纯 chat 客户端零改动可用
// ---------------------------------------------------------------------------

// ChatEnvelope 构造非流式 chat 响应体。
func ChatEnvelope(res Result, model string, content string, extra map[string]any) map[string]any {
	meta := res.Meta()
	meta["model_used"] = model
	out := map[string]any{
		"id":      "chatcmpl-" + newToken(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   orDefaultStr(res.ModelRequested, "agnes-auto"),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage":     map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		"agnes_hub": meta,
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ImageContent 把上游生图结果拼成 Markdown（任何支持 Markdown 的客户端都能直接显示）。
func ImageContent(items []map[string]any, fallbackPrompt string) (string, []map[string]any) {
	var blocks []string
	images := make([]map[string]any, 0, len(items))
	for i, item := range items {
		url := strings.TrimSpace(asString(item["url"]))
		if url == "" {
			if b64 := strings.TrimSpace(asString(item["b64_json"])); b64 != "" {
				url = "data:image/png;base64," + b64
			}
		}
		caption := strings.TrimSpace(asString(item["revised_prompt"]))
		if caption == "" {
			caption = fallbackPrompt
		}
		caption = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(caption, "\n", " "), "[", "("), "]", ")")
		if url != "" {
			blocks = append(blocks, fmt.Sprintf("![%s](%s)", truncate(caption, 120), url))
		}
		images = append(images, map[string]any{
			"index": i, "url": url, "revised_prompt": asString(item["revised_prompt"]),
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, "（上游未返回图片数据）")
	}
	return strings.Join(blocks, "\n\n"), images
}

// VideoContent 构造视频任务的说明文本。
func VideoContent(model string, job map[string]any, videoURL string) string {
	lines := []string{fmt.Sprintf("视频任务已提交（模型 %s）。", model)}
	if v := asString(job["job_id"]); v != "" {
		lines = append(lines, fmt.Sprintf("- job_id: `%s`", v))
	}
	if v := asString(job["video_id"]); v != "" {
		lines = append(lines, fmt.Sprintf("- video_id: `%s`", v))
	}
	if v := asString(job["poll_url"]); v != "" {
		lines = append(lines, fmt.Sprintf("- 轮询：`GET %s`", v))
	}
	if videoURL != "" {
		lines = append(lines, "\n生成完成："+videoURL)
	} else {
		lines = append(lines, "\n视频为异步生成，请按上面的轮询地址查询结果。")
	}
	return strings.Join(lines, "\n")
}

// SSEFromChat 把非流式 chat 结果合成 SSE 分片：客户端要 stream=true 时也能拿到完整内容。
func SSEFromChat(envelope map[string]any) [][]byte {
	content := ""
	if choices, ok := envelope["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			if msg, ok := c0["message"].(map[string]any); ok {
				content = asString(msg["content"])
			}
		}
	}
	base := map[string]any{
		"id": envelope["id"], "object": "chat.completion.chunk",
		"created": envelope["created"], "model": envelope["model"],
	}
	frame := func(payload map[string]any) []byte {
		buf, _ := json.Marshal(payload)
		return append(append([]byte("data: "), buf...), '\n', '\n')
	}
	first := copyMap(base)
	first["choices"] = []any{map[string]any{"index": 0,
		"delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}
	out := [][]byte{frame(first)}
	if content != "" {
		chunk := copyMap(base)
		chunk["choices"] = []any{map[string]any{"index": 0,
			"delta": map[string]any{"content": content}, "finish_reason": nil}}
		out = append(out, frame(chunk))
	}
	last := copyMap(base)
	last["choices"] = []any{map[string]any{"index": 0,
		"delta": map[string]any{}, "finish_reason": "stop"}}
	if v, ok := envelope["agnes_hub"]; ok {
		last["baipiao_hub"] = v
	}
	if v, ok := envelope["data"]; ok {
		last["data"] = v
	}
	out = append(out, frame(last), []byte("data: [DONE]\n\n"))
	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func matchAll(text string, patterns []*regexp.Regexp) []string {
	var hits []string
	for _, re := range patterns {
		if loc := re.FindString(text); loc != "" {
			hits = append(hits, strings.TrimSpace(loc))
		}
	}
	return hits
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case json.Number:
		return s.String()
	case float64:
		return fmt.Sprintf("%v", s)
	case bool:
		return fmt.Sprintf("%v", s)
	case nil:
		return ""
	}
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func orDefaultStr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func round3(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }

func sortStrings(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func newToken() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buf)
}
