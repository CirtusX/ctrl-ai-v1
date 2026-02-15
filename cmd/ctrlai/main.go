// Package main is the CLI entry point for CtrlAI — a transparent HTTP proxy
// that sits between an AI agent SDK (OpenClaw/Pi) and the LLM provider.
//
// CtrlAI intercepts LLM responses, evaluates tool calls against configurable
// guardrail rules, blocks dangerous ones, audits everything with a tamper-proof
// hash chain, and provides a kill switch — all with zero code changes to
// the upstream agent framework.
//
// Architecture overview:
//
//	SDK (OpenClaw) --> CtrlAI Proxy (:3100) --> LLM Provider (Anthropic/OpenAI)
//	                    |                          |
//	                    +-- buffer response --------+
//	                    |-- extract tool_use blocks
//	                    |-- evaluate against rules
//	                    |-- block/allow decision
//	                    |-- audit log (hash-chained)
//	                    +-- forward (modified or original) response to SDK
//
// CLI commands (cobra):
//
//	ctrlai              - Interactive first-run setup (TUI)
//	ctrlai start [-d]   - Start proxy (foreground or daemon)
//	ctrlai stop         - Stop proxy
//	ctrlai status       - Show proxy status + active agents
//	ctrlai agents       - List/inspect agents
//	ctrlai kill         - Kill an agent (emergency stop)
//	ctrlai revive       - Revive a killed agent
//	ctrlai rules        - Manage guardrail rules
//	ctrlai audit        - Query/verify the audit log
//	ctrlai config       - View/edit proxy configuration
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ctrlai/ctrlai/internal/agent"
	"github.com/ctrlai/ctrlai/internal/audit"
	"github.com/ctrlai/ctrlai/internal/config"
	"github.com/ctrlai/ctrlai/internal/dashboard"
	"github.com/ctrlai/ctrlai/internal/engine"
	"github.com/ctrlai/ctrlai/internal/proxy"
)

// Build-time variables injected via ldflags:
//
//	go build -ldflags "-X main.version=1.0.0 -X main.commit=abc123 -X main.buildDate=2026-02-10"
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// defaultConfigDir returns the path to ~/.ctrlai/ where all runtime state lives:
// config.yaml, rules.yaml, agents.yaml, killed.yaml, and the audit/ directory.
func defaultConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Fall back to current directory if home dir can't be determined.
		return ".ctrlai"
	}
	return filepath.Join(home, ".ctrlai")
}

// main is the entry point. It builds the cobra command tree and executes it.
// All commands share a common config directory (--config-dir flag on root).
func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// ============================================================================
// Root command
// ============================================================================

// configDir is the global flag for the CtrlAI config/state directory.
// Defaults to ~/.ctrlai/ but can be overridden for testing or custom setups.
var configDir string

// rootCmd is the top-level cobra command. When run with no subcommand,
// it launches the interactive first-run TUI setup (Phase 6 in design doc).
var rootCmd = &cobra.Command{
	Use:   "ctrlai",
	Short: "CtrlAI — Guardrail proxy for AI agents",
	Long: `CtrlAI is a transparent HTTP proxy that sits between your AI agent SDK
and the LLM provider. It intercepts LLM responses, evaluates tool calls
against configurable guardrail rules, blocks dangerous ones, audits
everything, and provides a kill switch.

Run 'ctrlai start' to start the proxy, or run 'ctrlai' with no arguments
for interactive first-run setup.`,
	Version: fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, buildDate),
	// When no subcommand is provided, run the first-time interactive setup.
	RunE: func(cmd *cobra.Command, args []string) error {
		return runFirstTimeSetup(cmd, args)
	},
}

func init() {
	// --config-dir: Override the default ~/.ctrlai/ directory.
	// This flag is persistent so all subcommands inherit it.
	rootCmd.PersistentFlags().StringVar(
		&configDir,
		"config-dir",
		defaultConfigDir(),
		"Path to CtrlAI config and state directory",
	)

	// Register all subcommands on the root command.
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(agentsCmd)
	rootCmd.AddCommand(killCmd)
	rootCmd.AddCommand(reviveCmd)
	rootCmd.AddCommand(rulesCmd)
	rootCmd.AddCommand(auditCmd)
	rootCmd.AddCommand(configCmd)
}

// ============================================================================
// ctrlai start — Start the proxy server
// ============================================================================

// daemonMode controls whether the proxy runs in the background (-d flag).
var daemonMode bool

// startCmd starts the CtrlAI proxy server. By default it runs in the
// foreground. With -d, it forks into the background as a daemon.
//
// The proxy listens on the host:port from config.yaml (default 127.0.0.1:3100)
// and serves both the HTTP proxy and the dashboard UI on the same port.
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the CtrlAI proxy server",
	Long: `Start the CtrlAI proxy server. The proxy intercepts LLM API calls,
evaluates tool calls against guardrail rules, and serves the dashboard.

By default runs in the foreground. Use -d for daemon/background mode.

The proxy binds to the address configured in ~/.ctrlai/config.yaml
(default: 127.0.0.1:3100). Both the proxy and the web dashboard are
served on this port:
  - Proxy: http://127.0.0.1:3100/provider/{provider}/agent/{agent}/{apiPath}
  - Dashboard: http://127.0.0.1:3100/dashboard`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStart(cmd, args)
	},
}

func init() {
	// -d / --daemon: Run in background mode.
	startCmd.Flags().BoolVarP(&daemonMode, "daemon", "d", false, "Run proxy in daemon/background mode")
}

