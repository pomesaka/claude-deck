package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

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

// GenerateSessionID creates a unique session identifier.
func GenerateSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
