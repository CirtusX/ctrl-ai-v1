// Package engine implements the guardrail rule evaluation engine.
//
// The engine loads rules from rules.yaml (custom) and merges them with
// built-in security rules. Each tool call extracted from an LLM response
// is evaluated against the rule set in order — first match wins.
//
// Rule matching supports:
//   - Tool name (case-insensitive, design doc Section 6.3)
//   - Action field (case-insensitive, for tools like "nodes", "browser")
//   - Agent ID (exact match)
//   - File path glob patterns (string or list, OR logic)
//   - Argument substrings (string or list, case-insensitive, OR logic)
//   - Command regex (for exec tool's "command" field)
//   - URL regex (for web_fetch/browser "url"/"targetUrl" fields)
//
// See design doc Section 6 for the full rule schema and evaluation logic.
package engine

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Rule defines a single guardrail rule. Each rule has a match condition
// and an action to take when the condition is met.
//
// Design doc Section 12:
//
//	type Rule struct {
//	    Name    string
//	    Match   RuleMatch
//	    Action  string // "block"
//	    Message string
//	}
type Rule struct {
	Name    string    `yaml:"name"`
	Match   RuleMatch `yaml:"match"`
	Action  string    `yaml:"action"`  // "block" or "allow"
	Message string    `yaml:"message"` // Human-readable explanation.
	Builtin bool      `yaml:"-"`       // True for built-in rules (not serialized).

	// compiled holds pre-compiled matchers (regex, glob).
	// Set by compileMatcher() after loading.
	compiled *compiledMatcher
}

// RuleMatch defines the conditions under which a rule fires.
// All non-empty fields must match for the rule to trigger (AND logic).
// Within list fields like Tool and Action, any value matching is sufficient (OR logic).
//
// Design doc Section 12:
//
//	type RuleMatch struct {
//	    Tool         []string // tool names (OR) — CASE-INSENSITIVE
//	    Action       []string // action values (OR) — CASE-INSENSITIVE
//	    Agent        string   // agent ID (exact)
//	    Path         []string // glob patterns for `path` arg (OR)
//	    ArgContains  []string // substrings in raw JSON (OR, case-insensitive)
//	    CommandRegex string   // regex for `command` field
//	    URLRegex     string   // regex for `url`/`targetUrl` field
//	}
type RuleMatch struct {
	Tool         stringOrList `yaml:"tool"`
	Action       stringOrList `yaml:"action"`
	Agent        string       `yaml:"agent"`
	Path         stringOrList `yaml:"path"`
	ArgContains  stringOrList `yaml:"arg_contains"`
	CommandRegex string       `yaml:"command_regex"`
	URLRegex     string       `yaml:"url_regex"`
}

// stringOrList handles YAML fields that can be either a single string
// or a list of strings. In rules.yaml, users can write either:
//
//	tool: exec          # single string
//	tool: [exec, read]  # list of strings
type stringOrList []string

// UnmarshalYAML handles both "tool: exec" and "tool: [exec, read]".
func (s *stringOrList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		*s = []string{value.Value}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		*s = list
		return nil
	default:
		return fmt.Errorf("expected string or list, got %v", value.Kind)
	}
}

// Decision is the outcome of evaluating a tool call against the rule set.
type Decision struct {
	Action  string // "allow" or "block"
	Rule    string // Name of the rule that matched (empty if default allow).
	Message string // Human-readable reason (from the rule).
}

// RuleInfo is a summary of a rule for display (used by `ctrlai rules list`).
type RuleInfo struct {
	Name    string
	Builtin bool
	Action  string
	Message string
}

// rulesFile is the YAML envelope for rules.yaml.
type rulesFile struct {
	Rules   []Rule                `yaml:"rules"`
	Builtin map[string]bool       `yaml:"builtin"`
}

// loadRulesFromFile reads and parses custom rules from the given YAML path.
// Returns an empty slice if the file doesn't exist (not an error).
func loadRulesFromFile(path string) ([]Rule, map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading rules %s: %w", path, err)
	}

	if len(data) == 0 {
		return nil, nil, nil
	}

	var file rulesFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, nil, fmt.Errorf("parsing rules %s: %w", path, err)
	}

	return file.Rules, file.Builtin, nil
}

// saveRulesToFile writes custom rules to the given YAML path.
// Only saves custom rules (not built-in) and the builtin toggle map.
func saveRulesToFile(path string, customRules []Rule, builtinToggles map[string]bool) error {
	file := rulesFile{
		Rules:   customRules,
		Builtin: builtinToggles,
	}

	data, err := yaml.Marshal(&file)
	if err != nil {
		return fmt.Errorf("marshaling rules: %w", err)
	}

	header := "# CtrlAI Guardrail Rules\n# See design doc Section 6.2 for the rule schema.\n\n"
	return os.WriteFile(path, []byte(header+string(data)), 0o644)
}

// WriteDefaultRules writes a default rules.yaml with all built-in rules enabled.
// Used by the first-run setup.
func WriteDefaultRules(path string) error {
	builtinToggles := defaultBuiltinToggles()
	return saveRulesToFile(path, nil, builtinToggles)
}