// runStart initializes all subsystems and starts the HTTP server.
// This is where the entire CtrlAI stack gets wired together:
//
//  1. Handle daemon mode (re-exec as background process if -d)
//  2. Load config from ~/.ctrlai/config.yaml
//  3. Initialize the rule engine (loads rules.yaml + built-in rules)
//  4. Initialize the audit log (hash-chained JSONL + SQLite index)
//  5. Initialize the agent registry (agents.yaml + kill switch state)
//  6. Create the proxy server with tuned HTTP transport
//  7. Mount the dashboard on /dashboard (if enabled in config)
//  8. Write PID file for process management
//  9. Start listening and block until SIGINT/SIGTERM or HTTP shutdown
func runStart(cmd *cobra.Command, args []string) error {
	// --- Daemon mode ---
	// When -d is passed and we're NOT the re-exec'd child, we spawn a
	// detached child process and exit the parent. The child runs the proxy
	// in the background with stdout/stderr redirected to a log file.
	//
	// We use CTRLAI_DAEMONIZED=1 env var to distinguish the parent (which
	// re-execs and exits) from the child (which actually runs the proxy).
	// This is the standard Go daemonization pattern — Go can't fork() safely
	// because the runtime is multi-threaded.
	if daemonMode && os.Getenv("CTRLAI_DAEMONIZED") != "1" {
		return spawnDaemon()
	}

	// Ensure the config directory exists (~/.ctrlai/).
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("failed to create config directory %s: %w", configDir, err)
	}

	// --- Step 1: Load configuration ---
	// config.yaml defines server bind address, upstream provider URLs,
	// streaming buffer settings, and dashboard toggle.
	cfg, err := config.Load(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// --- Step 2: Initialize the guardrail rule engine ---
	// The engine loads custom rules from rules.yaml and merges them with
	// built-in security rules (block SSH keys, .env files, etc.).
	// All tool name matching is case-insensitive to handle OAuth PascalCase.
	ruleEngine, err := engine.New(filepath.Join(configDir, "rules.yaml"))
	if err != nil {
		return fmt.Errorf("failed to initialize rule engine: %w", err)
	}
	fmt.Printf("[ctrlai] Loaded %d rules (%d builtin + %d custom)\n",
		ruleEngine.TotalRules(), ruleEngine.BuiltinCount(), ruleEngine.CustomCount())

	// --- Step 3: Initialize the audit log ---
	// The audit log is a hash-chained append-only JSONL file with a SQLite
	// index for fast queries. Each entry's hash = SHA-256(prev_hash + seq +
	// timestamp + agent + tool + decision). Tampering breaks the chain.
	auditDir := filepath.Join(configDir, "audit")
	auditLog, err := audit.New(auditDir)
	if err != nil {
		return fmt.Errorf("failed to initialize audit log: %w", err)
	}
	defer auditLog.Close()

	// Log proxy startup as a lifecycle event in the audit chain.
	auditLog.LogLifecycle("proxy_start", map[string]any{
		"version": version,
		"commit":  commit,
		"host":    cfg.Server.Host,
		"port":    cfg.Server.Port,
	})

	// --- Step 4: Initialize agent registry + kill switch ---
	// The agent registry auto-discovers agents on first request (by reading
	// the agent ID from the URL path). The kill switch checks killed.yaml
	// which is file-watched for live updates.
	registry, err := agent.NewRegistry(filepath.Join(configDir, "agents.yaml"))
	if err != nil {
		return fmt.Errorf("failed to initialize agent registry: %w", err)
	}

	killSwitch, err := agent.NewKillSwitch(filepath.Join(configDir, "killed.yaml"))
	if err != nil {
		return fmt.Errorf("failed to initialize kill switch: %w", err)
	}

	// --- Step 5: Create the proxy server with tuned HTTP transport ---
	// The upstream HTTP client is tuned for low-latency LLM proxying:
	//   - Connection pooling: reuse TCP connections to upstream LLM providers
	//     instead of dialing a new connection per request. We talk to very few
	//     upstreams (1-3 providers), so we set MaxIdleConnsPerHost high.
	//   - Keep-alive: enabled by default in Go, but we set IdleConnTimeout
	//     high since LLM requests are bursty (agent loop fires every few secs).
	//   - Compression disabled: we need to parse raw SSE bytes from the
	//     upstream. If we accept gzip, we'd have to decompress before parsing
	//     tool_use blocks, adding latency. The LLM response is text — minimal
	//     compression benefit vs the CPU cost.
	//   - HTTP/2: enabled for multiplexing — a single TCP connection can
	//     carry multiple concurrent requests to the same upstream.
	//   - No client timeout: streaming responses from LLMs can take minutes
	//     for long reasoning chains. The proxy's own bufferTimeoutMs (from
	//     config) handles stuck streams at the SSE level.
	//
	// Design doc Section 17 targets:
	//   - Non-streaming proxy overhead: < 2ms
	//   - Streaming buffer overhead: 0ms (we wait for LLM anyway)
	//   - Rule evaluation: < 50us per tool call
	upstreamTransport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     120 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
	}
	upstreamClient := &http.Client{
		Transport: upstreamTransport,
		// No Timeout — streaming responses can run for minutes.
		// The proxy's bufferTimeoutMs handles stuck/hung streams.
	}

	// The proxy is a standard http.Handler that:
	//   - Parses the URL to extract provider, agent ID, and API path
	//   - Checks the kill switch before forwarding
	//   - Forwards the request to the upstream LLM provider
	//   - Buffers the streaming response (SSE events)
	//   - Extracts tool_use blocks from the response
	//   - Evaluates each tool call against the rule engine
	//   - Modifies blocked responses (strip tool_use, change stop_reason)
	//   - Logs everything to the audit chain
	// --- Step 5b: Create the dashboard (before proxy, so we can wire broadcast) ---
	var dash *dashboard.Dashboard
	if cfg.Dashboard.Enabled {
		dash = dashboard.New(dashboard.Options{
			AuditLog:   auditLog,
			Registry:   registry,
			KillSwitch: killSwitch,
			Engine:     ruleEngine,
			RulesPath:  filepath.Join(configDir, "rules.yaml"),
		})
	}

	// Wire the proxy with an optional audit event broadcast callback
	// so the dashboard WebSocket live feed receives events in real time.
	proxyOpts := proxy.Options{
		Config:         cfg,
		Engine:         ruleEngine,
		AuditLog:       auditLog,
		Registry:       registry,
		KillSwitch:     killSwitch,
		UpstreamClient: upstreamClient,
	}
	if dash != nil {
		proxyOpts.OnAuditEvent = func(e audit.Entry) {
			dash.BroadcastEvent(e)
		}
	}
	proxyServer := proxy.New(proxyOpts)

	// --- Step 6: Set up HTTP mux ---
	// The proxy and dashboard share the same port. The mux routes:
	//   /provider/* -> proxy handler (intercepts LLM API calls)
	//   /dashboard* -> dashboard handler (web UI + WebSocket feed)
	//   /api/*      -> dashboard REST API (status, agents, rules, audit)
	//   /health     -> health check (used by `ctrlai status`)
	//   /shutdown   -> graceful shutdown trigger (used by `ctrlai stop`)
	mux := http.NewServeMux()

	// Mount the proxy handler for all provider routes.
	// URL format: /provider/{providerKey}/agent/{agentId}/{apiPath...}
	mux.Handle("/provider/", proxyServer)

	// Mount the dashboard if enabled in config.
	if dash != nil {
		// Dashboard web UI (static HTML/JS/CSS embedded in binary).
		mux.Handle("/dashboard", dash)
		mux.Handle("/dashboard/", dash)
		// Dashboard WebSocket endpoint for live activity feed.
		mux.Handle("/dashboard/ws", dash.WebSocketHandler())
		// Dashboard REST API endpoints.
		mux.Handle("/api/", dash.APIHandler())
	}

	// Health check endpoint — used by `ctrlai status` to detect running proxy.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","version":"%s"}`, version)
	})

	// Shutdown endpoint — used by `ctrlai stop` to trigger graceful shutdown.
	// This is the cross-platform way to stop the proxy (works on Windows
	// where Unix signals like SIGTERM are not available).
	// Only accepts POST from loopback addresses to prevent remote shutdown.
	shutdownCh := make(chan struct{}, 1)
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// Security: only accept shutdown requests from localhost.
		remoteIP := r.RemoteAddr
		if !isLoopback(remoteIP) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"shutting_down"}`)
		// Signal the main goroutine to begin graceful shutdown.
		select {
		case shutdownCh <- struct{}{}:
		default:
			// Already shutting down.
		}
	})

	// --- Step 7: Start the HTTP server ---
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout or ReadTimeout — streaming responses from LLMs
		// can take minutes for long reasoning chains. The proxy's own
		// bufferTimeoutMs (default 30s from config) handles stuck streams
		// at the SSE level, not the HTTP level.
	}

	// --- Step 8: Write PID file ---
	// The PID file allows `ctrlai stop` to find the running process.
	// Cleaned up on graceful shutdown.
	pidFile := filepath.Join(configDir, "ctrlai.pid")
	if err := writePIDFile(pidFile); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}
	defer removePIDFile(pidFile)

	// --- Step 9: Start config file watcher for hot-reload ---
	// The watcher monitors rules.yaml and killed.yaml for changes.
	// When rules.yaml changes, the rule engine reloads automatically.
	// When killed.yaml changes, the kill switch state updates live.
	// This is what makes `ctrlai kill` take effect instantly without
	// restarting the proxy — the CLI writes killed.yaml, the watcher
	// picks up the change, and the kill switch state updates in memory.
	watcher, err := config.NewWatcher(configDir, config.WatchTargets{
		OnRulesChange: func() {
			if reloadErr := ruleEngine.Reload(filepath.Join(configDir, "rules.yaml")); reloadErr != nil {
				fmt.Fprintf(os.Stderr, "[ctrlai] Warning: failed to reload rules: %v\n", reloadErr)
			} else {
				fmt.Println("[ctrlai] Rules reloaded")
			}
		},
		OnKillSwitchChange: func() {
			if reloadErr := killSwitch.Reload(); reloadErr != nil {
				fmt.Fprintf(os.Stderr, "[ctrlai] Warning: failed to reload kill switch: %v\n", reloadErr)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to start config watcher: %w", err)
	}
	defer watcher.Close()

	// --- Step 10: Graceful shutdown on SIGINT/SIGTERM or HTTP /shutdown ---
	// Three ways the proxy can shut down:
	//   1. SIGINT (Ctrl+C) — user stops foreground process
	//   2. SIGTERM — sent by `ctrlai stop` on Unix via PID file
	//   3. POST /shutdown — sent by `ctrlai stop` cross-platform via HTTP
	// All three trigger the same graceful shutdown path: drain in-flight
	// requests, flush audit log, close SQLite, persist agent stats.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start listening in a goroutine so we can block on the signal context.
	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("[ctrlai] Proxy listening on http://%s\n", addr)
		if cfg.Dashboard.Enabled {
			fmt.Printf("[ctrlai] Dashboard at http://%s/dashboard\n", addr)
		}
		if !daemonMode {
			fmt.Println("[ctrlai] Press Ctrl+C to stop")
		}
		errCh <- server.ListenAndServe()
	}()

	// Block until shutdown signal, HTTP shutdown request, or server error.
	select {
	case <-ctx.Done():
		// OS signal received (SIGINT or SIGTERM).
		fmt.Println("\n[ctrlai] Shutting down (signal received)...")
	case <-shutdownCh:
		// HTTP /shutdown endpoint was called by `ctrlai stop`.
		fmt.Println("[ctrlai] Shutting down (stop command received)...")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("server error: %w", err)
		}
	}

	// Graceful shutdown — give in-flight requests 10 seconds to drain.
	// This is important for streaming responses that are mid-flight:
	// the SDK gets a chance to receive the complete SSE stream.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		fmt.Fprintf(os.Stderr, "[ctrlai] Shutdown error: %v\n", shutdownErr)
	}

	// Log proxy shutdown in the audit chain.
	auditLog.LogLifecycle("proxy_stop", nil)

	// Persist agent stats to disk before exiting.
	if saveErr := registry.Save(); saveErr != nil {
		fmt.Fprintf(os.Stderr, "[ctrlai] Warning: failed to save agent registry: %v\n", saveErr)
	}

	fmt.Println("[ctrlai] Stopped")
	return nil
}

// spawnDaemon re-executes the ctrlai binary as a detached background process.
// The parent process prints the child PID and exits immediately.
//
// How it works:
//  1. Find our own executable path
//  2. Build the same command but with CTRLAI_DAEMONIZED=1 env var
//  3. Redirect stdout/stderr to ~/.ctrlai/ctrlai.log
//  4. Start the child process detached from the terminal
//  5. Print the PID and exit
//
// The child process detects CTRLAI_DAEMONIZED=1 at the top of runStart()
// and skips the re-exec, running the proxy normally.
func spawnDaemon() error {
	// Ensure config dir exists for the log file.
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Find our own executable.
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find executable path: %w", err)
	}

	// Open the log file for daemon stdout/stderr.
	logPath := filepath.Join(configDir, "ctrlai.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open log file %s: %w", logPath, err)
	}

	// Build the command: same binary, "start" subcommand (without -d),
	// with the daemonized env var so the child doesn't re-exec again.
	daemonArgs := []string{"start"}
	// Forward --config-dir if it was explicitly set.
	if configDir != defaultConfigDir() {
		daemonArgs = append(daemonArgs, "--config-dir", configDir)
	}

	child := exec.Command(exePath, daemonArgs...)
	child.Stdout = logFile
	child.Stderr = logFile
	child.Env = append(os.Environ(), "CTRLAI_DAEMONIZED=1")

	// Start the child process. It inherits no stdin and writes to the log.
	if err := child.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	fmt.Printf("[ctrlai] Proxy started in background (PID %d)\n", child.Process.Pid)
	fmt.Printf("[ctrlai] Log file: %s\n", logPath)
	fmt.Println("[ctrlai] Use 'ctrlai stop' to stop the proxy")

	// Release the child process so it survives parent exit.
	// We don't call child.Wait() — the child is now independent.
	if err := child.Process.Release(); err != nil {
		// Non-fatal — child is already running.
		fmt.Fprintf(os.Stderr, "[ctrlai] Warning: failed to release child process: %v\n", err)
	}

	logFile.Close()
	return nil
}

// writePIDFile writes the current process ID to the given file path.
// Used by `ctrlai stop` to find the running proxy process.
func writePIDFile(path string) error {
	pid := os.Getpid()
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}

// removePIDFile removes the PID file if it exists. Called on shutdown.
func removePIDFile(path string) {
	os.Remove(path)
}

// isLoopback checks if a remote address is a loopback address (127.x.x.x or ::1).
// Used to restrict the /shutdown endpoint to local-only access.
func isLoopback(remoteAddr string) bool {
	// remoteAddr is "ip:port" — strip the port.
	host := remoteAddr
	if idx := strings.LastIndex(remoteAddr, ":"); idx != -1 {
		host = remoteAddr[:idx]
	}
	// Strip brackets from IPv6 addresses like [::1].
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")

	return host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "127.")
}

// ============================================================================
// ctrlai stop — Stop the proxy server
// ============================================================================

// stopCmd sends a stop signal to a running CtrlAI proxy.
//
// Uses two strategies (in order):
//  1. HTTP POST to /shutdown — works cross-platform (Windows + Unix)
//  2. PID file + SIGTERM — Unix fallback if HTTP fails
//
// On Windows, only the HTTP strategy works because Windows doesn't have
// Unix signals. The /shutdown endpoint is restricted to localhost.
var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running CtrlAI proxy",
	Long: `Stop a running CtrlAI proxy. Tries HTTP shutdown first (cross-platform),
