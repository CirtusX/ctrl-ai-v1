// Package dashboard serves the CtrlAI web UI and REST API.
//
// The dashboard is mounted on /dashboard and /api/ on the same port as
// the proxy. It provides:
//
//   - Web UI:     GET /dashboard          — Single-page HTML dashboard
//   - WebSocket:  GET /dashboard/ws       — Live activity feed
//   - REST API:   GET /api/status         — Proxy status
//                 GET /api/agents         — Agent list with stats
//                 GET /api/audit          — Recent audit entries
//                 GET /api/rules          — List all rules
//                 POST /api/kill          — Kill an agent
//                 POST /api/revive        — Revive a killed agent
//
// The web UI is a minimal embedded HTML page (no build step, no framework).
// For Phase 1, it's a simple status page. Phase 2 adds the full dashboard.
//
// See design doc Section 15 for the dashboard specification.
package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ctrlai/ctrlai/internal/agent"
	"github.com/ctrlai/ctrlai/internal/audit"
	"github.com/ctrlai/ctrlai/internal/engine"
)

// Options holds the dependencies injected into the dashboard.
type Options struct {
	AuditLog   *audit.AuditLog
	Registry   *agent.Registry
	KillSwitch *agent.KillSwitch
	Engine     *engine.Engine
	RulesPath  string // Path to rules.yaml for saving after modifications.
}

// Dashboard serves the web UI and REST API.
// Implements http.Handler for the dashboard UI routes.
type Dashboard struct {
	auditLog   *audit.AuditLog
	registry   *agent.Registry
	killSwitch *agent.KillSwitch
	engine     *engine.Engine
	rulesPath  string
	wsHub      *wsHub
}

// New creates a new Dashboard with the given dependencies.
func New(opts Options) *Dashboard {
	d := &Dashboard{
		auditLog:   opts.AuditLog,
		registry:   opts.Registry,
		killSwitch: opts.KillSwitch,
		engine:     opts.Engine,
		rulesPath:  opts.RulesPath,
		wsHub:      newWSHub(),
	}

	// Start the WebSocket broadcast hub.
	go d.wsHub.run()

	return d
}

// ServeHTTP handles requests to /dashboard and /dashboard/.
// Serves a minimal embedded HTML dashboard.
func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(dashboardHTML))
}

// WebSocketHandler returns an http.Handler for the /dashboard/ws endpoint.
// Clients connect here to receive real-time audit events.
func (d *Dashboard) WebSocketHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.handleWebSocket(w, r)
	})
}

// APIHandler returns an http.Handler for the /api/ REST endpoints.
// Routes requests to the appropriate handler based on path and method.
func (d *Dashboard) APIHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", d.handleAPIStatus)
	mux.HandleFunc("/api/agents", d.handleAPIAgents)
	mux.HandleFunc("/api/audit", d.handleAPIAudit)
	mux.HandleFunc("/api/rules", d.handleAPIRules)
	mux.HandleFunc("/api/rules/delete", d.handleAPIRulesDelete)
	mux.HandleFunc("/api/kill", d.handleAPIKill)
	mux.HandleFunc("/api/revive", d.handleAPIRevive)

	return mux
}

// BroadcastEvent sends an audit event to all connected WebSocket clients.
// Called by the proxy after each tool call evaluation. Non-blocking —
// if no clients are connected, the event is dropped.
func (d *Dashboard) BroadcastEvent(e audit.Entry) {
	data, err := json.Marshal(e)
	if err != nil {
		slog.Error("failed to marshal broadcast event", "error", err)
		return
	}
	d.wsHub.broadcast(data)
}

// --- REST API Handlers ---

// handleAPIStatus returns proxy status information.
// GET /api/status
func (d *Dashboard) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	status := map[string]any{
		"status":         "running",
		"total_rules":    d.engine.TotalRules(),
		"builtin_rules":  d.engine.BuiltinCount(),
		"custom_rules":   d.engine.CustomCount(),
		"agents":         len(d.registry.List()),
	}

	writeJSON(w, http.StatusOK, status)
}

// handleAPIAgents returns the list of all registered agents with stats.
// GET /api/agents
//
// Response matches the statusAgentJSON struct in main.go.
func (d *Dashboard) handleAPIAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	agents := d.registry.List()
	writeJSON(w, http.StatusOK, agents)
}

// handleAPIAudit returns recent audit entries.
// GET /api/audit?limit=50&agent=main&decision=block
func (d *Dashboard) handleAPIAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	params := audit.QueryParams{
		Agent:    r.URL.Query().Get("agent"),
		Decision: r.URL.Query().Get("decision"),
		Limit:    limit,
	}

	entries, err := d.auditLog.Query(params)
	if err != nil {
		slog.Error("audit query failed", "error", err)
		http.Error(w, "audit query failed", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, entries)
}

