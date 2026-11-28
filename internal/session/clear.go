package session

import (
	"fmt"

	"github.com/pomesaka/claude-deck/internal/claudecode"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/tmux"
)

// clearCommand is what ClearStored types into the session's input box.
const clearCommand = "/clear"

// ClearStored types /clear into the input box of the deck session key names
// (an ID, or a name only one session has), unless CheckClear refuses it, and
// returns the session and whether /clear was typed (never with dryRun).
// self is the session the caller runs in. Used by `claude-deck clear`; needs no Manager.
//
// WHY 画面を読んでから送る: 入力欄に打ちかけの文があると、/clear がその後ろに足され、利用者の文が壊れる。
// 読んでから送るまでの間に打たれた文は防げない（tmux に条件つきの送信は無い）。
func ClearStored(st *store.Store, runner *tmux.Runner, self DeckSessionID, key string, dryRun bool) (Snapshot, bool, error) {
	s, err := FindStored(st, key)
	if err != nil {
		return Snapshot{}, false, err
	}
	// 自分と状態は画面を読まずに決まるので、先に見る。
	if err := CheckClear(s, self, ClearPrompt{Found: true}); err != nil {
		return s, false, err
	}
	b := newTmuxBackend(runner)
	screen, err := b.Screen(s.ID)
	if err != nil {
		return s, false, fmt.Errorf("reading the screen of %s: %w", s.ID, err)
	}
	typed, found := claudecode.TypedText(screen)
	if err := CheckClear(s, self, ClearPrompt{Found: found, Typed: typed}); err != nil {
		return s, false, err
	}
	if dryRun {
		return s, false, nil
	}
	if err := b.TypeLine(s.ID, clearCommand); err != nil {
		return s, false, fmt.Errorf("typing %s into %s: %w", clearCommand, s.ID, err)
	}
	return s, true, nil
}
