package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// DeckSessionID is a unique identifier for a claude-deck session (random hex).
// Distinct from ClaudeSessionID to prevent accidental mixing at the type level.
type DeckSessionID string

// ClaudeSessionID is a UUID assigned by Claude Code to a conversation session.
// /clear and /compact create new ClaudeSessionIDs; the history is tracked in SessionChain.
type ClaudeSessionID string

// =LOVE member names for workspace naming.
var loveMembers = []string{
	"emiri", "anna", "sana", "iori", "maika",
	"hana", "shoko", "risa", "kiara", "hitomi",
}

// GenerateWorkspaceName creates a workspace name from a random =LOVE member name + suffix.
func GenerateWorkspaceName() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	suffix := hex.EncodeToString(b)

	idx := make([]byte, 1)
	_, _ = rand.Read(idx)
	member := loveMembers[int(idx[0])%len(loveMembers)]

	return fmt.Sprintf("%s-%s", member, suffix)
}

// GenerateSessionID creates a unique deck session identifier.
func GenerateSessionID() DeckSessionID {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return DeckSessionID(hex.EncodeToString(b))
}
