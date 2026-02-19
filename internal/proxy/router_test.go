package proxy

import (
	"testing"

	"github.com/ctrlai/ctrlai/internal/extractor"
)

func TestParseRoute(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantRoute RouteInfo
		wantErr   bool
	}{
		{
			name: "anthropic with agent",
			path: "/provider/anthropic/agent/main/v1/messages",
			wantRoute: RouteInfo{
				ProviderKey: "anthropic",
				AgentID:     "main",
				APIPath:     "/v1/messages",
				APIType:     extractor.APITypeAnthropic,
			},
		},
		{
			name: "openai with agent",
			path: "/provider/openai/agent/work/v1/chat/completions",
			wantRoute: RouteInfo{
				ProviderKey: "openai",
				AgentID:     "work",
				APIPath:     "/v1/chat/completions",
				APIType:     extractor.APITypeOpenAI,
			},
		},
		{
			name: "anthropic without agent (default)",
			path: "/provider/anthropic/v1/messages",
			wantRoute: RouteInfo{
				ProviderKey: "anthropic",
				AgentID:     "default",
				APIPath:     "/v1/messages",
				APIType:     extractor.APITypeAnthropic,
			},
		},
		{
			name: "openai responses API",
			path: "/provider/openai/v1/responses",
			wantRoute: RouteInfo{
				ProviderKey: "openai",
				AgentID:     "default",
				APIPath:     "/v1/responses",
				APIType:     extractor.APITypeOpenAIResponses,
			},
		},
		{
			name: "moonshot with agent",
			path: "/provider/moonshot/agent/main/v1/chat/completions",
			wantRoute: RouteInfo{
				ProviderKey: "moonshot",
				AgentID:     "main",
				APIPath:     "/v1/chat/completions",
				APIType:     extractor.APITypeOpenAI,
			},
		},
		{
			name: "qwen without agent",
			path: "/provider/qwen/v1/chat/completions",
			wantRoute: RouteInfo{
				ProviderKey: "qwen",
				AgentID:     "default",
				APIPath:     "/v1/chat/completions",
				APIType:     extractor.APITypeOpenAI,
			},
		},
		{
			name: "minimax with agent",
			path: "/provider/minimax/agent/work/v1/chat/completions",
			wantRoute: RouteInfo{
				ProviderKey: "minimax",
				AgentID:     "work",
				APIPath:     "/v1/chat/completions",
				APIType:     extractor.APITypeOpenAI,
			},
		},
		{
			name: "zhipu GLM non-standard path",
			path: "/provider/zhipu/agent/main/paas/v4/chat/completions",
			wantRoute: RouteInfo{
				ProviderKey: "zhipu",
				AgentID:     "main",
				APIPath:     "/paas/v4/chat/completions",
				APIType:     extractor.APITypeOpenAI,
			},
		},
		{
			name: "zhipu GLM without agent",
			path: "/provider/zhipu/paas/v4/chat/completions",
			wantRoute: RouteInfo{
				ProviderKey: "zhipu",
				AgentID:     "default",
				APIPath:     "/paas/v4/chat/completions",
				APIType:     extractor.APITypeOpenAI,
			},
		},
		{
			name: "unknown API type",
			path: "/provider/custom/agent/bot/v1/something",
			wantRoute: RouteInfo{
				ProviderKey: "custom",
				AgentID:     "bot",
				APIPath:     "/v1/something",
				APIType:     extractor.APITypeUnknown,
			},
		},
		{
			name:    "invalid path - no provider prefix",
			path:    "/invalid/path",
			wantErr: true,
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
		},
		{
			name:    "root only",
			path:    "/",
			wantErr: true,
		},
		{
			name: "provider key only",
			path: "/provider/anthropic",
			wantRoute: RouteInfo{
				ProviderKey: "anthropic",
				AgentID:     "default",
				APIPath:     "",
				APIType:     extractor.APITypeUnknown,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, err := ParseRoute(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if route.ProviderKey != tt.wantRoute.ProviderKey {
				t.Errorf("ProviderKey: expected %q, got %q", tt.wantRoute.ProviderKey, route.ProviderKey)
			}
			if route.AgentID != tt.wantRoute.AgentID {
				t.Errorf("AgentID: expected %q, got %q", tt.wantRoute.AgentID, route.AgentID)
			}
			if route.APIPath != tt.wantRoute.APIPath {
				t.Errorf("APIPath: expected %q, got %q", tt.wantRoute.APIPath, route.APIPath)
			}
			if route.APIType != tt.wantRoute.APIType {
				t.Errorf("APIType: expected %d, got %d", tt.wantRoute.APIType, route.APIType)
			}
		})
	}
}

func TestDetectAPIType(t *testing.T) {
	tests := []struct {
		path string
		want extractor.APIType
	}{
		{"/v1/messages", extractor.APITypeAnthropic},
		{"/v1/messages?beta=true", extractor.APITypeAnthropic},
		{"/v1/chat/completions", extractor.APITypeOpenAI},
		{"/v1/responses", extractor.APITypeOpenAIResponses},
		{"/paas/v4/chat/completions", extractor.APITypeOpenAI}, // Zhipu/GLM
		{"/v1/embeddings", extractor.APITypeUnknown},
		{"/v2/messages", extractor.APITypeUnknown},
		{"", extractor.APITypeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := detectAPIType(tt.path)
			if got != tt.want {
				t.Errorf("detectAPIType(%q) = %d, want %d", tt.path, got, tt.want)
			}
		})
	}
}
