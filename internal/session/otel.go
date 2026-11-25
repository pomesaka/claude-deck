package session

import (
	"path/filepath"
	"strings"
)

// envOTelResourceAttributes carries the OpenTelemetry resource attributes that
// Claude Code attaches to the telemetry it exports.
const envOTelResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"

// otelResourceAttributes returns the OTEL_RESOURCE_ATTRIBUTES value that tags a
// session's telemetry with its deck ID, name and repository, so that a
// collector can filter the events by session. inherited is the value
// claude-deck itself was started with; its attributes other than deck.* are
// kept as they are, in front of the deck attributes. Empty values are left out.
//
// WHY no alias: the alias is renamed while the session runs, so the value
// taken at launch goes stale. Consumers look the alias up by deck.session_id.
// The attribute names are shared with xmetrics; change them only together.
func otelResourceAttributes(inherited string, sessionID DeckSessionID, name, repoPath string) string {
	repo := ""
	if repoPath != "" {
		repo = filepath.Base(repoPath)
	}
	var parts []string
	// WHY drop the inherited deck.* keys: sessions run `claude-deck new` and
	// `fork` themselves, and claude-deck then inherits the parent session's
	// attributes. Kept, the child's events would carry two deck.session_id.
	for kv := range strings.SplitSeq(inherited, ",") {
		if strings.TrimSpace(kv) == "" || strings.HasPrefix(strings.TrimSpace(kv), "deck.") {
			continue
		}
		parts = append(parts, kv)
	}
	for _, kv := range [][2]string{
		{"deck.session_id", string(sessionID)},
		{"deck.name", name},
		{"deck.repo", repo},
	} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+percentEncode(kv[1]))
		}
	}
	return strings.Join(parts, ",")
}

// percentEncode escapes every byte outside the URL unreserved set, which keeps
// ",", "=" and spaces in a value from breaking the key=value list.
func percentEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0F])
	}
	return b.String()
}
