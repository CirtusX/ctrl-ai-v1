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
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // JSON string (OpenAI, Moonshot, Qwen, MiniMax) or JSON object (Zhipu/GLM sometimes).
}

// extractOpenAI parses tool_calls from an OpenAI-compatible Chat Completions API response.
// Returns a ToolCall for each entry in choices[0].message.tool_calls[].
//
// Handles all OpenAI-compatible providers:
//   - OpenAI, Moonshot/Kimi, Qwen, MiniMax: arguments is a JSON string
//   - Zhipu/GLM: arguments may be a JSON string OR a JSON object
//
// We detect the type at runtime and handle both cases.
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
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Index: i,
		}

		// Parse arguments — could be a JSON string (standard OpenAI) or
		// a JSON object (Zhipu/GLM quirk). We detect by checking if the
		// raw bytes start with '"' (string) or '{' (object).
		call.RawJSON, call.Arguments = parseToolArguments(tc.Function.Arguments)

		calls = append(calls, call)
	}

	return calls
}

// parseToolArguments handles the arguments field which is normally a JSON string
// containing JSON (OpenAI, Moonshot, Qwen, MiniMax), but may be a direct JSON
// object (Zhipu/GLM quirk).
//
// Returns the raw JSON for arg_contains matching and the parsed map for rule matching.
func parseToolArguments(raw json.RawMessage) (json.RawMessage, map[string]any) {
	if len(raw) == 0 {
		return nil, nil
	}

	// Trim whitespace to detect the type reliably.
	trimmed := raw
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t' || trimmed[0] == '\n' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}

	if len(trimmed) == 0 {
		return nil, nil
	}

	switch trimmed[0] {
	case '"':
		// Standard case: arguments is a JSON string containing JSON.
		// e.g. "{\"command\": \"ls -la\"}"
		var argsStr string
		if err := json.Unmarshal(raw, &argsStr); err != nil {
			return raw, nil
		}

		// MiniMax quirk: sometimes returns an empty string "" instead of "{}"
		// for tools with no parameters. Treat as empty arguments.
		if argsStr == "" {
			return json.RawMessage("{}"), map[string]any{}
		}

		rawJSON := json.RawMessage(argsStr)

		// Try standard JSON parse first.
		var args map[string]any
		if err := json.Unmarshal([]byte(argsStr), &args); err == nil {
			return rawJSON, args
		}

		// Zhipu/GLM quirk: sometimes returns Python-style dict strings with
		// single quotes and True/False instead of true/false.
		// e.g. "{'command': 'ls', 'verbose': True}"
		// Attempt to fix common Python→JSON differences.
		if fixed, ok := tryFixPythonDict(argsStr); ok {
			return json.RawMessage(fixed), fixedToMap(fixed)
		}

		return rawJSON, nil

	case '{':
		// Zhipu/GLM quirk: arguments is a direct JSON object.
		// e.g. {"command": "ls -la"}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err == nil {
			return raw, args
		}
		return raw, nil

	default:
		// Unexpected format — return raw bytes as-is.
		return raw, nil
	}
}

// tryFixPythonDict attempts to convert a Python-style dict string to valid JSON.
// Handles: single quotes → double quotes, True/False → true/false, None → null.
// Returns the fixed string and true if it parses as valid JSON after fixing.
func tryFixPythonDict(s string) (string, bool) {
	// Only attempt fix if it looks like a Python dict (starts with {).
	if len(s) == 0 || s[0] != '{' {
		return "", false
	}

	// Simple character-level replacement. This is not a full Python parser
	// but handles the common cases seen from Zhipu/GLM.
	fixed := make([]byte, 0, len(s))
	inString := false
	stringChar := byte(0)

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if c == stringChar && (i == 0 || s[i-1] != '\\') {
				inString = false
				fixed = append(fixed, '"') // Close with double quote.
			} else {
				// Escape double quotes inside single-quoted strings.
				if c == '"' && stringChar == '\'' {
					fixed = append(fixed, '\\', '"')
				} else {
					fixed = append(fixed, c)
				}
			}
		} else {
			if c == '\'' {
				inString = true
				stringChar = '\''
				fixed = append(fixed, '"') // Open with double quote.
			} else if c == '"' {
				inString = true
				stringChar = '"'
				fixed = append(fixed, '"')
			} else {
				fixed = append(fixed, c)
			}
		}
	}

	result := string(fixed)
	// Replace Python booleans and None.
	// Use simple replacements — safe because we already handled string quoting.
	result = replacePythonKeywords(result)

	// Validate the result is valid JSON.
	if json.Valid([]byte(result)) {
		return result, true
	}
	return "", false
}

// replacePythonKeywords replaces Python True/False/None with JSON equivalents.
func replacePythonKeywords(s string) string {
	// We need to be careful to only replace these outside of strings.
	// For simplicity, we do naive replacement — the validation step after
	// will catch false positives.
	replacements := []struct{ old, new string }{
		{": True", ": true"},
		{": False", ": false"},
		{": None", ": null"},
		{",True", ",true"},
		{",False", ",false"},
		{",None", ",null"},
		{"[True", "[true"},
		{"[False", "[false"},
		{"[None", "[null"},
	}
	for _, r := range replacements {
		for {
			idx := indexOfSubstring(s, r.old)
			if idx == -1 {
				break
			}
			s = s[:idx] + r.new + s[idx+len(r.old):]
		}
	}
	return s
}

// indexOfSubstring returns the index of the first occurrence of sub in s,
// or -1 if not found.
func indexOfSubstring(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// fixedToMap parses a fixed JSON string into a map.
func fixedToMap(fixed string) map[string]any {
	var args map[string]any
	if err := json.Unmarshal([]byte(fixed), &args); err == nil {
		return args
	}
	return nil
}
