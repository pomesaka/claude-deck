package session

import (
	"errors"
	"fmt"
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

// Hints returns the next steps suggested for the session, in this order:
//   - HintAPIError while an API error has stopped it (waiting_action of WaitAPIError).
//   - HintContextFull when its context is at least contextFullPercent full or it
//     has compacted at least once.
//
// A session without a process gets none: there is nothing to resume or clear.
func Hints(s Snapshot) []Hint {
	if !s.HasProcess() {
		return nil
	}
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
			Kind:    HintContextFull,
			// CLI のフラグを変えたら、この文も直す（cmd/claude-deck の parseClearArgs）。
		Message: fmt.Sprintf("文脈が詰まっている（%s）。きりのいい所で claude-deck clear --id %s を打つ", contextFigures(s), s.ID),
		})
	}
	return hints
}

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

// Why CheckClear refuses to type /clear into a session.
var (
	ErrClearSelf      = errors.New("the session is this one")
	ErrClearBusy      = errors.New("the session is not waiting for input")
	ErrClearNoPrompt  = errors.New("the session's prompt was not found on its screen")
	ErrClearHalfTyped = errors.New("the session's prompt holds text the user is typing")
)

// clearable reports whether /clear may be typed in status s: the main
// loop has ended its turn and nothing waits on a dialog.
// WHY subagent_running を外す: 動いているサブエージェントの報告は、/clear の後の会話に届いても読む文脈が無い。
// WHY waiting_approval / waiting_answer を外す: 打った文がダイアログの選択に入る。
func clearable(s Status) bool {
	return s == StatusIdle || s == StatusWaitingAction
}

// ClearPrompt is what the prompt of a session's screen shows: found is false
// when no prompt was found, typed is the text typed into it.
type ClearPrompt struct {
	Found bool
	Typed string
}

// CheckClear reports why /clear must not be typed into the session, or nil.
// self is the session the caller runs in ("" outside claude-deck). The checks
// go from the one whose miss cannot be undone (typing into one's own prompt
// ends the caller's own turn) to the one that reads the screen.
func CheckClear(s Snapshot, self DeckSessionID, prompt ClearPrompt) error {
	if self != "" && s.ID == self {
		return fmt.Errorf("%w: %s", ErrClearSelf, s.ID)
	}
	if !clearable(s.Status) {
		return fmt.Errorf("%w: %s is %s", ErrClearBusy, s.ID, s.Status.ID())
	}
	if !prompt.Found {
		return fmt.Errorf("%w: %s", ErrClearNoPrompt, s.ID)
	}
	if prompt.Typed != "" {
		return fmt.Errorf("%w: %s (%s)", ErrClearHalfTyped, s.ID, truncateRunes(prompt.Typed, 30))
	}
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
