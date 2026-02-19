package proxy

import (
	"testing"

	"github.com/ctrlai/ctrlai/internal/extractor"
)

func TestReconstructAnthropic_FullStream(t *testing.T) {
	events := []SSEEvent{
		{Event: "message_start", Data: `{"type":"message_start","message":{"id":"msg_01"}}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":0}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello "}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"world"}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":1}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_01","name":"exec"}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ls -la\"}"}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":2}`},
		{Event: "message_delta", Data: `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`},
		{Event: "message_stop", Data: `{"type":"message_stop"}`},
	}

	msg := reconstructAnthropic(events)

	// Should have 3 content blocks: thinking, text, tool_use.
	if len(msg.ContentBlocks) != 3 {
		t.Fatalf("expected 3 content blocks, got %d", len(msg.ContentBlocks))
	}

	// Thinking block.
	if msg.ContentBlocks[0].Type != "thinking" {
		t.Errorf("block[0]: expected thinking, got %q", msg.ContentBlocks[0].Type)
	}
	if msg.ContentBlocks[0].Thinking != "Let me think..." {
		t.Errorf("thinking text: got %q", msg.ContentBlocks[0].Thinking)
	}
	if msg.ContentBlocks[0].Signature != "sig123" {
		t.Errorf("signature: got %q", msg.ContentBlocks[0].Signature)
	}

	// Text block.
	if msg.ContentBlocks[1].Type != "text" {
		t.Errorf("block[1]: expected text, got %q", msg.ContentBlocks[1].Type)
	}
	if msg.ContentBlocks[1].Text != "Hello world" {
		t.Errorf("text: expected 'Hello world', got %q", msg.ContentBlocks[1].Text)
	}

	// Tool use block.
	if msg.ContentBlocks[2].Type != "tool_use" {
		t.Errorf("block[2]: expected tool_use, got %q", msg.ContentBlocks[2].Type)
	}
	if msg.ContentBlocks[2].Name != "exec" {
		t.Errorf("tool name: expected exec, got %q", msg.ContentBlocks[2].Name)
	}
	if msg.ContentBlocks[2].ID != "toolu_01" {
		t.Errorf("tool ID: expected toolu_01, got %q", msg.ContentBlocks[2].ID)
	}

	// Tool calls extracted.
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Name != "exec" {
		t.Errorf("tool call name: expected exec, got %q", msg.ToolCalls[0].Name)
	}
	cmd, _ := msg.ToolCalls[0].Arguments["command"].(string)
	if cmd != "ls -la" {
		t.Errorf("tool call command: expected 'ls -la', got %q", cmd)
	}

	// Stop reason.
	if msg.StopReason != "tool_use" {
		t.Errorf("stop reason: expected tool_use, got %q", msg.StopReason)
	}
}

func TestReconstructAnthropic_TextOnly(t *testing.T) {
	events := []SSEEvent{
		{Event: "message_start", Data: `{"type":"message_start","message":{}}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Just text."}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":0}`},
		{Event: "message_delta", Data: `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`},
		{Event: "message_stop", Data: `{"type":"message_stop"}`},
	}

	msg := reconstructAnthropic(events)

	if len(msg.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(msg.ToolCalls))
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop reason: expected end_turn, got %q", msg.StopReason)
	}
}

func TestReconstructAnthropic_Empty(t *testing.T) {
	msg := reconstructAnthropic(nil)
	if len(msg.ContentBlocks) != 0 {
		t.Errorf("expected 0 blocks, got %d", len(msg.ContentBlocks))
	}
	if len(msg.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(msg.ToolCalls))
	}
}

func TestReconstructOpenAI_ToolCalls(t *testing.T) {
	events := []SSEEvent{
		{Data: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"exec","arguments":""}}]}}]}`},
		{Data: `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`},
		{Data: `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`},
		{Data: `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`},
		{Data: "[DONE]"},
	}

	msg := reconstructOpenAI(events)

	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Name != "exec" {
		t.Errorf("name: expected exec, got %q", msg.ToolCalls[0].Name)
	}
	if msg.ToolCalls[0].ID != "call_1" {
		t.Errorf("ID: expected call_1, got %q", msg.ToolCalls[0].ID)
	}
	cmd, _ := msg.ToolCalls[0].Arguments["command"].(string)
	if cmd != "ls" {
		t.Errorf("command: expected 'ls', got %q", cmd)
	}
	if msg.StopReason != "tool_calls" {
		t.Errorf("stop reason: expected tool_calls, got %q", msg.StopReason)
	}
}

func TestReconstructOpenAI_MultipleToolCalls(t *testing.T) {
	events := []SSEEvent{
		{Data: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"exec","arguments":"{\"command\":\"ls\"}"}}]}}]}`},
		{Data: `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"read","arguments":"{\"path\":\"/tmp\"}"}}]}}]}`},
		{Data: `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`},
		{Data: "[DONE]"},
	}

	msg := reconstructOpenAI(events)

	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Name != "exec" {
		t.Errorf("call[0]: expected exec, got %q", msg.ToolCalls[0].Name)
	}
	if msg.ToolCalls[1].Name != "read" {
		t.Errorf("call[1]: expected read, got %q", msg.ToolCalls[1].Name)
	}
}

func TestReconstructOpenAI_ContentOnly(t *testing.T) {
	events := []SSEEvent{
		{Data: `{"choices":[{"delta":{"content":"Hello"}}]}`},
		{Data: `{"choices":[{"delta":{},"finish_reason":"stop"}]}`},
		{Data: "[DONE]"},
	}

	msg := reconstructOpenAI(events)
	if len(msg.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(msg.ToolCalls))
	}
	if msg.StopReason != "stop" {
		t.Errorf("stop reason: expected stop, got %q", msg.StopReason)
	}
}

