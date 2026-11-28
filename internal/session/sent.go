package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
)

// SentSource is how claude-deck typed a text into a session on the user's behalf.
type SentSource string

const (
	SentByNew       SentSource = "new"       // the first instruction of `new --prompt`
	SentByFork      SentSource = "fork"      // the first instruction of `fork --prompt`
	SentBySend      SentSource = "send"      // `send`, `send --self`
	SentByBroadcast SentSource = "broadcast" // `broadcast`
)

// TextSHA256 is how a sent text is recorded and matched against the transcript:
// the hex SHA-256 of the text exactly as sent.
func TextSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// RecordSent notes that text was typed into s by source at now, and reports
// whether it did. Call it only once the text has reached the session.
//
// A slash command is not recorded: the transcript keeps it as a
// <command-name> line, not as the text typed, so its hash would match nothing,
// and the line already tells it from the user's words (Claude Code 2.1.332).
// So `clear` records nothing.
func RecordSent(st *store.Store, s Snapshot, source SentSource, text string, now time.Time) (bool, error) {
	if strings.HasPrefix(text, "/") {
		return false, nil
	}
	err := st.InsertSent(store.Sent{
		SessionID:       string(s.ID),
		ClaudeSessionID: string(s.RuntimeSessionID),
		SentAt:          now,
		Source:          string(source),
		TextSHA256:      TextSHA256(text),
	})
	if err != nil {
		return false, fmt.Errorf("recording the text sent to %s: %w", s.ID, err)
	}
	return true, nil
}
