package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/ctrlai/ctrlai/internal/extractor"
)

// BufferedMessage holds the reconstructed full message from a buffered
// SSE stream. After buffering all events until message_stop/[DONE],
// we reconstruct the complete message to extract tool calls.
//
// This is the intermediate representation between raw SSE events and
// the ToolCall structs that the engine evaluates.
type BufferedMessage struct {
	// ContentBlocks holds the reconstructed content blocks (Anthropic).
	// Each block has a type ("thinking", "text", "tool_use") and its
	// accumulated content.
	ContentBlocks []ContentBlock

	// ToolCalls holds the extracted tool calls (both APIs).
	ToolCalls []extractor.ToolCall

	// StopReason is the final stop_reason/finish_reason from the stream.
	StopReason string
}

// ContentBlock represents a reconstructed Anthropic content block.
// Built by accumulating deltas across multiple SSE events.
type ContentBlock struct {
	Index     int             `json:"index"`
	Type      string          `json:"type"`       // "thinking", "text", "tool_use"
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`        // tool_use only
	Name      string          `json:"name,omitempty"`      // tool_use only
	InputJSON string          `json:"-"`                    // accumulated input_json_delta
}

// bufferAll reads all SSE events from the response body with a timeout.
// Returns the raw events for replay and the reconstructed message for
// tool call evaluation.
//
// Design doc Section 5.4: Buffer-Then-Forward strategy.
// The SDK waits for SSE events. Buffering adds latency equal to the LLM's
// full response generation time (typically 1-5 seconds). The alternative
// (no buffering) means zero security.
//
// Timeout: if buffering exceeds timeoutMs, return what we have.
// This prevents the proxy from hanging on stuck/slow LLM responses.
func bufferAll(body io.ReadCloser, timeoutMs int, apiType extractor.APIType) ([]SSEEvent, *BufferedMessage, error) {
	// Set up timeout.
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// Parse SSE events with a timeout wrapper.
	type result struct {
		events []SSEEvent
		err    error
	}
	ch := make(chan result, 1)

	go func() {
		events, err := parseSSEStream(body)
		ch <- result{events, err}
	}()

	var events []SSEEvent
	select {
	case r := <-ch:
		events = r.events
		if r.err != nil {
			slog.Warn("SSE parsing error (using partial events)", "error", r.err)
		}
	case <-time.After(timeout):
		slog.Warn("SSE buffer timeout, flushing partial events", "timeout_ms", timeoutMs)
		// The goroutine may still be reading. We'll use whatever events
		// we have so far. The goroutine will eventually finish and the
		// channel will be garbage collected.
		select {
		case r := <-ch:
			events = r.events
		default:
			// No events yet — return empty.
		}
	}

	// Reconstruct the full message from the buffered events.
	msg := reconstruct(events, apiType)

	return events, msg, nil
}

// reconstruct builds a BufferedMessage from raw SSE events.
// Dispatches to API-specific reconstruction logic.
func reconstruct(events []SSEEvent, apiType extractor.APIType) *BufferedMessage {
	switch apiType {
	case extractor.APITypeAnthropic:
		return reconstructAnthropic(events)
	case extractor.APITypeOpenAI:
		return reconstructOpenAI(events)
	default:
		return &BufferedMessage{}
	}
}

// reconstructAnthropic builds a message from Anthropic SSE events.
// Tracks content blocks by index, accumulating deltas for each block.
//
// Event flow from design doc Section 5.2:
//
//	message_start → content_block_start → content_block_delta* →
//	content_block_stop → ... → message_delta → message_stop
//
// Delta types:
//   - text_delta: accumulate into text block
//   - thinking_delta: accumulate into thinking block
//   - signature_delta: set signature on thinking block
//   - input_json_delta: accumulate into tool_use block's input JSON
func reconstructAnthropic(events []SSEEvent) *BufferedMessage {
	msg := &BufferedMessage{}
	blocks := make(map[int]*ContentBlock)

	for _, evt := range events {
		if evt.Data == "" {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(evt.Data), &raw); err != nil {
			continue
		}

		eventType := unquote(raw["type"])

		switch eventType {
		case "content_block_start":
			var start struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type  string `json:"type"`
					ID    string `json:"id,omitempty"`
					Name  string `json:"name,omitempty"`
					Text  string `json:"text,omitempty"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &start); err != nil {
				continue
			}
			block := &ContentBlock{
				Index: start.Index,
				Type:  start.ContentBlock.Type,
				ID:    start.ContentBlock.ID,
				Name:  start.ContentBlock.Name,
				Text:  start.ContentBlock.Text,
			}
			blocks[start.Index] = block

		case "content_block_delta":
			var delta struct {
				Index int `json:"index"`
				Delta struct {
					Type       string `json:"type"`
					Text       string `json:"text,omitempty"`
					Thinking   string `json:"thinking,omitempty"`
					Signature  string `json:"signature,omitempty"`
					PartialJSON string `json:"partial_json,omitempty"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &delta); err != nil {
				continue
			}
			block, ok := blocks[delta.Index]
			if !ok {
				continue
			}
			switch delta.Delta.Type {
			case "text_delta":
				block.Text += delta.Delta.Text
			case "thinking_delta":
				block.Thinking += delta.Delta.Thinking
			case "signature_delta":
				block.Signature += delta.Delta.Signature
			case "input_json_delta":
				block.InputJSON += delta.Delta.PartialJSON
			}

		case "message_delta":
			var md struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(evt.Data), &md); err == nil {
				msg.StopReason = md.Delta.StopReason
			}
		}
	}

	// Convert blocks map to ordered slice and extract tool calls.
	maxIdx := -1
	for idx := range blocks {
		if idx > maxIdx {
			maxIdx = idx
		}
	}

	for i := 0; i <= maxIdx; i++ {
		block, ok := blocks[i]
		if !ok {
			continue
		}
		msg.ContentBlocks = append(msg.ContentBlocks, *block)

		// Extract tool calls from tool_use blocks.
		if block.Type == "tool_use" {
			tc := extractor.ToolCall{
				ID:    block.ID,
				Name:  block.Name,
				Index: block.Index,
			}
			if block.InputJSON != "" {
				tc.RawJSON = json.RawMessage(block.InputJSON)
				var args map[string]any
				if err := json.Unmarshal([]byte(block.InputJSON), &args); err == nil {
					tc.Arguments = args
				}
			}
			msg.ToolCalls = append(msg.ToolCalls, tc)
		}
	}

	return msg
}

// reconstructOpenAI builds a message from OpenAI SSE events.
// Accumulates tool_calls from delta chunks across multiple events.
//
// OpenAI delta format from design doc Section 5.2:
//
//	data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","function":{"name":"exec","arguments":""}}]}}]}
//	data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}
//	data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}
//	data: [DONE]
func reconstructOpenAI(events []SSEEvent) *BufferedMessage {
	msg := &BufferedMessage{}

	// Track tool calls by index, accumulating argument fragments.
	type toolCallAccum struct {
		ID        string
		Name      string
		Arguments string
		Index     int
	}
	toolCalls := make(map[int]*toolCallAccum)

	for _, evt := range events {
		if evt.Data == "" || evt.Data == "[DONE]" {
			continue
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id,omitempty"`
						Function *struct {
							Name      string `json:"name,omitempty"`
							Arguments string `json:"arguments,omitempty"`
						} `json:"function,omitempty"`
					} `json:"tool_calls,omitempty"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}

		if err := json.Unmarshal([]byte(evt.Data), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]

		// Accumulate tool call data from deltas.
		for _, tc := range choice.Delta.ToolCalls {
			accum, ok := toolCalls[tc.Index]
			if !ok {
				accum = &toolCallAccum{Index: tc.Index}
				toolCalls[tc.Index] = accum
			}
			if tc.ID != "" {
				accum.ID = tc.ID
			}
			if tc.Function != nil {
				if tc.Function.Name != "" {
					accum.Name = tc.Function.Name
				}
				accum.Arguments += tc.Function.Arguments
			}
		}

		// Capture finish reason from the final chunk.
		if choice.FinishReason != nil {
			msg.StopReason = *choice.FinishReason
		}
	}

	// Convert accumulated tool calls to ToolCall structs.
	for i := 0; i < len(toolCalls); i++ {
		accum, ok := toolCalls[i]
		if !ok {
			continue
		}
		tc := extractor.ToolCall{
			ID:      accum.ID,
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

// unquote removes surrounding quotes from a JSON string value.
// Used to extract the "type" field from raw JSON without full parsing.
func unquote(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
