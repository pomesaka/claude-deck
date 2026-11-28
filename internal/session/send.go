package session

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/pomesaka/claude-deck/internal/claudecode"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/tmux"
)

// Why CheckSend refuses to type into a session.
var (
	ErrSendSelf      = errors.New("the session is this one")
	ErrSendBusy      = errors.New("the session is not waiting for input")
	ErrSendNoPrompt  = errors.New("the session's prompt was not found on its screen")
	ErrSendHalfTyped = errors.New("the session's prompt holds text the user is typing")
	ErrSendNoSelf    = errors.New("this process does not run in a claude-deck session")
	ErrSendNoProcess = errors.New("the session has no running process")
)

// Why CheckSendText refuses a text to type.
var (
	ErrSendEmpty     = errors.New("the text to send is empty")
	ErrSendMultiline = errors.New("the text to send must be one line")
	ErrSendControl   = errors.New("the text to send holds a control character")
)

// acceptsInput reports whether text may be typed in status s: the main loop
// has ended its turn and nothing waits on a dialog.
// WHY subagent_running を外す: 動いているサブエージェントの報告は、送った文の後の会話に割り込む。
// WHY waiting_approval / waiting_answer を外す: 打った文がダイアログの選択に入る。
func acceptsInput(s Status) bool {
	return s == StatusIdle || s == StatusWaitingAction
}

// InputBox is what the input box of a session's screen shows: Found is false
// when no input box was found, Typed is the text typed into it.
type InputBox struct {
	Found bool
	Typed string
}

// CheckSendText reports why text must not be typed into a session, or nil.
// SendTo and Broadcast check it; the CLI checks it too, to fail before it opens the store.
// WHY 1 行だけ: 改行は入力欄で Enter として読まれ、文の途中が送られる。
// WHY 制御文字を断る: send-keys -l は字どおりに送るので、Esc は入力の取り消しに、C-c は中断に読まれる。
func CheckSendText(text string) error {
	switch {
	case strings.TrimSpace(text) == "":
		return ErrSendEmpty
	case strings.ContainsAny(text, "\r\n"):
		return ErrSendMultiline
	case strings.ContainsFunc(text, unicode.IsControl):
		return ErrSendControl
	}
	return nil
}

// CheckSend reports why nothing must be typed into the session, or nil.
// self is the session the caller runs in ("" outside claude-deck). The checks
// go from the one whose miss cannot be undone (typing into one's own prompt
// ends the caller's own turn) to the one that reads the screen.
func CheckSend(s Snapshot, self DeckSessionID, box InputBox) error {
	if self != "" && s.ID == self {
		return fmt.Errorf("%w: %s", ErrSendSelf, s.ID)
	}
	if !acceptsInput(s.Status) {
		return fmt.Errorf("%w: %s is %s", ErrSendBusy, s.ID, s.Status.ID())
	}
	return checkInputBox(s, box)
}

// CheckSendSelf is CheckSend for the session the caller runs in: it types
// into its own prompt on purpose (send --self).
// WHY ターンの途中（running）も送る: 打った文は Claude Code が列に入れ、ターンが終わった後に流す
// （画面に「Queued」が出て、ターンの後に /reload-plugins が効いた。2026-11-28 に PM のセッションで確かめた。
// 版はその日の手元の claude --version で 2.1.332、そのセッションの版そのものは未確認）。
// WHY waiting_approval / waiting_answer は断る: ダイアログの間に打った文は、ほかのセッションと同じく
// 選択に入る（acceptsInput の WHY）。列に入るかは確かめていない。
// 打ちかけも、ほかのセッションと同じく壊さないよう断る。
func CheckSendSelf(s Snapshot, box InputBox) error {
	switch {
	case !s.Status.IsLive():
		return fmt.Errorf("%w: %s is %s", ErrSendNoProcess, s.ID, s.Status.ID())
	case s.Status == StatusWaitingApproval || s.Status == StatusWaitingAnswer:
		return fmt.Errorf("%w: %s is %s", ErrSendBusy, s.ID, s.Status.ID())
	}
	return checkInputBox(s, box)
}

