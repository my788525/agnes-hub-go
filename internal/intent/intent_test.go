package intent

import (
	"testing"

	"agneshub/internal/pool"
)

func cfg() Config {
	return Config{
		ContentScan:      true,
		MinConfidence:    0.6,
		DefaultImageSize: "1K",
		ImageInputField:  "image",
		VideoInputField:  "image",
		PreferredModels:  map[string][]string{"text": {"agnes-2.5-flash"}},
	}
}

func msgs(text string) map[string]any {
	return map[string]any{
		"model":    "agnes-auto",
		"messages": []any{map[string]any{"role": "user", "content": text}},
	}
}

func decide(text string) Result {
	return Decide("/v1/chat/completions", msgs(text), "agnes-auto",
		cfg(), DefaultRules(), nil, pool.ModalityOfModel, "")
}

// TestContentDecisionMatrix 是意图判定的回归矩阵。
//
// 这些用例的价值不在于「通过」，而在于把判定口径固定下来：
// 任何一次规则调整都必须在同一批正例与反例上同时成立。
func TestContentDecisionMatrix(t *testing.T) {
	cases := []struct {
		text     string
		expected string
		why      string
	}{
		// ---- 生图正例 ----
		{"帮我画一张赛博朋克风格的城市夜景", Image, "触发词 + 画 + 量词，名词可以缺席"},
		{"画一只戴墨镜的柴犬", Image, "画 + 量词「只」"},
		{"生成一张产品海报，极简风格", Image, "生成 + 量词 + 海报"},
		{"给我来张封面图", Image, "来张 + 图"},
		{"please draw me a cat sitting on a fence", Image, "英文祈使句"},
		{"生成一段视频里的关键帧图片", Image, "同时含视频词，但图片意图更明确"},

		// ---- 生视频正例 ----
		{"生成一段海边日落的海浪视频", Video, "动词 + 视频名词"},
		{"帮我把这张照片做成视频", Video, "做成视频"},
		{"animate this image into a short clip", Video, "英文 animate + clip"},

		// ---- 应回落 text 的反例（误判代价最高的一类）----
		{"解释一下 Transformer 的注意力机制", Text, "纯提问"},
		{"how do I draw a cat in matplotlib?", Text, "教学类疑问句，含 draw + cat"},
		{"What is the difference between RPM and QPS?", Text, "元讨论"},
		{"这段代码报错怎么修？", Text, "排障类"},
		{"视频压缩工具有哪些推荐", Text, "含「视频」但没有生成动词"},
		{"请介绍一下 Agnes AI 的图片生成能力", Text, "元讨论：在问能力而非要图"},
		{"", Text, "空文本"},
	}

	for _, c := range cases {
		got := decide(c.text)
		if got.Modality != c.expected {
			t.Errorf("「%s」应判为 %s，实际 %s（依据 %s，置信度 %.2f，说明 %s）",
				c.text, c.expected, got.Modality, got.Source, got.Score, got.Reason)
			continue
		}
		if c.expected == Text && c.text != "" && got.Modality == Image {
			t.Logf("注意：%s", c.why)
		}
	}
}

func TestEndpointBeatsContent(t *testing.T) {
	// 显式端点必须压过内容判定：往生图端点发「解释一下注意力机制」也要生图。
	body := map[string]any{"model": "agnes-auto", "prompt": "解释一下注意力机制"}
	res := Decide("/v1/images/generations", body, "agnes-auto", cfg(), DefaultRules(),
		nil, pool.ModalityOfModel, "")
	if res.Modality != Image {
		t.Fatalf("端点已明确生图，实际 %s", res.Modality)
	}
	if res.Source != "endpoint" {
		t.Errorf("判定依据应为 endpoint，实际 %s", res.Source)
	}
}

func TestExplicitModelBeatsContent(t *testing.T) {
	// 用户指定了具体模型就不该被内容判定覆盖。
	res := Decide("/v1/chat/completions", msgs("画一张图"), "agnes-2.5-flash",
		cfg(), DefaultRules(), nil, pool.ModalityOfModel, "")
	if res.Modality != Text {
		t.Fatalf("显式文本模型应判为 text，实际 %s", res.Modality)
	}
	if res.Source != "model" {
		t.Errorf("判定依据应为 model，实际 %s", res.Source)
	}
}

func TestParamsSignal(t *testing.T) {
	body := msgs("随便说点什么")
	body["num_frames"] = 121
	res := Decide("/v1/chat/completions", body, "agnes-auto", cfg(), DefaultRules(),
		nil, pool.ModalityOfModel, "")
	if res.Modality != Video {
		t.Fatalf("带 num_frames 应判为 video，实际 %s", res.Modality)
	}
	if res.Source != "params" {
		t.Errorf("判定依据应为 params，实际 %s", res.Source)
	}
}

