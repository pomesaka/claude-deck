package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// DeckSessionID is a unique identifier for a claude-deck session (random hex).
// Distinct from RuntimeSessionID to prevent accidental mixing at the type level.
type DeckSessionID string

// RuntimeSessionID is the conversation/thread identifier assigned by the
// backing agent runtime (Claude Code, Codex, etc.).
//
// /clear, /compact, /new, or runtime-specific fork/resume flows may create a
// new runtime session ID while the deck session remains stable. The history is
// tracked in SessionChain.
type RuntimeSessionID string

// ClaudeSessionID is kept as a source-compatible alias while the codebase moves
// from Claude-specific naming to runtime-neutral naming.
//
// Deprecated: use RuntimeSessionID.
type ClaudeSessionID = RuntimeSessionID

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
