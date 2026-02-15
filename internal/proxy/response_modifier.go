package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ctrlai/ctrlai/internal/engine"
	"github.com/ctrlai/ctrlai/internal/extractor"
)

// modifyNonStreamingResponse modifies a non-streaming LLM response body
// to strip blocked tool_use blocks, change the stop_reason, and inject
// a block notice.
//
// Design doc Section 7.1 (Anthropic) and 7.2 (OpenAI):
//   - Keep all thinking blocks unchanged (preserve signature for verification)
//   - Keep all text blocks unchanged
//   - Remove blocked tool_use blocks
//   - Inject block notice as new text block
//   - Change stop_reason from "tool_use" to "end_turn" (Anthropic)
//     or finish_reason from "tool_calls" to "stop" (OpenAI)
//   - If partial block (some allowed, some blocked): keep stop_reason as "tool_use"
func modifyNonStreamingResponse(body []byte, apiType extractor.APIType, blocked []extractor.ToolCall, decisions []engine.Decision) []byte {
	switch apiType {
	case extractor.APITypeAnthropic:
		return modifyAnthropicResponse(body, blocked, decisions)
	case extractor.APITypeOpenAI:
		return modifyOpenAIResponse(body, blocked, decisions)
	default:
		return body
	}
}

// modifyAnthropicResponse modifies an Anthropic Messages API response.
//
// Design doc Section 7.1:
//
//	Before: content: [thinking, text, tool_use(blocked)]  stop_reason: "tool_use"
//	After:  content: [thinking, text, text("[CtrlAI] Blocked: ...")]  stop_reason: "end_turn"
func modifyAnthropicResponse(body []byte, blocked []extractor.ToolCall, decisions []engine.Decision) []byte {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.Error("failed to parse Anthropic response for modification", "error", err)
		return body
	}

	// Parse the content array.
	contentRaw, ok := resp["content"]
	if !ok {
		return body
	}

	var content []map[string]json.RawMessage
	if err := json.Unmarshal(contentRaw, &content); err != nil {
		return body
	}

	// Build set of blocked tool call IDs for fast lookup.
	blockedIDs := make(map[string]bool)
	for _, tc := range blocked {
		blockedIDs[tc.ID] = true
	}

	// Filter content: keep thinking + text, remove blocked tool_use.
	var filtered []map[string]json.RawMessage
	hasAllowedToolUse := false

	for _, block := range content {
		blockType := unquoteRaw(block["type"])

		if blockType == "tool_use" {
			// Check if this tool_use is blocked.
			id := unquoteRaw(block["id"])
			if blockedIDs[id] {
				continue // Strip blocked tool_use.
			}
			hasAllowedToolUse = true
		}

		filtered = append(filtered, block)
	}

	// Inject block notice as a text block.
	for i, d := range decisions {
		notice := formatBlockNotice(blocked[i].Name, d.Rule, d.Message)
		textBlock := map[string]json.RawMessage{
			"type": json.RawMessage(`"text"`),
			"text": mustMarshal(notice),
		}
		filtered = append(filtered, textBlock)
	}

	// Update the content array.
	resp["content"] = mustMarshalRaw(filtered)

	// Change stop_reason: "tool_use" → "end_turn" if all tools blocked.
	// Keep "tool_use" if some were allowed (partial blocking, design doc Section 7.4).
	if !hasAllowedToolUse {
		resp["stop_reason"] = json.RawMessage(`"end_turn"`)
	}

	modified, err := json.Marshal(resp)
	if err != nil {
		slog.Error("failed to marshal modified Anthropic response", "error", err)
		return body
	}
	return modified
}