then falls back to PID file + SIGTERM on Unix systems.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStop(cmd, args)
	},
}

// runStop attempts to stop the running proxy via HTTP, then falls back to
// PID-based signal delivery on Unix.
func runStop(cmd *cobra.Command, args []string) error {
	// Load config to determine the proxy address.
	cfg, err := config.Load(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	addr := fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port)

	// --- Strategy 1: HTTP shutdown (cross-platform) ---
	// POST to /shutdown on the running proxy. This is the primary method
	// and works on all platforms including Windows.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(addr+"/shutdown", "application/json", nil)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Println("[ctrlai] Stop signal sent to proxy")
			// Clean up PID file since proxy is shutting down.
			os.Remove(filepath.Join(configDir, "ctrlai.pid"))
			return nil
		}
	}

	// --- Strategy 2: PID file + SIGTERM (Unix only) ---
	// If HTTP failed (proxy might be hung, or /shutdown not reachable),
	// try to send SIGTERM via the PID file. This only works on Unix.
	if runtime.GOOS == "windows" {
		return fmt.Errorf("proxy is not responding at %s — cannot stop", addr)
	}

	pidFile := filepath.Join(configDir, "ctrlai.pid")
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("proxy is not running (no PID file and HTTP unreachable)")
		}
		return fmt.Errorf("failed to read PID file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		return fmt.Errorf("invalid PID in %s: %w", pidFile, err)
	}

	// Find the process and send SIGTERM for graceful shutdown.
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process %d: %w", pid, err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		// Process might already be dead — clean up PID file.
		os.Remove(pidFile)
		return fmt.Errorf("failed to stop proxy (PID %d): %w", pid, err)
	}

	// Clean up the PID file after successful signal delivery.
	os.Remove(pidFile)
	fmt.Printf("[ctrlai] Sent stop signal to proxy (PID %d)\n", pid)
	return nil
}

