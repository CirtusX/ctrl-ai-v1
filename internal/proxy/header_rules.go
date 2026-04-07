package proxy

import (
	"encoding/base64"
	"log/slog"
	"net/http"

	"github.com/ctrlai/ctrlai/internal/engine"
)

// extractRuntimeRules parses the X-Ctrl-Rules header if present.
// The header value should be base64-encoded YAML containing rules and builtin toggles.
// Returns fully merged rules (custom + enabled built-ins), or nil if header is missing or parsing fails.
//
// This allows per-org/per-request rule customization in enterprise deployments.
func extractRuntimeRules(r *http.Request) []engine.Rule {
	headerValue := r.Header.Get("X-Ctrl-Rules")
	if headerValue == "" {
		slog.Info("X-Ctrl-Rules header NOT found in request")
		return nil
	}

	slog.Info("X-Ctrl-Rules header found", "length", len(headerValue))

	// Base64 decode
	yamlData, err := base64.StdEncoding.DecodeString(headerValue)
	if err != nil {
		slog.Warn("failed to decode X-Ctrl-Rules header", "error", err)
		return nil
	}

	slog.Info("X-Ctrl-Rules header decoded successfully", "yaml_length", len(yamlData))
	slog.Info("Decoded YAML", "content", string(yamlData))

	// Parse YAML - returns custom rules and builtin toggles
	customRules, builtinToggles, err := engine.ParseRulesFromYAML(yamlData)
	if err != nil {
		slog.Warn("failed to parse runtime rules from header", "error", err)
		return nil
	}

	slog.Info("✅ Parsed runtime rules", "custom_count", len(customRules), "builtin_toggles", builtinToggles)

	// Merge built-in rules with toggles.
	// Rules explicitly set to false are excluded. Everything else is included.
	var mergedRules []engine.Rule

	allBuiltins, err := engine.GetAllBuiltinRules()
	if err != nil {
		slog.Warn("failed to get built-in rules", "error", err)
	} else {
		for _, rule := range allBuiltins {
			if builtinToggles != nil {
				enabled, exists := builtinToggles[rule.Name]
				if exists && !enabled {
					slog.Info("  Excluding built-in rule (disabled)", "name", rule.Name)
					continue
				}
			}
			// Include rule: either explicitly enabled, not in toggles, or no toggles at all
			slog.Info("  Including built-in rule", "name", rule.Name)
			mergedRules = append(mergedRules, rule)
		}
	}

	// Add custom rules after built-ins
	mergedRules = append(mergedRules, customRules...)

	slog.Info("✅ Merged runtime rules", "total_count", len(mergedRules))
	for i, rule := range mergedRules {
		slog.Info("  Merged rule", "index", i, "name", rule.Name, "action", rule.Action)
	}

	return mergedRules
}
