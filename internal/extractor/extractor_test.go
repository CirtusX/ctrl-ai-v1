package extractor

import (
	"encoding/json"
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

// --- Moonshot/Kimi extraction tests (standard OpenAI-compatible) ---

func TestExtractOpenAI_Moonshot_ToolCall(t *testing.T) {
	// Moonshot uses identical format to OpenAI.
	body := []byte(`{
		"id": "chatcmpl-moonshot-abc",
		"object": "chat.completion",
		"model": "moonshot-v1-32k",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_moonshot_123",
					"type": "function",
					"function": {"name": "get_weather", "arguments": "{\"city\": \"Beijing\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("Name: expected get_weather, got %q", calls[0].Name)
	}
	city, _ := calls[0].Arguments["city"].(string)
	if city != "Beijing" {
		t.Errorf("Arguments.city: expected Beijing, got %q", city)
	}
}

// --- Qwen extraction tests (finish_reason quirk) ---

func TestExtractOpenAI_Qwen_ToolCallWithStopFinishReason(t *testing.T) {
	// Qwen may return finish_reason:"stop" even when tool_calls are present.
	// The extractor must still find tool_calls regardless of finish_reason.
	body := []byte(`{
		"id": "chatcmpl-qwen-abc",
		"object": "chat.completion",
		"model": "qwen-plus",
		"system_fingerprint": "",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "",
				"tool_calls": [{
					"id": "call_qwen_123",
					"type": "function",
					"function": {"name": "exec", "arguments": "{\"command\": \"ls -la\"}"},
					"index": 0
				}]
			},
			"finish_reason": "stop",
			"logprobs": null
		}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150}
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call despite finish_reason:stop, got %d", len(calls))
	}
	if calls[0].Name != "exec" {
		t.Errorf("Name: expected exec, got %q", calls[0].Name)
	}
	cmd, _ := calls[0].Arguments["command"].(string)
	if cmd != "ls -la" {
		t.Errorf("Arguments.command: expected 'ls -la', got %q", cmd)
	}
}

func TestExtractOpenAI_Qwen_WithReasoningContent(t *testing.T) {
	// Qwen thinking models include reasoning_content in the response.
	// The extractor should still find tool_calls and ignore reasoning_content.
	body := []byte(`{
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "Let me check.",
				"reasoning_content": "I need to read the file to understand...",
				"tool_calls": [{
					"id": "call_qwq_456",
					"type": "function",
					"function": {"name": "read", "arguments": "{\"path\": \"/tmp/test.txt\"}"}
				}]
			},
			"finish_reason": "stop"
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "read" {
		t.Errorf("Name: expected read, got %q", calls[0].Name)
	}
}

// --- MiniMax extraction tests ---

func TestExtractOpenAI_MiniMax_ToolCallWithReasoningDetails(t *testing.T) {
	// MiniMax includes reasoning_details (array of strings) in the message.
	// The extractor should still find tool_calls and ignore reasoning_details.
	body := []byte(`{
		"id": "chatcmpl-minimax-abc",
		"object": "chat.completion",
		"model": "MiniMax-M2.5",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_minimax_789",
					"type": "function",
					"function": {"name": "web_fetch", "arguments": "{\"url\": \"https://example.com\"}"}
				}],
				"reasoning_details": ["thinking step 1", "thinking step 2"]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50,
			"total_tokens": 150,
			"completion_tokens_details": {"reasoning_tokens": 20}
		}
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "web_fetch" {
		t.Errorf("Name: expected web_fetch, got %q", calls[0].Name)
	}
	url, _ := calls[0].Arguments["url"].(string)
	if url != "https://example.com" {
		t.Errorf("Arguments.url: expected https://example.com, got %q", url)
	}
}

// --- Zhipu/GLM extraction tests ---

