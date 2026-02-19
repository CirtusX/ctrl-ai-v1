package proxy

import (
	"encoding/json"

	"github.com/ctrlai/ctrlai/internal/extractor"
)

// reconstructOpenAIResponses builds a BufferedMessage from OpenAI Responses API
// SSE events. The Responses API streaming format uses typed events:
//
//	event: response.output_item.added
//	data: {"type":"function_call","call_id":"call_abc","name":"exec","arguments":""}
//
//	event: response.function_call_arguments.delta
//	data: {"call_id":"call_abc","delta":"{\"command\":"}
//
//	event: response.function_call_arguments.done
//	data: {"call_id":"call_abc","arguments":"{\"command\": \"ls\"}"}
//
//	event: response.completed
//	data: {"id":"resp_abc","status":"completed",...}
//
// We accumulate function call arguments from delta events, keyed by call_id.
func reconstructOpenAIResponses(events []SSEEvent) *BufferedMessage {
	msg := &BufferedMessage{}

	// Track function calls by call_id, accumulating argument fragments.
	type fcAccum struct {
		CallID    string
		Name      string
		Arguments string
		Index     int
	}
	funcCalls := make(map[string]*fcAccum)
	nextIndex := 0

	for _, evt := range events {
		if evt.Data == "" {
			continue
		}

		switch evt.Event {
		case "response.output_item.added":
			// A new output item was added. If it's a function_call, start tracking it.
			var item struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &item); err != nil {
				continue
			}
			if item.Type == "function_call" {
				accum := &fcAccum{
					CallID: item.CallID,
					Name:   item.Name,
					Index:  nextIndex,
				}
				funcCalls[item.CallID] = accum
				nextIndex++
			}

		case "response.function_call_arguments.delta":
			// Incremental argument fragment for a function call.
			var delta struct {
				CallID string `json:"call_id"`
				Delta  string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &delta); err != nil {
				continue
			}
			if accum, ok := funcCalls[delta.CallID]; ok {
				accum.Arguments += delta.Delta
			}

		case "response.function_call_arguments.done":
			// Final complete arguments for a function call.
			var done struct {
				CallID    string `json:"call_id"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &done); err != nil {
				continue
			}
			if accum, ok := funcCalls[done.CallID]; ok {
				// Use the complete arguments from the done event (more reliable than
				// accumulated deltas which may have gaps on timeout).
				accum.Arguments = done.Arguments
			}

		case "response.completed":
			// Extract final status.
			var completed struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &completed); err == nil {
				msg.StopReason = completed.Status
			}
		}
	}

	// Convert accumulated function calls to ToolCall structs.
	// Sort by index to maintain original order.
	sorted := make([]*fcAccum, nextIndex)
	for _, accum := range funcCalls {
		if accum.Index < len(sorted) {
			sorted[accum.Index] = accum
		}
	}

	for _, accum := range sorted {
		if accum == nil {
			continue
		}
		tc := extractor.ToolCall{
			ID:      accum.CallID,
			Name:    accum.Name,
			RawJSON: json.RawMessage(accum.Arguments),
			Index:   accum.Index,
		}
		if accum.Arguments != "" {
			var args map[string]any
			if err := json.Unmarshal([]byte(accum.Arguments), &args); err == nil {
				tc.Arguments = args
			}
		}
		msg.ToolCalls = append(msg.ToolCalls, tc)
	}

	return msg
}

// buildModifiedOpenAIResponsesStream rebuilds an OpenAI Responses API SSE stream
// with blocked function_call items removed.
//
// Strategy:
//  1. Skip all events related to blocked function calls (matched by call_id)
//  2. Inject a text output item with the block notice before response.completed
//  3. Pass through all other events unchanged
func buildModifiedOpenAIResponsesStream(events []SSEEvent, blocked []extractor.ToolCall, blockMessages []string) []SSEEvent {
	// Build set of blocked call IDs.
	blockedCallIDs := make(map[string]bool)
	for _, tc := range blocked {
		blockedCallIDs[tc.ID] = true
	}

	var modified []SSEEvent

	for _, evt := range events {
		// Check if this event belongs to a blocked function call by extracting call_id.
		if isBlockedResponsesEvent(evt, blockedCallIDs) {
			continue
		}

		// Inject block notice before the response.completed event.
		if evt.Event == "response.completed" && len(blockMessages) > 0 {
			notice := buildBlockNoticeText(blockMessages)
			modified = append(modified, buildResponsesNoticeEvent(notice))
		}

		modified = append(modified, evt)
	}

	return modified
}

// isBlockedResponsesEvent checks if an SSE event belongs to a blocked function call.
func isBlockedResponsesEvent(evt SSEEvent, blockedCallIDs map[string]bool) bool {
	switch evt.Event {
	case "response.output_item.added":
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &item); err == nil {
			if item.Type == "function_call" && blockedCallIDs[item.CallID] {
				return true
			}
		}
	case "response.function_call_arguments.delta",
		"response.function_call_arguments.done":
		var delta struct {
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &delta); err == nil {
			if blockedCallIDs[delta.CallID] {
				return true
			}
		}
	case "response.output_item.done":
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &item); err == nil {
			if item.Type == "function_call" && blockedCallIDs[item.CallID] {
				return true
			}
		}
	}
	return false
}

// buildResponsesNoticeEvent creates an SSE event that injects a text output item
// with the block notice into a Responses API stream.
func buildResponsesNoticeEvent(text string) SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type": "message",
		"content": []map[string]any{
			{"type": "output_text", "text": text},
		},
	})
	return SSEEvent{
		Event: "response.output_item.added",
		Data:  string(data),
	}
}
