package proxy

import (
	"encoding/json"
	"fmt"

	"github.com/ctrlai/ctrlai/internal/extractor"
)

// buildModifiedStream creates a new SSE event stream with blocked tool_use
// blocks removed and a block notice injected. The key complexity is
// re-indexing content blocks to maintain contiguous indexes.
//
// Design doc Section 7.3 — Streaming block (Anthropic):
//  1. Replay all thinking content_block events as-is
//  2. Replay all text content_block events as-is
//  3. Skip all tool_use content_block events (start/delta/stop)
//  4. Inject a new text content_block with the block notice
//  5. Change message_delta.stop_reason from "tool_use" to "end_turn"
//  6. Send message_stop
//  7. Re-index all content_block_start/delta/stop events sequentially
//
// For OpenAI, the approach is simpler: filter tool_calls from delta chunks
// and change finish_reason.
func buildModifiedStream(events []SSEEvent, apiType extractor.APIType, blocked []extractor.ToolCall, blockMessages []string) []SSEEvent {
	switch apiType {
	case extractor.APITypeAnthropic:
		return buildModifiedAnthropicStream(events, blocked, blockMessages)
	case extractor.APITypeOpenAI:
		return buildModifiedOpenAIStream(events, blocked, blockMessages)
	default:
		return events
	}
}

// buildModifiedAnthropicStream rebuilds an Anthropic SSE stream with
// blocked tool_use blocks stripped and re-indexed.
//
// Critical: When we strip tool_use blocks, the remaining blocks need
// contiguous indexes (0, 1, 2...). We must update the "index" field in
// content_block_start, content_block_delta, and content_block_stop events.
func buildModifiedAnthropicStream(events []SSEEvent, blocked []extractor.ToolCall, blockMessages []string) []SSEEvent {
	// Build a set of blocked tool_use block indexes for fast lookup.
	blockedIndexes := make(map[int]bool)
	for _, tc := range blocked {
		blockedIndexes[tc.Index] = true
	}

	// First pass: identify which original indexes are kept vs removed.
	// Build the old-index → new-index mapping for re-indexing.
	indexMap := make(map[int]int) // old index → new index
	newIndex := 0

	// Scan events to find all content block indexes and their types.
	for _, evt := range events {
		if evt.Event != "content_block_start" {
			continue
		}
		var start struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &start); err != nil {
			continue
		}

		if start.ContentBlock.Type == "tool_use" && blockedIndexes[start.Index] {
			// This block is blocked — skip it (no new index).
			continue
		}
		indexMap[start.Index] = newIndex
		newIndex++
	}

	// nextIndex for the block notice text block we'll inject.
	blockNoticeIndex := newIndex

	// Second pass: replay events with re-indexed blocks.
	var modified []SSEEvent
	skipBlock := -1 // Track which block we're currently skipping.

	for _, evt := range events {
		switch evt.Event {
		case "message_start":
			// Pass through unchanged.
			modified = append(modified, evt)

		case "content_block_start":
			var start struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &start); err != nil {
				modified = append(modified, evt)
				continue
			}

			if start.ContentBlock.Type == "tool_use" && blockedIndexes[start.Index] {
				// Skip this entire tool_use block.
				skipBlock = start.Index
				continue
			}

			// Re-index the block.
			modified = append(modified, reindexEvent(evt, start.Index, indexMap))
			skipBlock = -1

		case "content_block_delta":
			var delta struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &delta); err != nil {
				modified = append(modified, evt)
				continue
			}

			if delta.Index == skipBlock {
				continue // Part of a blocked tool_use block.
			}

			modified = append(modified, reindexEvent(evt, delta.Index, indexMap))

		case "content_block_stop":
			var stop struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &stop); err != nil {
				modified = append(modified, evt)
				continue
			}

			if stop.Index == skipBlock {
				skipBlock = -1
				continue // End of a blocked tool_use block.
			}

			modified = append(modified, reindexEvent(evt, stop.Index, indexMap))

		case "message_delta":
			// Change stop_reason from "tool_use" to "end_turn" if all
			// tools were blocked. Keep "tool_use" if some were allowed
			// (partial blocking — design doc Section 7.4).
			if len(blocked) > 0 && allToolsBlocked(events, blockedIndexes) {
				modified = append(modified, rewriteStopReason(evt, "end_turn"))
			} else {
				modified = append(modified, evt)
			}

		case "message_stop":
			// Inject block notice text block before message_stop.
			if len(blockMessages) > 0 {
				notice := buildBlockNoticeText(blockMessages)
				modified = append(modified, buildTextBlockEvents(blockNoticeIndex, notice)...)
			}
			modified = append(modified, evt)

		default:
			// Pass through any unknown events.
			modified = append(modified, evt)
		}
	}

	return modified
}