// ============================================================================
// ctrlai status — Show proxy status
// ============================================================================

// statusCmd displays the current proxy status: whether it's running, which
// port it's listening on, and a summary of active/killed agents.
//
// Queries the running proxy via HTTP (/health and /api/agents) to get
// live in-memory state rather than reading stale files from disk.
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show proxy status and active agents",
	Long: `Display whether the CtrlAI proxy is running, its listen address, and a
summary of all known agents with their current status (active/killed).

Queries the live proxy process for accurate real-time data.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStatus(cmd, args)
	},
}

// statusAgentJSON is the JSON schema returned by GET /api/agents on the
// running proxy. We only decode the fields we need for display.
type statusAgentJSON struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Stats    struct {
		TotalRequests    uint64 `json:"total_requests"`
		TotalToolCalls   uint64 `json:"total_tool_calls"`
		BlockedToolCalls uint64 `json:"blocked_tool_calls"`
	} `json:"stats"`
}

// runStatus queries the live proxy via HTTP for status and agent data.
func runStatus(cmd *cobra.Command, args []string) error {
	// Load config for the listen address.
	cfg, err := config.Load(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	addr := fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port)
	client := &http.Client{Timeout: 2 * time.Second}

	// Check if the proxy is reachable via the health endpoint.
	resp, err := client.Get(addr + "/health")
	if err != nil {
		fmt.Println("[ctrlai] Status: NOT RUNNING")
		fmt.Printf("[ctrlai] Expected at: %s\n", addr)
		return nil
	}
	resp.Body.Close()

	fmt.Println("[ctrlai] Status: RUNNING")
	fmt.Printf("[ctrlai] Listening on: %s\n", addr)

	// Query the live proxy for agent data via the dashboard API.
	// This gives us the accurate in-memory state (request counts, last seen,
	// etc.) rather than the stale-on-disk agents.yaml.
	agentsResp, err := client.Get(addr + "/api/agents")
	if err != nil {
		// Proxy is running but dashboard API might be disabled.
		fmt.Println("[ctrlai] Could not query agent data (dashboard API may be disabled)")
		return nil
	}
	defer agentsResp.Body.Close()

	body, err := io.ReadAll(agentsResp.Body)
	if err != nil {
		fmt.Println("[ctrlai] Could not read agent data")
		return nil
	}

	var agents []statusAgentJSON
	if err := json.Unmarshal(body, &agents); err != nil {
		// API might return a different envelope — try to display raw.
		fmt.Println("[ctrlai] Could not parse agent data")
		return nil
	}

	if len(agents) == 0 {
		fmt.Println("[ctrlai] No agents registered yet")
		return nil
	}

	fmt.Printf("[ctrlai] Agents: %d total\n", len(agents))
	fmt.Println()
	fmt.Printf("  %-15s %-10s %-12s %-30s %-8s %-8s %-8s\n",
		"AGENT", "STATUS", "PROVIDER", "MODEL", "REQS", "TOOLS", "BLOCKED")
	fmt.Printf("  %-15s %-10s %-12s %-30s %-8s %-8s %-8s\n",
		"-----", "------", "--------", "-----", "----", "-----", "-------")
	for _, a := range agents {
		fmt.Printf("  %-15s %-10s %-12s %-30s %-8d %-8d %-8d\n",
			a.ID, a.Status, a.Provider, a.Model,
			a.Stats.TotalRequests, a.Stats.TotalToolCalls, a.Stats.BlockedToolCalls)
	}
	return nil
}

// ============================================================================
// ctrlai agents — List and inspect agents
// ============================================================================

// agentsCmd lists all known agents, or shows details for a specific agent.
// Agents are auto-discovered on their first request through the proxy.
// The agent ID comes from the URL path: /provider/{p}/agent/{agentId}/...
var agentsCmd = &cobra.Command{
	Use:   "agents [agent-id]",
	Short: "List all agents or show details for a specific agent",
	Long: `List all agents that have been seen by the proxy, with their status,