func TestReconstructOpenAI_Empty(t *testing.T) {
	msg := reconstructOpenAI(nil)
	if len(msg.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(msg.ToolCalls))
	}
}

// ==========================================================================
// OpenAI Responses API stream reconstruction tests
// ==========================================================================

func TestReconstructOpenAIResponses_SingleFunctionCall(t *testing.T) {
	events := []SSEEvent{
		{Event: "response.output_item.added", Data: `{"type":"function_call","call_id":"call_abc","name":"exec"}`},
		{Event: "response.function_call_arguments.delta", Data: `{"call_id":"call_abc","delta":"{\"command\":"}`},
		{Event: "response.function_call_arguments.delta", Data: `{"call_id":"call_abc","delta":"\"ls -la\"}"}`},
		{Event: "response.function_call_arguments.done", Data: `{"call_id":"call_abc","arguments":"{\"command\": \"ls -la\"}"}`},
		{Event: "response.completed", Data: `{"id":"resp_abc","status":"completed"}`},
	}

	msg := reconstructOpenAIResponses(events)

	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Name != "exec" {
		t.Errorf("name: expected exec, got %q", msg.ToolCalls[0].Name)
	}
	if msg.ToolCalls[0].ID != "call_abc" {
		t.Errorf("ID: expected call_abc, got %q", msg.ToolCalls[0].ID)
	}
	cmd, _ := msg.ToolCalls[0].Arguments["command"].(string)
	if cmd != "ls -la" {
		t.Errorf("command: expected 'ls -la', got %q", cmd)
	}
	if msg.StopReason != "completed" {
		t.Errorf("stop reason: expected completed, got %q", msg.StopReason)
	}
}

func TestReconstructOpenAIResponses_MultipleFunctionCalls(t *testing.T) {
	events := []SSEEvent{
		{Event: "response.output_item.added", Data: `{"type":"function_call","call_id":"call_1","name":"exec"}`},
		{Event: "response.output_item.added", Data: `{"type":"function_call","call_id":"call_2","name":"read"}`},
		{Event: "response.function_call_arguments.done", Data: `{"call_id":"call_1","arguments":"{\"command\": \"ls\"}"}`},
		{Event: "response.function_call_arguments.done", Data: `{"call_id":"call_2","arguments":"{\"path\": \"/tmp\"}"}`},
		{Event: "response.completed", Data: `{"id":"resp_multi","status":"completed"}`},
	}

	msg := reconstructOpenAIResponses(events)

	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Name != "exec" {
		t.Errorf("call[0]: expected exec, got %q", msg.ToolCalls[0].Name)
	}
	if msg.ToolCalls[1].Name != "read" {
		t.Errorf("call[1]: expected read, got %q", msg.ToolCalls[1].Name)
	}
}

func TestReconstructOpenAIResponses_DoneOverridesDeltas(t *testing.T) {
	events := []SSEEvent{
		{Event: "response.output_item.added", Data: `{"type":"function_call","call_id":"call_x","name":"exec"}`},
		{Event: "response.function_call_arguments.delta", Data: `{"call_id":"call_x","delta":"partial_garbage"}`},
		{Event: "response.function_call_arguments.done", Data: `{"call_id":"call_x","arguments":"{\"command\": \"correct\"}"}`},
		{Event: "response.completed", Data: `{"status":"completed"}`},
	}

	msg := reconstructOpenAIResponses(events)
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	cmd, _ := msg.ToolCalls[0].Arguments["command"].(string)
	if cmd != "correct" {
		t.Errorf("done event should override deltas, got %q", cmd)
	}
}

func TestReconstructOpenAIResponses_MessageOnly(t *testing.T) {
	events := []SSEEvent{
		{Event: "response.output_item.added", Data: `{"type":"message","content":[]}`},
		{Event: "response.completed", Data: `{"status":"completed"}`},
	}

	msg := reconstructOpenAIResponses(events)
	if len(msg.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(msg.ToolCalls))
	}
	if msg.StopReason != "completed" {
		t.Errorf("stop reason: expected completed, got %q", msg.StopReason)
	}
}

func TestReconstructOpenAIResponses_Empty(t *testing.T) {
	msg := reconstructOpenAIResponses(nil)
	if len(msg.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(msg.ToolCalls))
	}
}

func TestReconstructOpenAIResponses_EmptyEvents(t *testing.T) {
	// Events with empty data should be skipped.
	events := []SSEEvent{
		{Event: "response.output_item.added", Data: ""},
		{Event: "response.completed", Data: `{"status":"completed"}`},
	}
	msg := reconstructOpenAIResponses(events)
	if len(msg.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(msg.ToolCalls))
	}
}

// --- reconstruct dispatch ---

func TestReconstruct_DispatchOpenAIResponses(t *testing.T) {
	events := []SSEEvent{
		{Event: "response.output_item.added", Data: `{"type":"function_call","call_id":"c1","name":"exec"}`},
		{Event: "response.function_call_arguments.done", Data: `{"call_id":"c1","arguments":"{}"}`},
	}
	msg := reconstruct(events, extractor.APITypeOpenAIResponses)
	if len(msg.ToolCalls) != 1 {
		t.Errorf("OpenAI Responses dispatch: expected 1 tool call, got %d", len(msg.ToolCalls))
	}

	// Unknown type should return empty message.
	msg = reconstruct(events, extractor.APITypeUnknown)
	if len(msg.ToolCalls) != 0 {
		t.Errorf("Unknown dispatch: expected 0 tool calls, got %d", len(msg.ToolCalls))
	}
}
