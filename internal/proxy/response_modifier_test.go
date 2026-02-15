package proxy

import (
	"encoding/json"
	"testing"

	"github.com/ctrlai/ctrlai/internal/engine"
	"github.com/ctrlai/ctrlai/internal/extractor"
)

// --- Anthropic response modification ---

func TestModifyAnthropicResponse_SingleBlock(t *testing.T) {
	body := []byte(`{
		"id":"msg_01","type":"message","role":"assistant",
		"content":[
			{"type":"text","text":"Let me check."},
			{"type":"tool_use","id":"toolu_01","name":"exec","input":{"command":"cat /etc/shadow"}}
		],
		"stop_reason":"tool_use"
	}`)

	blocked := []extractor.ToolCall{{ID: "toolu_01", Name: "exec", Index: 1}}
	decisions := []engine.Decision{{Action: "block", Rule: "block_system_files", Message: "Cannot access system files"}}

	modified := modifyNonStreamingResponse(body, extractor.APITypeAnthropic, blocked, decisions)

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(modified, &resp); err != nil {
		t.Fatalf("failed to parse modified response: %v", err)
	}

	// stop_reason should change to end_turn.
	var stopReason string
	json.Unmarshal(resp["stop_reason"], &stopReason)
	if stopReason != "end_turn" {
		t.Errorf("stop_reason: expected end_turn, got %q", stopReason)
	}

	// Content should have text + block notice (no tool_use).
	var content []map[string]json.RawMessage
	json.Unmarshal(resp["content"], &content)

	for _, block := range content {
		blockType := unquoteRaw(block["type"])
		if blockType == "tool_use" {
			t.Error("tool_use block should have been stripped")
		}
	}

	// Last block should be the block notice.
	lastBlock := content[len(content)-1]
	if unquoteRaw(lastBlock["type"]) != "text" {
		t.Error("last block should be text (block notice)")
	}
	var text string
	json.Unmarshal(lastBlock["text"], &text)
	if text == "" {
		t.Error("block notice text should not be empty")
	}
}

func TestModifyAnthropicResponse_PartialBlock(t *testing.T) {
	body := []byte(`{
		"content":[
			{"type":"tool_use","id":"toolu_a","name":"exec","input":{"command":"ls"}},
			{"type":"tool_use","id":"toolu_b","name":"exec","input":{"command":"rm -rf /"}}
		],
		"stop_reason":"tool_use"
	}`)

	// Only block the second tool call.
	blocked := []extractor.ToolCall{{ID: "toolu_b", Name: "exec", Index: 1}}
	decisions := []engine.Decision{{Action: "block", Rule: "block_destructive", Message: "Destructive"}}

	modified := modifyNonStreamingResponse(body, extractor.APITypeAnthropic, blocked, decisions)

	var resp map[string]json.RawMessage
	json.Unmarshal(modified, &resp)

	// stop_reason should remain tool_use (partial block).
	var stopReason string
	json.Unmarshal(resp["stop_reason"], &stopReason)
	if stopReason != "tool_use" {
		t.Errorf("partial block: stop_reason should remain tool_use, got %q", stopReason)
	}

	// Should still have one tool_use (the allowed one).
	var content []map[string]json.RawMessage
	json.Unmarshal(resp["content"], &content)
	toolUseCount := 0
	for _, block := range content {
		if unquoteRaw(block["type"]) == "tool_use" {
			toolUseCount++
			if unquoteRaw(block["id"]) != "toolu_a" {
				t.Error("wrong tool_use kept — should be toolu_a")
			}
		}
	}
	if toolUseCount != 1 {
		t.Errorf("expected 1 remaining tool_use, got %d", toolUseCount)
	}
}

func TestModifyAnthropicResponse_PreservesThinking(t *testing.T) {
	body := []byte(`{
		"content":[
			{"type":"thinking","thinking":"reasoning...","signature":"sig123"},
			{"type":"text","text":"Here's what I'll do."},
			{"type":"tool_use","id":"toolu_01","name":"exec","input":{"command":"bad"}}
		],
		"stop_reason":"tool_use"
	}`)

	blocked := []extractor.ToolCall{{ID: "toolu_01", Name: "exec", Index: 2}}
	decisions := []engine.Decision{{Action: "block", Rule: "r1", Message: "blocked"}}

	modified := modifyNonStreamingResponse(body, extractor.APITypeAnthropic, blocked, decisions)

	var resp map[string]json.RawMessage
	json.Unmarshal(modified, &resp)

	var content []map[string]json.RawMessage
	json.Unmarshal(resp["content"], &content)

	// Should have: thinking, text, block_notice_text.
	types := make([]string, len(content))
	for i, block := range content {
		types[i] = unquoteRaw(block["type"])
	}
	if types[0] != "thinking" {
		t.Errorf("expected thinking first, got %q", types[0])
	}
	if types[1] != "text" {
		t.Errorf("expected text second, got %q", types[1])
	}
}