// handleAPIRules handles rule listing and creation.
// GET  /api/rules              — List all rules
// POST /api/rules  { "yaml": "..." }  — Add a custom rule
func (d *Dashboard) handleAPIRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rules := d.engine.ListRules()
		writeJSON(w, http.StatusOK, rules)

	case http.MethodPost:
		var req struct {
			YAML string `json:"yaml"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if req.YAML == "" {
			http.Error(w, "yaml field required", http.StatusBadRequest)
			return
		}
		if err := d.engine.AddRule(req.YAML); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if d.rulesPath != "" {
			if err := d.engine.Save(d.rulesPath); err != nil {
				slog.Error("failed to save rules after add", "error", err)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "added"})

	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleAPIRulesDelete removes a custom rule by name.
// POST /api/rules/delete  { "name": "my_rule" }
func (d *Dashboard) handleAPIRulesDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name field required", http.StatusBadRequest)
		return
	}

	if err := d.engine.RemoveRule(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if d.rulesPath != "" {
		if err := d.engine.Save(d.rulesPath); err != nil {
			slog.Error("failed to save rules after remove", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "name": req.Name})
}

// handleAPIKill kills an agent via the REST API.
// POST /api/kill  { "agent": "main", "reason": "suspicious activity" }
func (d *Dashboard) handleAPIKill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Agent  string `json:"agent"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Agent == "" {
		http.Error(w, "agent field required", http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		req.Reason = "killed via dashboard API"
	}

	if err := d.killSwitch.Kill(req.Agent, req.Reason, "dashboard"); err != nil {
		slog.Error("kill via API failed", "agent", req.Agent, "error", err)
		http.Error(w, "kill failed", http.StatusInternalServerError)
		return
	}

	d.registry.SetStatus(req.Agent, "killed")
	writeJSON(w, http.StatusOK, map[string]string{"status": "killed", "agent": req.Agent})
}

// handleAPIRevive revives a killed agent via the REST API.
// POST /api/revive  { "agent": "main" }
func (d *Dashboard) handleAPIRevive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Agent == "" {
		http.Error(w, "agent field required", http.StatusBadRequest)
		return
	}

	if err := d.killSwitch.Revive(req.Agent); err != nil {
		slog.Error("revive via API failed", "agent", req.Agent, "error", err)
		http.Error(w, "revive failed", http.StatusInternalServerError)
		return
	}

	d.registry.SetStatus(req.Agent, "active")
	writeJSON(w, http.StatusOK, map[string]string{"status": "revived", "agent": req.Agent})
}

// --- Helpers ---

// writeJSON sends a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(data)
}

