// Package store persists deck session metadata in SQLite.
//
// The store is the source of truth shared by every claude-deck process: the TUI,
// the CLI subcommands, and the hook commands run from inside Claude Code
// sessions (ADR-011). Every mutation is a read-modify-write inside a
// BEGIN IMMEDIATE transaction, so concurrent writers from different processes
// never lose each other's updates.
package store

import (
	"context"
	"database/sql"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// FileName is the basename of the SQLite database under the data directory.
const FileName = "deck.db"

// ErrNotFound is returned when no record has the requested ID.
var ErrNotFound = errors.New("session not found")

// Record is one deck session row. The store does not interpret the values;
// the session package owns their meaning (Status is a session.Status ID string).
type Record struct {
	ID            string
	Name          string
	RepoPath      string
	RepoName      string
	WorkspacePath string
	WorkspaceName string
	SubProjectDir string
	SessionChain  []string
	Status        string
	FinishedAt    *time.Time
	PID           int
	ErrorMessage  string
	TerminalTitle string
	BookmarkName  string

	LastJJRevision       string
	LastJJParentRevision string

	Prompt         string
	PermissionMode string
	StartedAt      time.Time
	LastActivity   time.Time

	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	EstimatedCostUSD         float64

	// ClosingAt is set while a close (TUI x / CLI close) is in progress.
	// It guards against two processes closing the same session at once.
	ClosingAt *time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id                          TEXT PRIMARY KEY,
	name                        TEXT NOT NULL DEFAULT '',
	repo_path                   TEXT NOT NULL DEFAULT '',
	repo_name                   TEXT NOT NULL DEFAULT '',
	workspace_path              TEXT NOT NULL DEFAULT '',
	workspace_name              TEXT NOT NULL DEFAULT '',
	sub_project_dir             TEXT NOT NULL DEFAULT '',
	session_chain               TEXT NOT NULL DEFAULT '[]',
	status                      TEXT NOT NULL,
	finished_at                 INTEGER,
	pid                         INTEGER NOT NULL DEFAULT 0,
	error_message               TEXT NOT NULL DEFAULT '',
	terminal_title              TEXT NOT NULL DEFAULT '',
	bookmark_name               TEXT NOT NULL DEFAULT '',
	last_jj_revision            TEXT NOT NULL DEFAULT '',
	last_jj_parent_revision     TEXT NOT NULL DEFAULT '',
	prompt                      TEXT NOT NULL DEFAULT '',
	permission_mode             TEXT NOT NULL DEFAULT '',
	started_at                  INTEGER NOT NULL DEFAULT 0,
	last_activity               INTEGER NOT NULL DEFAULT 0,
	input_tokens                INTEGER NOT NULL DEFAULT 0,
	output_tokens               INTEGER NOT NULL DEFAULT 0,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_input_tokens     INTEGER NOT NULL DEFAULT 0,
	estimated_cost_usd          REAL NOT NULL DEFAULT 0,
	closing_at                  INTEGER
);`

const columns = `id, name, repo_path, repo_name, workspace_path, workspace_name, sub_project_dir,
	session_chain, status, finished_at, pid, error_message, terminal_title, bookmark_name,
	last_jj_revision, last_jj_parent_revision, prompt, permission_mode, started_at, last_activity,
	input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
	estimated_cost_usd, closing_at`

// Store is a handle to the session database. Safe for concurrent use.
type Store struct {
	db *sql.DB
	// versionConn is a dedicated connection for PRAGMA data_version.
	// WHY 専用の接続: data_version は接続ごとの値で、「その接続以外」がコミットしたときだけ変わる。
	// 書き込みと同じプールの接続で読むと、どの接続に当たるかで値が飛び、変更を取りこぼす。
	versionConn *sql.Conn
}

// Open opens (creating if needed) the database under dataDir.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	return OpenPath(filepath.Join(dataDir, FileName))
}

// OpenPath opens the database at path.
func OpenPath(path string) (*Store, error) {
	// WHY _txlock=immediate: 読んでから書くトランザクションを DEFERRED で始めると、
	// 2 プロセスが同時に読んだ後どちらかの書き込みが SQLITE_BUSY で失敗する（busy_timeout も効かない）。
	// IMMEDIATE なら開始時点で書き込みロックを取り、後続は busy_timeout の範囲で待つ。
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening store: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("opening version connection: %w", err)
	}
	return &Store{db: db, versionConn: conn}, nil
}

// Close releases the database.
func (s *Store) Close() error {
	_ = s.versionConn.Close()
	return s.db.Close()
}

// DataVersion returns a value that changes whenever another connection
// (including other processes) commits a change.
func (s *Store) DataVersion() (int64, error) {
	var v int64
	err := s.versionConn.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&v)
	return v, err
}

// Get returns the record with the given ID, or ErrNotFound.
func (s *Store) Get(id string) (Record, error) {
	return get(s.db, id)
}

// List returns all records.
func (s *Store) List() ([]Record, error) {
	return list(s.db)
}

// Insert adds a new record. It fails if the ID already exists.
func (s *Store) Insert(r Record) error {
	return s.Tx(func(tx *Tx) error {
		if _, err := tx.Get(r.ID); err == nil {
			return fmt.Errorf("session %s already exists", r.ID)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		return tx.Put(r)
	})
}

// Update reads the record, applies fn, and writes it back in one transaction.
// If fn returns an error, nothing is written and the error is returned.
func (s *Store) Update(id string, fn func(r *Record) error) (Record, error) {
	var out Record
	err := s.Tx(func(tx *Tx) error {
		r, err := tx.Get(id)
		if err != nil {
			return err
		}
		if err := fn(&r); err != nil {
			return err
		}
		out = r
		return tx.Put(r)
	})
	return out, err
}

// Delete removes the record. Deleting a missing record is not an error.
func (s *Store) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", id)
	return err
}

// Tx runs fn inside a write transaction. Use it when a change depends on other
// records (e.g. uniqueness checks across sessions).
func (s *Store) Tx(fn func(tx *Tx) error) error {
	sqlTx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(&Tx{tx: sqlTx}); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	return sqlTx.Commit()
}

// Tx is a write transaction handed to Store.Tx callbacks.
type Tx struct {
	tx *sql.Tx
}

// Get returns the record with the given ID, or ErrNotFound.
func (t *Tx) Get(id string) (Record, error) { return get(t.tx, id) }

// List returns all records.
func (t *Tx) List() ([]Record, error) { return list(t.tx) }

// Delete removes the record.
func (t *Tx) Delete(id string) error {
	_, err := t.tx.Exec("DELETE FROM sessions WHERE id = ?", id)
	return err
}

// Put inserts or replaces the record.
func (t *Tx) Put(r Record) error {
	chain := r.SessionChain
	if chain == nil {
		chain = []string{}
	}
	chainJSON, err := json.Marshal(chain)
	if err != nil {
		return fmt.Errorf("marshaling session chain: %w", err)
	}
	_, err = t.tx.Exec(`INSERT OR REPLACE INTO sessions (`+columns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.RepoPath, r.RepoName, r.WorkspacePath, r.WorkspaceName, r.SubProjectDir,
		string(chainJSON), r.Status, nullableTime(r.FinishedAt), r.PID, r.ErrorMessage, r.TerminalTitle, r.BookmarkName,
		r.LastJJRevision, r.LastJJParentRevision, r.Prompt, r.PermissionMode, unixNano(r.StartedAt), unixNano(r.LastActivity),
		r.InputTokens, r.OutputTokens, r.CacheCreationInputTokens, r.CacheReadInputTokens,
		r.EstimatedCostUSD, nullableTime(r.ClosingAt),
	)
	return err
}

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func get(q querier, id string) (Record, error) {
	rows, err := q.Query("SELECT "+columns+" FROM sessions WHERE id = ?", id)
	if err != nil {
		return Record{}, err
	}
	recs, err := scanAll(rows)
	if err != nil {
		return Record{}, err
	}
	if len(recs) == 0 {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return recs[0], nil
}

func list(q querier) ([]Record, error) {
	rows, err := q.Query("SELECT " + columns + " FROM sessions ORDER BY id")
	if err != nil {
		return nil, err
	}
	return scanAll(rows)
}

func scanAll(rows *sql.Rows) ([]Record, error) {
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var (
			r                       Record
			chainJSON               string
			finishedAt, closingAt   sql.NullInt64
			startedAt, lastActivity int64
		)
		if err := rows.Scan(
			&r.ID, &r.Name, &r.RepoPath, &r.RepoName, &r.WorkspacePath, &r.WorkspaceName, &r.SubProjectDir,
			&chainJSON, &r.Status, &finishedAt, &r.PID, &r.ErrorMessage, &r.TerminalTitle, &r.BookmarkName,
			&r.LastJJRevision, &r.LastJJParentRevision, &r.Prompt, &r.PermissionMode, &startedAt, &lastActivity,
			&r.InputTokens, &r.OutputTokens, &r.CacheCreationInputTokens, &r.CacheReadInputTokens,
			&r.EstimatedCostUSD, &closingAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(chainJSON), &r.SessionChain); err != nil {
			return nil, fmt.Errorf("session %s: parsing session chain: %w", r.ID, err)
		}
		if len(r.SessionChain) == 0 {
			r.SessionChain = nil
		}
		r.FinishedAt = timePtr(finishedAt)
		r.ClosingAt = timePtr(closingAt)
		r.StartedAt = fromUnixNano(startedAt)
		r.LastActivity = fromUnixNano(lastActivity)
		out = append(out, r)
	}
	return out, rows.Err()
}

// WHY UnixNano の整数で持つ: 文字列の時刻はドライバの書式設定に依存し、比較・並べ替えも面倒になる。
// ゼロ値の time.Time は 0 として保存し、読み戻すときにゼロ値へ戻す。
func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func timePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64)
	return &t
}
