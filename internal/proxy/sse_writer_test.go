package proxy

import (
	"encoding/json"
	"testing"

	"github.com/ctrlai/ctrlai/internal/extractor"
)

// Helper to build Anthropic SSE events for testing.
func anthropicTestEvents() []SSEEvent {
	return []SSEEvent{
		{Event: "message_start", Data: `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude","stop_reason":null}}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":0}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"exec"}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":1}`},
		{Event: "message_delta", Data: `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":50}}`},
		{Event: "message_stop", Data: `{"type":"message_stop"}`},
	}
}

func TestBuildModifiedAnthropicStream_AllBlocked(t *testing.T) {
	events := anthropicTestEvents()
	blocked := []extractor.ToolCall{{ID: "toolu_01", Name: "exec", Index: 1}}
	messages := []string{"[CtrlAI] Blocked: exec (rule: test_rule)"}

	modified := buildModifiedStream(events, extractor.APITypeAnthropic, blocked, messages)

	// Tool_use events (index 1) should be stripped.
	for _, evt := range modified {
		if evt.Event == "content_block_start" {
			var start struct {
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
			}
			json.Unmarshal([]byte(evt.Data), &start)
			if start.ContentBlock.Type == "tool_use" {
				t.Error("tool_use content_block_start should have been stripped")
			}
		}
	}

	// Stop reason should be changed to end_turn.
	for _, evt := range modified {
		if evt.Event == "message_delta" {
			var md struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			json.Unmarshal([]byte(evt.Data), &md)
			if md.Delta.StopReason != "end_turn" {
				t.Errorf("stop_reason should be end_turn, got %q", md.Delta.StopReason)
			}
		}
	}

	// Should have a block notice text block injected.
	hasNotice := false
	for _, evt := range modified {
		if evt.Event == "content_block_delta" {
			var delta struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			json.Unmarshal([]byte(evt.Data), &delta)
			if delta.Delta.Type == "text_delta" && delta.Delta.Text != "" && delta.Delta.Text != "Hello" {
				hasNotice = true
			}
		}
	}
	if !hasNotice {
		t.Error("expected block notice text block to be injected")
	}
}

func TestBuildModifiedAnthropicStream_TextPreserved(t *testing.T) {
	events := anthropicTestEvents()
	blocked := []extractor.ToolCall{{ID: "toolu_01", Name: "exec", Index: 1}}
	messages := []string{"blocked"}

	modified := buildModifiedStream(events, extractor.APITypeAnthropic, blocked, messages)

	// Text block (index 0) should be preserved with its content.
	foundText := false
	for _, evt := range modified {
		if evt.Event == "content_block_delta" {
			var delta struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			json.Unmarshal([]byte(evt.Data), &delta)
			if delta.Delta.Text == "Hello" {
				foundText = true
			}
		}
	}
	if !foundText {
		t.Error("original text block should be preserved")
	}
}

func TestBuildModifiedAnthropicStream_ReindexesBlocks(t *testing.T) {
	// Events: text@0, tool_use@1 (blocked), tool_use@2 (allowed).
	events := []SSEEvent{
		{Event: "message_start", Data: `{"type":"message_start","message":{}}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":0}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"bad"}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":1}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t2","name":"good"}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":2}`},
		{Event: "message_delta", Data: `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`},
		{Event: "message_stop", Data: `{"type":"message_stop"}`},
	}

	// Block only index 1.
	blocked := []extractor.ToolCall{{ID: "t1", Name: "bad", Index: 1}}
	messages := []string{"blocked"}

	modified := buildModifiedStream(events, extractor.APITypeAnthropic, blocked, messages)

	// After stripping index 1, the remaining tool_use (originally index 2) should be reindexed to 1.
	for _, evt := range modified {
		if evt.Event == "content_block_start" {
			var start struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			json.Unmarshal([]byte(evt.Data), &start)
			if start.ContentBlock.Name == "good" && start.Index != 1 {
				t.Errorf("'good' tool_use should be reindexed to 1, got %d", start.Index)
			}
		}
	}

	// stop_reason should stay tool_use (partial block — one tool still allowed).
	for _, evt := range modified {
		if evt.Event == "message_delta" {
			var md struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			json.Unmarshal([]byte(evt.Data), &md)
			if md.Delta.StopReason != "tool_use" {
				t.Errorf("partial block: stop_reason should remain tool_use, got %q", md.Delta.StopReason)
			}
		}
	}
}

// --- OpenAI SSE stream tests ---

func openaiTestEvents() []SSEEvent {
	return []SSEEvent{
		{Data: `{"id":"chatcmpl-1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"exec","arguments":""}}]}}]}`},
		{Data: `{"id":"chatcmpl-1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`},
		{Data: `{"id":"chatcmpl-1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`},
		{Data: `{"id":"chatcmpl-1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`},
		{Data: "[DONE]"},
	}
}

func TestBuildModifiedOpenAIStream_AllBlocked(t *testing.T) {
	events := openaiTestEvents()
	blocked := []extractor.ToolCall{{ID: "call_1", Name: "exec", Index: 0}}
	messages := []string{"[CtrlAI] Blocked: exec"}

	modified := buildModifiedStream(events, extractor.APITypeOpenAI, blocked, messages)

	// Check finish_reason changed to stop.
	for _, evt := range modified {
		if evt.Data == "" || evt.Data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
			if *chunk.Choices[0].FinishReason != "stop" {
				t.Errorf("finish_reason should be stop when all blocked, got %q", *chunk.Choices[0].FinishReason)
			}
		}
	}
}

// --- allToolsBlocked tests ---

func TestAllToolsBlocked_Anthropic(t *testing.T) {
	events := anthropicTestEvents() // has one tool_use at index 1.

	// Block index 1 → all blocked.
	if !allToolsBlocked(events, map[int]bool{1: true}) {
		t.Error("expected allToolsBlocked=true when index 1 is blocked")
	}

	// Don't block index 1 → not all blocked.
	if allToolsBlocked(events, map[int]bool{99: true}) {
		t.Error("expected allToolsBlocked=false when index 1 is not blocked")
	}

	// No tool_use events → vacuously true.
	textOnly := []SSEEvent{
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	}
	if !allToolsBlocked(textOnly, map[int]bool{}) {
		t.Error("no tool_use events should return true (vacuously)")
	}
}

func TestAllOpenAIToolsBlocked(t *testing.T) {
	events := openaiTestEvents() // has tool call at index 0.

	// Block index 0 → all blocked.
	if !allOpenAIToolsBlocked(events, map[int]bool{0: true}) {
		t.Error("expected true when index 0 is blocked")
	}

	// Don't block index 0 → not all blocked.
	if allOpenAIToolsBlocked(events, map[int]bool{99: true}) {
		t.Error("expected false when index 0 is not in blocked set")
	}

	// No tool calls → false (len(seenIndexes) == 0).
	noTools := []SSEEvent{
		{Data: `{"choices":[{"delta":{"content":"Hello"}}]}`},
		{Data: "[DONE]"},
	}
	if allOpenAIToolsBlocked(noTools, map[int]bool{}) {
		t.Error("no tool calls should return false")
	}
}

// --- buildBlockNoticeText ---

func TestBuildBlockNoticeText(t *testing.T) {
	// Single message.
	single := buildBlockNoticeText([]string{"[CtrlAI] Blocked: exec"})
	if single != "[CtrlAI] Blocked: exec" {
		t.Errorf("single message: got %q", single)
	}

	// Multiple messages.
	multi := buildBlockNoticeText([]string{"msg1", "msg2"})
	if multi == "" {
		t.Error("multi message should not be empty")
	}
}
