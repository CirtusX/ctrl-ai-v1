package extractor

import "encoding/json"

// openaiResponse models the OpenAI Chat Completions API response body.
// We only parse the fields we need for tool call extraction.
//
// Full response structure from design doc Section 5.1:
//
//	{
//	  "choices": [{
//	    "message": {
//	      "role": "assistant",
//	      "content": "...",
//	      "tool_calls": [{
//	        "id": "call_abc123",
//	        "type": "function",
//	        "function": { "name": "exec", "arguments": "{...}" }
//	      }]
//	    },
//	    "finish_reason": "tool_calls"
//	  }]
//	}
type openaiResponse struct {
	Choices []openaiChoice `json:"choices"`
}

type openaiChoice struct {
	Message openaiMessage `json:"message"`
}

type openaiMessage struct {
	ToolCalls []openaiToolCall `json:"tool_calls"`
}

type openaiToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openaiFunction `json:"function"`
}

type openaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string, not object.
}

// extractOpenAI parses tool_calls from an OpenAI Chat Completions API response.
// Returns a ToolCall for each entry in choices[0].message.tool_calls[].
//
// Note: OpenAI's arguments field is a JSON string (not a JSON object),
// so we parse it twice: once from the response, once to get the actual
// argument map.
func extractOpenAI(body []byte) []ToolCall {
	var resp openaiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}

	// OpenAI responses always have at most one choice for tool calls.
	if len(resp.Choices) == 0 {
		return nil
	}

	msg := resp.Choices[0].Message
	if len(msg.ToolCalls) == 0 {
		return nil
	}

	var calls []ToolCall
	for i, tc := range msg.ToolCalls {
		call := ToolCall{
			ID:      tc.ID,
			Name:    tc.Function.Name,
			RawJSON: json.RawMessage(tc.Function.Arguments),
			Index:   i,
		}

		// Parse the arguments JSON string into a map for rule matching.
		if tc.Function.Arguments != "" {
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
				call.Arguments = args
			}
		}

		calls = append(calls, call)
	}

	return calls
}