// --- OpenAI response modification ---

func TestModifyOpenAIResponse_SingleBlock(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-abc",
		"choices":[{
			"message":{
				"role":"assistant",
				"content":"Checking...",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec","arguments":"{\"command\":\"bad\"}"}}]
			},
			"finish_reason":"tool_calls"
		}]
	}`)

	blocked := []extractor.ToolCall{{ID: "call_1", Name: "exec", Index: 0}}
	decisions := []engine.Decision{{Action: "block", Rule: "r1", Message: "blocked"}}

	modified := modifyNonStreamingResponse(body, extractor.APITypeOpenAI, blocked, decisions)

	var resp map[string]json.RawMessage
	json.Unmarshal(modified, &resp)

	var choices []map[string]json.RawMessage
	json.Unmarshal(resp["choices"], &choices)

	// finish_reason should change to stop.
	var fr string
	json.Unmarshal(choices[0]["finish_reason"], &fr)
	if fr != "stop" {
		t.Errorf("finish_reason: expected stop, got %q", fr)
	}

	// tool_calls should be empty array.
	var msg map[string]json.RawMessage
	json.Unmarshal(choices[0]["message"], &msg)
	var tcs []json.RawMessage
	json.Unmarshal(msg["tool_calls"], &tcs)
	if len(tcs) != 0 {
		t.Errorf("expected empty tool_calls, got %d", len(tcs))
	}

	// Content should include block notice.
	var content string
	json.Unmarshal(msg["content"], &content)
	if content == "" {
		t.Error("content should not be empty after blocking")
	}
}

func TestModifyOpenAIResponse_PartialBlock(t *testing.T) {
	body := []byte(`{
		"choices":[{
			"message":{
				"tool_calls":[
					{"id":"call_a","type":"function","function":{"name":"exec","arguments":"{}"}},
					{"id":"call_b","type":"function","function":{"name":"read","arguments":"{}"}}
				]
			},
			"finish_reason":"tool_calls"
		}]
	}`)

	blocked := []extractor.ToolCall{{ID: "call_b", Name: "read", Index: 1}}
	decisions := []engine.Decision{{Action: "block", Rule: "r1", Message: "blocked"}}

	modified := modifyNonStreamingResponse(body, extractor.APITypeOpenAI, blocked, decisions)

	var resp map[string]json.RawMessage
	json.Unmarshal(modified, &resp)

	var choices []map[string]json.RawMessage
	json.Unmarshal(resp["choices"], &choices)

	// finish_reason should remain tool_calls (partial).
	var fr string
	json.Unmarshal(choices[0]["finish_reason"], &fr)
	if fr != "tool_calls" {
		t.Errorf("partial block: finish_reason should remain tool_calls, got %q", fr)
	}
}

// --- buildKilledResponse ---

func TestBuildKilledResponse_Anthropic(t *testing.T) {
	body := buildKilledResponse(extractor.APITypeAnthropic)
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	var stopReason string
	json.Unmarshal(resp["stop_reason"], &stopReason)
	if stopReason != "end_turn" {
		t.Errorf("expected end_turn, got %q", stopReason)
	}
}

func TestBuildKilledResponse_OpenAI(t *testing.T) {
	body := buildKilledResponse(extractor.APITypeOpenAI)
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	var choices []map[string]json.RawMessage
	json.Unmarshal(resp["choices"], &choices)
	if len(choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	var fr string
	json.Unmarshal(choices[0]["finish_reason"], &fr)
	if fr != "stop" {
		t.Errorf("expected stop, got %q", fr)
	}
}

func TestBuildKilledResponse_Unknown(t *testing.T) {
	body := buildKilledResponse(extractor.APITypeUnknown)
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := resp["error"]; !ok {
		t.Error("expected error field for unknown API type")
	}
}

// --- formatBlockNotice ---

func TestFormatBlockNotice(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		rule     string
		msg      string
		contains string
	}{
		{"with message and rule", "exec", "my_rule", "Bad command", "[CtrlAI] Blocked: Bad command (rule: my_rule)"},
		{"empty message", "exec", "my_rule", "", "[CtrlAI] Blocked: Tool call 'exec' was blocked (rule: my_rule)"},
		{"empty rule", "exec", "", "Bad", "[CtrlAI] Blocked: Bad"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatBlockNotice(tt.tool, tt.rule, tt.msg)
			if result != tt.contains {
				t.Errorf("expected %q, got %q", tt.contains, result)
			}
		})
	}
}
