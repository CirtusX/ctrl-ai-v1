package engine

// builtinRules returns all built-in security rules.
// These are always loaded and can be individually toggled on/off
// via the "builtin" section in rules.yaml.
//
// Built-in rules cover the common attack patterns from design doc Section 6.2:
//   - File system access to sensitive paths (SSH keys, .env, credentials)
//   - Destructive commands (rm -rf /, mkfs, dd)
//   - Credential exfiltration via network tools
//   - Privacy/surveillance (camera, screen recording, location)
//   - Messaging admin actions (kick, ban, timeout)
//   - Gateway/config modification
func builtinRules() []Rule {
	return []Rule{
		// --- File system rules ---
		{
			Name:    "block_ssh_private_keys",
			Match:   RuleMatch{Tool: stringOrList{"exec", "read"}, ArgContains: ".ssh/id_"},
			Action:  "block",
			Message: "Cannot access SSH private keys",
			Builtin: true,
		},
		{
			Name:    "block_env_files",
			Match:   RuleMatch{Tool: stringOrList{"read", "write", "edit"}, Path: "**/.env"},
			Action:  "block",
			Message: "Cannot access .env files",
			Builtin: true,
		},
		{
			Name:    "block_credential_files",
			Match:   RuleMatch{Tool: stringOrList{"read", "write", "edit"}, ArgContains: ".aws/credentials"},
			Action:  "block",
			Message: "Cannot access credential files",
			Builtin: true,
		},
		// Shell config blocking uses ArgContains to match common shell config
		// filenames in the path argument. Design doc: .bashrc, .zshrc, .profile.
		{
			Name:    "block_shell_config_write",
			Match:   RuleMatch{Tool: stringOrList{"write", "edit"}, ArgContains: ".bashrc"},
			Action:  "block",
			Message: "Cannot modify shell configuration files",
			Builtin: true,
		},
		{
			Name:    "block_shell_config_write_zsh",
			Match:   RuleMatch{Tool: stringOrList{"write", "edit"}, ArgContains: ".zshrc"},
			Action:  "block",
			Message: "Cannot modify shell configuration files",
			Builtin: true,
		},
		{
			Name:    "block_shell_config_write_profile",
			Match:   RuleMatch{Tool: stringOrList{"write", "edit"}, ArgContains: ".profile"},
			Action:  "block",
			Message: "Cannot modify shell configuration files",
			Builtin: true,
		},
		{
			Name:    "block_browser_passwords",
			Match:   RuleMatch{Tool: stringOrList{"read", "exec"}, ArgContains: "Login Data"},
			Action:  "block",
			Message: "Cannot access browser password databases",
			Builtin: true,
		},
		{
			Name:    "block_private_key_content",
			Match:   RuleMatch{Tool: stringOrList{"write", "exec"}, ArgContains: "PRIVATE KEY-----"},
			Action:  "block",
			Message: "Cannot write or transmit private key content",
			Builtin: true,
		},
		{
			Name:    "block_system_files",
			Match:   RuleMatch{Tool: stringOrList{"read", "write", "edit"}, ArgContains: "/etc/shadow"},
			Action:  "block",
			Message: "Cannot access system credential files",
			Builtin: true,
		},
		{
			Name:    "block_self_modification",
			Match:   RuleMatch{Tool: stringOrList{"write", "edit"}, ArgContains: ".ctrlai/"},
			Action:  "block",
			Message: "Cannot modify CtrlAI configuration directory",
			Builtin: true,
		},

		// --- Destructive command rules ---
		{
			Name:    "block_destructive_commands",
			Match:   RuleMatch{Tool: stringOrList{"exec"}, CommandRegex: `rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\{\s*:\|:&\s*\};:`},
			Action:  "block",
			Message: "Destructive command blocked",
			Builtin: true,
		},

		// --- Credential exfiltration ---
		{
			Name:    "block_exfiltration",
			Match:   RuleMatch{Tool: stringOrList{"exec"}, CommandRegex: `(curl|wget|nc|ncat).*\.(env|pem|key|credentials)`},
			Action:  "block",
			Message: "Credential exfiltration attempt blocked",
			Builtin: true,
		},

		// --- Privacy / surveillance rules ---
		// These match on the "nodes" tool with specific action values.
		// camera_snap, camera_clip, screen_record, location_get are NOT separate
		// tools — they are action values of the "nodes" tool (design doc Section 6.4).
		{
			Name:    "block_camera",
			Match:   RuleMatch{Tool: stringOrList{"nodes"}, Action: stringOrList{"camera_snap", "camera_clip", "camera_list"}},
			Action:  "block",
			Message: "Camera access blocked",
			Builtin: true,
		},
		{
			Name:    "block_screen_record",
			Match:   RuleMatch{Tool: stringOrList{"nodes"}, Action: stringOrList{"screen_record"}},
			Action:  "block",
			Message: "Screen recording blocked",
			Builtin: true,
		},
		{
			Name:    "block_location",
			Match:   RuleMatch{Tool: stringOrList{"nodes"}, Action: stringOrList{"location_get"}},
			Action:  "block",
			Message: "Location tracking blocked",
			Builtin: true,
		},
		{
			Name:    "block_node_rce",
			Match:   RuleMatch{Tool: stringOrList{"nodes"}, Action: stringOrList{"run", "invoke"}},
			Action:  "block",
			Message: "Remote code execution on paired device blocked",
			Builtin: true,
		},

		// --- Messaging rules ---
		{
			Name:    "block_unsolicited_messages",
			Match:   RuleMatch{Tool: stringOrList{"message"}},
			Action:  "block",
			Message: "Message tool usage blocked",
			Builtin: true,
		},
		{
			Name:    "block_message_send",
			Match:   RuleMatch{Tool: stringOrList{"message"}, Action: stringOrList{"send", "sendWithEffect", "sendAttachment", "reply", "thread-reply", "broadcast"}},
			Action:  "block",
			Message: "Message sending blocked",
			Builtin: true,
		},
		{
			Name:    "block_message_admin",
			Match:   RuleMatch{Tool: stringOrList{"message"}, Action: stringOrList{"kick", "ban", "timeout", "role-add", "role-remove"}},
			Action:  "block",
			Message: "Messaging admin action blocked",
			Builtin: true,
		},

		// --- Session tools ---
		{
			Name:    "block_sessions_spawn",
			Match:   RuleMatch{Tool: stringOrList{"sessions_spawn"}},
			Action:  "block",
			Message: "Agent spawning blocked",
			Builtin: true,
		},
		{
			Name:    "block_sessions_send",
			Match:   RuleMatch{Tool: stringOrList{"sessions_send"}},
			Action:  "block",
			Message: "Cross-session messaging blocked",
			Builtin: true,
		},

		// --- Memory tools ---
		{
			Name:    "block_memory_search",
			Match:   RuleMatch{Tool: stringOrList{"memory_search"}},
			Action:  "block",
			Message: "Memory search blocked",
			Builtin: true,
		},
		{
			Name:    "block_memory_get",
			Match:   RuleMatch{Tool: stringOrList{"memory_get"}},
			Action:  "block",
			Message: "Memory access blocked",
			Builtin: true,
		},

		// --- Persistence / cron ---
		{
			Name:    "block_cron_create",
			Match:   RuleMatch{Tool: stringOrList{"cron"}, Action: stringOrList{"add"}},
			Action:  "block",
			Message: "Cron job creation blocked",
			Builtin: true,
		},

		// --- Gateway rules ---
		{
			Name:    "block_gateway_modify",
			Match:   RuleMatch{Tool: stringOrList{"gateway"}, Action: stringOrList{"config.apply", "config.patch"}},
			Action:  "block",
			Message: "Gateway configuration modification blocked",
			Builtin: true,
		},
		{
			Name:    "block_gateway_restart",
			Match:   RuleMatch{Tool: stringOrList{"gateway"}, Action: stringOrList{"restart"}},
			Action:  "block",
			Message: "Gateway restart blocked",
			Builtin: true,
		},
	}
}

