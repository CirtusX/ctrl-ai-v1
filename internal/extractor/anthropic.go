package extractor

import "encoding/json"

// anthropicResponse models the Anthropic Messages API response body.
// We only parse the fields we need for tool call extraction.
//
// Full response structure from design doc Section 5.1:
//
//	{
//	  "content": [
//	    { "type": "thinking", "thinking": "...", "signature": "..." },
//	    { "type": "text", "text": "..." },
//	    { "type": "tool_use", "id": "toolu_...", "name": "exec", "input": {...} }
//	  ],
//	  "stop_reason": "tool_use"
//	}
//
// We only extract "tool_use" blocks. "thinking" and "text" blocks are
// passed through unchanged — they are never evaluated against rules.
type anthropicResponse struct {
	Content []anthropicContentBlock `json:"content"`
}

// anthropicContentBlock represents one block in the content array.
// The Type field determines which other fields are populated:
//   - "text":     Text field
//   - "thinking": Thinking + Signature fields
//   - "tool_use": ID + Name + Input fields (what we extract)
type anthropicContentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// extractAnthropic parses tool_use blocks from an Anthropic Messages API response.
// Returns a ToolCall for each content block with type="tool_use".
//
// Case sensitivity note (design doc Section 6.3):
// Tool names are stored as-is. With OAuth tokens (sk-ant-oat prefix),
// tool names are PascalCase (Bash, Read, Write). With regular API keys,
// they're lowercase (bash, read, write). Case-insensitive matching
// happens in the engine, not here.
func extractAnthropic(body []byte) []ToolCall {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}

	var calls []ToolCall
	for i, block := range resp.Content {
		if block.Type != "tool_use" {
			continue
		}

		tc := ToolCall{
			ID:      block.ID,
			Name:    block.Name,
			RawJSON: block.Input,
			Index:   i,
		}

		// Parse the input JSON into a map for rule matching.
		// If parsing fails, keep RawJSON for arg_contains matching.
		if len(block.Input) > 0 {
			var args map[string]any
			if err := json.Unmarshal(block.Input, &args); err == nil {
				tc.Arguments = args
			}
		}

		calls = append(calls, tc)
	}

	return calls
}
