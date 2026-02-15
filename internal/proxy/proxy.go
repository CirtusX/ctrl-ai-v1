package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ctrlai/ctrlai/internal/agent"
	"github.com/ctrlai/ctrlai/internal/audit"
	"github.com/ctrlai/ctrlai/internal/config"
	"github.com/ctrlai/ctrlai/internal/engine"
	"github.com/ctrlai/ctrlai/internal/extractor"
)

// Options holds the dependencies injected into the proxy at creation.
// These are initialized by main.go's runStart() and wired together here.
type Options struct {
	Config         *config.Config
	Engine         *engine.Engine
	AuditLog       *audit.AuditLog
	Registry       *agent.Registry
	KillSwitch     *agent.KillSwitch
	UpstreamClient *http.Client
	// OnAuditEvent is called after each audit entry is logged, allowing the
	// dashboard to broadcast events to WebSocket clients in real time.
	// Optional — nil means no broadcast.
	OnAuditEvent func(audit.Entry)
}

// Proxy is the HTTP handler that intercepts LLM API calls, evaluates
// tool calls against guardrail rules, and modifies blocked responses.
//
// Implements http.Handler — mounted on /provider/ in the main mux.
//
// Design doc Section 13: Request Handler pseudocode.
type Proxy struct {
	config       *config.Config
	engine       *engine.Engine
	auditLog     *audit.AuditLog
	registry     *agent.Registry
	killSwitch   *agent.KillSwitch
	client       *http.Client
	onAuditEvent func(audit.Entry)
}

// New creates a new Proxy handler with the given dependencies.
func New(opts Options) *Proxy {
	return &Proxy{
		config:       opts.Config,
		engine:       opts.Engine,
		auditLog:     opts.AuditLog,
		registry:     opts.Registry,
		killSwitch:   opts.KillSwitch,
		client:       opts.UpstreamClient,
		onAuditEvent: opts.OnAuditEvent,
	}
}

// broadcastAuditEvent sends an audit entry to the dashboard WebSocket feed.
func (p *Proxy) broadcastAuditEvent(e audit.Entry) {
	if p.onAuditEvent != nil {
		p.onAuditEvent(e)
	}
}