// defaultBuiltinToggles returns the default enable/disable state for each
// built-in rule. Matches design doc Section 6.2 exactly.
func defaultBuiltinToggles() map[string]bool {
	return map[string]bool{
		// File system — all on by default.
		"block_ssh_private_keys":    true,
		"block_env_files":           true,
		"block_credential_files":    true,
		"block_shell_config_write":         true,
		"block_shell_config_write_zsh":     true,
		"block_shell_config_write_profile": true,
		"block_browser_passwords":   true,
		"block_private_key_content": true,
		"block_system_files":        true,
		"block_self_modification":   true,

		// Destructive commands — on by default.
		"block_destructive_commands": true,
		"block_exfiltration":         true,

		// Privacy/surveillance — on by default.
		"block_camera":       true,
		"block_screen_record": true,
		"block_location":      true,
		"block_node_rce":      true,

		// Messaging — admin on, send off by default.
		"block_unsolicited_messages": false,
		"block_message_send":         false,
		"block_message_admin":        true,

		// Session tools — off by default.
		"block_sessions_spawn": false,
		"block_sessions_send":  false,

		// Memory — off by default.
		"block_memory_search": false,
		"block_memory_get":    false,

		// Persistence/admin — on by default.
		"block_cron_create":     false,
		"block_gateway_modify":  true,
		"block_gateway_restart": true,
	}
}
