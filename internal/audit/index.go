package audit

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	_ "github.com/glebarez/go-sqlite"
)

// sqliteIndex provides fast queries over the audit log using SQLite.
// The JSONL files are the source of truth; the SQLite index is a
// queryable projection that can be rebuilt from the JSONL files.
//
// The index is stored at ~/.ctrlai/audit/index.db.
type sqliteIndex struct {
	db *sql.DB
}

// openIndex opens (or creates) the SQLite index database.
// Creates the entries table and indexes if they don't exist.
func openIndex(path string) (*sqliteIndex, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening sqlite index %s: %w", path, err)
	}

	// Create the entries table if it doesn't exist.
	// WAL mode is used for concurrent read/write (proxy writes, CLI reads).
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS entries (
			seq        INTEGER PRIMARY KEY,
			ts         TEXT NOT NULL,
			agent      TEXT NOT NULL DEFAULT '',
			provider   TEXT NOT NULL DEFAULT '',
			model      TEXT NOT NULL DEFAULT '',
			type       TEXT NOT NULL DEFAULT '',
			tool       TEXT NOT NULL DEFAULT '',
			arguments  TEXT NOT NULL DEFAULT '',
			decision   TEXT NOT NULL DEFAULT '',
			rule       TEXT NOT NULL DEFAULT '',
			latency_us INTEGER NOT NULL DEFAULT 0,
			hash       TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_agent ON entries(agent);
		CREATE INDEX IF NOT EXISTS idx_decision ON entries(decision);
		CREATE INDEX IF NOT EXISTS idx_ts ON entries(ts);
		CREATE INDEX IF NOT EXISTS idx_type ON entries(type);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("creating sqlite schema: %w", err)
	}

	return &sqliteIndex{db: db}, nil
}

// insert adds an entry to the SQLite index. Non-blocking — errors are
// logged but don't affect the primary JSONL audit log.
func (idx *sqliteIndex) insert(e *Entry) {
	argsJSON, _ := json.Marshal(e.Arguments)

	_, err := idx.db.Exec(
		`INSERT OR REPLACE INTO entries (seq, ts, agent, provider, model, type, tool, arguments, decision, rule, latency_us, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Seq, e.Timestamp, e.Agent, e.Provider, e.Model, e.Type,
		e.Tool, string(argsJSON), e.Decision, e.Rule, e.LatencyUs, e.Hash,
	)
	if err != nil {
		slog.Error("sqlite index insert failed", "seq", e.Seq, "error", err)
	}
}

// query retrieves entries from the SQLite index matching the given params.
func (idx *sqliteIndex) query(params QueryParams) ([]Entry, error) {
	query := "SELECT seq, ts, agent, provider, model, type, tool, arguments, decision, rule, latency_us, hash FROM entries WHERE 1=1"
	var args []any

	if params.Agent != "" {
		query += " AND agent = ?"
		args = append(args, params.Agent)
	}
	if params.Decision != "" {
		query += " AND decision = ?"
		args = append(args, params.Decision)
	}
	if params.Since != "" {
		// Since is an ISO timestamp string, computed by the caller.
		query += " AND ts >= ?"
		args = append(args, params.Since)
	}

	query += " ORDER BY seq DESC"

	if params.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, params.Limit)
	}

	rows, err := idx.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying sqlite index: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var argsJSON string
		err := rows.Scan(
			&e.Seq, &e.Timestamp, &e.Agent, &e.Provider, &e.Model,
			&e.Type, &e.Tool, &argsJSON, &e.Decision, &e.Rule,
			&e.LatencyUs, &e.Hash,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning sqlite row: %w", err)
		}
		// Parse arguments JSON back into any.
		if argsJSON != "" && argsJSON != "null" {
			var parsed any
			if jsonErr := json.Unmarshal([]byte(argsJSON), &parsed); jsonErr == nil {
				e.Arguments = parsed
			}
		}
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// tail returns the N most recent entries from the index.
func (idx *sqliteIndex) tail(limit int) ([]Entry, error) {
	return idx.query(QueryParams{Limit: limit})
}

// lastSeq returns the highest sequence number in the index.
// Returns 0 if the index is empty.
func (idx *sqliteIndex) lastSeq() uint64 {
	var seq sql.NullInt64
	err := idx.db.QueryRow("SELECT MAX(seq) FROM entries").Scan(&seq)
	if err != nil || !seq.Valid {
		return 0
	}
	return uint64(seq.Int64)
}

// close closes the SQLite database connection.
func (idx *sqliteIndex) close() error {
	return idx.db.Close()
}
