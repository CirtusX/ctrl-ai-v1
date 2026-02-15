package audit

import (
	"strings"
	"testing"
)

func TestComputeHash_Deterministic(t *testing.T) {
	e := &Entry{
		Seq:       1,
		Timestamp: "2026-02-12T10:00:00Z",
		Agent:     "test-agent",
		Tool:      "exec",
		Decision:  "allow",
		PrevHash:  "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}

	hash1 := computeHash(e)
	hash2 := computeHash(e)

	if hash1 != hash2 {
		t.Error("same input should produce the same hash")
	}
	if !strings.HasPrefix(hash1, "sha256:") {
		t.Errorf("hash should start with 'sha256:', got %q", hash1)
	}
}

func TestComputeHash_DifferentEntries(t *testing.T) {
	e1 := &Entry{Seq: 1, Agent: "a", Tool: "exec", Decision: "allow", PrevHash: "sha256:00"}
	e2 := &Entry{Seq: 2, Agent: "a", Tool: "exec", Decision: "allow", PrevHash: "sha256:00"}

	if computeHash(e1) == computeHash(e2) {
		t.Error("different seq should produce different hashes")
	}
}

func TestComputeHash_SensitiveToAllFields(t *testing.T) {
	base := Entry{
		Seq:       1,
		Timestamp: "2026-02-12T10:00:00Z",
		Agent:     "agent1",
		Tool:      "exec",
		Decision:  "allow",
		PrevHash:  "sha256:abc",
	}

	baseHash := computeHash(&base)

	// Change each field and verify hash changes.
	tests := []struct {
		name   string
		modify func(e *Entry)
	}{
		{"seq", func(e *Entry) { e.Seq = 99 }},
		{"timestamp", func(e *Entry) { e.Timestamp = "2026-12-31T00:00:00Z" }},
		{"agent", func(e *Entry) { e.Agent = "different" }},
		{"tool", func(e *Entry) { e.Tool = "read" }},
		{"decision", func(e *Entry) { e.Decision = "block" }},
		{"prev_hash", func(e *Entry) { e.PrevHash = "sha256:xyz" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modified := base // copy
			tt.modify(&modified)
			if computeHash(&modified) == baseHash {
				t.Errorf("changing %s should produce a different hash", tt.name)
			}
		})
	}
}

func TestVerifyEntry_Valid(t *testing.T) {
	e := &Entry{
		Seq:       0,
		Timestamp: "2026-02-12T10:00:00Z",
		Agent:     "",
		Tool:      "",
		Decision:  "info",
		PrevHash:  "0000000000000000000000000000000000000000000000000000000000000000",
	}
	e.Hash = computeHash(e)

	if !verifyEntry(e) {
		t.Error("entry with correct hash should verify as true")
	}
}

func TestVerifyEntry_TamperedHash(t *testing.T) {
	e := &Entry{
		Seq:      1,
		Agent:    "a",
		Tool:     "exec",
		Decision: "allow",
		PrevHash: "sha256:00",
	}
	e.Hash = "sha256:tampered"

	if verifyEntry(e) {
		t.Error("entry with tampered hash should verify as false")
	}
}

func TestVerifyEntry_TamperedField(t *testing.T) {
	e := &Entry{
		Seq:      1,
		Agent:    "a",
		Tool:     "exec",
		Decision: "allow",
		PrevHash: "sha256:00",
	}
	e.Hash = computeHash(e)

	// Tamper with the decision field after computing hash.
	e.Decision = "block"

	if verifyEntry(e) {
		t.Error("entry with tampered field should verify as false")
	}
}

func TestHashChain_Integrity(t *testing.T) {
	genesis := "0000000000000000000000000000000000000000000000000000000000000000"

	e1 := &Entry{Seq: 0, Timestamp: "t0", Decision: "info", PrevHash: genesis}
	e1.Hash = computeHash(e1)

	e2 := &Entry{Seq: 1, Timestamp: "t1", Agent: "a", Tool: "exec", Decision: "allow", PrevHash: e1.Hash}
	e2.Hash = computeHash(e2)

	e3 := &Entry{Seq: 2, Timestamp: "t2", Agent: "a", Tool: "read", Decision: "block", PrevHash: e2.Hash}
	e3.Hash = computeHash(e3)

	// All three should verify.
	if !verifyEntry(e1) {
		t.Error("e1 should verify")
	}
	if !verifyEntry(e2) {
		t.Error("e2 should verify")
	}
	if !verifyEntry(e3) {
		t.Error("e3 should verify")
	}

	// Tamper with e2 — e2 verification should fail.
	e2.Agent = "tampered"
	if verifyEntry(e2) {
		t.Error("tampered e2 should not verify")
	}

	// e3 still verifies on its own (it has the OLD e2.Hash as prev_hash,
	// but the chain is broken because e2 itself is invalid).
	// Individual verification of e3 still passes — you need chain verification
	// to catch this (verify e2.Hash == computeHash(e2) for the chain to hold).
}
