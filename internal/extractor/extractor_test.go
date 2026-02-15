package extractor

import (
	"testing"
)

// --- Anthropic extraction tests ---

func TestExtractAnthropic_SingleToolUse(t *testing.T) {
	body := []byte(`{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"content": [
			{"type": "tool_use", "id": "toolu_01", "name": "exec", "input": {"command": "ls -la"}}
		],
		"stop_reason": "tool_use"
	}`)

	calls := Extract(body, APITypeAnthropic)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	tc := calls[0]
	if tc.ID != "toolu_01" {
		t.Errorf("ID: expected toolu_01, got %q", tc.ID)
	}
	if tc.Name != "exec" {
		t.Errorf("Name: expected exec, got %q", tc.Name)
	}
	if tc.Index != 0 {
		t.Errorf("Index: expected 0, got %d", tc.Index)
	}
	cmd, _ := tc.Arguments["command"].(string)
	if cmd != "ls -la" {
		t.Errorf("Arguments.command: expected 'ls -la', got %q", cmd)
	}
	if len(tc.RawJSON) == 0 {
		t.Error("RawJSON should be populated")
	}
}

func TestExtractAnthropic_ThinkingTextToolUse(t *testing.T) {
	body := []byte(`{
		"content": [
			{"type": "thinking", "thinking": "reasoning...", "signature": "sig"},
			{"type": "text", "text": "Let me check."},
			{"type": "tool_use", "id": "toolu_02", "name": "read", "input": {"path": "/tmp/file"}}
		]
	}`)

	calls := Extract(body, APITypeAnthropic)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Index != 2 {
		t.Errorf("expected index 2 (after thinking+text), got %d", calls[0].Index)
	}
	if calls[0].Name != "read" {
		t.Errorf("expected name read, got %q", calls[0].Name)
	}
}

func TestExtractAnthropic_MultipleToolUse(t *testing.T) {
	body := []byte(`{
		"content": [
			{"type": "text", "text": "I'll do both."},
			{"type": "tool_use", "id": "toolu_a", "name": "exec", "input": {"command": "ls"}},
			{"type": "tool_use", "id": "toolu_b", "name": "read", "input": {"path": "/tmp"}}
		]
	}`)

	calls := Extract(body, APITypeAnthropic)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "exec" || calls[0].Index != 1 {
		t.Errorf("call[0]: expected exec@1, got %s@%d", calls[0].Name, calls[0].Index)
	}
	if calls[1].Name != "read" || calls[1].Index != 2 {
		t.Errorf("call[1]: expected read@2, got %s@%d", calls[1].Name, calls[1].Index)
	}
}

func TestExtractAnthropic_NoToolUse(t *testing.T) {
	body := []byte(`{
		"content": [
			{"type": "text", "text": "Just text, no tools."}
		],
		"stop_reason": "end_turn"
	}`)

	calls := Extract(body, APITypeAnthropic)
	if len(calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(calls))
	}
}

func TestExtractAnthropic_EmptyContent(t *testing.T) {
	body := []byte(`{"content": []}`)
	calls := Extract(body, APITypeAnthropic)
	if len(calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(calls))
	}
}

func TestExtractAnthropic_MalformedJSON(t *testing.T) {
	calls := Extract([]byte(`not json`), APITypeAnthropic)
	if calls != nil {
		t.Errorf("expected nil for malformed JSON, got %v", calls)
	}
}

func TestExtractAnthropic_NestedArguments(t *testing.T) {
	body := []byte(`{
		"content": [
			{"type": "tool_use", "id": "toolu_c", "name": "write", "input": {
				"path": "/app/config.json",
				"content": "{\"key\": \"value\", \"nested\": {\"a\": 1}}"
			}}
		]
	}`)

	calls := Extract(body, APITypeAnthropic)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Arguments["path"] != "/app/config.json" {
		t.Errorf("expected path, got %v", calls[0].Arguments["path"])
	}
}

// --- OpenAI extraction tests ---

