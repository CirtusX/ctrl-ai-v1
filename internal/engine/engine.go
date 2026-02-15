package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/ctrlai/ctrlai/internal/extractor"
	"gopkg.in/yaml.v3"
)

// Engine is the guardrail rule evaluation engine. It holds the combined
// set of built-in and custom rules and evaluates tool calls against them.
//
// Thread-safe — Evaluate() is called concurrently from proxy handler
// goroutines, while Reload() modifies the rule set on config changes.
// Uses RWMutex so evaluations don't block each other.
type Engine struct {
	mu             sync.RWMutex
	rules          []Rule            // Combined built-in + custom rules, in evaluation order.
	customRules    []Rule            // Custom rules only (for serialization).
	builtinToggles map[string]bool   // Toggle map for built-in rules.
	builtinCount   int
	customCount    int
}

// New creates a rule engine, loading custom rules from the given YAML path
// and merging them with built-in security rules.
//
// Returns an error if the rules file is malformed or contains invalid
// regex/glob patterns. Missing file is not an error (empty custom rules).
func New(rulesPath string) (*Engine, error) {
	e := &Engine{}
	if err := e.load(rulesPath); err != nil {
		return nil, err
	}
	return e, nil
}

// Evaluate checks a tool call against all rules in order.
// First matching rule wins. If no rule matches, default is allow.
//
// Design doc Section 6.3:
//
//	func (e *Engine) Evaluate(agentID string, tc ToolCall) Decision {
//	    for _, rule := range e.rules {
//	        if rule.Matches(agentID, tc) {
//	            return Decision{Action: rule.Action, Rule: rule.Name, Message: rule.Message}
//	        }
//	    }
//	    return Decision{Action: "allow"}
//	}
//
// Performance target: < 50us per tool call (design doc Section 17).
func (e *Engine) Evaluate(agentID string, tc extractor.ToolCall) Decision {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, rule := range e.rules {
		if matchesRule(&rule, agentID, tc) {
			return Decision{
				Action:  rule.Action,
				Rule:    rule.Name,
				Message: rule.Message,
			}
		}
	}

	// No rule matched — default allow.
	return Decision{Action: "allow"}
}

// TestJSON evaluates a tool call provided as a JSON string.
// Used by `ctrlai rules test` to verify rules without running a live agent.
// The JSON should contain "name" and "arguments" fields.
func (e *Engine) TestJSON(jsonStr string) (Decision, error) {
	var raw struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return Decision{}, fmt.Errorf("parsing tool call JSON: %w", err)
	}

	tc := extractor.ToolCall{
		Name:      raw.Name,
		Arguments: raw.Arguments,
	}

	// Marshal arguments back to RawJSON for arg_contains matching.
	if raw.Arguments != nil {
		if data, err := json.Marshal(raw.Arguments); err == nil {
			tc.RawJSON = data
		}
	}

	// Use empty agent ID for testing (matches all non-agent-specific rules).
	return e.Evaluate("", tc), nil
}

// TotalRules returns the total number of active rules (builtin + custom).
func (e *Engine) TotalRules() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.rules)
}

// BuiltinCount returns the number of active built-in rules.
func (e *Engine) BuiltinCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.builtinCount
}

// CustomCount returns the number of custom rules.
func (e *Engine) CustomCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.customCount
}

// ListRules returns summary info for all active rules.
// Used by `ctrlai rules list`.
func (e *Engine) ListRules() []RuleInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()

	infos := make([]RuleInfo, 0, len(e.rules))
	for _, r := range e.rules {
		infos = append(infos, RuleInfo{
			Name:    r.Name,
			Builtin: r.Builtin,
			Action:  r.Action,
			Message: r.Message,
		})
	}
	return infos
}

// AddRule parses a rule from a YAML string and adds it to the custom rules.
// The new rule is compiled (regex/glob patterns validated) before adding.
func (e *Engine) AddRule(yamlStr string) error {
	var rule Rule
	if err := yaml.Unmarshal([]byte(yamlStr), &rule); err != nil {
		return fmt.Errorf("parsing rule YAML: %w", err)
	}

	if rule.Name == "" {
		return fmt.Errorf("rule must have a name")
	}
	if rule.Action == "" {
		rule.Action = "block"
	}

	if err := compileMatcher(&rule); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.customRules = append(e.customRules, rule)
	e.rebuild()
	return nil
}

// RemoveRule removes a custom rule by name.
// Returns an error if the rule is a built-in (can't be removed, only toggled)
// or doesn't exist.
func (e *Engine) RemoveRule(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	found := false
	filtered := make([]Rule, 0, len(e.customRules))
	for _, r := range e.customRules {
		if r.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, r)
	}

	if !found {
		return fmt.Errorf("custom rule %q not found (built-in rules can only be toggled)", name)
	}

	e.customRules = filtered
	e.rebuild()
	return nil
}

// Save persists the current custom rules and builtin toggles to rules.yaml.
func (e *Engine) Save(path string) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return saveRulesToFile(path, e.customRules, e.builtinToggles)
}

// Reload reloads rules from the given YAML path.
// Called by the file watcher when rules.yaml changes.
func (e *Engine) Reload(path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.loadUnlocked(path); err != nil {
		return err
	}

	slog.Info("rules reloaded", "total", len(e.rules), "builtin", e.builtinCount, "custom", e.customCount)
	return nil
}

// load reads rules from file and builds the combined rule set.
func (e *Engine) load(rulesPath string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loadUnlocked(rulesPath)
}

// loadUnlocked does the actual loading. Caller must hold the mutex.
func (e *Engine) loadUnlocked(rulesPath string) error {
	customRules, builtinToggles, err := loadRulesFromFile(rulesPath)
	if err != nil {
		return err
	}

	// Merge file toggles with defaults. If the file specifies a toggle, use it.
	// Otherwise, fall back to the default (some builtins are off by default).
	defaults := defaultBuiltinToggles()
	if builtinToggles == nil {
		builtinToggles = defaults
	} else {
		for name, defaultVal := range defaults {
			if _, exists := builtinToggles[name]; !exists {
				builtinToggles[name] = defaultVal
			}
		}
	}

	// Compile matchers for custom rules.
	for i := range customRules {
		if err := compileMatcher(&customRules[i]); err != nil {
			return err
		}
	}

	e.customRules = customRules
	e.builtinToggles = builtinToggles
	e.rebuild()
	return nil
}

// rebuild merges built-in and custom rules into the combined evaluation list.
// Built-in rules come first (higher priority), then custom rules.
// Caller must hold the mutex.
func (e *Engine) rebuild() {
	var combined []Rule

	// Add enabled built-in rules.
	builtins := builtinRules()
	for _, r := range builtins {
		enabled, exists := e.builtinToggles[r.Name]
		if !exists {
			// Unknown built-in — default to enabled for safety.
			enabled = true
		}
		if !enabled {
			continue
		}

		// Compile matchers for built-in rules.
		if err := compileMatcher(&r); err != nil {
			slog.Error("failed to compile built-in rule", "rule", r.Name, "error", err)
			continue
		}
		combined = append(combined, r)
	}

	// Add custom rules after built-ins.
	combined = append(combined, e.customRules...)

	e.rules = combined
	e.builtinCount = len(combined) - len(e.customRules)
	e.customCount = len(e.customRules)
}
