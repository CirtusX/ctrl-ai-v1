// Package proxy implements the transparent HTTP proxy that sits between
// the AI agent SDK and the LLM provider.
//
// The proxy:
//  1. Parses the URL to extract provider, agent ID, and API path
//  2. Checks the kill switch before forwarding
//  3. Forwards the request to the upstream LLM
//  4. Buffers the response (SSE or non-streaming)
//  5. Extracts tool_use blocks
//  6. Evaluates each tool call against the rule engine
//  7. Modifies blocked responses (strips tool_use, changes stop_reason)
//  8. Sends the response (modified or original) back to the SDK
//
// See design doc Sections 4-7 and 13 for the full data flow.
package proxy

import (
	"fmt"
	"strings"

	"github.com/ctrlai/ctrlai/internal/extractor"
)

// RouteInfo holds the parsed components of an incoming proxy request URL.
//
// URL format: /provider/{providerKey}/agent/{agentId}/{apiPath...}
//
// Examples:
//
//	/provider/anthropic/agent/main/v1/messages
//	  → ProviderKey="anthropic", AgentID="main", APIPath="/v1/messages", APIType=Anthropic
//
//	/provider/openai/v1/chat/completions
//	  → ProviderKey="openai", AgentID="default", APIPath="/v1/chat/completions", APIType=OpenAI
//
// Design doc Section 12:
//
//	type RouteInfo struct {
//	    ProviderKey string
//	    AgentID     string
//	    APIPath     string
//	    APIType     APIType
//	}
type RouteInfo struct {
	ProviderKey string
	AgentID     string
	APIPath     string
	APIType     extractor.APIType
}

// ParseRoute parses a request URL path into its route components.
//
// Path format: /provider/{providerKey}/agent/{agentId}/{apiPath...}
// The /agent/{agentId} segment is optional — defaults to "default".
//
// API type detection from design doc Section 2:
//
//	/v1/messages           → Anthropic
//	/v1/chat/completions   → OpenAI
//	/v1/responses          → OpenAI
//	anything else          → Unknown (passed through without inspection)
func ParseRoute(path string) (RouteInfo, error) {
	// Strip leading slash and split into segments.
	path = strings.TrimPrefix(path, "/")
	parts := strings.Split(path, "/")

	// Minimum: "provider" / {key} / {apiPath...}
	// Must start with "provider".
	if len(parts) < 2 || parts[0] != "provider" {
		return RouteInfo{}, fmt.Errorf("invalid path: must start with /provider/")
	}

	route := RouteInfo{
		ProviderKey: parts[1],
		AgentID:     "default", // Default if no /agent/ segment.
	}

	// Parse remaining segments after provider key.
	// Two cases:
	//   /provider/{key}/agent/{id}/{apiPath...}
	//   /provider/{key}/{apiPath...}
	remaining := parts[2:]

	if len(remaining) >= 2 && remaining[0] == "agent" {
		// Has explicit agent ID.
		route.AgentID = remaining[1]
		remaining = remaining[2:]
	}

	// Whatever's left is the API path (joined with /).
	if len(remaining) > 0 {
		route.APIPath = "/" + strings.Join(remaining, "/")
	}

	// Detect API type from the path — not from guessing.
	// Design doc Section 2: API Type Detection table.
	route.APIType = detectAPIType(route.APIPath)

	return route, nil
}

// detectAPIType determines the LLM API format from the API path.
// This is deterministic — no guessing from headers or body.
//
// Design doc Section 2:
//
//	/v1/messages           → Anthropic
//	/v1/chat/completions   → OpenAI (completions)
//	/v1/responses          → OpenAI (responses)
//	anything else          → Unknown (pass through without inspection)
func detectAPIType(apiPath string) extractor.APIType {
	switch {
	case strings.HasPrefix(apiPath, "/v1/messages"):
		return extractor.APITypeAnthropic
	case strings.HasPrefix(apiPath, "/v1/chat/completions"):
		return extractor.APITypeOpenAI
	case strings.HasPrefix(apiPath, "/v1/responses"):
		return extractor.APITypeOpenAI
	default:
		return extractor.APITypeUnknown
	}
}
