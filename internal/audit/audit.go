package audit

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is a single audit log record. Each entry contains the full context
// of a tool call evaluation, lifecycle event, or kill switch trigger.
//
// The hash chain links entries: each entry's Hash depends on the previous
// entry's PrevHash, making the log tamper-evident.
//
// See design doc Section 8.2 for the JSON schema.
type Entry struct {
	Seq       uint64 `json:"seq"`
	Timestamp string `json:"ts"`
	Agent     string `json:"agent"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Type      string `json:"type"`                // "tool_call", "kill", "lifecycle"
	Tool      string `json:"tool,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
	Decision  string `json:"decision"`
	Rule      string `json:"rule,omitempty"`
	Message   string `json:"message,omitempty"`
	LatencyUs int64  `json:"latency_us,omitempty"`
	PrevHash  string `json:"prev_hash"`
	Hash      string `json:"hash"`
}

// QueryParams defines filters for querying the audit log.
// All fields are optional — empty/zero values mean "no filter".
type QueryParams struct {
	Agent    string // Filter by agent ID (exact match).
	Decision string // Filter by decision: "allow" or "block".
	Since    string // ISO timestamp or duration string (e.g. "1h", "24h").
	Limit    int    // Maximum entries to return.
}

// VerifyResult holds the outcome of a hash chain verification.
type VerifyResult struct {
	Valid          bool   `json:"valid"`
	EntriesChecked int    `json:"entries_checked"`
	BrokenAt       int    `json:"broken_at,omitempty"`
	ExpectedHash   string `json:"expected_hash,omitempty"`
	ActualHash     string `json:"actual_hash,omitempty"`
}

// AuditLog manages the hash-chained, append-only audit log.
//
// Storage layout (design doc Section 8.1):
//
//	~/.ctrlai/audit/
//	├── genesis.json        # First entry, establishes chain
//	├── 2026-02-10.jsonl    # Today's entries (append-only)
//	└── index.db            # SQLite index for fast queries
//
// Thread-safe — the proxy writes entries concurrently from multiple
// HTTP handler goroutines.
type AuditLog struct {
	mu       sync.Mutex
	dir      string        // Path to the audit directory.
	seq      uint64        // Next sequence number.
	lastHash string        // Hash of the last entry (for chain continuity).
	index    *sqliteIndex  // SQLite index for fast queries.
	file     *os.File      // Currently open daily JSONL file.
	fileDate string        // Date string of the currently open file (YYYY-MM-DD).
}

// New opens or creates an audit log in the given directory.
// If the directory doesn't exist, it's created. If no genesis block
// exists, one is created to establish the hash chain.
func New(dir string) (*AuditLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating audit directory %s: %w", dir, err)
	}

	a := &AuditLog{
		dir:      dir,
		lastHash: "sha256:genesis",
	}

	// Open the SQLite index.
	idx, err := openIndex(filepath.Join(dir, "index.db"))
	if err != nil {
		return nil, fmt.Errorf("opening audit index: %w", err)
	}
	a.index = idx

	// Load the genesis block to establish chain continuity.
	if err := a.loadGenesis(); err != nil {
		idx.close()
		return nil, err
	}

	// Scan existing JSONL files to find the last sequence number and hash.
	// This ensures we continue the chain correctly after restart.
	if err := a.recoverState(); err != nil {
		idx.close()
		return nil, err
	}

	slog.Info("audit log initialized", "dir", dir, "seq", a.seq)
	return a, nil
}

// Close flushes and closes the audit log and SQLite index.
func (a *AuditLog) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	var errs []error
	if a.file != nil {
		if err := a.file.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if a.index != nil {
		if err := a.index.close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("closing audit log: %v", errs)
	}
	return nil
}

// LogToolCall records a tool call evaluation in the audit log.
// Called by the proxy after each tool call is evaluated against rules.
//
// Parameters match the entry format from design doc Section 8.2.
func (a *AuditLog) LogToolCall(agent, provider, model, tool string, arguments any, decision, rule, message string, latencyUs int64) {
	a.append(Entry{
		Agent:     agent,
		Provider:  provider,
		Model:     model,
		Type:      "tool_call",
		Tool:      tool,
		Arguments: arguments,
		Decision:  decision,
		Rule:      rule,
		Message:   message,
		LatencyUs: latencyUs,
	})
}