// dashboardHTML is the embedded HTML for the Phase 1 dashboard.
// Minimal single-page UI that shows proxy status, agent list, and
// recent audit entries. Refreshes via periodic fetch + WebSocket.
//
// Phase 2 will replace this with a full React-based dashboard,
// but for Phase 1 this is sufficient and has zero build dependencies.
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>CtrlAI Dashboard</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
         background: #0f1117; color: #e1e4e8; padding: 24px; }
  h1 { font-size: 24px; margin-bottom: 8px; }
  .subtitle { color: #8b949e; margin-bottom: 24px; }
  .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-bottom: 24px; }
  .card { background: #161b22; border: 1px solid #30363d; border-radius: 8px; padding: 16px; }
  .card h2 { font-size: 14px; color: #8b949e; text-transform: uppercase; margin-bottom: 12px; }
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th { text-align: left; color: #8b949e; padding: 6px 8px; border-bottom: 1px solid #30363d; }
  td { padding: 6px 8px; border-bottom: 1px solid #21262d; }
  .status-active { color: #3fb950; }
  .status-killed { color: #f85149; }
  .decision-block { color: #f85149; font-weight: bold; }
  .decision-allow { color: #3fb950; }
  .decision-info { color: #58a6ff; }
  #live-feed { max-height: 300px; overflow-y: auto; font-family: monospace; font-size: 12px; }
  .feed-entry { padding: 4px 0; border-bottom: 1px solid #21262d; }
  .btn { background: #21262d; border: 1px solid #30363d; color: #e1e4e8;
         padding: 4px 12px; border-radius: 4px; cursor: pointer; font-size: 12px; }
  .btn:hover { background: #30363d; }
  .btn-danger { border-color: #f85149; color: #f85149; }
  .btn-success { border-color: #3fb950; color: #3fb950; }
</style>
</head>
<body>
<h1>CtrlAI Dashboard</h1>
<p class="subtitle">Guardrail proxy for AI agents</p>

<div class="grid">
  <div class="card">
    <h2>Agents</h2>
    <table>
      <thead><tr><th>Agent</th><th>Status</th><th>Provider</th><th>Requests</th><th>Blocked</th><th>Action</th></tr></thead>
      <tbody id="agents-tbody"><tr><td colspan="6">Loading...</td></tr></tbody>
    </table>
  </div>
  <div class="card">
    <h2>Rules</h2>
    <table>
      <thead><tr><th>Name</th><th>Type</th><th>Action</th></tr></thead>
      <tbody id="rules-tbody"><tr><td colspan="3">Loading...</td></tr></tbody>
    </table>
  </div>
</div>

<div class="card">
  <h2>Live Activity Feed</h2>
  <div id="live-feed"><div class="feed-entry">Connecting...</div></div>
</div>

<script>
function esc(s) {
  if (s == null) return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}
async function refresh() {
  try {
    const [agentsRes, rulesRes, auditRes] = await Promise.all([
      fetch('/api/agents'), fetch('/api/rules'), fetch('/api/audit?limit=20')
    ]);
    const agents = await agentsRes.json();
    const rules = await rulesRes.json();
    const auditEntries = await auditRes.json();
    renderAgents(agents);
    renderRules(rules);
    renderAudit(auditEntries);
  } catch(e) { console.error('refresh failed:', e); }
}

function renderAgents(agents) {
  const tbody = document.getElementById('agents-tbody');
  if (!agents || agents.length === 0) { tbody.innerHTML = '<tr><td colspan="6">No agents yet</td></tr>'; return; }
  tbody.innerHTML = agents.map(a => {
    const cls = a.status === 'killed' ? 'status-killed' : 'status-active';
    const id = esc(a.id);
    const btn = a.status === 'killed'
      ? '<button class="btn btn-success" onclick="reviveAgent(\'' + id + '\')">Revive</button>'
      : '<button class="btn btn-danger" onclick="killAgent(\'' + id + '\')">Kill</button>';
    return '<tr><td>' + id + '</td><td class="' + cls + '">' + esc(a.status) +
      '</td><td>' + esc(a.provider) + '</td><td>' + (a.stats?.total_requests||0) +
      '</td><td>' + (a.stats?.blocked_tool_calls||0) + '</td><td>' + btn + '</td></tr>';
  }).join('');
}

function renderRules(rules) {
  const tbody = document.getElementById('rules-tbody');
  if (!rules || rules.length === 0) { tbody.innerHTML = '<tr><td colspan="3">No rules</td></tr>'; return; }
  tbody.innerHTML = rules.map(r =>
    '<tr><td>' + esc(r.Name) + '</td><td>' + (r.Builtin?'builtin':'custom') + '</td><td>' + esc(r.Action) + '</td></tr>'
  ).join('');
}

function renderAudit(entries) {
  const feed = document.getElementById('live-feed');
  if (!entries || entries.length === 0) { feed.innerHTML = '<div class="feed-entry">No entries yet</div>'; return; }
  feed.innerHTML = entries.map(e => {
    const cls = e.decision === 'block' ? 'decision-block' : e.decision === 'allow' ? 'decision-allow' : 'decision-info';
    return '<div class="feed-entry">[' + esc(e.ts) + '] agent=' + esc(e.agent||'-') +
      ' tool=' + esc(e.tool||e.type||'-') + ' <span class="' + cls + '">' + esc(e.decision) + '</span>' +
      (e.rule ? ' rule=' + esc(e.rule) : '') + '</div>';
  }).join('');
}

async function killAgent(id) {
  await fetch('/api/kill', { method: 'POST', headers: {'Content-Type':'application/json'},
    body: JSON.stringify({agent: id, reason: 'killed via dashboard'}) });
  refresh();
}

async function reviveAgent(id) {
  await fetch('/api/revive', { method: 'POST', headers: {'Content-Type':'application/json'},
    body: JSON.stringify({agent: id}) });
  refresh();
}

// WebSocket for live updates.
function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(proto + '//' + location.host + '/dashboard/ws');
  ws.onmessage = function(e) {
    try {
      const entry = JSON.parse(e.data);
      const feed = document.getElementById('live-feed');
      const cls = entry.decision === 'block' ? 'decision-block' : entry.decision === 'allow' ? 'decision-allow' : 'decision-info';
      const div = document.createElement('div');
      div.className = 'feed-entry';
      div.innerHTML = '[' + esc(entry.ts) + '] agent=' + esc(entry.agent||'-') +
        ' tool=' + esc(entry.tool||entry.type||'-') + ' <span class="' + cls + '">' + esc(entry.decision) + '</span>' +
        (entry.rule ? ' rule=' + esc(entry.rule) : '');
      feed.insertBefore(div, feed.firstChild);
      // Keep feed under 100 entries.
      while (feed.children.length > 100) feed.removeChild(feed.lastChild);
    } catch(err) { console.error('ws parse error:', err); }
  };
  ws.onclose = function() { setTimeout(connectWS, 3000); };
  ws.onerror = function() { ws.close(); };
}

refresh();
setInterval(refresh, 5000);
connectWS();
</script>
</body>
</html>`