provider, model, and stats. Optionally provide an agent ID to see
detailed information for that specific agent.

Agents are auto-registered when their first request passes through
the proxy. The agent ID is extracted from the URL path.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgents(cmd, args)
	},
}

// runAgents displays agent information from the registry file.
func runAgents(cmd *cobra.Command, args []string) error {
	registry, err := agent.NewRegistry(filepath.Join(configDir, "agents.yaml"))
	if err != nil {
		return fmt.Errorf("failed to load agent registry: %w", err)
	}

	// If a specific agent ID was provided, show detailed info.
	if len(args) == 1 {
		agentID := args[0]
		a, err := registry.Get(agentID)
		if err != nil {
			return fmt.Errorf("agent %q not found", agentID)
		}
		fmt.Printf("Agent: %s\n", a.ID)
		fmt.Printf("  Status:     %s\n", a.Status)
		fmt.Printf("  Provider:   %s\n", a.Provider)
		fmt.Printf("  Model:      %s\n", a.Model)
		fmt.Printf("  First seen: %s\n", a.FirstSeen.Format(time.RFC3339))
		fmt.Printf("  Last seen:  %s\n", a.LastSeen.Format(time.RFC3339))
		fmt.Printf("  Requests:   %d\n", a.Stats.TotalRequests)
		fmt.Printf("  Tool calls: %d\n", a.Stats.TotalToolCalls)
		fmt.Printf("  Blocked:    %d\n", a.Stats.BlockedToolCalls)
		return nil
	}

	// No agent ID — list all agents.
	agents := registry.List()
	if len(agents) == 0 {
		fmt.Println("No agents registered yet. Start the proxy and send a request to register agents.")
		return nil
	}

	fmt.Printf("%-15s %-10s %-12s %-30s %-8s %-8s %-8s\n",
		"AGENT", "STATUS", "PROVIDER", "MODEL", "REQS", "TOOLS", "BLOCKED")
	fmt.Printf("%-15s %-10s %-12s %-30s %-8s %-8s %-8s\n",
		"-----", "------", "--------", "-----", "----", "-----", "-------")
	for _, a := range agents {
		fmt.Printf("%-15s %-10s %-12s %-30s %-8d %-8d %-8d\n",
			a.ID, a.Status, a.Provider, a.Model,
			a.Stats.TotalRequests, a.Stats.TotalToolCalls, a.Stats.BlockedToolCalls)
	}
	return nil
}

// ============================================================================
// ctrlai kill — Kill an agent (emergency stop)
// ============================================================================

// killReason is the human-readable reason for killing an agent.
var killReason string

// killAll controls whether all agents should be killed at once (--all flag).
var killAll bool

// killCmd immediately kills an agent by adding it to killed.yaml.
// Once killed, all subsequent requests from this agent get a fake "end_turn"
// response — the agent's loop stops and no more LLM calls go through.
// The running proxy picks up the change via file watching on killed.yaml.
var killCmd = &cobra.Command{
	Use:   "kill <agent-id>",
	Short: "Kill an agent (emergency stop)",
	Long: `Immediately kill an agent by its ID. All subsequent LLM requests from
this agent will receive a fake "end_turn" response, causing the agent's
loop to stop. No requests are forwarded to the LLM provider.

Use --all to kill every known agent at once (emergency shutdown).
Use --reason to provide a human-readable explanation for the kill.
The kill can be reversed with 'ctrlai revive <agent-id>'.

Takes effect immediately — the running proxy file-watches killed.yaml.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runKill(cmd, args)
	},
}

func init() {
	killCmd.Flags().StringVar(&killReason, "reason", "", "Reason for killing the agent (required)")
	killCmd.Flags().BoolVar(&killAll, "all", false, "Kill all agents")
	// Reason is required — we always want to know why an agent was killed.
	killCmd.MarkFlagRequired("reason")
}

// runKill adds an agent (or all agents) to the kill switch file.
// The proxy file-watches killed.yaml, so this takes effect immediately
// without restarting the proxy.
func runKill(cmd *cobra.Command, args []string) error {
	killSwitch, err := agent.NewKillSwitch(filepath.Join(configDir, "killed.yaml"))
	if err != nil {
		return fmt.Errorf("failed to load kill switch: %w", err)
	}

	if killAll {
		// Kill every agent in the registry.
		registry, err := agent.NewRegistry(filepath.Join(configDir, "agents.yaml"))
		if err != nil {
			return fmt.Errorf("failed to load agent registry: %w", err)
		}
		agents := registry.List()
		if len(agents) == 0 {
			return fmt.Errorf("no agents registered — nothing to kill")
		}
		for _, a := range agents {
			if err := killSwitch.Kill(a.ID, killReason, "user"); err != nil {
				fmt.Fprintf(os.Stderr, "[ctrlai] Warning: failed to kill agent %q: %v\n", a.ID, err)
			} else {
				fmt.Printf("[ctrlai] Killed agent: %s\n", a.ID)
			}
		}
		return nil
	}

	// Kill a single agent by ID.
	if len(args) == 0 {
		return fmt.Errorf("provide an agent ID or use --all")
	}
	agentID := args[0]
	if err := killSwitch.Kill(agentID, killReason, "user"); err != nil {
		return fmt.Errorf("failed to kill agent %q: %w", agentID, err)
	}
	fmt.Printf("[ctrlai] Killed agent: %s (reason: %s)\n", agentID, killReason)
	return nil
}

// ============================================================================
// ctrlai revive — Revive a killed agent
// ============================================================================

// reviveCmd removes an agent from the kill switch, allowing its requests
// to flow through to the LLM provider again.
// Reviving all agents at once is not supported as each agent should be
// thoroughly reviewed before being revived so do it one by one.
var reviveCmd = &cobra.Command{
	Use:   "revive <agent-id>",
	Short: "Revive a killed agent",
	Long: `Revive a previously killed agent, allowing its LLM requests to flow
through the proxy again. The agent's kill entry is removed from
killed.yaml and the proxy picks up the change via file watching.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRevive(cmd, args)
	},
}

