package session

import (
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
)

// DataSource identifies where a session field update originates from.
// This type documents the "projection" rules: which source owns which fields.
//
// Session state is projected (assembled) from multiple sources:
//
//	Store  → ID, Name, RepoPath, RepoName, WorkspacePath, WorkspaceName,
//	         SubProjectDir, SessionChain, Status, FinishedAt, PID, BookmarkName
//	JSONL  → Prompt, PermissionMode, StartedAt, LastActivity, TokenUsage
//	Hook   → Status (transitions), SessionChain (append on /clear)
//	PTY    → LogLines, CurrentTool, Status (Running via spinner detection)
//
// The Apply* methods below encapsulate these projection rules so that callers
// (Manager, file watchers, hook processors) don't need to know which fields
// to update — they just call the appropriate Apply method with the source data.
type DataSource int

const (
	SourceStore DataSource = iota
	SourceJSONL
	SourceHook
	SourcePTY
)

func (d DataSource) String() string {
	switch d {
	case SourceStore:
		return "Store"
	case SourceJSONL:
		return "JSONL"
	case SourceHook:
		return "Hook"
	case SourcePTY:
		return "PTY"
	default:
		return "Unknown"
	}
}

// JSONLTokenData holds token usage data extracted from JSONL files.
// Used by ApplyJSONLTokens to update session token state.
type JSONLTokenData struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// ApplyJSONLTokens updates token usage from JSONL data using the given pricing.
// This is the single point where JSONL token data flows into Session state.
// Replaces the scattered token field assignments in hydrateSession.
func (s *Session) ApplyJSONLTokens(data JSONLTokenData, pricing PricingPolicy) {
	tu := TokenUsage{
		InputTokens:              data.InputTokens,
		OutputTokens:             data.OutputTokens,
		CacheCreationInputTokens: data.CacheCreationInputTokens,
		CacheReadInputTokens:     data.CacheReadInputTokens,
	}
	tu.EstimatedCostUSD = tu.EstimateCost(pricing)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.TokenUsage = tu
}

// ApplyFileActivity updates LastActivity from a JSONL file write event.
// This is the single point where file modification timestamps flow into Session state.
func (s *Session) ApplyFileActivity(modTime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastActivity = modTime
}

// ApplyBookmark updates the nearest jj bookmark name.
// Only updates if the new bookmark differs from the current one.
func (s *Session) ApplyBookmark(bookmark string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.BookmarkName != bookmark {
		debuglog.Printf("[session:%s] bookmark %s -> %s", s.ID, s.BookmarkName, bookmark)
		s.BookmarkName = bookmark
	}
}
