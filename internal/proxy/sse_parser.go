package proxy

import (
	"bufio"
	"io"
	"strings"
)

// SSEEvent represents a single Server-Sent Event.
// Both Anthropic and OpenAI use SSE for streaming, but with different
// event naming conventions:
//
//	Anthropic: "event: <type>\ndata: <json>\n\n"
//	           event types: message_start, content_block_start, content_block_delta,
//	                       content_block_stop, message_delta, message_stop, ping
//
//	OpenAI:    "data: <json>\n\n" (no event: line)
//	           stream terminates with "data: [DONE]"
type SSEEvent struct {
	Event string // Event type (Anthropic) or empty string (OpenAI).
	Data  string // JSON payload or "[DONE]".
}

// parseSSEStream reads SSE events from a reader until EOF or stream termination.
// Handles both Anthropic and OpenAI SSE formats.
//
// Anthropic format:
//
//	event: content_block_start
//	data: {"type":"content_block_start","index":0,...}
//	<blank line>
//
// OpenAI format:
//
//	data: {"id":"chatcmpl-abc",...}
//	<blank line>
//
// Design doc Section 5.2-5.3:
//   - Skip ping events (Anthropic keep-alive, no content)
//   - Terminate on "event: message_stop" (Anthropic) or "data: [DONE]" (OpenAI)
func parseSSEStream(reader io.Reader) ([]SSEEvent, error) {
	var events []SSEEvent
	scanner := bufio.NewScanner(reader)
	// Large buffer for potentially huge JSON payloads (e.g. thinking blocks).
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var currentEvent string
	var currentData string

	for scanner.Scan() {
		line := scanner.Text()

		// Blank line = end of event.
		if line == "" {
			if currentData != "" {
				// Skip ping events — they're Anthropic keep-alives with no
				// content payload (design doc Section 5.3).
				if currentEvent != "ping" {
					events = append(events, SSEEvent{
						Event: currentEvent,
						Data:  currentData,
					})
				}

				// Check for stream termination.
				if currentEvent == "message_stop" || currentData == "[DONE]" {
					break
				}
			}
			currentEvent = ""
			currentData = ""
			continue
		}

		// Parse "event: <type>" line.
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		// Parse "data: <payload>" line.
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if currentData == "" {
				currentData = data
			} else {
				// Multi-line data — join with newline.
				currentData += "\n" + data
			}
			continue
		}

		// Ignore comment lines (starting with ':') and unknown lines.
	}

	if err := scanner.Err(); err != nil {
		return events, err
	}

	return events, nil
}