func TestExtractOpenAI_Zhipu_ArgumentsAsString(t *testing.T) {
	// Zhipu sometimes sends arguments as a JSON string (standard).
	body := []byte(`{
		"id": "chatcmpl-zhipu-abc",
		"request_id": "req-zhipu-123",
		"object": "chat.completion",
		"model": "glm-5",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_zhipu_123",
					"type": "function",
					"function": {"name": "exec", "arguments": "{\"command\": \"pwd\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "exec" {
		t.Errorf("Name: expected exec, got %q", calls[0].Name)
	}
	cmd, _ := calls[0].Arguments["command"].(string)
	if cmd != "pwd" {
		t.Errorf("Arguments.command: expected pwd, got %q", cmd)
	}
}

func TestExtractOpenAI_Zhipu_ArgumentsAsObject(t *testing.T) {
	// Zhipu quirk: arguments may be a JSON object instead of a JSON string.
	body := []byte(`{
		"id": "chatcmpl-zhipu-def",
		"request_id": "req-zhipu-456",
		"object": "chat.completion",
		"model": "glm-5",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_zhipu_456",
					"type": "function",
					"function": {"name": "read", "arguments": {"path": "/etc/hostname"}}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "read" {
		t.Errorf("Name: expected read, got %q", calls[0].Name)
	}
	path, _ := calls[0].Arguments["path"].(string)
	if path != "/etc/hostname" {
		t.Errorf("Arguments.path: expected /etc/hostname, got %q", path)
	}
	if len(calls[0].RawJSON) == 0 {
		t.Error("RawJSON should be populated for arguments-as-object")
	}
}

func TestExtractOpenAI_Zhipu_SensitiveFinishReason(t *testing.T) {
	// Zhipu has unique finish_reason "sensitive" — no tool calls in this case.
	body := []byte(`{
		"choices": [{
			"message": {"role": "assistant", "content": "I cannot help with that."},
			"finish_reason": "sensitive"
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 0 {
		t.Errorf("expected 0 calls for sensitive finish, got %d", len(calls))
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

// ==========================================================================
// OpenAI Responses API extraction tests
// ==========================================================================

func TestExtractOpenAIResponses_SingleFunctionCall(t *testing.T) {
	body := []byte(`{
		"id": "resp_abc123",
		"output": [
			{"type": "message", "content": [{"type": "output_text", "text": "Let me check."}]},
			{"type": "function_call", "id": "fc_001", "call_id": "call_abc", "name": "exec", "arguments": "{\"command\": \"ls -la\"}"}
		],
		"status": "completed"
	}`)

	calls := Extract(body, APITypeOpenAIResponses)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	tc := calls[0]
	if tc.ID != "call_abc" {
		t.Errorf("ID: expected call_abc (from call_id), got %q", tc.ID)
	}
	if tc.Name != "exec" {
		t.Errorf("Name: expected exec, got %q", tc.Name)
	}
	if tc.Index != 1 {
		t.Errorf("Index: expected 1 (second output item), got %d", tc.Index)
	}
	cmd, _ := tc.Arguments["command"].(string)
	if cmd != "ls -la" {
		t.Errorf("Arguments.command: expected 'ls -la', got %q", cmd)
	}
	if len(tc.RawJSON) == 0 {
		t.Error("RawJSON should be populated")
	}
}

func TestExtractOpenAIResponses_MultipleFunctionCalls(t *testing.T) {
	body := []byte(`{
		"id": "resp_multi",
		"output": [
			{"type": "function_call", "call_id": "call_1", "name": "exec", "arguments": "{\"command\": \"ls\"}"},
			{"type": "function_call", "call_id": "call_2", "name": "read", "arguments": "{\"path\": \"/tmp/file\"}"},
			{"type": "function_call", "call_id": "call_3", "name": "write", "arguments": "{\"path\": \"/tmp/out\", \"content\": \"hi\"}"}
		],
		"status": "completed"
	}`)

	calls := Extract(body, APITypeOpenAIResponses)
	if len(calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(calls))
	}
	if calls[0].Name != "exec" || calls[0].ID != "call_1" {
		t.Errorf("call[0]: got %s/%s", calls[0].Name, calls[0].ID)
	}
	if calls[1].Name != "read" || calls[1].ID != "call_2" {
		t.Errorf("call[1]: got %s/%s", calls[1].Name, calls[1].ID)
	}
	if calls[2].Name != "write" || calls[2].ID != "call_3" {
		t.Errorf("call[2]: got %s/%s", calls[2].Name, calls[2].ID)
	}
}

func TestExtractOpenAIResponses_MixedOutputTypes(t *testing.T) {
	// Messages interleaved with function calls — only function_calls extracted.
	body := []byte(`{
		"id": "resp_mixed",
		"output": [
			{"type": "message", "content": [{"type": "output_text", "text": "Thinking..."}]},
			{"type": "function_call", "call_id": "call_x", "name": "exec", "arguments": "{\"command\": \"pwd\"}"},
			{"type": "message", "content": [{"type": "output_text", "text": "Done."}]}
		],
		"status": "completed"
	}`)

	calls := Extract(body, APITypeOpenAIResponses)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call (only function_call types), got %d", len(calls))
	}
	if calls[0].Index != 1 {
		t.Errorf("expected index 1 (middle output item), got %d", calls[0].Index)
	}
}

func TestExtractOpenAIResponses_NoFunctionCalls(t *testing.T) {
	body := []byte(`{
		"id": "resp_text",
		"output": [
			{"type": "message", "content": [{"type": "output_text", "text": "Just text."}]}
		],
		"status": "completed"
	}`)

	calls := Extract(body, APITypeOpenAIResponses)
	if len(calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(calls))
	}
}

func TestExtractOpenAIResponses_EmptyOutput(t *testing.T) {
	body := []byte(`{"id": "resp_empty", "output": [], "status": "completed"}`)
	calls := Extract(body, APITypeOpenAIResponses)
	if len(calls) != 0 {
		t.Errorf("expected 0 calls for empty output, got %d", len(calls))
	}
}

func TestExtractOpenAIResponses_FallbackToID(t *testing.T) {
	// When call_id is absent, fall back to id field.
	body := []byte(`{
		"id": "resp_fallback",
		"output": [
			{"type": "function_call", "id": "fc_fallback_001", "name": "exec", "arguments": "{\"command\": \"ls\"}"}
		],
		"status": "completed"
	}`)

	calls := Extract(body, APITypeOpenAIResponses)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].ID != "fc_fallback_001" {
		t.Errorf("ID: expected fc_fallback_001 (fallback to id), got %q", calls[0].ID)
	}
}

func TestExtractOpenAIResponses_MalformedJSON(t *testing.T) {
	calls := Extract([]byte(`not json`), APITypeOpenAIResponses)
	if calls != nil {
		t.Errorf("expected nil for malformed JSON, got %v", calls)
	}
}

func TestExtractOpenAIResponses_IncompleteStatus(t *testing.T) {
	// status "incomplete" — should still extract function_calls if present.
	body := []byte(`{
		"id": "resp_incomplete",
		"output": [
			{"type": "function_call", "call_id": "call_inc", "name": "exec", "arguments": "{\"command\": \"ls\"}"}
		],
		"status": "incomplete"
	}`)

	calls := Extract(body, APITypeOpenAIResponses)
	if len(calls) != 1 {
		t.Fatalf("should still extract tool calls on incomplete status, got %d", len(calls))
	}
}

// --- Dispatch test for OpenAI Responses API ---

func TestExtract_DispatchOpenAIResponses(t *testing.T) {
	body := []byte(`{"id":"resp_1","output":[{"type":"function_call","call_id":"c1","name":"exec","arguments":"{\"command\":\"ls\"}"}],"status":"completed"}`)

	calls := Extract(body, APITypeOpenAIResponses)
	if len(calls) != 1 || calls[0].Name != "exec" {
		t.Errorf("OpenAI Responses dispatch failed: got %v", calls)
	}

	// Same body with OpenAI type should fail (different schema).
	calls = Extract(body, APITypeOpenAI)
	if len(calls) != 0 {
		t.Errorf("OpenAI dispatch should not parse Responses format, got %d calls", len(calls))
	}
}

// ==========================================================================
// MiniMax empty-string arguments tests
// ==========================================================================

func TestExtractOpenAI_MiniMax_EmptyStringArguments(t *testing.T) {
	// MiniMax sometimes returns "" instead of "{}" for tools with no parameters.
	body := []byte(`{
		"id": "chatcmpl-minimax-empty",
		"object": "chat.completion",
		"model": "MiniMax-M2.5",
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [{
					"id": "call_mm_empty",
					"type": "function",
					"function": {"name": "get_time", "arguments": ""}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "get_time" {
		t.Errorf("Name: expected get_time, got %q", calls[0].Name)
	}
	// Arguments should be an empty map, not nil.
	if calls[0].Arguments == nil {
		t.Error("Arguments should be empty map, not nil")
	}
	if len(calls[0].Arguments) != 0 {
		t.Errorf("Arguments should be empty, got %v", calls[0].Arguments)
	}
	// RawJSON should be "{}".
	if string(calls[0].RawJSON) != "{}" {
		t.Errorf("RawJSON: expected '{}', got %q", string(calls[0].RawJSON))
	}
}

func TestExtractOpenAI_MiniMax_NormalArguments(t *testing.T) {
	// Verify MiniMax with normal JSON string arguments still works.
	body := []byte(`{
		"choices": [{
			"message": {
				"tool_calls": [{
					"id": "call_mm_normal",
					"type": "function",
					"function": {"name": "exec", "arguments": "{\"command\": \"ls\"}"}
				}]
			}
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	cmd, _ := calls[0].Arguments["command"].(string)
	if cmd != "ls" {
		t.Errorf("Arguments.command: expected 'ls', got %q", cmd)
	}
}

// ==========================================================================
// Zhipu/GLM Python-style dict arguments tests
// ==========================================================================

func TestExtractOpenAI_Zhipu_PythonStyleDict(t *testing.T) {
	// Zhipu sometimes returns Python-style dict strings with single quotes.
	body := []byte(`{
		"id": "chatcmpl-zhipu-pydict",
		"choices": [{
			"message": {
				"tool_calls": [{
					"id": "call_zhipu_py",
					"type": "function",
					"function": {"name": "exec", "arguments": "{'command': 'ls -la', 'verbose': True}"}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "exec" {
		t.Errorf("Name: expected exec, got %q", calls[0].Name)
	}
	cmd, _ := calls[0].Arguments["command"].(string)
	if cmd != "ls -la" {
		t.Errorf("Arguments.command: expected 'ls -la', got %q", cmd)
	}
	verbose, ok := calls[0].Arguments["verbose"].(bool)
	if !ok || !verbose {
		t.Errorf("Arguments.verbose: expected true, got %v", calls[0].Arguments["verbose"])
	}
}

func TestExtractOpenAI_Zhipu_PythonFalseAndNone(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"message": {
				"tool_calls": [{
					"id": "call_py2",
					"type": "function",
					"function": {"name": "config", "arguments": "{'debug': False, 'extra': None}"}
				}]
			}
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	debug, ok := calls[0].Arguments["debug"].(bool)
	if !ok || debug {
		t.Errorf("Arguments.debug: expected false, got %v", calls[0].Arguments["debug"])
	}
	extra := calls[0].Arguments["extra"]
	if extra != nil {
		t.Errorf("Arguments.extra: expected nil (from None), got %v", extra)
	}
}

func TestExtractOpenAI_Zhipu_PythonDictWithDoubleQuotesInside(t *testing.T) {
	// Edge case: Python single-quoted string containing a double quote.
	body := []byte(`{
		"choices": [{
			"message": {
				"tool_calls": [{
					"id": "call_edge",
					"type": "function",
					"function": {"name": "exec", "arguments": "{'command': 'echo \"hello\"'}"}
				}]
			}
		}]
	}`)

	calls := Extract(body, APITypeOpenAI)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	cmd, _ := calls[0].Arguments["command"].(string)
	if cmd != `echo "hello"` {
		t.Errorf("Arguments.command: expected 'echo \"hello\"', got %q", cmd)
	}
}

// ==========================================================================
// parseToolArguments direct tests
// ==========================================================================

func TestParseToolArguments_EmptyRaw(t *testing.T) {
	rawJSON, args := parseToolArguments(nil)
	if rawJSON != nil || args != nil {
		t.Error("empty raw should return nil, nil")
	}
}

func TestParseToolArguments_WhitespaceOnly(t *testing.T) {
	rawJSON, args := parseToolArguments([]byte("   \t\n  "))
	if rawJSON != nil || args != nil {
		t.Error("whitespace-only should return nil, nil")
	}
}

func TestParseToolArguments_JSONString(t *testing.T) {
	raw := []byte(`"{\"key\": \"value\"}"`)
	rawJSON, args := parseToolArguments(raw)
	if args["key"] != "value" {
		t.Errorf("expected key=value, got %v", args)
	}
	if len(rawJSON) == 0 {
		t.Error("RawJSON should be populated")
	}
}

func TestParseToolArguments_JSONObject(t *testing.T) {
	raw := []byte(`{"key": "value"}`)
	rawJSON, args := parseToolArguments(raw)
	if args["key"] != "value" {
		t.Errorf("expected key=value, got %v", args)
	}
	if string(rawJSON) != `{"key": "value"}` {
		t.Errorf("RawJSON: got %q", string(rawJSON))
	}
}

func TestParseToolArguments_UnexpectedFormat(t *testing.T) {
	raw := []byte(`[1, 2, 3]`)
	rawJSON, args := parseToolArguments(raw)
	if args != nil {
		t.Error("unexpected format should return nil args")
	}
	if string(rawJSON) != `[1, 2, 3]` {
		t.Errorf("RawJSON: got %q", string(rawJSON))
	}
}

// ==========================================================================
// tryFixPythonDict tests
// ==========================================================================

func TestTryFixPythonDict_SimpleDict(t *testing.T) {
	result, ok := tryFixPythonDict("{'a': 'b'}")
	if !ok {
		t.Fatal("expected ok")
	}
	if result != `{"a": "b"}` {
		t.Errorf("got %q", result)
	}
}

func TestTryFixPythonDict_WithBooleans(t *testing.T) {
	result, ok := tryFixPythonDict("{'flag': True, 'other': False}")
	if !ok {
		t.Fatal("expected ok")
	}
	// Should contain lowercase true/false.
	var m map[string]any
	if err := json.Unmarshal([]byte(result), &m); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if m["flag"] != true {
		t.Errorf("flag: expected true, got %v", m["flag"])
	}
	if m["other"] != false {
		t.Errorf("other: expected false, got %v", m["other"])
	}
}

func TestTryFixPythonDict_WithNone(t *testing.T) {
	result, ok := tryFixPythonDict("{'key': None}")
	if !ok {
		t.Fatal("expected ok")
	}
	var m map[string]any
	json.Unmarshal([]byte(result), &m)
	if m["key"] != nil {
		t.Errorf("key: expected nil, got %v", m["key"])
	}
}

func TestTryFixPythonDict_NotADict(t *testing.T) {
	_, ok := tryFixPythonDict("not a dict")
	if ok {
		t.Error("non-dict should return false")
	}
}

func TestTryFixPythonDict_EmptyString(t *testing.T) {
	_, ok := tryFixPythonDict("")
	if ok {
		t.Error("empty string should return false")
	}
}

func TestTryFixPythonDict_AlreadyValidJSON(t *testing.T) {
	// A valid JSON dict with double quotes should pass through.
	result, ok := tryFixPythonDict(`{"key": "value"}`)
	if !ok {
		t.Fatal("valid JSON dict should succeed")
	}
	var m map[string]any
	json.Unmarshal([]byte(result), &m)
	if m["key"] != "value" {
		t.Errorf("key: expected value, got %v", m["key"])
	}
}

// ==========================================================================
// replacePythonKeywords tests
// ==========================================================================

func TestReplacePythonKeywords(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`{"a": True}`, `{"a": true}`},
		{`{"a": False}`, `{"a": false}`},
		{`{"a": None}`, `{"a": null}`},
		{`{"a": True, "b": False, "c": None}`, `{"a": true, "b": false, "c": null}`},
		{`{"a": "True"}`, `{"a": "True"}`}, // Inside string — naive replacement is OK, validated after.
		{`[True,False,None]`, `[true,false,null]`},
	}

	for _, tt := range tests {
		result := replacePythonKeywords(tt.input)
		if result != tt.expected {
			t.Errorf("replacePythonKeywords(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}