// modifyOpenAIResponse modifies an OpenAI Chat Completions API response.
//
// Design doc Section 7.2:
//
//	Before: tool_calls: [blocked_call]  finish_reason: "tool_calls"
//	After:  tool_calls: []  content: "...\n[CtrlAI] Blocked: ..."  finish_reason: "stop"
func modifyOpenAIResponse(body []byte, blocked []extractor.ToolCall, decisions []engine.Decision) []byte {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.Error("failed to parse OpenAI response for modification", "error", err)
		return body
	}

	choicesRaw, ok := resp["choices"]
	if !ok {
		return body
	}

	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(choicesRaw, &choices); err != nil || len(choices) == 0 {
		return body
	}

	choice := choices[0]
	messageRaw, ok := choice["message"]
	if !ok {
		return body
	}

	var message map[string]json.RawMessage
	if err := json.Unmarshal(messageRaw, &message); err != nil {
		return body
	}

	// Filter blocked tool_calls.
	blockedIDs := make(map[string]bool)
	for _, tc := range blocked {
		blockedIDs[tc.ID] = true
	}

	hasAllowedToolCalls := false
	if tcRaw, ok := message["tool_calls"]; ok {
		var toolCalls []map[string]json.RawMessage
		if err := json.Unmarshal(tcRaw, &toolCalls); err == nil {
			var kept []map[string]json.RawMessage
			for _, tc := range toolCalls {
				id := unquoteRaw(tc["id"])
				if blockedIDs[id] {
					continue
				}
				kept = append(kept, tc)
				hasAllowedToolCalls = true
			}
			if len(kept) == 0 {
				message["tool_calls"] = json.RawMessage(`[]`)
			} else {
				message["tool_calls"] = mustMarshalRaw(kept)
			}
		}
	}

	// Append block notice to content.
	var existingContent string
	if contentRaw, ok := message["content"]; ok {
		json.Unmarshal(contentRaw, &existingContent)
	}

	for i, d := range decisions {
		notice := formatBlockNotice(blocked[i].Name, d.Rule, d.Message)
		if existingContent != "" {
			existingContent += "\n\n" + notice
		} else {
			existingContent = notice
		}
	}
	message["content"] = mustMarshal(existingContent)

	// Change finish_reason if all tool calls blocked.
	if !hasAllowedToolCalls {
		choice["finish_reason"] = json.RawMessage(`"stop"`)
	}

	// Rebuild the response.
	choice["message"] = mustMarshalRaw(message)
	choices[0] = choice
	resp["choices"] = mustMarshalRaw(choices)

	modified, err := json.Marshal(resp)
	if err != nil {
		slog.Error("failed to marshal modified OpenAI response", "error", err)
		return body
	}
	return modified
}

// formatBlockNotice creates the user-visible block notice.
// Format: [CtrlAI] Blocked: <message> (rule: <rule_name>)
func formatBlockNotice(toolName, ruleName, message string) string {
	if message == "" {
		message = fmt.Sprintf("Tool call '%s' was blocked", toolName)
	}
	if ruleName != "" {
		return fmt.Sprintf("[CtrlAI] Blocked: %s (rule: %s)", message, ruleName)
	}
	return fmt.Sprintf("[CtrlAI] Blocked: %s", message)
}

// buildKilledResponse creates a fake LLM response for a killed agent.
// The response looks like a normal "end_turn" so the SDK stops the agent loop.
//
// Design doc Section 9.1.
func buildKilledResponse(apiType extractor.APIType) []byte {
	switch apiType {
	case extractor.APITypeAnthropic:
		resp := map[string]any{
			"id":   "msg_ctrlai_killed",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "This agent has been terminated by the administrator."},
			},
			"model":       "ctrlai-kill-switch",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
		}
		data, _ := json.Marshal(resp)
		return data

	case extractor.APITypeOpenAI:
		resp := map[string]any{
			"id":     "chatcmpl-ctrlai-killed",
			"object": "chat.completion",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "This agent has been terminated by the administrator.",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0},
		}
		data, _ := json.Marshal(resp)
		return data

	default:
		// For unknown API types, return a simple JSON response.
		data, _ := json.Marshal(map[string]any{
			"error": "This agent has been terminated by the administrator.",
		})
		return data
	}
}

// unquoteRaw extracts a string from a json.RawMessage.
func unquoteRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	json.Unmarshal(raw, &s)
	return s
}

// mustMarshal marshals a value to json.RawMessage, panicking on error.
func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("json.Marshal failed: %v", err))
	}
	return data
}

// mustMarshalRaw marshals a value to json.RawMessage.
func mustMarshalRaw(v any) json.RawMessage {
	return mustMarshal(v)
}