func TestExtractOpenAI_SingleToolCall(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc",
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [{
					"id": "call_123",
					"type": "function",
					"function": {"name": "exec", "arguments": "{\"command\": \"ls -la\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	tc := calls[0]
	if tc.ID != "call_123" {
		t.Errorf("ID: expected call_123, got %q", tc.ID)
	}
	if tc.Name != "exec" {
		t.Errorf("Name: expected exec, got %q", tc.Name)
	}
	if tc.Index != 0 {
		t.Errorf("Index: expected 0, got %d", tc.Index)
	}
	cmd, _ := tc.Arguments["command"].(string)
	if cmd != "ls -la" {
		t.Errorf("Arguments.command: expected 'ls -la', got %q", cmd)
	}
}

func TestExtractOpenAI_MultipleToolCalls(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"message": {
				"tool_calls": [
					{"id": "call_a", "type": "function", "function": {"name": "exec", "arguments": "{\"command\": \"ls\"}"}},
					{"id": "call_b", "type": "function", "function": {"name": "read", "arguments": "{\"path\": \"/tmp\"}"}}
				]
			}
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "exec" || calls[0].Index != 0 {
		t.Errorf("call[0]: expected exec@0, got %s@%d", calls[0].Name, calls[0].Index)
	}
	if calls[1].Name != "read" || calls[1].Index != 1 {
		t.Errorf("call[1]: expected read@1, got %s@%d", calls[1].Name, calls[1].Index)
	}
}

func TestExtractOpenAI_NoToolCalls(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"message": {"role": "assistant", "content": "Hello!"},
			"finish_reason": "stop"
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(calls))
	}
}

func TestExtractOpenAI_EmptyChoices(t *testing.T) {
	body := []byte(`{"choices": []}`)
	calls := Extract(body, APITypeOpenAI)
	if calls != nil {
		t.Errorf("expected nil for empty choices, got %v", calls)
	}
}

func TestExtractOpenAI_MalformedJSON(t *testing.T) {
	calls := Extract([]byte(`broken`), APITypeOpenAI)
	if calls != nil {
		t.Errorf("expected nil, got %v", calls)
	}
}

// --- Dispatch tests ---

func TestExtract_Dispatch(t *testing.T) {
	anthropicBody := []byte(`{"content":[{"type":"tool_use","id":"t1","name":"exec","input":{"command":"ls"}}]}`)
	openaiBody := []byte(`{"choices":[{"message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`)

	a := Extract(anthropicBody, APITypeAnthropic)
	if len(a) != 1 || a[0].Name != "exec" {
		t.Errorf("Anthropic dispatch: got %v", a)
	}

	o := Extract(openaiBody, APITypeOpenAI)
	if len(o) != 1 || o[0].Name != "read" {
		t.Errorf("OpenAI dispatch: got %v", o)
	}

	u := Extract(anthropicBody, APITypeUnknown)
	if u != nil {
		t.Errorf("Unknown dispatch: expected nil, got %v", u)
	}
}

// --- ExtractRequestMeta tests ---

func TestExtractRequestMeta_Anthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-5-20250918",
		"stream": true,
		"tools": [{"name": "exec"}, {"name": "read"}, {"name": "write"}]
	}`)

	meta := ExtractRequestMeta(body, APITypeAnthropic)
	if meta.Model != "claude-opus-4-5-20250918" {
		t.Errorf("Model: expected claude-opus-4-5-20250918, got %q", meta.Model)
	}
	if !meta.Stream {
		t.Error("Stream: expected true")
	}
	if len(meta.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(meta.Tools))
	}
	expected := []string{"exec", "read", "write"}
	for i, name := range expected {
		if meta.Tools[i] != name {
			t.Errorf("tool[%d]: expected %q, got %q", i, name, meta.Tools[i])
		}
	}
}

func TestExtractRequestMeta_OpenAI(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"stream": false,
		"tools": [
			{"type": "function", "function": {"name": "exec"}},
			{"type": "function", "function": {"name": "read"}}
		]
	}`)

	meta := ExtractRequestMeta(body, APITypeOpenAI)
	if meta.Model != "gpt-4o" {
		t.Errorf("Model: expected gpt-4o, got %q", meta.Model)
	}
	if meta.Stream {
		t.Error("Stream: expected false")
	}
	if len(meta.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(meta.Tools))
	}
	if meta.Tools[0] != "exec" || meta.Tools[1] != "read" {
		t.Errorf("tools: expected [exec, read], got %v", meta.Tools)
	}
}

func TestExtractRequestMeta_Empty(t *testing.T) {
	meta := ExtractRequestMeta([]byte(`{}`), APITypeAnthropic)
	if meta.Model != "" || meta.Stream || len(meta.Tools) != 0 {
		t.Errorf("empty body should give zero meta, got %+v", meta)
	}
}

func TestExtractRequestMeta_Malformed(t *testing.T) {
	meta := ExtractRequestMeta([]byte(`not json`), APITypeAnthropic)
	if meta.Model != "" {
		t.Errorf("malformed JSON should give zero meta, got %+v", meta)
	}
}