// LogKill records a kill switch activation in the audit log.
func (a *AuditLog) LogKill(agent, reason string) {
	a.append(Entry{
		Agent:    agent,
		Type:     "kill",
		Decision: "block",
		Message:  reason,
	})
}

// LogLifecycle records a proxy lifecycle event (start, stop, config change).
func (a *AuditLog) LogLifecycle(event string, metadata map[string]any) {
	a.append(Entry{
		Type:      "lifecycle",
		Tool:      event,
		Decision:  "info",
		Arguments: metadata,
	})
}

// Tail returns the N most recent audit entries.
func (a *AuditLog) Tail(limit int) ([]Entry, error) {
	if a.index != nil {
		return a.index.tail(limit)
	}
	// Fallback: read from JSONL files (slower).
	return a.readAllEntries(limit)
}

// Follow watches for new audit entries in real-time, calling the callback
// for each new entry. Blocks until the context is cancelled.
// Similar to `tail -f` for the audit log.
func (a *AuditLog) Follow(ctx context.Context, callback func(Entry)) error {
	// Poll the JSONL file for new entries every 500ms.
	// A more sophisticated approach would use fsnotify, but polling is
	// simple and reliable for the tail -f use case.
	lastSeq := a.seq
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			entries, err := a.readEntriesAfter(lastSeq)
			if err != nil {
				slog.Error("follow: error reading entries", "error", err)
				continue
			}
			for _, e := range entries {
				callback(e)
				if e.Seq > lastSeq {
					lastSeq = e.Seq
				}
			}
		}
	}
}

// Query retrieves entries matching the given filter parameters.
// Uses the SQLite index for fast filtered queries.
func (a *AuditLog) Query(params QueryParams) ([]Entry, error) {
	// Convert "since" duration string (e.g. "1h", "24h") to ISO timestamp.
	if params.Since != "" && !strings.Contains(params.Since, "T") {
		// Parse as Go duration (e.g. "1h", "30m", "24h").
		d, err := time.ParseDuration(params.Since)
		if err != nil {
			return nil, fmt.Errorf("invalid since duration %q: %w", params.Since, err)
		}
		params.Since = time.Now().UTC().Add(-d).Format(time.RFC3339Nano)
	}

	if a.index != nil {
		return a.index.query(params)
	}

	// Fallback: read all entries and filter in memory.
	return a.readAllEntriesFiltered(params)
}

// VerifyChain reads all audit entries and verifies the hash chain integrity.
// Each entry's hash must match SHA-256(prev_hash | seq | ts | agent | tool | decision).
// Returns the verification result, including where the chain broke (if at all).
func (a *AuditLog) VerifyChain() (VerifyResult, error) {
	entries, err := a.readAllEntries(0) // 0 = no limit, read all
	if err != nil {
		return VerifyResult{}, fmt.Errorf("reading entries for verification: %w", err)
	}

	if len(entries) == 0 {
		return VerifyResult{Valid: true, EntriesChecked: 0}, nil
	}

	for i, e := range entries {
		expected := computeHash(&e)
		if e.Hash != expected {
			return VerifyResult{
				Valid:          false,
				EntriesChecked: i + 1,
				BrokenAt:       i,
				ExpectedHash:   expected,
				ActualHash:     e.Hash,
			}, nil
		}

		// Also verify chain linkage: entry's PrevHash must match
		// the previous entry's Hash (except for the genesis entry).
		if i > 0 && e.PrevHash != entries[i-1].Hash {
			return VerifyResult{
				Valid:          false,
				EntriesChecked: i + 1,
				BrokenAt:       i,
				ExpectedHash:   entries[i-1].Hash,
				ActualHash:     e.PrevHash,
			}, nil
		}
	}

	return VerifyResult{Valid: true, EntriesChecked: len(entries)}, nil
}

