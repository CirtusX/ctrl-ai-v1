# CtrlAI

A transparent HTTP proxy that sits between your AI agent SDK and the LLM provider. It intercepts LLM responses, evaluates tool calls against configurable guardrail rules, blocks dangerous ones, audits everything, and provides a kill switch — all with **zero code changes** to your agent.

```
Agent SDK  ──►  CtrlAI Proxy (:3100)  ──►  LLM Provider (Anthropic / OpenAI)
                     │
              Inspect tool calls
              Apply guardrail rules
              Audit everything
              Kill switch
```

Built for [OpenClaw](https://github.com/openclaw) but works with any SDK that lets you set a custom `baseUrl` for the LLM provider.

## Install

```bash
git clone https://github.com/CirtusX/ctrl-ai-v1.git
cd ctrl-ai-v1
go build -o ctrlai ./cmd/ctrlai/       # Linux / macOS
go build -o ctrlai.exe ./cmd/ctrlai/   # Windows
```

Requires Go 1.24+. Then move the binary somewhere on your PATH:

```bash
# Linux / macOS
sudo mv ctrlai /usr/local/bin/

# Windows (PowerShell)
Move-Item -Force ctrlai.exe "$env:USERPROFILE\go\bin\"
```

## Quick Start

### 1. First-time setup

```bash
ctrlai
```

Running with no arguments triggers interactive setup. It:
- Creates the config directory (`~/.ctrlai/` on Linux/macOS, `%USERPROFILE%\.ctrlai\` on Windows)
- Writes a default `config.yaml` (proxy on `127.0.0.1:3100`)
- Writes a default `rules.yaml` with built-in security rules enabled
- Creates the audit log directory


### 2. Start the proxy

```bash
ctrlai start
```

The proxy starts on `http://127.0.0.1:3100` with the dashboard at `http://127.0.0.1:3100/dashboard`.

To run in the background:

```bash
ctrlai start -d
```

### 3. Point your agent SDK at the proxy

In your OpenClaw config (`~/.openclaw/openclaw.json`), set the `baseUrl` to route through CtrlAI:

**Single agent, single provider:**

```json
{
  "models": {
    "providers": {
      "anthropic": {
        "baseUrl": "http://127.0.0.1:3100/provider/anthropic"
      }
    }
  }
}
```

**Multi-agent setup:**

```json
{
  "models": {
    "mode": "merge",
    "providers": {
      "ctrlai-main": {
        "baseUrl": "http://127.0.0.1:3100/provider/anthropic/agent/main",
        "apiKey": "${ANTHROPIC_API_KEY}",
        "api": "anthropic-messages",
        "models": [{ "id": "claude-opus-4-5", "name": "Claude Opus 4.5" }]
      },
      "ctrlai-work": {
        "baseUrl": "http://127.0.0.1:3100/provider/anthropic/agent/work",
        "apiKey": "${ANTHROPIC_API_KEY}",
        "api": "anthropic-messages",
        "models": [{ "id": "claude-opus-4-5", "name": "Claude Opus 4.5" }]
      }
    }
  }
}
```

Or generate a snippet:

```bash
ctrlai config generate
```

### 4. That's it

Your agent SDK sends requests to the proxy as if it were the LLM provider. The proxy forwards to the real provider, inspects the response, blocks dangerous tool calls, and sends the (possibly modified) response back. The SDK has no idea.

## URL Structure

```
http://127.0.0.1:3100/provider/{providerKey}/agent/{agentId}/{apiPath}
```

| Part | Example | Description |
|------|---------|-------------|
| `providerKey` | `anthropic`, `openai` | Matches a key in `config.yaml` providers |
| `agentId` | `main`, `work` | Per-agent rules, audit, kill switch. Defaults to `"default"` if omitted |
| `apiPath` | `/v1/messages`, `/v1/chat/completions` | Forwarded as-is to the upstream provider |

Examples:
```
/provider/anthropic/v1/messages                    → agent "default", Anthropic API
/provider/anthropic/agent/main/v1/messages         → agent "main", Anthropic API
/provider/openai/agent/work/v1/chat/completions    → agent "work", OpenAI API
```

## Guardrail Rules

Rules are defined in `~/.ctrlai/rules.yaml` (or `%USERPROFILE%\.ctrlai\rules.yaml` on Windows). First match wins. Default action is allow.

### Built-in Rules

19 built-in security rules are enabled by default. They cover:

| Category | Rules | Default |
|----------|-------|---------|
| File system | SSH keys, .env, credentials, shell config, browser passwords, private keys, system files, self-modification | ON |
| Destructive commands | `rm -rf /`, `mkfs`, `dd if=`, fork bombs, credential exfiltration via curl/wget | ON |
| Privacy | Camera, screen recording, GPS location, remote code execution on paired devices | ON |
| Messaging | Admin actions (kick, ban, timeout, role changes) | ON |
| Messaging | Sending messages (send, reply, broadcast) | OFF |
| Sessions | Spawning sub-agents, cross-session messaging | OFF |
| Memory | Memory search, memory read | OFF |
| Gateway | Config modification, restart | ON |
| Cron | Creating scheduled tasks | OFF |

Toggle any built-in rule in `rules.yaml`:

```yaml
builtin:
  block_ssh_private_keys: true
  block_message_send: false    # off by default, flip to true to enable
```

### Writing Custom Rules

The `rules.yaml` file has two sections: `rules` (your custom rules) and `builtin` (toggles for built-in rules). Here's a complete annotated example:

```yaml
# ~/.ctrlai/rules.yaml

# --- Custom rules ---
# These are evaluated in order, first match wins.
# If no rule matches, the tool call is ALLOWED.
rules:

  # Block a specific tool entirely
  - name: block-secret-files
    match:
      tool: [read, write, edit]     # matches ANY of these tools
      path: "**/.env"               # glob pattern on the 'path' argument
    action: block
    message: "Cannot access .env files"

  # Block a shell command pattern
  - name: block-dangerous-commands
    match:
      tool: exec
      command_regex: "rm\\s+-rf\\s+/"
    action: block
    message: "Destructive command blocked"

  # Block a tool only for a specific agent
  - name: no-browser-for-work-agent
    match:
      tool: browser
      agent: work                   # only applies to agent "work"
    action: block

  # Block based on any substring in the arguments
  - name: block-password-access
    match:
      tool: exec
      arg_contains: "password"      # matches anywhere in the JSON arguments
    action: block
    message: "Cannot access password-related resources"

  # Block URLs matching a pattern
  - name: block-internal-urls
    match:
      tool: [web_fetch, browser]
      url_regex: "internal\\.company\\.com"
    action: block
    message: "Cannot access internal URLs"

  # Block specific actions on multi-action tools
  # (nodes, message, gateway, browser, canvas all use action fields)
  - name: block-notifications
    match:
      tool: nodes
      action: notify                # matches the "action" field in the tool arguments
    action: block

# --- Built-in rule toggles ---
# Set to true to enable, false to disable.
# If a toggle is not listed here, it uses its default value.
builtin:
  block_ssh_private_keys: true
  block_env_files: true
  block_credential_files: true
  block_shell_config_write: true
  block_destructive_commands: true
  block_exfiltration: true
  block_camera: true
  block_screen_record: true
  block_location: true
  block_node_rce: true
  block_message_admin: true
  block_gateway_modify: true
  block_gateway_restart: true
  # These are OFF by default — set to true if you want them:
  # block_unsolicited_messages: false
  # block_message_send: false
  # block_sessions_spawn: false
  # block_sessions_send: false
  # block_memory_search: false
  # block_memory_get: false
  # block_cron_create: false
```

### Rule Structure

Every rule has this shape:

```yaml
- name: my-rule-name          # Required. Unique identifier.
  match:                       # Required. Conditions to match (ALL must match).
    tool: exec                 # Which tool(s) this rule applies to.
    # ...other match fields
  action: block                # "block" (default) or "allow"
  message: "Why it was blocked" # Optional. Shown to the agent.
```

### Match Fields

| Field | What it does | Accepts | Example |
|-------|-------------|---------|---------|
| `tool` | Match the tool name | String or list | `exec` or `[read, write, edit]` |
| `action` | Match the `action` field in tool arguments | String or list | `camera_snap` or `[send, reply, broadcast]` |
| `agent` | Match the agent ID (from URL path) | String | `work` |
| `path` | Glob match on `path` argument | String or list | `**/.env` or `["**/.env", "**/.secrets"]` |
| `arg_contains` | Substring search in the raw arguments JSON | String or list | `password` or `[".ssh/id_", ".aws/credentials"]` |
| `command_regex` | Regex match on `command` argument (exec tool) | Regex | `rm\s+-rf\s+/`, `sudo\s+` |
| `url_regex` | Regex match on `url` or `targetUrl` argument | Regex | `evil\.com`, `http://` |

**How matching works:**
- Multiple fields in the same rule are **AND'd** — all must match
- Lists within a field are **OR'd** — any item in the list can match
- `tool` matching is always **case-insensitive** (handles OAuth PascalCase: `Bash`/`bash`, `Read`/`read`)
- `action` matching is case-insensitive
- `arg_contains` matching is case-insensitive
- Rules are evaluated top-to-bottom, **first match wins**
- Built-in rules are evaluated before custom rules
- If nothing matches, the tool call is **allowed**

**Single-value vs list fields:**
- `tool`, `action`, `path`, and `arg_contains` accept **string or list** — `tool: exec` or `tool: [exec, bash, read]`
- Lists within a field use **OR logic** — any item matching is sufficient
- `agent`, `command_regex`, `url_regex` are **single values**
- For multiple patterns with `command_regex` or `url_regex`, use regex OR: `'(pattern1|pattern2)'`

Example with list-based `arg_contains` and `path`:
```yaml
- name: block-sensitive-configs
  match:
    tool: [read, write, edit]
    arg_contains: [".env", ".aws/credentials", ".ssh/id_"]  # any of these triggers the rule
  action: block

- name: block-secret-paths
  match:
    tool: [read, write]
    path: ["**/.env", "**/secrets/**", "**/.ssh/*"]  # any glob matching triggers the rule
  action: block
```

### Which Tools Exist

These are the tools an AI agent can call. CtrlAI doesn't define these — the LLM returns them in its response, and CtrlAI intercepts.

| Tool | What it does | Key arguments to match on |
|------|-------------|--------------------------|
| `exec` | Run a shell command | `command` (use `command_regex`) |
| `read` | Read a file | `path` (use `path` glob) |
| `write` | Write/create a file | `path` (use `path` glob) |
| `edit` | Edit an existing file | `path` (use `path` glob) |
| `apply_patch` | Apply a multi-file patch | `input` (use `arg_contains`) |
| `process` | Interact with running processes | `action` (continue/wait/kill/status) |
| `web_search` | Search the web | `query` (use `arg_contains`) |
| `web_fetch` | Fetch a URL | `url` (use `url_regex`) |
| `browser` | Control a browser | `action`, `targetUrl` (use `url_regex`) |
| `canvas` | Run JS on a canvas element | `action`, `javaScript` |
| `nodes` | Control paired devices | `action` (camera_snap/run/invoke/location_get/etc.) |
| `message` | Send messages (WhatsApp/Slack/etc.) | `action` (send/reply/kick/ban/etc.) |
| `tts` | Text-to-speech | `text` |
| `sessions_spawn` | Spawn a sub-agent | `task`, `agentId` |
| `sessions_send` | Send cross-session message | `sessionKey`, `message` |
| `memory_search` | Search agent memory | `query` |
| `memory_get` | Read from agent memory | `path` |
| `gateway` | Modify gateway config | `action` (config.apply/config.patch/restart) |
| `cron` | Manage scheduled tasks | `action` (add/update/remove) |
| `image` | Analyze images | `url_or_path` |

**Important:** `nodes` is a single tool with many actions. Camera, screen recording, GPS, and remote code execution are all `action` values on the `nodes` tool — not separate tools. Match them like:

```yaml
- name: block-camera
  match:
    tool: nodes
    action: [camera_snap, camera_clip, camera_list]
  action: block
```

### Common Rule Recipes

**Block all exec for a specific agent:**
```yaml
- name: no-exec-for-work
  match:
    tool: exec
    agent: work
  action: block
  message: "work agent cannot run commands"
```

**Block writing to any config file:**
```yaml
- name: block-config-writes
  match:
    tool: [write, edit]
    path: "**/*.config"
  action: block
```

**Block all outbound network from exec:**
```yaml
- name: block-network-commands
  match:
    tool: exec
    command_regex: "(curl|wget|nc|ncat|ssh|scp|rsync|ftp)"
  action: block
  message: "Network commands not allowed"
```

**Block agent from sending messages on any platform:**
```yaml
- name: block-all-messaging
  match:
    tool: message
    action: [send, sendWithEffect, sendAttachment, reply, thread-reply, broadcast]
  action: block
  message: "Message sending is disabled"
```

**Block reading anything outside the project directory:**
```yaml
- name: project-only-reads
  match:
    tool: read
    arg_contains: "../"
  action: block
  message: "Cannot read files outside project directory"
```

**Block access to multiple sensitive file types (list-based arg_contains):**
```yaml
- name: block-all-secrets
  match:
    tool: [read, write, edit, exec]
    arg_contains: [".env", ".aws/credentials", ".ssh/id_", "secret-passwords"]
  action: block
  message: "Access to sensitive files blocked"
```

**Block writes to multiple protected directories (list-based path):**
```yaml
- name: protect-directories
  match:
    tool: [write, edit]
    path: ["**/production/**", "**/deploy/**", "**/.github/**"]
  action: block
  message: "Writes to protected directories blocked"
```

**Block multiple exfiltration tools with regex OR:**
```yaml
- name: block-exfil-sensitive
  match:
    tool: [exec, bash]
    command_regex: '(scp|rsync|nc).*(/etc/passwd|/etc/shadow|\.ssh)'
  action: block
  message: "Exfiltration of sensitive files blocked"
```

**Restrict a specific agent (e.g., "intern") from writing/executing:**
```yaml
- name: restrict-intern
  match:
    tool: [write_file, write, exec, bash]
    agent: intern
  action: block
  message: "Intern agent cannot write files or run commands"
```
Note: the same request from a different agent (e.g., `senior`) will pass through normally. Agent identity is derived from the URL path: `/provider/{provider}/agent/{agentId}/...`

### Test Rules Without Live Traffic

```bash
# Linux / macOS
ctrlai rules test '{"name":"exec","arguments":{"command":"cat ~/.ssh/id_rsa"}}'
ctrlai rules test '{"name":"exec","arguments":{"command":"ls -la"}}'

# Windows (PowerShell) — use double quotes and escape inner quotes
ctrlai rules test "{\"name\":\"exec\",\"arguments\":{\"command\":\"cat ~/.ssh/id_rsa\"}}"
ctrlai rules test "{\"name\":\"exec\",\"arguments\":{\"command\":\"ls -la\"}}"

# Windows (CMD) — same as PowerShell
ctrlai rules test "{\"name\":\"exec\",\"arguments\":{\"command\":\"ls -la\"}}"
```

## Kill Switch

Instantly terminate any agent. The proxy returns a fake "end_turn" response so the SDK stops its loop.

```bash
ctrlai kill main --reason "suspicious activity"
ctrlai kill --all --reason "emergency shutdown"
ctrlai revive main
```

The kill state is persisted to `~/.ctrlai/killed.yaml` and file-watched for hot reload — kill an agent while the proxy is running and it takes effect within seconds.

## Audit Log

Every tool call through the proxy is logged with a tamper-evident hash chain.

```bash
ctrlai audit tail              # Recent entries
ctrlai audit tail -f           # Follow (live)
ctrlai audit query --agent main --decision block --since 1h
ctrlai audit verify            # Verify hash chain integrity
ctrlai audit export --format csv
```

Each entry includes: sequence number, timestamp, agent ID, tool name, arguments, decision (allow/block), matched rule, and a SHA-256 hash linking to the previous entry. Modifying any entry breaks the chain from that point forward.

**Timestamp format:** All timestamps are stored in UTC using ISO 8601 / RFC 3339 with nanosecond precision (e.g., `2026-02-14T21:36:05.2918658Z`). The dashboard automatically converts these to your local timezone for display. When querying the audit API directly (`/api/audit`), timestamps are returned in UTC.

Log files:
```
~/.ctrlai/audit/
├── genesis.json          # Chain root
├── 2026-02-13.jsonl      # Daily append-only log
└── index.db              # SQLite index for fast queries
```

## Dashboard

Web UI at `http://127.0.0.1:3100/dashboard` (same port as proxy).

**Features:**
- Agent list with status, provider, request count, blocked count
- Kill / Revive buttons per agent
- Full rule list (builtin + custom)
- Live activity feed via WebSocket (real-time tool call decisions)
- Audit log viewer with recent entries
- Auto-refreshes every 5 seconds + instant WebSocket updates

**Limitations:**
- No rule editor UI — you can add/remove rules via the REST API (POST /api/rules, POST /api/rules/delete) but there's no form in the HTML yet
- No filtering/search UI — the audit REST API supports ?agent=, ?decision=, ?limit= but there's no UI filter controls
- No charts/graphs — just tables and a text-based live feed
- No auth — anyone who can reach port 3100 can see the dashboard and kill agents

**REST API** (for scripting / external integrations):

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/status` | GET | Proxy status (running, rule counts, agent count) |
| `/api/agents` | GET | All agents with stats |
| `/api/audit` | GET | Recent audit entries (supports `?limit=`, `?agent=`, `?decision=`) |
| `/api/rules` | GET | All rules |
| `/api/rules` | POST | Add a custom rule `{"yaml": "..."}` |
| `/api/rules/delete` | POST | Remove a custom rule `{"name": "..."}` |
| `/api/kill` | POST | Kill an agent `{"agent": "main", "reason": "..."}` |
| `/api/revive` | POST | Revive an agent `{"agent": "main"}` |
| `/dashboard/ws` | WS | Live activity feed (real-time audit events) |

## CLI Reference

```
ctrlai                     Interactive first-run setup
ctrlai start [-d]          Start proxy (foreground or daemon)
ctrlai stop                Stop running proxy
ctrlai status              Show proxy status and active agents

ctrlai agents              List all agents with stats
ctrlai agents <id>         Show details for one agent

ctrlai kill <agent> --reason "..."    Kill an agent
ctrlai kill --all --reason "..."      Kill all agents
ctrlai revive <agent>                 Revive a killed agent

ctrlai rules list          List all rules (builtin + custom)
ctrlai rules add <yaml>    Add a custom rule
ctrlai rules remove <name> Remove a custom rule
ctrlai rules test <json>   Test a tool call against rules

ctrlai audit tail [-f]     Show recent entries (optionally follow)
ctrlai audit query         Query with filters (--agent, --decision, --since)
ctrlai audit verify        Verify hash chain integrity
ctrlai audit export        Export log (--format csv|json)

ctrlai config show         Show current config
ctrlai config edit         Open config in $EDITOR
ctrlai config generate     Generate OpenClaw integration snippet
```

## Configuration

`~/.ctrlai/config.yaml` (Linux/macOS) or `%USERPROFILE%\.ctrlai\config.yaml` (Windows):

```yaml
server:
  host: "127.0.0.1"
  port: 3100

providers:
  anthropic:
    upstream: "https://api.anthropic.com"
  openai:
    upstream: "https://api.openai.com"

streaming:
  buffer: true              # Buffer SSE to inspect tool calls (required for security)
  bufferTimeoutMs: 30000    # Max buffer time before flushing

dashboard:
  enabled: true
```

Config and rules are file-watched — edit them while the proxy is running and changes take effect automatically.

## Blocking Behavior: All-or-Nothing Per Response

When an LLM responds to a user message, it may return **multiple tool calls** in a single response (e.g., "read this file AND run this command"). CtrlAI evaluates **each tool call individually** against the rules, but if **any single tool call** in a response is blocked, the **entire response** is stripped and replaced with a block notice. The agent executes nothing from that response.

**Why all-or-nothing?** AI models plan tool calls as a coordinated sequence — action B may depend on the result of action A. If we only blocked the bad action and let the rest through, the remaining actions could behave unpredictably since they were planned assuming the blocked action would succeed. Blocking the entire response is the safer default.

### Separate messages = separate evaluation

Each user message triggers a separate LLM request. CtrlAI evaluates each request **independently**. Blocking one request has **zero effect** on any other request from the same agent.

**Example — two separate messages (independently evaluated):**
```
Message 1: "Take a photo of the restricted lab"     → BLOCKED (camera rule)
Message 2: "Look up today's S&P 500 price"          → ALLOWED (no rule match)
```

**Example — one combined message (all-or-nothing):**
```
Message: "Take a photo of the restricted lab AND look up today's S&P 500 price"
  → LLM returns: [camera_snap(...), web_search("S&P 500")]
  → camera_snap trips the camera rule
  → BOTH tool calls blocked (entire response stripped)
```

The stock lookup isn't lost — the user just sends it as a separate message and it goes through. CtrlAI errs on the side of caution: better to temporarily block an innocent action than to let a dangerous one slip through.

### Real-world scenarios

| Scenario | Combined message | What gets blocked | Fix |
|----------|-----------------|-------------------|-----|
| **IT Security** | "Read SSH keys and run tests" | Both — SSH key read trips `block_ssh_private_keys` | Send "run tests" separately |
| **Financial Compliance** | "Pull client tax returns and check stock price" | Both — tax file read trips a custom rule | Send stock lookup separately |
| **Smart Home** | "Turn on lights and unlock front door" | Both — door unlock trips a safety rule | Send "turn on lights" separately |
| **Workplace Privacy** | "Take photo of whiteboard and email meeting notes" | Both — camera trips `block_camera` | Send email request separately |

### Future: partial blocking mode

The current all-or-nothing policy is the safe default. A future `blocking_mode: partial` config option could enable per-tool-call granularity (block only the offending tool call, pass the rest through). The engine already evaluates each tool call individually — only the response rewriting step enforces all-or-nothing. This is an architectural extension point, not a limitation.

## How It Works

1. Request arrives from SDK at `/provider/anthropic/agent/main/v1/messages`
2. Proxy checks the kill switch for agent `main`
3. If killed: return fake "end_turn" response immediately
4. If alive: forward request to `https://api.anthropic.com/v1/messages`
5. Buffer the streaming SSE response until `message_stop`
6. Reconstruct the full response and extract tool calls
7. Evaluate each tool call against the rule engine (first match wins)
8. If blocked: strip the tool_use block, inject a block notice, change `stop_reason` to `end_turn`
9. If allowed: pass through unchanged
10. Log everything to the audit chain
11. Send the (possibly modified) SSE stream to the SDK

The SDK sees a normal LLM response. If a tool was blocked, it looks like the LLM decided not to call it and instead said "[CtrlAI] Blocked: reason".

## Supported Providers

| Provider | API Path | Status |
|----------|----------|--------|
| Anthropic Messages | `/v1/messages` | Full support |
| OpenAI Chat Completions | `/v1/chat/completions` | Full support |
| OpenAI Responses | `/v1/responses` | Pass-through (no inspection) |
| Other | Any path | Pass-through (transparent proxy) |

## Runtime Files

All state lives in a single directory:
- Linux / macOS: `~/.ctrlai/`
- Windows: `%USERPROFILE%\.ctrlai\`

Override with `--config-dir /path/to/dir` on any command.

```
.ctrlai/
├── config.yaml        # Proxy configuration
├── rules.yaml         # Guardrail rules (builtin toggles + custom)
├── agents.yaml        # Agent registry (auto-populated)
├── killed.yaml        # Kill switch state
├── ctrlai.pid         # PID file when running as daemon
└── audit/
    ├── genesis.json   # Hash chain root
    ├── YYYY-MM-DD.jsonl  # Daily audit logs
    └── index.db       # SQLite query index
```

## Development

```bash
go build -o ctrlai ./cmd/ctrlai/     # Build (add .exe on Windows)
go test ./...                         # Run all 155 tests
go test -race ./...                   # Tests with race detector
go vet ./...                          # Lint
go install ./cmd/ctrlai/             # Install to GOPATH/bin
```

A `Makefile` is included for Linux/macOS users who prefer `make build`, `make test`, etc. On Windows, just use the `go` commands above — `make` is not installed by default.

## Platform Notes

| | Linux / macOS | Windows |
|--|--|--|
| Config dir | `~/.ctrlai/` | `%USERPROFILE%\.ctrlai\` |
| Binary name | `ctrlai` | `ctrlai.exe` |
| Build | `go build -o ctrlai ./cmd/ctrlai/` | `go build -o ctrlai.exe ./cmd/ctrlai/` |
| Shell quoting | Single quotes: `'{"name":"exec"}'` | Escaped doubles: `"{\"name\":\"exec\"}"` |
| Daemon mode | `ctrlai start -d` | `ctrlai start -d` (same) |
| Background alt | `ctrlai start &` | `Start-Process ctrlai -ArgumentList start` |

## Security

Found a vulnerability? Please report it responsibly to **security@cirtus.com**. Do not open a public issue for security bugs.

## Enterprise

Self-hosting CtrlAI works great for individual developers and small teams. For organizations that need more, **CtrlAI Enterprise** adds:

- **Centralized policy management** — push rules across all agents and developers from one place
- **Team-wide audit** — unified audit log with search, retention policies, and compliance exports
- **SSO & RBAC** — integrate with your identity provider, scope permissions by role
- **Managed deployment** — hosted proxy with SLA, no self-hosting overhead
- **Priority support** — direct access to the engineering team

Get in touch: **enterprise@cirtus.com**

## License

MIT
