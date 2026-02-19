package extractor

import "encoding/json"

// openaiResponsesResponse models the OpenAI Responses API response body.
// This is a completely different format from Chat Completions:
//
//	{
//	  "id": "resp_abc123",
//	  "output": [
//	    { "type": "message", "content": [{"type": "output_text", "text": "..."}] },
//	    { "type": "function_call", "id": "fc_abc", "call_id": "call_abc",
//	      "name": "exec", "arguments": "{\"command\": \"ls\"}" },
//	    { "type": "function_call", "id": "fc_def", "call_id": "call_def",
//	      "name": "read", "arguments": "{\"path\": \"/etc/passwd\"}" }
//	  ],
//	  "status": "completed"
//	}
//
// Key differences from Chat Completions:
//   - Tool calls are in output[] with type="function_call", not in choices[].message.tool_calls[]
//   - Each function_call is a top-level output item, not nested under a message
//   - arguments is a JSON string (same as Chat Completions standard)
//   - call_id is the tool call ID (not "id" which is the output item ID)
//   - status replaces finish_reason ("completed", "incomplete", "failed")
type openaiResponsesResponse struct {
	ID     string                    `json:"id"`
	Output []openaiResponsesOutput   `json:"output"`
	Status string                    `json:"status"`
}

type openaiResponsesOutput struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// extractOpenAIResponses parses function_call items from an OpenAI Responses API response.
// Returns a ToolCall for each output item with type="function_call".
//
// The Responses API uses a flat output array where function calls sit alongside
// message outputs, unlike Chat Completions where they're nested under choices[0].message.
func extractOpenAIResponses(body []byte) []ToolCall {
	var resp openaiResponsesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}

	var calls []ToolCall
	for i, item := range resp.Output {
		if item.Type != "function_call" {
			continue
		}

		tc := ToolCall{
			ID:    item.CallID,
			Name:  item.Name,
			Index: i,
		}

		// If call_id is empty, fall back to id (some early API versions).
		if tc.ID == "" {
			tc.ID = item.ID
		}

		// Parse arguments — same format as Chat Completions (JSON string),
		// but may also be a direct JSON object in some edge cases.
		tc.RawJSON, tc.Arguments = parseToolArguments(item.Arguments)

		calls = append(calls, tc)
	}

	return calls
}
