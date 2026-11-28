package store

import (
	"fmt"
	"time"
)

// Sent is one text claude-deck typed into a session on the user's behalf:
// the first instruction of new / fork --prompt, or a line of send, clear or
// broadcast. Claude Code records such a text as the user's own (origin human),
// so a reader of the transcript tells them apart by this row.
//
// WHY 文でなく hash を持つ: 文は会話の記録にあり、照らすには hash で足りる。長い依頼文を store に写さない。
// WHY NOT 文に印を足す: 受けたモデルの読み方が変わる。
type Sent struct {
	// SessionID is the deck session the text went to.
	SessionID string
	// ClaudeSessionID is the session's Claude Code session ID when the text was
	// sent; empty for the first instruction of a session that has none yet.
	ClaudeSessionID string
	SentAt          time.Time
	// Source is how it was sent (session.SentSource).
	Source string
	// TextSHA256 is the hex SHA-256 of the text exactly as sent.
	TextSHA256 string
}

// sentSchema is the table of Sent rows. It is not one of the sessions' columns:
// a row stays after its session is pruned, as the transcript it marks does.
const sentSchema = `
CREATE TABLE IF NOT EXISTS sent_texts (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id        TEXT NOT NULL,
	claude_session_id TEXT NOT NULL DEFAULT '',
	sent_at           INTEGER NOT NULL,
	source            TEXT NOT NULL,
	text_sha256       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS sent_texts_sent_at ON sent_texts (sent_at);`

// InsertSent records a text sent to a session.
func (s *Store) InsertSent(r Sent) error {
	_, err := s.db.Exec(
		"INSERT INTO sent_texts (session_id, claude_session_id, sent_at, source, text_sha256) VALUES (?, ?, ?, ?, ?)",
		r.SessionID, r.ClaudeSessionID, unixNano(r.SentAt), r.Source, r.TextSHA256)
	return err
}

// FillSentClaudeSessionID gives the rows of sessionID that have no Claude Code
// session ID (a first instruction, sent before the session had one) claudeID.
func (s *Store) FillSentClaudeSessionID(sessionID, claudeID string) error {
	_, err := s.db.Exec("UPDATE sent_texts SET claude_session_id = ? WHERE session_id = ? AND claude_session_id = ''", claudeID, sessionID)
	return err
}

// ListSent returns the texts sent at since or later, oldest first.
func (s *Store) ListSent(since time.Time) ([]Sent, error) {
	rows, err := s.db.Query(
		"SELECT session_id, claude_session_id, sent_at, source, text_sha256 FROM sent_texts WHERE sent_at >= ? ORDER BY sent_at, id",
		unixNano(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sent
	for rows.Next() {
		var (
			r  Sent
			at int64
		)
		if err := rows.Scan(&r.SessionID, &r.ClaudeSessionID, &at, &r.Source, &r.TextSHA256); err != nil {
			return nil, fmt.Errorf("reading sent_texts: %w", err)
		}
		r.SentAt = fromUnixNano(at)
		out = append(out, r)
	}
	return out, rows.Err()
}