// checkInputBox reports why the input box must not be typed into: none was
// found, or it holds text being typed.
func checkInputBox(s Snapshot, box InputBox) error {
	if !box.Found {
		return fmt.Errorf("%w: %s", ErrSendNoPrompt, s.ID)
	}
	if box.Typed != "" {
		return fmt.Errorf("%w: %s (%s)", ErrSendHalfTyped, s.ID, truncateRunes(box.Typed, 30))
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

// SendTo types text and Enter into the session's input box, as if the user
// typed it, unless CheckSendText or CheckSend refuses it, and returns whether
// it was typed (never with dryRun). self is the session the caller runs in.
//
// WHY 画面を読んでから送る: 入力欄に打ちかけの文があると、送る文がその後ろに足され、利用者の文が壊れる。
// 読んでから送るまでの間に打たれた文は防げない（tmux に条件つきの送信は無い）。
func SendTo(runner *tmux.Runner, s Snapshot, self DeckSessionID, text string, dryRun bool) (bool, error) {
	if err := CheckSendText(text); err != nil {
		return false, err
	}
	return sendChecked(runner, s, func(box InputBox) error { return CheckSend(s, self, box) }, text, dryRun)
}

// sendChecked types text, which CheckSendText has accepted, into s when check
// accepts its input box.
func sendChecked(runner *tmux.Runner, s Snapshot, check func(InputBox) error, text string, dryRun bool) (bool, error) {
	// 自分と状態は画面を読まずに決まるので、先に見る。
	if err := check(InputBox{Found: true}); err != nil {
		return false, err
	}
	b := newTmuxBackend(runner)
	screen, err := b.Screen(s.ID)
	if err != nil {
		return false, fmt.Errorf("reading the screen of %s: %w", s.ID, err)
	}
	typed, found := claudecode.TypedText(screen)
	if err := check(InputBox{Found: found, Typed: typed}); err != nil {
		return false, err
	}
	if dryRun {
		return false, nil
	}
	if err := b.TypeLine(s.ID, text); err != nil {
		return false, fmt.Errorf("typing into %s: %w", s.ID, err)
	}
	return true, nil
}

// SendStored is SendTo for the deck session key names in the store (an ID, or
// a name only one session has). Used by `claude-deck send` and `clear`; needs no Manager.
func SendStored(st *store.Store, runner *tmux.Runner, self DeckSessionID, key, text string, dryRun bool) (Snapshot, bool, error) {
	s, err := FindStored(st, key)
	if err != nil {
		return Snapshot{}, false, err
	}
	sent, err := SendTo(runner, s, self, text, dryRun)
	return s, sent, err
}

// SendSelf types text into the input box of self, the session the caller
// runs in, as CheckSendSelf allows: mid-turn too (send --self).
// The CLI checks self too, to fail before it opens the store.
func SendSelf(st *store.Store, runner *tmux.Runner, self DeckSessionID, text string, dryRun bool) (Snapshot, bool, error) {
	if self == "" {
		return Snapshot{}, false, ErrSendNoSelf
	}
	if err := CheckSendText(text); err != nil {
		return Snapshot{}, false, err
	}
	s, err := FindStored(st, string(self))
	if err != nil {
		return Snapshot{}, false, err
	}
	sent, err := sendChecked(runner, s, func(box InputBox) error { return CheckSendSelf(s, box) }, text, dryRun)
	return s, sent, err
}

// Delivery is what Broadcast did for one session: Sent, or why it skipped it
// (nil when dryRun found nothing against sending).
type Delivery struct {
	Session Snapshot
	Sent    bool
	Skipped error
}

// Broadcast types text into each of targets that SendTo accepts, in order,
// and reports what it did for each. A refused session does not stop the rest.
func Broadcast(runner *tmux.Runner, targets []Snapshot, self DeckSessionID, text string, dryRun bool) ([]Delivery, error) {
	if err := CheckSendText(text); err != nil {
		return nil, err
	}
	out := make([]Delivery, 0, len(targets))
	for _, s := range targets {
		sent, err := sendChecked(runner, s, func(box InputBox) error { return CheckSend(s, self, box) }, text, dryRun)
		out = append(out, Delivery{Session: s, Sent: sent, Skipped: err})
	}
	return out, nil
}
