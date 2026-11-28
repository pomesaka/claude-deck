package session

import (
	"fmt"
	"strings"
	"time"
)

// ContextUsage is the context window's fill as Claude Code measured it: the
// input tokens its last response was answered over, against the window of the
// session's model. claude-deck copies it and never estimates it.
type ContextUsage struct {
	UsedTokens   int
	WindowTokens int
}

// Percent is UsedTokens over WindowTokens as a whole percentage, rounded down;
// 0 when the window is 0.
func (c ContextUsage) Percent() int {
	if c.WindowTokens <= 0 {
		return 0
	}
	return c.UsedTokens * 100 / c.WindowTokens
}

// contextFromRecord is the fill of a row, or nil until both figures are known.
func contextFromRecord(tokens, window *int) *ContextUsage {
	if tokens == nil || window == nil {
		return nil
	}
	return &ContextUsage{UsedTokens: *tokens, WindowTokens: *window}
}

// HintKind names what a Hint suggests.
type HintKind string

const (
	// HintContextFull: the conversation is long enough that it is worth clearing.
	HintContextFull HintKind = "context_full"
	// HintAPIError: an API error ended the turn, and the session waits for anything to be sent.
	HintAPIError HintKind = "api_error"
	// HintStale: the session has not moved for staleAfter; close it if it is not needed.
	HintStale HintKind = "stale"
	// HintUnsaved: a stale session's workspace holds work that is not committed or not pushed.
	HintUnsaved HintKind = "unsaved"
)

// Hint is a next step suggested for a session, derived from what the store
// holds each time it is read. It is never stored.
type Hint struct {
	Kind    HintKind
	Message string
}

// contextFullPercent is the fill at which a session is told to clear.
// WHY 50%: 仮の値。根拠になる測定や公式の目安は見つかっていない（2026-11-28）。
// 毎ターン、使っている文脈をまるごとキャッシュから読み直すので、読み直す量は使用率に比例する。
// 自動の compaction（窓の上限の手前で走る）を待たず、半分を越えたら区切りを勧める。
const contextFullPercent = 50

// Hints returns the next steps suggested for the session at now, in this order:
//   - HintAPIError while an API error has stopped it (waiting_action of WaitAPIError).
//   - HintContextFull when its context is at least contextFullPercent full or it
//     has compacted at least once.
//   - HintStale when IsStale, and HintUnsaved after it when changes, its
//     workspace's work outside trunk(), is not empty. changes is nil when it was
//     not read.
//
// HintAPIError and HintContextFull need a process: there is nothing to resume or clear without one.
func Hints(s Snapshot, now time.Time, changes *WorkspaceChanges) []Hint {
	var hints []Hint
	if s.HasProcess() {
		hints = append(hints, processHints(s)...)
	}
	if IsStale(s, now) {
		days := int(now.Sub(s.LastMoved()) / (24 * time.Hour))
		hints = append(hints, Hint{
			Kind:    HintStale,
			Message: fmt.Sprintf("%d日動いていない。要らなければ claude-deck close %s", days, s.ID),
		})
	}
	if changes != nil && !changes.Empty() && NeedsWorkspaceRead(s, now) {
		hints = append(hints, Hint{Kind: HintUnsaved, Message: unsavedMessage(*changes)})
	}
	return hints
}

// processHints are the hints of a session with a live process.
func processHints(s Snapshot) []Hint {
	var hints []Hint
	if s.Status == StatusWaitingAction && s.WaitKind == WaitAPIError {
		hints = append(hints, Hint{
			Kind:    HintAPIError,
			Message: "API の誤りで止まっている。何か送ると再開する（例: SendMessage か send-to-sessions で「続けて」を送る）",
		})
	}
	full := s.Context != nil && s.Context.WindowTokens > 0 && s.Context.Percent() >= contextFullPercent
	compacted := s.Compactions != nil && *s.Compactions > 0
	if full || compacted {
		hints = append(hints, Hint{
			Kind: HintContextFull,
			// CLI のフラグを変えたら、この文も直す（cmd/claude-deck の parseClearArgs）。
			Message: fmt.Sprintf("文脈が詰まっている（%s）。きりのいい所で claude-deck clear --id %s を打つ", contextFigures(s), s.ID),
		})
	}
	return hints
}

// staleAfter is how long a session may go without moving before it is told to close.
// WHY 2 日: 仮の値。根拠になる測定は無い（2026-11-28、pomesaka の目安）。
// 一晩おいて翌日に続ける仕事は stale にしない。
const staleAfter = 48 * time.Hour

// IsStale reports whether the session has not moved for staleAfter at now and
// closing it would do something: it has a process, or a workspace to remove.
// A session at work (running, subagent_running) is never stale, however long its turn.
// A session with no time known is not stale: how long it has rested is not known.
func IsStale(s Snapshot, now time.Time) bool {
	if s.Status == StatusRunning || s.Status == StatusSubagentRunning {
		return false
	}
	if !s.HasProcess() && !s.Isolation.HasWorkspace() {
		return false
	}
	last := s.LastMoved()
	return !last.IsZero() && now.Sub(last) >= staleAfter
}

// NeedsWorkspaceRead reports whether Hints wants the session's workspace
// read: it is stale and has a workspace. The caller reads only then, since jj
// takes a while for each session.
func NeedsWorkspaceRead(s Snapshot, now time.Time) bool {
	return IsStale(s, now) && s.Isolation.HasWorkspace()
}

// unsavedMessage says what of c is not empty.
func unsavedMessage(c WorkspaceChanges) string {
	var parts []string
	if c.Files > 0 {
		parts = append(parts, fmt.Sprintf("未コミットの変更が %d ファイル", c.Files))
	}
	if c.Commits > 0 {
		parts = append(parts, fmt.Sprintf("push していないコミットが %d 個", c.Commits))
	}
	return strings.Join(parts, "、") + "ある。閉じる前に入れるか要らないかを決める"
}

// WorkspaceChanges is the work a session's workspace holds that nobody else has.
type WorkspaceChanges struct {
	// Files is how many files the working-copy commit (@) changes.
	Files int
	// Commits is how many non-empty commits between trunk() and @'s parent
	// were not pushed.
	Commits int
}

// Empty reports whether the workspace holds no work of its own.
func (c WorkspaceChanges) Empty() bool { return c.Files == 0 && c.Commits == 0 }

// contextFigures is what Hints says it measured: the figures that are known.
func contextFigures(s Snapshot) string {
	var percent, compactions string
	if s.Context != nil && s.Context.WindowTokens > 0 {
		percent = fmt.Sprintf("使用率 %d%%", s.Context.Percent())
	}
	if s.Compactions != nil {
		compactions = fmt.Sprintf("compaction %d 回", *s.Compactions)
	}
	switch {
	case percent != "" && compactions != "":
		return percent + "、" + compactions
	case percent != "":
		return percent
	default:
		return compactions
	}
}