// ServeHTTP is the main entry point for all proxy requests.
// It implements the full data flow from design doc Section 13:
//
//  1. Parse route (provider, agent, apiPath, apiType)
//  2. Check kill switch
//  3. Read request body (extract metadata for audit)
//  4. Update agent registry
//  5. Forward to upstream LLM
//  6. Handle response (streaming or non-streaming)
//
// maxRequestBody is 10MB — larger requests are rejected. LLM request
// bodies rarely exceed a few hundred KB even with long conversations.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// --- Step 1: Parse route ---
	route, err := ParseRoute(r.URL.Path)
	if err != nil {
		slog.Warn("invalid route", "path", r.URL.Path, "error", err)
		http.Error(w, "invalid proxy path", http.StatusBadRequest)
		return
	}

	slog.Debug("proxy request",
		"provider", route.ProviderKey,
		"agent", route.AgentID,
		"apiPath", route.APIPath,
		"method", r.Method,
	)

	// --- Step 2: Check kill switch ---
	// Design doc Section 4.3: If agent is killed, return fake response immediately.
	if p.killSwitch.IsKilled(route.AgentID) {
		slog.Warn("request from killed agent", "agent", route.AgentID)
		p.respondKilled(w, route)
		p.auditLog.LogKill(route.AgentID, "request from killed agent")
		return
	}

	// --- Step 3: Read request body ---
	// We read the body to extract metadata (model, tools, stream flag).
	// The body is forwarded to upstream unchanged.
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		slog.Error("failed to read request body", "error", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	reqMeta := extractor.ExtractRequestMeta(body, route.APIType)

	// --- Step 4: Update agent registry ---
	// Auto-register on first request, update last_seen and stats.
	p.registry.Touch(route.AgentID, route.ProviderKey, reqMeta.Model)

	// --- Step 5: Look up upstream URL ---
	provider, ok := p.config.Providers[route.ProviderKey]
	if !ok {
		slog.Warn("unknown provider", "provider", route.ProviderKey)
		http.Error(w, fmt.Sprintf("unknown provider: %s", route.ProviderKey), http.StatusBadGateway)
		return
	}
	upstream := provider.Upstream + route.APIPath

	// --- Step 6: Forward request to upstream LLM ---
	resp, err := forwardRequest(p.client, upstream, r, body)
	if err != nil {
		slog.Error("upstream request failed",
			"upstream", upstream,
			"error", err,
			"latency_ms", time.Since(start).Milliseconds(),
		)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// --- Step 7: Handle response ---
	// If API type is unknown, pass through without tool inspection.
	// Design doc Section 2: "Other API types should be passed through
	// transparently without tool inspection."
	if route.APIType == extractor.APITypeUnknown {
		p.passThrough(w, resp)
		return
	}

	if reqMeta.Stream && p.config.Streaming.Buffer {
		p.handleStreaming(w, resp, route, reqMeta, start)
	} else {
		p.handleNonStreaming(w, resp, route, reqMeta, start)
	}
}

// handleNonStreaming processes a non-streaming (stream: false) response.
// Reads the full body, extracts tool calls, evaluates each against rules,
// and modifies the response if any are blocked.
//
// Design doc Section 13 — handleNonStreaming pseudocode.
func (p *Proxy) handleNonStreaming(w http.ResponseWriter, resp *http.Response, route RouteInfo, meta extractor.RequestMeta, start time.Time) {
	// Read the full response body.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("failed to read upstream response", "error", err)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	// Extract tool calls from the response.
	toolCalls := extractor.Extract(body, route.APIType)

	if len(toolCalls) == 0 {
		// No tool calls — pass through unchanged.
		copyResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	// Evaluate each tool call against the rule engine.
	var blocked []extractor.ToolCall
	var blockedDecisions []engine.Decision

	for _, tc := range toolCalls {
		evalStart := time.Now()
		decision := p.engine.Evaluate(route.AgentID, tc)
		latencyUs := time.Since(evalStart).Microseconds()

		// Log to audit chain.
		p.auditLog.LogToolCall(
			route.AgentID, route.ProviderKey, meta.Model,
			tc.Name, tc.Arguments,
			decision.Action, decision.Rule, decision.Message,
			latencyUs,
		)

		// Broadcast to dashboard WebSocket feed.
		p.broadcastAuditEvent(audit.Entry{
			Agent: route.AgentID, Provider: route.ProviderKey, Model: meta.Model,
			Type: "tool_call", Tool: tc.Name, Decision: decision.Action,
			Rule: decision.Rule, Message: decision.Message, LatencyUs: latencyUs,
		})

		// Update agent stats.
		p.registry.RecordToolCall(route.AgentID, decision.Action == "block")

		if decision.Action == "block" {
			blocked = append(blocked, tc)
			blockedDecisions = append(blockedDecisions, decision)

			slog.Warn("tool call blocked",
				"agent", route.AgentID,
				"tool", tc.Name,
				"rule", decision.Rule,
				"message", decision.Message,
			)
		} else {
			slog.Debug("tool call allowed",
				"agent", route.AgentID,
				"tool", tc.Name,
			)
		}
	}

	// Modify response if any tool calls were blocked.
	if len(blocked) > 0 {
		body = modifyNonStreamingResponse(body, route.APIType, blocked, blockedDecisions)
	}

	// Send response to SDK.
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// handleStreaming processes a streaming (stream: true) SSE response.
// Buffers all SSE events, reconstructs the full message, evaluates tool
// calls, and replays the events (modified or original) to the SDK.
//
// Design doc Section 5.4: Buffer-Then-Forward strategy.
// Design doc Section 13 — handleStreaming pseudocode.
func (p *Proxy) handleStreaming(w http.ResponseWriter, resp *http.Response, route RouteInfo, meta extractor.RequestMeta, start time.Time) {
	// Buffer all SSE events until message_stop / [DONE].
	events, msg, err := bufferAll(resp.Body, p.config.Streaming.BufferTimeoutMs, route.APIType)
	if err != nil {
		slog.Error("failed to buffer SSE stream", "error", err)
		http.Error(w, "failed to buffer SSE stream", http.StatusBadGateway)
		return
	}

	// Evaluate tool calls from the reconstructed message.
	var blocked []extractor.ToolCall
	var blockMessages []string

	for _, tc := range msg.ToolCalls {
		evalStart := time.Now()
		decision := p.engine.Evaluate(route.AgentID, tc)
		latencyUs := time.Since(evalStart).Microseconds()

		// Log to audit chain.
		p.auditLog.LogToolCall(
			route.AgentID, route.ProviderKey, meta.Model,
			tc.Name, tc.Arguments,
			decision.Action, decision.Rule, decision.Message,
			latencyUs,
		)

		// Broadcast to dashboard WebSocket feed.
		p.broadcastAuditEvent(audit.Entry{
			Agent: route.AgentID, Provider: route.ProviderKey, Model: meta.Model,
			Type: "tool_call", Tool: tc.Name, Decision: decision.Action,
			Rule: decision.Rule, Message: decision.Message, LatencyUs: latencyUs,
		})

		// Update agent stats.
		p.registry.RecordToolCall(route.AgentID, decision.Action == "block")

		if decision.Action == "block" {
			blocked = append(blocked, tc)
			blockMessages = append(blockMessages, formatBlockNotice(tc.Name, decision.Rule, decision.Message))

			slog.Warn("tool call blocked (streaming)",
				"agent", route.AgentID,
				"tool", tc.Name,
				"rule", decision.Rule,
			)
		}
	}

	// Prepare for SSE response to SDK.
	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Error("ResponseWriter does not support flushing (required for SSE)")
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Set SSE response headers.
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Del("Content-Length") // SSE is chunked — no Content-Length.
	w.WriteHeader(resp.StatusCode)

	// Choose which events to replay.
	var replayEvents []SSEEvent
	if len(blocked) > 0 {
		// Build modified stream with blocked tool_use blocks stripped.
		replayEvents = buildModifiedStream(events, route.APIType, blocked, blockMessages)
	} else {
		// All allowed — replay original events as-is.
		replayEvents = events
	}

	// Write SSE events to the SDK.
	for _, evt := range replayEvents {
		if evt.Event != "" {
			fmt.Fprintf(w, "event: %s\n", evt.Event)
		}
		fmt.Fprintf(w, "data: %s\n\n", evt.Data)
		flusher.Flush()
	}
}

// respondKilled sends a fake LLM response for a killed agent.
// The response looks like a normal "end_turn" so the SDK stops gracefully.
// Design doc Section 9.1.
func (p *Proxy) respondKilled(w http.ResponseWriter, route RouteInfo) {
	body := buildKilledResponse(route.APIType)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// passThrough forwards the upstream response to the SDK without any
// inspection or modification. Used for unknown API types.
func (p *Proxy) passThrough(w http.ResponseWriter, resp *http.Response) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
