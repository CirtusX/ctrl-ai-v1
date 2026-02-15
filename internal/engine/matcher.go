package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ctrlai/ctrlai/internal/extractor"
	"github.com/gobwas/glob"
)

// compiledMatcher holds pre-compiled patterns for a rule.
// Compiling regex and glob patterns once at load time keeps per-evaluation
// cost under the 50us target (design doc Section 17).
type compiledMatcher struct {
	commandRegex *regexp.Regexp
	urlRegex     *regexp.Regexp
	pathGlobs    []glob.Glob
}

// compileMatcher pre-compiles all pattern matchers for a rule.
// Returns an error if any regex or glob pattern is invalid.
func compileMatcher(r *Rule) error {
	r.compiled = &compiledMatcher{}

	if r.Match.CommandRegex != "" {
		re, err := regexp.Compile(r.Match.CommandRegex)
		if err != nil {
			return fmt.Errorf("rule %q: invalid command_regex: %w", r.Name, err)
		}
		r.compiled.commandRegex = re
	}

	if r.Match.URLRegex != "" {
		re, err := regexp.Compile(r.Match.URLRegex)
		if err != nil {
			return fmt.Errorf("rule %q: invalid url_regex: %w", r.Name, err)
		}
		r.compiled.urlRegex = re
	}

	for _, p := range r.Match.Path {
		g, err := glob.Compile(p)
		if err != nil {
			return fmt.Errorf("rule %q: invalid path glob %q: %w", r.Name, p, err)
		}
		r.compiled.pathGlobs = append(r.compiled.pathGlobs, g)
	}

	return nil
}

// matchesRule checks whether a tool call matches a rule's conditions.
// All non-empty match fields must be satisfied (AND logic).
// Returns true if the rule fires for this tool call.
//
// Match logic from design doc Section 6.3:
//   - tool:          case-insensitive match (handles OAuth PascalCase)
//   - action:        case-insensitive match on "action" argument field
//   - agent:         exact match on agent ID from URL path
//   - path:          glob match on "path" argument (OR across list)
//   - arg_contains:  case-insensitive substring in raw arguments JSON (OR across list)
//   - command_regex: regex match on "command" argument field
//   - url_regex:     regex match on "url" or "targetUrl" argument field
func matchesRule(r *Rule, agentID string, tc extractor.ToolCall) bool {
	m := r.Match

	// Tool name match (case-insensitive, OR across list).
	if len(m.Tool) > 0 {
		matched := false
		for _, t := range m.Tool {
			if strings.EqualFold(t, tc.Name) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Agent match (exact).
	if m.Agent != "" && m.Agent != agentID {
		return false
	}

	// Action match (case-insensitive, OR across list).
	// Checks the "action" field in the tool call arguments.
	if len(m.Action) > 0 {
		actionVal := getStringArg(tc.Arguments, "action")
		if actionVal == "" {
			return false
		}
		matched := false
		for _, a := range m.Action {
			if strings.EqualFold(a, actionVal) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Path glob match (OR across list).
	// Checks the "path" field in arguments (for read/write/edit tools).
	if len(m.Path) > 0 && r.compiled != nil && len(r.compiled.pathGlobs) > 0 {
		pathVal := getStringArg(tc.Arguments, "path")
		if pathVal == "" {
			return false
		}
		matched := false
		for _, g := range r.compiled.pathGlobs {
			if g.Match(pathVal) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Argument substring match (case-insensitive, OR across list).
	// Searches the raw JSON representation of arguments.
	if len(m.ArgContains) > 0 {
		rawStr := string(tc.RawJSON)
		if rawStr == "" {
			// Fallback: marshal Arguments to JSON for searching.
			if data, err := json.Marshal(tc.Arguments); err == nil {
				rawStr = string(data)
			}
		}
		rawLower := strings.ToLower(rawStr)
		matched := false
		for _, s := range m.ArgContains {
			if strings.Contains(rawLower, strings.ToLower(s)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Command regex match.
	// Checks the "command" field in arguments (for exec tool).
	if r.compiled != nil && r.compiled.commandRegex != nil {
		cmdVal := getStringArg(tc.Arguments, "command")
		if cmdVal == "" || !r.compiled.commandRegex.MatchString(cmdVal) {
			return false
		}
	}

	// URL regex match.
	// Checks "url" or "targetUrl" fields (for web_fetch, browser tools).
	if r.compiled != nil && r.compiled.urlRegex != nil {
		urlVal := getStringArg(tc.Arguments, "url")
		if urlVal == "" {
			urlVal = getStringArg(tc.Arguments, "targetUrl")
		}
		if urlVal == "" || !r.compiled.urlRegex.MatchString(urlVal) {
			return false
		}
	}

	// All non-empty conditions matched.
	return true
}

// getStringArg safely extracts a string value from a tool call's arguments map.
// Returns "" if the key doesn't exist or the value isn't a string.
func getStringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	val, ok := args[key]
	if !ok {
		return ""
	}
	s, ok := val.(string)
	if !ok {
		return ""
	}
	return s
}
