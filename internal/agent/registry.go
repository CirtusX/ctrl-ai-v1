// Package agent manages agent identity, tracking, and the kill switch.
//
// Agents are auto-discovered when their first request passes through the
// proxy. The agent ID is extracted from the URL path:
//
//	/provider/{provider}/agent/{agentId}/{apiPath}
//
// If no /agent/ segment is present, the agent ID defaults to "default".
//
// The registry persists to ~/.ctrlai/agents.yaml and tracks per-agent stats:
// total requests, tool calls, blocked tool calls, provider, model, and
// first/last seen timestamps.
//
// See design doc Section 10 for the agent identity model.
package agent

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Agent represents a tracked AI agent. Agents are identified by their ID
// (from the URL path) and accumulate stats over their lifetime.
type Agent struct {
	ID        string     `yaml:"-" json:"id"`
	FirstSeen time.Time  `yaml:"first_seen" json:"first_seen"`
	LastSeen  time.Time  `yaml:"last_seen" json:"last_seen"`
	Status    string     `yaml:"status" json:"status"`
	Provider  string     `yaml:"provider" json:"provider"`
	Model     string     `yaml:"model" json:"model"`
	Stats     AgentStats `yaml:"stats" json:"stats"`
}

// AgentStats holds cumulative counters for an agent's activity.
type AgentStats struct {
	TotalRequests    uint64 `yaml:"total_requests" json:"total_requests"`
	TotalToolCalls   uint64 `yaml:"total_tool_calls" json:"total_tool_calls"`
	BlockedToolCalls uint64 `yaml:"blocked_tool_calls" json:"blocked_tool_calls"`
}

// Registry manages the set of known agents and their stats.
// Thread-safe — the proxy calls Touch() and RecordToolCall() concurrently
// from multiple HTTP handler goroutines.
type Registry struct {
	mu     sync.RWMutex
	agents map[string]*Agent
	path   string // Path to agents.yaml for persistence.
}

// registryFile is the YAML envelope for agents.yaml.
// Top-level key "agents" maps agent IDs to their data.
type registryFile struct {
	Agents map[string]*Agent `yaml:"agents"`
}

// NewRegistry loads the agent registry from the given YAML file path.
// If the file doesn't exist, returns an empty registry (not an error).
func NewRegistry(path string) (*Registry, error) {
	r := &Registry{
		agents: make(map[string]*Agent),
		path:   path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, fmt.Errorf("reading agent registry %s: %w", path, err)
	}

	// Handle empty file gracefully.
	if len(data) == 0 {
		return r, nil
	}

	var file registryFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parsing agent registry %s: %w", path, err)
	}

	// Populate the ID field from the map key (it's not stored in YAML value).
	for id, agent := range file.Agents {
		if agent == nil {
			continue
		}
		agent.ID = id
		r.agents[id] = agent
	}

	slog.Info("agent registry loaded", "agents", len(r.agents), "path", path)
	return r, nil
}

// List returns all registered agents, sorted alphabetically by ID.
func (r *Registry) List() []Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	agents := make([]Agent, 0, len(r.agents))
	for _, a := range r.agents {
		agents = append(agents, *a)
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].ID < agents[j].ID
	})
	return agents
}

// Get returns the agent with the given ID, or an error if not found.
func (r *Registry) Get(id string) (Agent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	a, ok := r.agents[id]
	if !ok {
		return Agent{}, fmt.Errorf("agent %q not found", id)
	}
	return *a, nil
}

// Touch updates the agent's last seen timestamp, provider, model, and
// increments the request count. If the agent doesn't exist, it's
// auto-registered (first seen on first request through the proxy).
//
// Called by the proxy on every incoming request.
func (r *Registry) Touch(agentID, provider, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	a, ok := r.agents[agentID]
	if !ok {
		// Auto-register on first request — design doc Section 10.2.
		a = &Agent{
			ID:        agentID,
			FirstSeen: now,
			Status:    "active",
		}
		r.agents[agentID] = a
		slog.Info("new agent registered", "agent", agentID, "provider", provider, "model", model)
	}

	a.LastSeen = now
	a.Provider = provider
	a.Model = model
	a.Stats.TotalRequests++
}

// RecordToolCall increments the tool call counters for an agent.
// If blocked is true, both TotalToolCalls and BlockedToolCalls are
// incremented; otherwise only TotalToolCalls.
//
// Called by the proxy after evaluating each tool call against rules.
func (r *Registry) RecordToolCall(agentID string, blocked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	a, ok := r.agents[agentID]
	if !ok {
		// Agent should already exist (Touch is called first), but be safe.
		return
	}
	a.Stats.TotalToolCalls++
	if blocked {
		a.Stats.BlockedToolCalls++
	}
}

// SetStatus updates an agent's status (e.g. "active" or "killed").
// Used by the kill switch to reflect the killed state in the registry.
func (r *Registry) SetStatus(agentID, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if a, ok := r.agents[agentID]; ok {
		a.Status = status
	}
}

// Save persists the current registry state to agents.yaml.
// Called on graceful shutdown to avoid losing in-memory stats.
func (r *Registry) Save() error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	file := registryFile{Agents: r.agents}
	data, err := yaml.Marshal(&file)
	if err != nil {
		return fmt.Errorf("marshaling agent registry: %w", err)
	}

	if err := os.WriteFile(r.path, data, 0o644); err != nil {
		return fmt.Errorf("writing agent registry %s: %w", r.path, err)
	}

	return nil
}