func TestForcedModalityWins(t *testing.T) {
	res := Decide("/v1/chat/completions", msgs("你好"), "agnes-auto", cfg(), DefaultRules(),
		nil, pool.ModalityOfModel, Video)
	if res.Modality != Video || res.Source != "forced" {
		t.Fatalf("强制模态应生效，实际 %s / %s", res.Modality, res.Source)
	}
}

func TestContentScanDisabled(t *testing.T) {
	c := cfg()
	c.ContentScan = false
	res := Decide("/v1/chat/completions", msgs("画一张图"), "agnes-auto", c, DefaultRules(),
		nil, pool.ModalityOfModel, "")
	if res.Modality != Text {
		t.Fatalf("关闭内容判定后应回落 text，实际 %s", res.Modality)
	}
}

func TestAgentGuards(t *testing.T) {
	guards := []map[string]any{
		{"tools": []any{map[string]any{"type": "function"}}},
		{"response_format": map[string]any{"type": "json_object"}},
		{"messages": []any{
			map[string]any{"role": "user", "content": "画一张图"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "1"}}},
			map[string]any{"role": "tool", "content": "{}"},
		}},
	}
	for i, extra := range guards {
		body := msgs("画一张图")
		for k, v := range extra {
			body[k] = v
		}
		res := Decide("/v1/chat/completions", body, "agnes-auto", cfg(), DefaultRules(),
			nil, pool.ModalityOfModel, "")
		if res.Modality != Text {
			t.Errorf("第 %d 个 agent 信号未生效：判成了 %s（%s）", i+1, res.Modality, res.Reason)
		}
	}
}

func TestPromptExtraction(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "第一句"},
			map[string]any{"role": "assistant", "content": "回答"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "把这张图改成水彩风格"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/y.png"}},
			}},
		},
	}
	p := ExtractPrompt(body)
	if p.Text != "把这张图改成水彩风格" {
		t.Errorf("应取最后一条 user 消息，实际 %q", p.Text)
	}
	if len(p.Images) != 1 || p.Images[0] != "https://x/y.png" {
		t.Errorf("应提取到输入图，实际 %v", p.Images)
	}
}

// TestExtractPromptAcceptsTypedMessageSlice 覆盖一类不会报错的静默失败。
//
// JSON 解码出来的 messages 是 []any，但服务端内部构造的 payload（控制台干跑）
// 是 []map[string]any —— `v.([]any)` 对后者断言失败且不报错，于是提示词为空、
// 一律回落 text。控制台的意图测试 Tab 就是这样长期对所有样例都显示
// 「默认回落 / 没有可判定的文本」的，而日志里看不到任何异常。
func TestExtractPromptAcceptsTypedMessageSlice(t *testing.T) {
	typed := map[string]any{
		"model":    "agnes-auto",
		"messages": []map[string]any{{"role": "user", "content": "帮我画一张产品海报"}},
	}
	if got := ExtractPrompt(typed).Text; got != "帮我画一张产品海报" {
		t.Fatalf("typed slice 必须能被解析，实际 %q", got)
	}
	if m := ModalityFromParams(typed); m != "" {
		t.Errorf("有 messages 时不得因为断言失败而误判出参数信号，实际 %q", m)
	}
	if guard := AgentRequest(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "画一张图"},
			{"role": "tool", "content": "{}"},
		},
	}); guard == "" {
		t.Error("typed slice 里的 tool 消息必须被 agent 守卫识别，否则 agent 循环会被改道去生图")
	}

	// 端到端：同样的 typed payload 必须判出 image，而不是回落 text
	res := Decide("/v1/chat/completions", typed, "agnes-auto",
		cfg(), DefaultRules(), nil, pool.ModalityOfModel, "")
	if res.Modality != Image || res.Source != "content" {
		t.Fatalf("typed payload 应判为 image/content，实际 %s/%s（%s）",
			res.Modality, res.Source, res.Reason)
	}
}

