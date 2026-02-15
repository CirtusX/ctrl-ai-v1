package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// === KillSwitch Tests ===

func TestNewKillSwitch_NonexistentFile(t *testing.T) {
	ks, err := NewKillSwitch(filepath.Join(t.TempDir(), "killed.yaml"))
	if err != nil {
		t.Fatalf("NewKillSwitch with nonexistent file should not error: %v", err)
	}
	if ks.IsKilled("any-agent") {
		t.Error("no agents should be killed initially")
	}
}

func TestNewKillSwitch_LoadExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killed.yaml")
	data := []byte("- agent: rogue\n  killed_at: \"2026-01-01T00:00:00Z\"\n  reason: \"test\"\n  killed_by: \"user\"\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	ks, err := NewKillSwitch(path)
	if err != nil {
		t.Fatal(err)
	}

	if !ks.IsKilled("rogue") {
		t.Error("rogue should be killed after loading")
	}
	if ks.IsKilled("other") {
		t.Error("other should not be killed")
	}
}

func TestKillSwitch_Kill(t *testing.T) {
	ks, _ := NewKillSwitch(filepath.Join(t.TempDir(), "killed.yaml"))

	if err := ks.Kill("agent1", "suspicious", "user"); err != nil {
		t.Fatal(err)
	}

	if !ks.IsKilled("agent1") {
		t.Error("agent1 should be killed after Kill()")
	}
	if ks.IsKilled("agent2") {
		t.Error("agent2 should not be killed")
	}
}

func TestKillSwitch_KillIdempotent(t *testing.T) {
	ks, _ := NewKillSwitch(filepath.Join(t.TempDir(), "killed.yaml"))

	_ = ks.Kill("agent1", "reason1", "user")
	err := ks.Kill("agent1", "reason2", "user")
	if err != nil {
		t.Errorf("killing already-killed agent should not error: %v", err)
	}
}

func TestKillSwitch_Revive(t *testing.T) {
	ks, _ := NewKillSwitch(filepath.Join(t.TempDir(), "killed.yaml"))

	_ = ks.Kill("agent1", "reason", "user")
	if !ks.IsKilled("agent1") {
		t.Fatal("agent1 should be killed")
	}

	if err := ks.Revive("agent1"); err != nil {
		t.Fatal(err)
	}
	if ks.IsKilled("agent1") {
		t.Error("agent1 should not be killed after Revive()")
	}
}

func TestKillSwitch_ReviveNonKilled(t *testing.T) {
	ks, _ := NewKillSwitch(filepath.Join(t.TempDir(), "killed.yaml"))

	err := ks.Revive("never-killed")
	if err != nil {
		t.Errorf("reviving non-killed agent should not error: %v", err)
	}
}

func TestKillSwitch_PersistsToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killed.yaml")

	ks, _ := NewKillSwitch(path)
	_ = ks.Kill("agent1", "reason", "user")

	// Read the file and verify it contains the agent.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("killed.yaml should not be empty after Kill()")
	}

	// Load into a new KillSwitch.
	ks2, err := NewKillSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ks2.IsKilled("agent1") {
		t.Error("persisted kill should be loaded by new KillSwitch")
	}
}

func TestKillSwitch_RevivePersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killed.yaml")

	ks, _ := NewKillSwitch(path)
	_ = ks.Kill("agent1", "reason", "user")
	_ = ks.Revive("agent1")

	// Load into a new KillSwitch.
	ks2, _ := NewKillSwitch(path)
	if ks2.IsKilled("agent1") {
		t.Error("revived agent should not be killed after reload")
	}
}

func TestKillSwitch_Reload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killed.yaml")

	ks, _ := NewKillSwitch(path)

	// Externally write a killed agent.
	data := []byte("- agent: external\n  killed_at: \"2026-01-01T00:00:00Z\"\n  reason: \"external\"\n  killed_by: \"script\"\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ks.Reload(); err != nil {
		t.Fatal(err)
	}
	if !ks.IsKilled("external") {
		t.Error("external agent should be killed after Reload()")
	}
}

// === Registry Tests ===

func TestNewRegistry_NonexistentFile(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "agents.yaml"))
	if err != nil {
		t.Fatalf("NewRegistry with nonexistent file should not error: %v", err)
	}
	agents := r.List()
	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
}