// runRevive removes an agent from the kill list.
func runRevive(cmd *cobra.Command, args []string) error {
	agentID := args[0]

	killSwitch, err := agent.NewKillSwitch(filepath.Join(configDir, "killed.yaml"))
	if err != nil {
		return fmt.Errorf("failed to load kill switch: %w", err)
	}

	if err := killSwitch.Revive(agentID); err != nil {
		return fmt.Errorf("failed to revive agent %q: %w", agentID, err)
	}

	fmt.Printf("[ctrlai] Revived agent: %s\n", agentID)
	return nil
}

// ============================================================================
// ctrlai rules — Manage guardrail rules
// ============================================================================

// rulesCmd is the parent command for rule management subcommands.
// Rules define what tool calls are blocked or allowed for each agent.
var rulesCmd = &cobra.Command{
	Use:   "rules",
	Short: "Manage guardrail rules",
	Long: `View, add, remove, and test guardrail rules. Rules define which tool
calls are blocked or allowed. They support matching on tool name (case-
insensitive), action field, agent ID, file path globs, argument substrings,
command regexes, and URL regexes.

Built-in rules are always active and cover common security patterns like
blocking SSH key access, .env files, and credential exfiltration.`,
}

func init() {
	rulesCmd.AddCommand(rulesListCmd)
	rulesCmd.AddCommand(rulesAddCmd)
	rulesCmd.AddCommand(rulesRemoveCmd)
	rulesCmd.AddCommand(rulesTestCmd)
}

// rulesListCmd shows all active rules (both built-in and custom).
var rulesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all rules (builtin + custom)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ruleEngine, err := engine.New(filepath.Join(configDir, "rules.yaml"))
		if err != nil {
			return fmt.Errorf("failed to load rules: %w", err)
		}

		rules := ruleEngine.ListRules()
		if len(rules) == 0 {
			fmt.Println("No rules configured.")
			return nil
		}

		fmt.Printf("%-25s %-10s %-10s %s\n", "NAME", "TYPE", "ACTION", "DESCRIPTION")
		fmt.Printf("%-25s %-10s %-10s %s\n", "----", "----", "------", "-----------")
		for _, r := range rules {
			ruleType := "custom"
			if r.Builtin {
				ruleType = "builtin"
			}
			fmt.Printf("%-25s %-10s %-10s %s\n", r.Name, ruleType, r.Action, r.Message)
		}
		return nil
	},
}

// rulesAddCmd adds a new custom rule from a YAML string argument.
// Example: ctrlai rules add 'name: block-curl\nmatch:\n  tool: exec\n  command_regex: "curl.*"'
var rulesAddCmd = &cobra.Command{
	Use:   "add <yaml>",
	Short: "Add a custom rule (YAML format)",
	Long: `Add a new custom guardrail rule. Provide the rule as a YAML string.

Example:
  ctrlai rules add 'name: block-curl
    match:
      tool: exec
      command_regex: "curl.*"
    action: block
    message: "curl commands are not allowed"'`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ruleEngine, err := engine.New(filepath.Join(configDir, "rules.yaml"))
		if err != nil {
			return fmt.Errorf("failed to load rules: %w", err)
		}

		if err := ruleEngine.AddRule(args[0]); err != nil {
			return fmt.Errorf("failed to add rule: %w", err)
		}

		// Persist the updated rules back to rules.yaml.
		if err := ruleEngine.Save(filepath.Join(configDir, "rules.yaml")); err != nil {
			return fmt.Errorf("failed to save rules: %w", err)
		}

		fmt.Println("[ctrlai] Rule added successfully")
		return nil
	},
}

// rulesRemoveCmd removes a custom rule by name.
var rulesRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a custom rule by name",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ruleEngine, err := engine.New(filepath.Join(configDir, "rules.yaml"))
		if err != nil {
			return fmt.Errorf("failed to load rules: %w", err)
		}

		if err := ruleEngine.RemoveRule(args[0]); err != nil {
			return fmt.Errorf("failed to remove rule: %w", err)
		}

		if err := ruleEngine.Save(filepath.Join(configDir, "rules.yaml")); err != nil {
			return fmt.Errorf("failed to save rules: %w", err)
		}

		fmt.Printf("[ctrlai] Rule %q removed\n", args[0])
		return nil
	},
}

// rulesTestCmd tests a tool call JSON against the current rule set.
// This lets users verify rules without running a live agent.
// Example: ctrlai rules test '{"name":"exec","arguments":{"command":"cat /etc/passwd"}}'
var rulesTestCmd = &cobra.Command{
	Use:   "test <json>",
	Short: "Test a tool call against rules",
	Long: `Test a tool call JSON string against the current rule set to see
whether it would be blocked or allowed. Useful for verifying rules.

Example:
  ctrlai rules test '{"name":"exec","arguments":{"command":"cat /etc/passwd"}}'`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ruleEngine, err := engine.New(filepath.Join(configDir, "rules.yaml"))
		if err != nil {
			return fmt.Errorf("failed to load rules: %w", err)
		}

		decision, err := ruleEngine.TestJSON(args[0])
		if err != nil {
			return fmt.Errorf("failed to test tool call: %w", err)
		}

		if decision.Action == "block" {
			fmt.Printf("[ctrlai] BLOCKED by rule %q: %s\n", decision.Rule, decision.Message)
		} else {
			fmt.Println("[ctrlai] ALLOWED (no rule matched)")
		}
		return nil
	},
}

// ============================================================================
// ctrlai audit — Query and verify the audit log
// ============================================================================

// auditCmd is the parent command for audit log operations.
// The audit log is a tamper-proof, hash-chained JSONL file stored in
// ~/.ctrlai/audit/ with a SQLite index for fast queries.
var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Query and verify the audit log",
	Long: `The audit log records every tool call that passes through the proxy,
including the decision (allow/block), the matched rule, timestamps,
and agent identity. Entries are hash-chained: each entry's hash depends
on the previous entry, making tampering detectable.`,
}

// auditFollowMode enables real-time following of new audit entries (-f flag).
var auditFollowMode bool

// auditTailLimit controls how many recent entries to show.
var auditTailLimit int

func init() {
	auditCmd.AddCommand(auditTailCmd)
	auditCmd.AddCommand(auditQueryCmd)
	auditCmd.AddCommand(auditVerifyCmd)
	auditCmd.AddCommand(auditExportCmd)
}