// TestCleanUserMessage 验证「查看请求」日志只保留用户真实输入，剥离客户端注入的系统开销。
func TestCleanUserMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "workbuddy 包裹：剥离 system-reminder 只取 user_query",
			in:   "<system-reminder>cra\u200bft_mode ...</system-reminder>\n<system-reminder>current_time ...</system-reminder>\n<user_query>系统盘压缩可以加上吗？收益大吗</user_query>",
			want: "系统盘压缩可以加上吗？收益大吗",
		},
		{
			name: "无 user_query：去掉 system-reminder 后取剩余",
			in:   "<system-reminder>memory reminder</system-reminder>\n帮我画一张产品海报",
			want: "帮我画一张产品海报",
		},
		{
			name: "普通 chat 请求：无包裹原样透传",
			in:   "把这张图改成水彩风格",
			want: "把这张图改成水彩风格",
		},
		{
			name: "空输入",
			in:   "",
			want: "",
		},
	}
	for _, c := range cases {
		if got := CleanUserMessage(c.in); got != c.want {
			t.Errorf("[%s] CleanUserMessage = %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestChooseModelNeverEscapesDeclaredList(t *testing.T) {
	declared := []string{"agnes-image-2.1-flash"}
	pref := []string{"agnes-image-2.5-flash", "agnes-image-2.1-flash"}
	if got := ChooseModel(declared, pref); got != "agnes-image-2.1-flash" {
		t.Errorf("偏好里没有的模型绝不能用，实际 %q", got)
	}
	if got := ChooseModel(nil, pref); got != "" {
		t.Errorf("账号未声明任何模型时应返回空串，实际 %q", got)
	}
	if got := ChooseModel([]string{"agnes-image-2.0-flash"}, pref); got != "agnes-image-2.0-flash" {
		t.Errorf("偏好全都未声明时应回落到声明列表的第一项，实际 %q", got)
	}
}

func TestBuildImageBody(t *testing.T) {
	body := map[string]any{
		"model": "agnes-auto", "messages": []any{map[string]any{"role": "user", "content": "画一只猫"}},
		"size": "2k", "n": 1, "quality": "hd", "unknown_field": "x",
	}
	res := Decide("/v1/chat/completions", body, "agnes-auto", cfg(), DefaultRules(),
		nil, pool.ModalityOfModel, "")
	out := BuildImageBody(res, "agnes-image-2.5-flash", cfg(), body)

	if out["prompt"] != "画一只猫" {
		t.Errorf("prompt 应来自用户那句话，实际 %v", out["prompt"])
	}
	// 上游只接受大写 1K/2K/3K/4K 或 WIDTHxHEIGHT
	if out["size"] != "2K" {
		t.Errorf("size 应被归一化为大写 2K，实际 %v", out["size"])
	}
	if out["quality"] != "hd" {
		t.Errorf("白名单内的字段应透传，实际 %v", out["quality"])
	}
	if _, exists := out["messages"]; exists {
		t.Error("messages 不应出现在生图请求体里")
	}
	dropped := DroppedFields(body, ImageFieldWhitelist)
	found := false
	for _, d := range dropped {
		if d == "unknown_field" {
			found = true
		}
	}
	if !found {
		t.Errorf("未知字段应被报告为丢弃，实际 %v", dropped)
	}
}

func TestBuildVideoBodyDoesNotInjectDefaults(t *testing.T) {
	// 视频的 height/width/num_frames 直接决定时长与算力消耗，
	// 凭空注入默认值等于替用户花钱，因此绝不允许注入。
	body := map[string]any{
		"model": "agnes-auto", "prompt": "海边日落",
	}
	res := Decide("/v1/videos", body, "agnes-auto", cfg(), DefaultRules(),
		nil, pool.ModalityOfModel, "")
	out := BuildVideoBody(res, "agnes-video-2.5-flash", cfg(), body)
	for _, k := range []string{"height", "width", "num_frames", "frame_rate"} {
		if _, exists := out[k]; exists {
			t.Errorf("不应注入 %s 默认值", k)
		}
	}
	if out["prompt"] != "海边日落" || out["model"] != "agnes-video-2.5-flash" {
		t.Errorf("基础字段不正确：%v", out)
	}
}

func TestImageContent(t *testing.T) {
	content, images := ImageContent([]map[string]any{
		{"url": "https://cdn/x.png", "revised_prompt": "a cat"},
		{"b64_json": "AAAA"},
	}, "画一只猫")
	if len(images) != 2 {
		t.Fatalf("应解析出 2 张图，实际 %d", len(images))
	}
	if images[1]["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("b64 应被转成 data URI，实际 %v", images[1]["url"])
	}
	if content == "" {
		t.Error("应生成 Markdown 内容")
	}
}

func TestSSEFromChat(t *testing.T) {
	envelope := map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "agnes-auto",
		"choices": []any{map[string]any{"index": 0,
			"message": map[string]any{"role": "assistant", "content": "hi"}}},
		"agnes_hub": map[string]any{"intent": "image"},
	}
	frames := SSEFromChat(envelope)
	if len(frames) < 4 {
		t.Fatalf("SSE 至少应有 4 帧，实际 %d", len(frames))
	}
	last := string(frames[len(frames)-1])
	if last != "data: [DONE]\n\n" {
		t.Errorf("最后一帧应为 [DONE]，实际 %q", last)
	}
}