// Export writes all audit entries to the given writer in the specified format.
// Supported formats: "jsonl" (default), "json", "csv".
func (a *AuditLog) Export(w io.Writer, format string) error {
	entries, err := a.readAllEntries(0)
	if err != nil {
		return fmt.Errorf("reading entries for export: %w", err)
	}

	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)

	case "csv":
		cw := csv.NewWriter(w)
		defer cw.Flush()
		// Write header.
		if err := cw.Write([]string{"seq", "ts", "agent", "provider", "model", "type", "tool", "decision", "rule", "latency_us", "hash"}); err != nil {
			return err
		}
		for _, e := range entries {
			if err := cw.Write([]string{
				fmt.Sprintf("%d", e.Seq),
				e.Timestamp,
				e.Agent,
				e.Provider,
				e.Model,
				e.Type,
				e.Tool,
				e.Decision,
				e.Rule,
				fmt.Sprintf("%d", e.LatencyUs),
				e.Hash,
			}); err != nil {
				return err
			}
		}
		return nil

	case "jsonl", "":
		enc := json.NewEncoder(w)
		for _, e := range entries {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil

	default:
		return fmt.Errorf("unsupported export format: %s (use json, jsonl, or csv)", format)
	}
}

// append adds an entry to the audit log. Thread-safe.
// Computes the hash chain, writes to the daily JSONL file, and updates
// the SQLite index.
func (a *AuditLog) append(e Entry) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Fill in chain fields.
	a.seq++
	e.Seq = a.seq
	e.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	e.PrevHash = a.lastHash
	e.Hash = computeHash(&e)

	// Write to the daily JSONL file.
	if err := a.writeToFile(&e); err != nil {
		slog.Error("audit write failed", "seq", e.Seq, "error", err)
		return
	}

	// Update the SQLite index (non-blocking, errors logged internally).
	if a.index != nil {
		a.index.insert(&e)
	}

	// Update chain state.
	a.lastHash = e.Hash
}

// writeToFile appends the entry as a single JSON line to today's JSONL file.
// Opens a new file if the date has changed.
func (a *AuditLog) writeToFile(e *Entry) error {
	today := time.Now().UTC().Format("2006-01-02")

	// Rotate to a new file if the date has changed.
	if a.file == nil || a.fileDate != today {
		if a.file != nil {
			a.file.Close()
		}

		path := filepath.Join(a.dir, today+".jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("opening audit file %s: %w", path, err)
		}
		a.file = f
		a.fileDate = today
	}

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshaling audit entry: %w", err)
	}

	// Write the JSON line followed by a newline.
	if _, err := a.file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing audit entry: %w", err)
	}

	// Flush immediately — audit entries must survive crashes.
	return a.file.Sync()
}

// loadGenesis loads or creates the genesis block that establishes the chain.
// The genesis block has seq=0 and a fixed prev_hash.
func (a *AuditLog) loadGenesis() error {
	genesisPath := filepath.Join(a.dir, "genesis.json")

	data, err := os.ReadFile(genesisPath)
	if err != nil {
		if os.IsNotExist(err) {
			return a.createGenesis(genesisPath)
		}
		return fmt.Errorf("reading genesis: %w", err)
	}

	var genesis Entry
	if err := json.Unmarshal(data, &genesis); err != nil {
		return fmt.Errorf("parsing genesis: %w", err)
	}

	a.lastHash = genesis.Hash
	a.seq = genesis.Seq
	return nil
}

// createGenesis writes the genesis block that starts the hash chain.
func (a *AuditLog) createGenesis(path string) error {
	genesis := Entry{
		Seq:       0,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Type:      "lifecycle",
		Tool:      "genesis",
		Decision:  "info",
		PrevHash:  "sha256:genesis",
	}
	genesis.Hash = computeHash(&genesis)

	data, err := json.MarshalIndent(genesis, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling genesis: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing genesis: %w", err)
	}

	a.lastHash = genesis.Hash
	a.seq = 0

	slog.Info("audit genesis created", "hash", genesis.Hash)
	return nil
}