// buildModifiedOpenAIStream rebuilds an OpenAI SSE stream with blocked
// tool_calls removed and finish_reason changed.
func buildModifiedOpenAIStream(events []SSEEvent, blocked []extractor.ToolCall, blockMessages []string) []SSEEvent {
	blockedIndexes := make(map[int]bool)
	for _, tc := range blocked {
		blockedIndexes[tc.Index] = true
	}

	var modified []SSEEvent

	for _, evt := range events {
		if evt.Data == "" || evt.Data == "[DONE]" {
			modified = append(modified, evt)
			continue
		}

		var chunk map[string]json.RawMessage
		if err := json.Unmarshal([]byte(evt.Data), &chunk); err != nil {
			modified = append(modified, evt)
			continue
		}

		// Parse choices to filter tool_calls and modify finish_reason.
		choicesRaw, hasChoices := chunk["choices"]
		if !hasChoices {
			modified = append(modified, evt)
			continue
		}

		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(choicesRaw, &choices); err != nil || len(choices) == 0 {
			modified = append(modified, evt)
			continue
		}

		choice := choices[0]
		deltaRaw, hasDelta := choice["delta"]
		if !hasDelta {
			modified = append(modified, evt)
			continue
		}

		var delta map[string]json.RawMessage
		if err := json.Unmarshal(deltaRaw, &delta); err != nil {
			modified = append(modified, evt)
			continue
		}

		// Filter blocked tool_calls from the delta.
		if tcRaw, hasTC := delta["tool_calls"]; hasTC {
			var toolCalls []map[string]json.RawMessage
			if err := json.Unmarshal(tcRaw, &toolCalls); err == nil {
				var kept []map[string]json.RawMessage
				for _, tc := range toolCalls {
					var idx int
					if idxRaw, ok := tc["index"]; ok {
						json.Unmarshal(idxRaw, &idx)
					}
					if !blockedIndexes[idx] {
						kept = append(kept, tc)
					}
				}
				if len(kept) == 0 {
					delete(delta, "tool_calls")
				} else {
					keptJSON, _ := json.Marshal(kept)
					delta["tool_calls"] = keptJSON
				}
			}
		}

		// Change finish_reason if all tool calls were blocked.
		// Only change to "stop" if every tool call was blocked (partial blocking
		// keeps "tool_calls" — design doc Section 7.4).
		if frRaw, hasFR := choice["finish_reason"]; hasFR {
			var fr string
			if err := json.Unmarshal(frRaw, &fr); err == nil && fr == "tool_calls" {
				if allOpenAIToolsBlocked(events, blockedIndexes) {
					choice["finish_reason"] = json.RawMessage(`"stop"`)
				}
			}
		}

		// Rebuild the event.
		deltaJSON, _ := json.Marshal(delta)
		choice["delta"] = deltaJSON
		choicesJSON, _ := json.Marshal([]map[string]json.RawMessage{choice})
		chunk["choices"] = choicesJSON
		rebuiltJSON, _ := json.Marshal(chunk)
		modified = append(modified, SSEEvent{Event: evt.Event, Data: string(rebuiltJSON)})
	}

	// Inject block notice as a final content delta before [DONE].
	if len(blockMessages) > 0 {
		notice := buildBlockNoticeText(blockMessages)
		noticeEvt := buildOpenAIContentDelta(notice)
		// Insert before the last event ([DONE]).
		if len(modified) > 0 && modified[len(modified)-1].Data == "[DONE]" {
			tail := modified[len(modified)-1]
			modified = append(modified[:len(modified)-1], noticeEvt, tail)
		} else {
			modified = append(modified, noticeEvt)
		}
	}

	return modified
}

