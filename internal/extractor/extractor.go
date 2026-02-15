// Package extractor parses LLM response bodies and extracts tool calls.
//
// Supports two API formats:
//   - Anthropic Messages API: tool calls are in content[].type="tool_use" blocks
//   - OpenAI Chat Completions API: tool calls are in choices[0].message.tool_calls[]
//
// Tool names are stored as-is (preserving original case). Case-insensitive
// matching happens in the engine package during rule evaluation.
//
// See design doc Section 6.1 for the ToolCall struct definition.
package extractor

import "encoding/json"

// APIType identifies which LLM provider API format to parse.
// Determined from the URL path, not from guessing.
type APIType int

const (
	// APITypeAnthropic handles /v1/messages responses.
	APITypeAnthropic APIType = iota
	// APITypeOpenAI handles /v1/chat/completions responses.
	APITypeOpenAI
	// APITypeUnknown is for unrecognized API paths — passed through
	// without tool inspection.
	APITypeUnknown
)

// ToolCall represents a single tool invocation extracted from an LLM response.
// Both Anthropic (content blocks) and OpenAI (tool_calls array) responses
// are normalized into this common struct for rule evaluation.
//
// Design doc Section 6.1:
//
//	type ToolCall struct {
//	    ID        string          // "toolu_01..." or "call_abc..."
//	    Name      string          // "exec", "read", "write", "edit", etc.
//	    Arguments json.RawMessage // raw JSON of the tool arguments
//	    Index     int             // position in content array
//	}
type ToolCall struct {
	ID        string            // Tool call ID (provider-specific format).
	Name      string            // Tool name as returned by the LLM.
	Arguments map[string]any    // Parsed arguments for rule matching.
	RawJSON   json.RawMessage   // Raw argument JSON for arg_contains matching.
	Index     int               // Position in the response content array.
}

// Extract parses tool calls from a non-streaming response body.
// Dispatches to the appropriate parser based on API type.
// Returns an empty slice if no tool calls are found or apiType is unknown.
func Extract(body []byte, apiType APIType) []ToolCall {
	switch apiType {
	case APITypeAnthropic:
		return extractAnthropic(body)
	case APITypeOpenAI:
		return extractOpenAI(body)
	default:
		return nil
	}
}

// RequestMeta holds metadata extracted from the request body.
// Used for audit logging and agent registry updates.
// The request body is read once and these fields are pulled out;
// the body itself is forwarded to the upstream LLM unchanged.
type RequestMeta struct {
	Model  string   // The model name from the request body.
	Tools  []string // Tool names available to the LLM (from "tools" array).
	Stream bool     // Whether the request asks for streaming (SSE).
}

// ExtractRequestMeta parses metadata from the request body.
// Only reads the fields we need — does not modify the body.
func ExtractRequestMeta(body []byte, apiType APIType) RequestMeta {
	var meta RequestMeta

	// Both Anthropic and OpenAI use "model" and "stream" at the top level.
	var raw struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []struct {
			Name     string `json:"name"`               // Anthropic format.
			Function *struct {
				Name string `json:"name"`
			} `json:"function,omitempty"`               // OpenAI format.
		} `json:"tools"`
	}

	if err := json.Unmarshal(body, &raw); err != nil {
		return meta
	}

	meta.Model = raw.Model
	meta.Stream = raw.Stream

	for _, t := range raw.Tools {
		if t.Name != "" {
			meta.Tools = append(meta.Tools, t.Name)
		} else if t.Function != nil && t.Function.Name != "" {
			meta.Tools = append(meta.Tools, t.Function.Name)
		}
	}

	return meta
}
