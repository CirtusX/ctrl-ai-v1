package agent

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// KilledEntry represents a single agent kill record in killed.yaml.
// Each entry records who killed the agent, when, and why.
//
// Design doc Section 9.1: When a killed agent's request hits the proxy,
// the proxy returns a fake LLM response immediately (no forwarding).
type KilledEntry struct {
	Agent    string    `yaml:"agent"`
	KilledAt time.Time `yaml:"killed_at"`
	Reason   string    `yaml:"reason"`
	KilledBy string    `yaml:"killed_by"`
}

// KillSwitch manages the set of killed agents. It persists state to
// killed.yaml and maintains an in-memory set for fast lookups.
//
// Thread-safe — IsKilled() is called on every proxy request from
// concurrent goroutines, while Kill/Revive/Reload modify the state.
//
// The proxy file-watches killed.yaml and calls Reload() when it changes,
// so `ctrlai kill` takes effect immediately without restarting the proxy.
type KillSwitch struct {
	mu      sync.RWMutex
	killed  map[string]KilledEntry // In-memory set for O(1) lookups.
	entries []KilledEntry          // Ordered list for YAML serialization.
	path    string                 // Path to killed.yaml.
}

// NewKillSwitch loads the kill switch state from the given YAML file.
// If the file doesn't exist, returns an empty kill switch (no agents killed).
func NewKillSwitch(path string) (*KillSwitch, error) {
	ks := &KillSwitch{
		killed: make(map[string]KilledEntry),
		path:   path,
	}

	if err := ks.loadFromFile(); err != nil {
		return nil, err
	}

	return ks, nil
}

// IsKilled checks whether the given agent ID is currently killed.
// Returns true if the agent is in the kill list, false otherwise.
//
// This is called on EVERY request through the proxy (design doc Section 4.3),
// so it must be fast — O(1) map lookup under a read lock.
func (ks *KillSwitch) IsKilled(agentID string) bool {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	_, killed := ks.killed[agentID]
	return killed
}

// Kill adds an agent to the kill list and persists to killed.yaml.
// If the agent is already killed, this is a no-op (not an error).
//
// Parameters:
//   - id:     Agent ID (from URL path)
//   - reason: Human-readable reason for the kill
//   - by:     Who initiated the kill ("user", "system", etc.)
func (ks *KillSwitch) Kill(id, reason, by string) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	// Skip if already killed.
	if _, exists := ks.killed[id]; exists {
		return nil
	}

	entry := KilledEntry{
		Agent:    id,
		KilledAt: time.Now().UTC(),
		Reason:   reason,
		KilledBy: by,
	}

	ks.killed[id] = entry
	ks.entries = append(ks.entries, entry)

	slog.Warn("agent killed", "agent", id, "reason", reason, "by", by)
	return ks.saveToFile()
}

// Revive removes an agent from the kill list and persists to killed.yaml.
// If the agent is not killed, this is a no-op (not an error).
func (ks *KillSwitch) Revive(id string) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	if _, exists := ks.killed[id]; !exists {
		return nil
	}

	delete(ks.killed, id)

	// Rebuild the entries slice without the revived agent.
	filtered := make([]KilledEntry, 0, len(ks.entries))
	for _, e := range ks.entries {
		if e.Agent != id {
			filtered = append(filtered, e)
		}
	}
	ks.entries = filtered

	slog.Info("agent revived", "agent", id)
	return ks.saveToFile()
}

// Reload re-reads killed.yaml from disk and updates the in-memory state.
// Called by the file watcher when killed.yaml changes on disk (e.g. when
// another process like `ctrlai kill` modifies it).
func (ks *KillSwitch) Reload() error {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	// Reset state and reload from file.
	ks.killed = make(map[string]KilledEntry)
	ks.entries = nil

	if err := ks.loadFromFile(); err != nil {
		return err
	}

	slog.Info("kill switch reloaded", "killed_agents", len(ks.killed))
	return nil
}

// loadFromFile reads killed.yaml and populates the in-memory state.
// NOT thread-safe — caller must hold the mutex.
func (ks *KillSwitch) loadFromFile() error {
	data, err := os.ReadFile(ks.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading kill switch %s: %w", ks.path, err)
	}

	// Handle empty file gracefully.
	if len(data) == 0 {
		return nil
	}

	var entries []KilledEntry
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parsing kill switch %s: %w", ks.path, err)
	}

	ks.entries = entries
	for _, e := range entries {
		ks.killed[e.Agent] = e
	}

	return nil
}

// saveToFile writes the current kill list to killed.yaml.
// NOT thread-safe — caller must hold the mutex.
func (ks *KillSwitch) saveToFile() error {
	// If no agents are killed, write an empty file rather than "[]".
	if len(ks.entries) == 0 {
		return os.WriteFile(ks.path, []byte(""), 0o644)
	}

	data, err := yaml.Marshal(ks.entries)
	if err != nil {
		return fmt.Errorf("marshaling kill switch: %w", err)
	}

	return os.WriteFile(ks.path, data, 0o644)
}
