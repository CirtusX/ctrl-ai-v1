// Package audit implements the tamper-proof, hash-chained audit log.
//
// Every tool call evaluation, kill switch event, and proxy lifecycle event
// is recorded as an Entry in an append-only JSONL file. Each entry's hash
// is computed as SHA-256(prev_hash | seq | timestamp | agent | tool | decision),
// forming a hash chain where tampering with any entry breaks the chain
// from that point forward.
//
// See design doc Section 8 for the audit log design.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// computeHash calculates the SHA-256 hash for an audit entry.
// The hash depends on the previous entry's hash, creating a chain
// where modifying any entry invalidates all subsequent entries.
//
// Hash formula from design doc Section 8.3:
//
//	SHA-256(prev_hash | seq | timestamp | agent | tool | decision)
//
// Returns a prefixed hash string: "sha256:<hex>".
func computeHash(e *Entry) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%s|%s|%s",
		e.PrevHash, e.Seq, e.Timestamp,
		e.Agent, e.Tool, e.Decision)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// verifyEntry checks whether an entry's hash is valid given its contents.
// Returns true if the stored hash matches the computed hash.
func verifyEntry(e *Entry) bool {
	expected := computeHash(e)
	return e.Hash == expected
}
