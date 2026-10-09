package config

import (
	"testing"
)

// 构造一个典型的 WorkBuddy/agnes 客户端请求体：巨大 system + 一条 user + tools/thinking/stream 等能力字段。
func sampleBody() map[string]any {
	return map[string]any{
		"model": "agnes-3.0-flash",
		"stream": true,
		"stream_options": map[string]any{"include_usage": true},
		"thinking": map[string]any{"type": "enabled"},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}},
		},
		"tool_choice": "auto",
		"temperature": 0.7,
		"max_tokens": 4096,
		"messages": []any{
			map[string]any{"role": "system", "content": "THIS IS THE HUGE WORKBUDDY SYSTEM PROMPT THAT WASTES TOKENS ..."},
			map[string]any{"role": "user", "content": "帮我写一个快排"},
		},
	}
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

func systemText(m map[string]any) string {
	msgs, ok := m["messages"].([]any)
	if !ok {
		return ""
	}
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if ok && asString(msg["role"]) == "system" {
			return asString(msg["content"])
		}
	}
	return ""
}

func TestApplySystemPromptPolicy_Forward(t *testing.T) {
	b := sampleBody()
	ApplySystemPromptPolicy(b, Settings{SystemPromptPolicy: "forward"})
	if systemText(b) == "" {
		t.Fatal("forward 不应改动 system；但 system 消失了")
	}
	if !hasKey(b, "tools") || !hasKey(b, "thinking") || !hasKey(b, "stream_options") {
		t.Fatal("forward 不应改动能力字段")
	}
}

func TestApplySystemPromptPolicy_Strip(t *testing.T) {
	b := sampleBody()
	ApplySystemPromptPolicy(b, Settings{SystemPromptPolicy: "strip"})
	if systemText(b) != "" {
		t.Fatalf("strip 应移除 system，但剩下了: %q", systemText(b))
	}
	// 能力字段必须原样保留
	for _, k := range []string{"tools", "thinking", "stream_options", "tool_choice", "temperature", "max_tokens", "stream"} {
		if !hasKey(b, k) {
			t.Errorf("strip 误伤了能力字段 %s", k)
		}
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("strip 后只应留 1 条 user，实际 %d", len(msgs))
	}
	if asString(msgs[0].(map[string]any)["role"]) != "user" {
		t.Fatal("strip 后应保留 user 消息")
	}
}

func TestApplySystemPromptPolicy_Override(t *testing.T) {
	// 自定义 override
	b := sampleBody()
	ApplySystemPromptPolicy(b, Settings{SystemPromptPolicy: "override", SystemPromptOverride: "CODE GEN PROMPT"})
	if systemText(b) != "CODE GEN PROMPT" {
		t.Fatalf("override 应注入自定义提示词，实际: %q", systemText(b))
	}
	if !hasKey(b, "tools") || !hasKey(b, "thinking") {
		t.Fatal("override 误伤了能力字段")
	}
	// override 为空 → 回退内置默认
	b2 := sampleBody()
	ApplySystemPromptPolicy(b2, Settings{SystemPromptPolicy: "override"})
	if systemText(b2) != DefaultCodeSystemPrompt {
		t.Fatal("override 空时应回退内置默认代码生成提示词")
	}
}

func TestApplySystemPromptPolicy_Scenario(t *testing.T) {
	b := sampleBody()
	ApplySystemPromptPolicy(b, Settings{
		SystemPromptPolicy: "scenario",
		PrePrompts:         map[string]string{"text": "TEXT SCENARIO PROMPT"},
	})
	if systemText(b) != "TEXT SCENARIO PROMPT" {
		t.Fatalf("scenario 应命中 PrePrompts[text]，实际: %q", systemText(b))
	}
	// PrePrompts 缺 text → 回退 override → 再回退默认
	b2 := sampleBody()
	ApplySystemPromptPolicy(b2, Settings{SystemPromptPolicy: "scenario"})
	if systemText(b2) != DefaultCodeSystemPrompt {
		t.Fatal("scenario 缺 PrePrompts 时应回退内置默认")
	}
}

func TestApplySystemPromptPolicy_AnthropicTopLevelSystem(t *testing.T) {
	// Anthropic 风格：system 是顶层字段而非 messages 内
	b := map[string]any{
		"system": "ANTHROPIC SYSTEM PROMPT",
		"model":  "claude-x",
		"tools":  []any{map[string]any{"name": "t"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	ApplySystemPromptPolicy(b, Settings{SystemPromptPolicy: "strip"})
	if _, ok := b["system"]; ok {
		t.Fatal("strip 应移除顶层 system 字段")
	}
	if !hasKey(b, "tools") {
		t.Fatal("strip 误伤顶层 tools")
	}
	if systemText(b) != "" {
		t.Fatal("strip 后不应有 system 消息")
	}
}