// auditTailCmd shows recent audit entries, optionally following in real-time.
var auditTailCmd = &cobra.Command{
	Use:   "tail",
	Short: "Show recent audit entries",
	Long:  `Show the most recent audit log entries. Use -f to follow in real-time (like tail -f).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		auditDir := filepath.Join(configDir, "audit")
		auditLog, err := audit.New(auditDir)
		if err != nil {
			return fmt.Errorf("failed to open audit log: %w", err)
		}
		defer auditLog.Close()

		entries, err := auditLog.Tail(auditTailLimit)
		if err != nil {
			return fmt.Errorf("failed to read audit log: %w", err)
		}

		for _, entry := range entries {
			printAuditEntry(entry)
		}

		// If -f flag is set, keep watching for new entries.
		if auditFollowMode {
			return auditLog.Follow(context.Background(), func(entry audit.Entry) {
				printAuditEntry(entry)
			})
		}
		return nil
	},
}

func init() {
	auditTailCmd.Flags().BoolVarP(&auditFollowMode, "follow", "f", false, "Follow new entries in real-time")
	auditTailCmd.Flags().IntVarP(&auditTailLimit, "limit", "n", 20, "Number of recent entries to show")
}

// Audit query filter flags.
var (
	auditQueryAgent    string
	auditQueryDecision string
	auditQuerySince    string
	auditQueryLimit    int
)

// auditQueryCmd queries the audit log with filters (agent, decision, time range).
var auditQueryCmd = &cobra.Command{
	Use:   "query",
	Short: "Query audit entries with filters",
	Long: `Query the audit log with filters. Supports filtering by agent ID,
decision (allow/block), and time range.

Examples:
  ctrlai audit query --agent main --decision block --since 1h
  ctrlai audit query --agent work --limit 100`,
	RunE: func(cmd *cobra.Command, args []string) error {
		auditDir := filepath.Join(configDir, "audit")
		auditLog, err := audit.New(auditDir)
		if err != nil {
			return fmt.Errorf("failed to open audit log: %w", err)
		}
		defer auditLog.Close()

		entries, err := auditLog.Query(audit.QueryParams{
			Agent:    auditQueryAgent,
			Decision: auditQueryDecision,
			Since:    auditQuerySince,
			Limit:    auditQueryLimit,
		})
		if err != nil {
			return fmt.Errorf("audit query failed: %w", err)
		}

		if len(entries) == 0 {
			fmt.Println("No matching audit entries found.")
			return nil
		}

		for _, entry := range entries {
			printAuditEntry(entry)
		}
		fmt.Printf("\n%d entries found.\n", len(entries))
		return nil
	},
}

func init() {
	auditQueryCmd.Flags().StringVar(&auditQueryAgent, "agent", "", "Filter by agent ID")
	auditQueryCmd.Flags().StringVar(&auditQueryDecision, "decision", "", "Filter by decision (allow/block)")
	auditQueryCmd.Flags().StringVar(&auditQuerySince, "since", "", "Show entries since duration (e.g., 1h, 30m, 24h)")
	auditQueryCmd.Flags().IntVar(&auditQueryLimit, "limit", 50, "Maximum number of entries to return")
}

// auditVerifyCmd verifies the integrity of the hash chain. Each entry's
// hash depends on the previous entry's hash, so any tampering breaks the
// chain from that point forward.
var auditVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify hash chain integrity",
	Long: `Verify the integrity of the audit log hash chain. Each entry's hash
is computed as SHA-256(prev_hash | seq | timestamp | agent | tool | decision).
If any entry has been tampered with, the chain breaks and this command
reports where the inconsistency was detected.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		auditDir := filepath.Join(configDir, "audit")
		auditLog, err := audit.New(auditDir)
		if err != nil {
			return fmt.Errorf("failed to open audit log: %w", err)
		}
		defer auditLog.Close()

		result, err := auditLog.VerifyChain()
		if err != nil {
			return fmt.Errorf("verification failed: %w", err)
		}

		if result.Valid {
			fmt.Printf("[ctrlai] Hash chain VALID (%d entries verified)\n", result.EntriesChecked)
		} else {
			fmt.Printf("[ctrlai] Hash chain BROKEN at entry #%d\n", result.BrokenAt)
			fmt.Printf("  Expected hash: %s\n", result.ExpectedHash)
			fmt.Printf("  Actual hash:   %s\n", result.ActualHash)
			return fmt.Errorf("audit chain integrity violation detected")
		}
		return nil
	},
}

// auditExportFormat controls the export output format (csv, json, jsonl).
var auditExportFormat string

// auditExportCmd exports the full audit log to a file in the specified format.
var auditExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export audit log",
	Long: `Export the full audit log to stdout in the specified format.
Supported formats: csv, json, jsonl.

Example:
  ctrlai audit export --format csv > audit_export.csv`,
	RunE: func(cmd *cobra.Command, args []string) error {
		auditDir := filepath.Join(configDir, "audit")
		auditLog, err := audit.New(auditDir)
		if err != nil {
			return fmt.Errorf("failed to open audit log: %w", err)
		}
		defer auditLog.Close()

		return auditLog.Export(os.Stdout, auditExportFormat)
	},
}

func init() {
	auditExportCmd.Flags().StringVar(&auditExportFormat, "format", "jsonl", "Export format: csv, json, jsonl")
}

// printAuditEntry formats and prints a single audit entry to stdout.
func printAuditEntry(e audit.Entry) {
	decision := e.Decision
	// Uppercase blocked decisions for terminal visibility.
	if decision == "block" {
		decision = "BLOCK"
	}
	if e.Tool != "" {
		fmt.Printf("[%s] agent=%-10s tool=%-12s decision=%-6s rule=%s\n",
			e.Timestamp, e.Agent, e.Tool, decision, e.Rule)
	} else {
		fmt.Printf("[%s] agent=%-10s type=%-12s decision=%s\n",
			e.Timestamp, e.Agent, e.Type, decision)
	}
}

// ============================================================================
// ctrlai config — Configuration management
// ============================================================================

// configCmd is the parent command for configuration operations.
var configCmd = &cobra.Command{
	Use:   "config",
	Short: "View and edit proxy configuration",
	Long: `Manage the CtrlAI proxy configuration. The config file lives at
~/.ctrlai/config.yaml and defines the server bind address, upstream
LLM provider URLs, streaming buffer settings, and dashboard toggle.`,
}

func init() {
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configEditCmd)
	configCmd.AddCommand(configGenerateCmd)
}

// configShowCmd prints the current configuration to stdout.
var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show current configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		configPath := filepath.Join(configDir, "config.yaml")
		data, err := os.ReadFile(configPath)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("No config file found at %s\n", configPath)
				fmt.Println("Run 'ctrlai' for interactive setup or 'ctrlai config generate' for a template.")
				return nil
			}
			return fmt.Errorf("failed to read config: %w", err)
		}
		fmt.Println(string(data))
		return nil
	},
}