func TestRegistry_Touch_AutoRegisters(t *testing.T) {
	r, _ := NewRegistry(filepath.Join(t.TempDir(), "agents.yaml"))

	r.Touch("agent1", "anthropic", "claude-opus-4-5-20250918")

	a, err := r.Get("agent1")
	if err != nil {
		t.Fatal(err)
	}

	if a.ID != "agent1" {
		t.Errorf("ID: expected agent1, got %q", a.ID)
	}
	if a.Status != "active" {
		t.Errorf("Status: expected active, got %q", a.Status)
	}
	if a.Provider != "anthropic" {
		t.Errorf("Provider: expected anthropic, got %q", a.Provider)
	}
	if a.Model != "claude-opus-4-5-20250918" {
		t.Errorf("Model: expected claude-opus-4-5-20250918, got %q", a.Model)
	}
	if a.Stats.TotalRequests != 1 {
		t.Errorf("TotalRequests: expected 1, got %d", a.Stats.TotalRequests)
	}
}

func TestRegistry_Touch_UpdatesExisting(t *testing.T) {
	r, _ := NewRegistry(filepath.Join(t.TempDir(), "agents.yaml"))

	r.Touch("agent1", "anthropic", "claude-3")
	r.Touch("agent1", "anthropic", "claude-4")

	a, _ := r.Get("agent1")
	if a.Stats.TotalRequests != 2 {
		t.Errorf("TotalRequests: expected 2, got %d", a.Stats.TotalRequests)
	}
	if a.Model != "claude-4" {
		t.Errorf("Model should be updated to claude-4, got %q", a.Model)
	}
}

func TestRegistry_Get_NotFound(t *testing.T) {
	r, _ := NewRegistry(filepath.Join(t.TempDir(), "agents.yaml"))

	_, err := r.Get("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestRegistry_List(t *testing.T) {
	r, _ := NewRegistry(filepath.Join(t.TempDir(), "agents.yaml"))

	r.Touch("a1", "anthropic", "claude")
	r.Touch("a2", "openai", "gpt-4")

	agents := r.List()
	if len(agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(agents))
	}
}

func TestRegistry_RecordToolCall(t *testing.T) {
	r, _ := NewRegistry(filepath.Join(t.TempDir(), "agents.yaml"))

	r.Touch("agent1", "anthropic", "claude")
	r.RecordToolCall("agent1", false)
	r.RecordToolCall("agent1", true)
	r.RecordToolCall("agent1", false)

	a, _ := r.Get("agent1")
	if a.Stats.TotalToolCalls != 3 {
		t.Errorf("TotalToolCalls: expected 3, got %d", a.Stats.TotalToolCalls)
	}
	if a.Stats.BlockedToolCalls != 1 {
		t.Errorf("BlockedToolCalls: expected 1, got %d", a.Stats.BlockedToolCalls)
	}
}

func TestRegistry_RecordToolCall_UnknownAgent(t *testing.T) {
	r, _ := NewRegistry(filepath.Join(t.TempDir(), "agents.yaml"))

	// Should not panic on unknown agent.
	r.RecordToolCall("unknown", true)
}

func TestRegistry_SetStatus(t *testing.T) {
	r, _ := NewRegistry(filepath.Join(t.TempDir(), "agents.yaml"))

	r.Touch("agent1", "anthropic", "claude")
	r.SetStatus("agent1", "killed")

	a, _ := r.Get("agent1")
	if a.Status != "killed" {
		t.Errorf("Status: expected killed, got %q", a.Status)
	}
}

func TestRegistry_SaveAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")

	r, _ := NewRegistry(path)
	r.Touch("agent1", "anthropic", "claude")
	r.RecordToolCall("agent1", false)
	r.RecordToolCall("agent1", true)

	if err := r.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload.
	r2, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}

	a, err := r2.Get("agent1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Provider != "anthropic" {
		t.Errorf("reloaded Provider: expected anthropic, got %q", a.Provider)
	}
	if a.Stats.TotalRequests != 1 {
		t.Errorf("reloaded TotalRequests: expected 1, got %d", a.Stats.TotalRequests)
	}
	if a.Stats.TotalToolCalls != 2 {
		t.Errorf("reloaded TotalToolCalls: expected 2, got %d", a.Stats.TotalToolCalls)
	}
	if a.Stats.BlockedToolCalls != 1 {
		t.Errorf("reloaded BlockedToolCalls: expected 1, got %d", a.Stats.BlockedToolCalls)
	}
}
