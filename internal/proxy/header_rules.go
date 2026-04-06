package proxy

import (
	"encoding/base64"
	"log/slog"
	"net/http"

	"github.com/ctrlai/ctrlai/internal/engine"
)

// extractRuntimeRules parses the X-Ctrl-Rules header if present.
// The header value should be base64-encoded YAML containing rules.
// Returns parsed rules, or nil if header is missing or parsing fails.
//
// This allows per-org/per-request rule customization in enterprise deployments.
func extractRuntimeRules(r *http.Request) []engine.Rule {
	headerValue := r.Header.Get("X-Ctrl-Rules")
	if headerValue == "" {
		return nil
	}

	// Base64 decode
	yamlData, err := base64.StdEncoding.DecodeString(headerValue)
	if err != nil {
		slog.Warn("failed to decode X-Ctrl-Rules header", "error", err)
		return nil
	}

	// Parse YAML
	rules, _, err := engine.ParseRulesFromYAML(yamlData)
	if err != nil {
		slog.Warn("failed to parse runtime rules from header", "error", err)
		return nil
	}

	slog.Debug("loaded runtime rules from header", "count", len(rules))
	return rules
}