// configEditCmd opens the config file in the user's preferred editor.
// Uses $EDITOR or $VISUAL env vars, falling back to platform defaults.
var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Open config in editor",
	Long:  `Open the CtrlAI config file in your default editor ($EDITOR or $VISUAL).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		configPath := filepath.Join(configDir, "config.yaml")

		// Determine which editor to use. Check standard env vars first,
		// then fall back to platform-appropriate defaults.
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = os.Getenv("VISUAL")
		}
		if editor == "" {
			if runtime.GOOS == "windows" {
				editor = "notepad"
			} else {
				editor = "vi"
			}
		}

		// Ensure the config file exists (create default if not).
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			if err := config.WriteDefault(configPath); err != nil {
				return fmt.Errorf("failed to create default config: %w", err)
			}
		}

		// Launch the editor using exec.Command for cross-platform PATH
		// resolution. os.StartProcess requires an absolute binary path
		// and doesn't search PATH, making it unreliable.
		fmt.Printf("[ctrlai] Opening %s in %s...\n", configPath, editor)
		editorCmd := exec.Command(editor, configPath)
		editorCmd.Stdin = os.Stdin
		editorCmd.Stdout = os.Stdout
		editorCmd.Stderr = os.Stderr
		return editorCmd.Run()
	},
}

// configGenerateCmd generates an OpenClaw configuration snippet that routes
// LLM traffic through the CtrlAI proxy. This is pasted into OpenClaw's
// ~/.openclaw/openclaw.json to enable zero-code integration.
var configGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate OpenClaw config snippet for proxy integration",
	Long: `Generate a JSON configuration snippet for OpenClaw that routes LLM
API traffic through the CtrlAI proxy. Copy the output into your
~/.openclaw/openclaw.json file.

Supports single-agent, multi-agent, and multi-provider setups.
See the design doc for detailed examples.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(filepath.Join(configDir, "config.yaml"))
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		addr := fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port)

		fmt.Println("// Add this to your ~/.openclaw/openclaw.json")
		fmt.Println("// Single agent, single provider (simplest setup):")
		fmt.Println("{")
		fmt.Println("  \"models\": {")
		fmt.Println("    \"providers\": {")
		fmt.Println("      \"anthropic\": {")
		fmt.Printf("        \"baseUrl\": \"%s/provider/anthropic\"\n", addr)
		fmt.Println("      }")
		fmt.Println("    }")
		fmt.Println("  }")
		fmt.Println("}")
		fmt.Println()
		fmt.Println("// For multi-agent setup, use per-agent provider entries:")
		fmt.Printf("//   \"baseUrl\": \"%s/provider/anthropic/agent/main\"\n", addr)
		fmt.Printf("//   \"baseUrl\": \"%s/provider/anthropic/agent/work\"\n", addr)
		fmt.Println("//")
		fmt.Println("// For multi-provider setup:")
		fmt.Printf("//   \"baseUrl\": \"%s/provider/anthropic/agent/main\"\n", addr)
		fmt.Printf("//   \"baseUrl\": \"%s/provider/openai/agent/work\"\n", addr)
		fmt.Println("//")
		fmt.Println("// See the design doc (Section 2) for full multi-agent/multi-provider examples.")
		return nil
	},
}

// ============================================================================
// First-run interactive setup (TUI)
// ============================================================================

// openclawConfigPaths lists the known locations where OpenClaw stores its
// configuration. Used by the first-run setup to auto-detect an existing
// OpenClaw installation (Phase 6, step 2 in design doc).
var openclawConfigPaths = []string{
	filepath.Join(".openclaw", "openclaw.json"),         // relative to home
	filepath.Join(".config", "openclaw", "config.json"), // XDG
}

// runFirstTimeSetup runs when 'ctrlai' is invoked with no subcommand.
// It guides the user through initial configuration:
//  1. Creates the ~/.ctrlai/ directory
//  2. Auto-detects OpenClaw installation
//  3. Generates a default config.yaml
//  4. Generates a default rules.yaml with built-in rules enabled
//  5. Shows the OpenClaw integration snippet
func runFirstTimeSetup(cmd *cobra.Command, args []string) error {
	fmt.Println("=== CtrlAI — First-Time Setup ===")
	fmt.Println()

	// Check if already configured.
	configPath := filepath.Join(configDir, "config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		fmt.Printf("Config already exists at %s\n", configPath)
		fmt.Println("Use 'ctrlai start' to start the proxy.")
		fmt.Println("Use 'ctrlai config edit' to modify the configuration.")
		return nil
	}

	// Create the config directory.
	fmt.Printf("Creating config directory: %s\n", configDir)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// --- Auto-detect OpenClaw ---
	// Check known paths for an existing OpenClaw config file.
	// If found, we can tell the user exactly what to modify.
	home, _ := os.UserHomeDir()
	var foundOpenClawConfig string
	if home != "" {
		for _, relPath := range openclawConfigPaths {
			candidate := filepath.Join(home, relPath)
			if _, err := os.Stat(candidate); err == nil {
				foundOpenClawConfig = candidate
				break
			}
		}
	}

	if foundOpenClawConfig != "" {
		fmt.Printf("Detected OpenClaw config at: %s\n", foundOpenClawConfig)
	} else {
		fmt.Println("No OpenClaw config detected (will generate integration snippet)")
	}
	fmt.Println()

	// Write default config.
	fmt.Println("Writing default config.yaml...")
	if err := config.WriteDefault(configPath); err != nil {
		return fmt.Errorf("failed to write default config: %w", err)
	}

	// Write default rules with all built-in rules enabled.
	rulesPath := filepath.Join(configDir, "rules.yaml")
	fmt.Println("Writing default rules.yaml (built-in security rules enabled)...")
	if err := engine.WriteDefaultRules(rulesPath); err != nil {
		return fmt.Errorf("failed to write default rules: %w", err)
	}

	// Create the audit directory.
	auditDir := filepath.Join(configDir, "audit")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		return fmt.Errorf("failed to create audit directory: %w", err)
	}

	fmt.Println()
	fmt.Println("Setup complete! Next steps:")
	fmt.Println()
	fmt.Println("  1. Start the proxy:")
	fmt.Println("     ctrlai start")
	fmt.Println()

	if foundOpenClawConfig != "" {
		// If we found OpenClaw, give specific instructions.
		fmt.Println("  2. Add this to your OpenClaw config:")
		fmt.Printf("     %s\n", foundOpenClawConfig)
		fmt.Println()
		fmt.Println("     \"anthropic\": {")
		fmt.Println("       \"baseUrl\": \"http://127.0.0.1:3100/provider/anthropic\"")
		fmt.Println("     }")
		fmt.Println()
		fmt.Println("     Or run 'ctrlai config generate' for more examples.")
	} else {
		fmt.Println("  2. Configure your agent SDK to route through CtrlAI:")
		fmt.Println("     ctrlai config generate")
	}

	fmt.Println()
	fmt.Println("  3. View the dashboard:")
	fmt.Println("     http://127.0.0.1:3100/dashboard")
	fmt.Println()
	return nil
}
