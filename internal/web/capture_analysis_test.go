package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCapAnalyzeRealShapes 用合成样本验证字段盘点、模态判定与报告渲染的关键不变量。
func TestCapAnalyzeRealShapes(t *testing.T) {
	dir := t.TempDir()

	// 直接写原始 JSON 字节（与网关 RawCapture 的真实落盘方式一致：未做 HTML 转义），
	// 这样才能验证 content_policy / user_query 标签的子串检测。
	sampleA := `{"model":"agnes-3.0-flash","stream":true,"messages":[` +
		`{"role":"system","content":"<content_policy>keep it short</content_policy>"},` +
		`{"role":"user","content":"<user_query>你好</user_query>"},` +
		`{"role":"assistant","content":"hi"},` +
		`{"role":"user","content":[{"type":"text","text":"看图"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,xxx"}}]}]}`
	sampleB := `{"model":"agnes-3.0-flash","stream":false,"messages":[` +
		`{"role":"user","content":"ping"},{"role":"assistant","content":"pong"}]}`
	for i, s := range []string{sampleA, sampleB} {
		if err := os.WriteFile(filepath.Join(dir, "req_"+string(rune('0'+i))+".json"), []byte(s), 0o644); err != nil {
			t.Fatalf("写样本失败：%v", err)
		}
	}

	rep, err := capAnalyze(dir)
	if err != nil {
		t.Fatalf("capAnalyze 失败：%v", err)
	}
	if rep.n != 2 {
		t.Errorf("样本数应为 2，实际 %d", rep.n)
	}
	if rep.modalities["chat(messages)"] != 2 {
		t.Errorf("chat(messages) 应为 2，实际 %d", rep.modalities["chat(messages)"])
	}
	if rep.hasContentPolicy != 1 {
		t.Errorf("含 content_policy 应为 1，实际 %d", rep.hasContentPolicy)
	}
	if rep.hasUserQuery != 1 {
		t.Errorf("含 user_query 应为 1，实际 %d", rep.hasUserQuery)
	}
	if rep.totalTurns != 6 {
		t.Errorf("消息轮次合计应为 6，实际 %d", rep.totalTurns)
	}
	// sampleA 含 1 个数组 content，sampleB 全字符串 content（2 条）；sampleA 另有 3 条字符串 content
	if rep.contentIsArray != 1 {
		t.Errorf("content 数组形态应为 1，实际 %d", rep.contentIsArray)
	}
	if rep.contentIsString != 5 {
		t.Errorf("content 字符串形态应为 5，实际 %d", rep.contentIsString)
	}
	// 关键字段应被盘点出来
	for _, want := range []string{"(root).model", "(root).messages", "(root).messages[].role", "(root).messages[].content"} {
		if _, ok := rep.acc[want]; !ok {
			t.Errorf("缺少字段路径 %s", want)
		}
	}
	// content 数组展开后，多模态块字段应被记录
	if _, ok := rep.acc["(root).messages[].content[].type"]; !ok {
		t.Errorf("缺少数组元素字段 (root).messages[].content[].type")
	}

	doc := capRenderHTML(rep)
	if !strings.Contains(doc, "RawCapture 全字段分析报告") {
		t.Errorf("报告缺少标题")
	}
	if !strings.Contains(doc, "模态与规模画像") {
		t.Errorf("报告缺少模态画像章节")
	}
	if !strings.Contains(doc, "全字段清单") {
		t.Errorf("报告缺少全字段清单章节")
	}
	// 疑似凭证脱敏：image_url 的 data: 前缀不含 sk-/bearer/token/secret，不应被脱敏；
	// 但若未来样本含 token 则必须脱敏——此处仅验证脱敏函数本身。
	if got := capMask("Bearer secret-token-123"); !strings.Contains(got, "脱敏") {
		t.Errorf("capMask 未对 Bearer 凭证脱敏：%s", got)
	}
	if got := capMask("普通文本"); got != "普通文本" {
		t.Errorf("capMask 误伤普通文本：%s", got)
	}
}