// recoverState scans existing JSONL files to find the last seq and hash.
// This ensures the chain continues correctly after a proxy restart.
func (a *AuditLog) recoverState() error {
	// List all JSONL files in the audit directory.
	files, err := filepath.Glob(filepath.Join(a.dir, "*.jsonl"))
	if err != nil {
		return fmt.Errorf("listing audit files: %w", err)
	}

	if len(files) == 0 {
		return nil
	}

	// Read the last entry from the most recent file (files are date-sorted).
	// We only need the last entry's seq and hash to continue the chain.
	lastFile := files[len(files)-1]
	lastEntry, err := readLastEntry(lastFile)
	if err != nil {
		return fmt.Errorf("recovering audit state from %s: %w", lastFile, err)
	}

	if lastEntry != nil {
		a.seq = lastEntry.Seq
		a.lastHash = lastEntry.Hash

		// Re-index entries that might be missing from the SQLite index
		// (e.g. if the proxy crashed before indexing).
		if a.index != nil {
			a.reindex(files)
		}
	}

	return nil
}

// reindex scans JSONL files and inserts any entries missing from the
// SQLite index. Called on startup to recover from incomplete indexing.
func (a *AuditLog) reindex(files []string) {
	indexLastSeq := a.index.lastSeq()

	for _, file := range files {
		entries, err := readEntriesFromFile(file)
		if err != nil {
			slog.Error("reindex: error reading file", "file", file, "error", err)
			continue
		}
		for _, e := range entries {
			if e.Seq > indexLastSeq {
				a.index.insert(&e)
			}
		}
	}
}

// readLastEntry reads the last non-empty line from a JSONL file and
// parses it as an Entry. Returns nil if the file is empty.
func readLastEntry(path string) (*Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lastLine string
	scanner := bufio.NewScanner(f)
	// Set a large buffer for potentially long argument JSON.
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) != "" {
			lastLine = line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if lastLine == "" {
		return nil, nil
	}

	var entry Entry
	if err := json.Unmarshal([]byte(lastLine), &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

// readEntriesFromFile reads all entries from a single JSONL file.
func readEntriesFromFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			slog.Warn("skipping malformed audit entry", "error", err)
			continue
		}
		entries = append(entries, e)
	}
	return entries, scanner.Err()
}

// readAllEntries reads entries from all JSONL files. If limit > 0, returns
// only the last N entries. If limit == 0, returns all entries.
func (a *AuditLog) readAllEntries(limit int) ([]Entry, error) {
	files, err := filepath.Glob(filepath.Join(a.dir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("listing audit files: %w", err)
	}

	var all []Entry
	for _, file := range files {
		entries, err := readEntriesFromFile(file)
		if err != nil {
			return nil, err
		}
		all = append(all, entries...)
	}

	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

// readAllEntriesFiltered reads all entries and applies filters in memory.
// Used as a fallback when the SQLite index is unavailable.
func (a *AuditLog) readAllEntriesFiltered(params QueryParams) ([]Entry, error) {
	entries, err := a.readAllEntries(0)
	if err != nil {
		return nil, err
	}

	var filtered []Entry
	for _, e := range entries {
		if params.Agent != "" && e.Agent != params.Agent {
			continue
		}
		if params.Decision != "" && e.Decision != params.Decision {
			continue
		}
		if params.Since != "" && e.Timestamp < params.Since {
			continue
		}
		filtered = append(filtered, e)
	}

	if params.Limit > 0 && len(filtered) > params.Limit {
		filtered = filtered[len(filtered)-params.Limit:]
	}
	return filtered, nil
}

// readEntriesAfter reads entries with seq > afterSeq from today's JSONL file.
// Used by Follow() for efficient polling.
func (a *AuditLog) readEntriesAfter(afterSeq uint64) ([]Entry, error) {
	today := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(a.dir, today+".jsonl")

	entries, err := readEntriesFromFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result []Entry
	for _, e := range entries {
		if e.Seq > afterSeq {
			result = append(result, e)
		}
	}
	return result, nil
}