// reindexEvent creates a new SSE event with the index field remapped.
func reindexEvent(evt SSEEvent, oldIndex int, indexMap map[int]int) SSEEvent {
	newIdx, ok := indexMap[oldIndex]
	if !ok || newIdx == oldIndex {
		return evt // No remapping needed.
	}

	// Parse, modify index, re-serialize.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(evt.Data), &raw); err != nil {
		return evt
	}

	raw["index"] = json.RawMessage(fmt.Sprintf("%d", newIdx))
	data, err := json.Marshal(raw)
	if err != nil {
		return evt
	}
	return SSEEvent{Event: evt.Event, Data: string(data)}
}

// rewriteStopReason modifies a message_delta event to use a different stop_reason.
func rewriteStopReason(evt SSEEvent, newReason string) SSEEvent {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(evt.Data), &raw); err != nil {
		return evt
	}

	if deltaRaw, ok := raw["delta"]; ok {
		var delta map[string]json.RawMessage
		if err := json.Unmarshal(deltaRaw, &delta); err == nil {
			delta["stop_reason"] = json.RawMessage(fmt.Sprintf(`"%s"`, newReason))
			newDelta, _ := json.Marshal(delta)
			raw["delta"] = newDelta
		}
	}

	data, err := json.Marshal(raw)
	if err != nil {
		return evt
	}
	return SSEEvent{Event: evt.Event, Data: string(data)}
}

// buildTextBlockEvents generates the SSE events for a new text content block.
// Used to inject the "[CtrlAI] Blocked: ..." notice into the stream.
func buildTextBlockEvents(index int, text string) []SSEEvent {
	startData, _ := json.Marshal(map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": map[string]any{"type": "text", "text": ""},
	})

	deltaData, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})

	stopData, _ := json.Marshal(map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})

	return []SSEEvent{
		{Event: "content_block_start", Data: string(startData)},
		{Event: "content_block_delta", Data: string(deltaData)},
		{Event: "content_block_stop", Data: string(stopData)},
	}
}

// buildOpenAIContentDelta generates an OpenAI delta chunk with content text.
func buildOpenAIContentDelta(text string) SSEEvent {
	chunk := map[string]any{
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"content": "\n\n" + text,
				},
				"finish_reason": nil,
			},
		},
	}
	data, _ := json.Marshal(chunk)
	return SSEEvent{Data: string(data)}
}

// buildBlockNoticeText creates the block notice message.
// Format: "[CtrlAI] Blocked: <message> (rule: <name>)"
func buildBlockNoticeText(messages []string) string {
	if len(messages) == 1 {
		return messages[0]
	}
	result := "[CtrlAI] Multiple tool calls blocked:\n"
	for _, m := range messages {
		result += "  - " + m + "\n"
	}
	return result
}

// allToolsBlocked checks if every tool_use block in the event stream was blocked.
func allToolsBlocked(events []SSEEvent, blockedIndexes map[int]bool) bool {
	for _, evt := range events {
		if evt.Event != "content_block_start" {
			continue
		}
		var start struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &start); err != nil {
			continue
		}
		if start.ContentBlock.Type == "tool_use" && !blockedIndexes[start.Index] {
			return false // At least one tool_use was allowed.
		}
	}
	return true
}

// allOpenAIToolsBlocked checks if every tool call in an OpenAI SSE stream was
// blocked. Scans delta chunks for tool_calls with unique indexes and compares
// against the blocked set.
func allOpenAIToolsBlocked(events []SSEEvent, blockedIndexes map[int]bool) bool {
	seenIndexes := make(map[int]bool)
	for _, evt := range events {
		if evt.Data == "" || evt.Data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index int `json:"index"`
					} `json:"tool_calls,omitempty"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		for _, tc := range chunk.Choices[0].Delta.ToolCalls {
			seenIndexes[tc.Index] = true
		}
	}
	// All blocked if every seen tool call index is in the blocked set.
	for idx := range seenIndexes {
		if !blockedIndexes[idx] {
			return false
		}
	}
	return len(seenIndexes) > 0
}
