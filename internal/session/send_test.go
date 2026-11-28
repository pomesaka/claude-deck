package session

import (
	"errors"
	"testing"
)

func TestCheckSend(t *testing.T) {
	empty := InputBox{Found: true}
	tests := []struct {
		name   string
		id     DeckSessionID
		origin Origin
		status Status
		prompt InputBox
		want   error
	}{
		{"idle with an empty prompt", "t", OriginDeck, StatusIdle, empty, nil},
		{"waiting_action with an empty prompt", "t", OriginDeck, StatusWaitingAction, empty, nil},
		{"running", "t", OriginDeck, StatusRunning, empty, ErrSendBusy},
		{"subagent_running", "t", OriginDeck, StatusSubagentRunning, empty, ErrSendBusy},
		{"waiting_approval", "t", OriginDeck, StatusWaitingApproval, empty, ErrSendBusy},
		{"waiting_answer", "t", OriginDeck, StatusWaitingAnswer, empty, ErrSendBusy},
		{"completed", "t", OriginDeck, StatusCompleted, empty, ErrSendBusy},
		{"error", "t", OriginDeck, StatusError, empty, ErrSendBusy},
		{"unknown", "t", OriginDeck, StatusUnknown, empty, ErrSendBusy},
		{"half-typed text", "t", OriginDeck, StatusIdle, InputBox{Found: true, Typed: "fix the"}, ErrSendHalfTyped},
		{"no prompt on the screen", "t", OriginDeck, StatusIdle, InputBox{}, ErrSendNoPrompt},
		{"self while idle", "self", OriginDeck, StatusIdle, empty, ErrSendSelf},
		{"self while running is still self", "self", OriginDeck, StatusRunning, empty, ErrSendSelf},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSend(Snapshot{ID: tt.id, Origin: tt.origin, Status: tt.status}, "self", tt.prompt)
			if tt.want == nil {
				if err != nil {
					t.Errorf("CheckSend() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("CheckSend() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCheckSendText(t *testing.T) {
	tests := []struct {
		name string
		text string
		want error
	}{
		{"a command", "/clear", nil},
		{"Japanese", "続けて", nil},
		{"empty", "", ErrSendEmpty},
		{"blank", "  \t", ErrSendEmpty},
		{"a line break", "a\nb", ErrSendMultiline},
		{"a carriage return", "a\rb", ErrSendMultiline},
		{"a trailing line break", "/clear\n", ErrSendMultiline},
		{"Esc", "\x1b[A", ErrSendControl},
		{"C-c", "stop\x03", ErrSendControl},
		{"a tab", "a\tb", ErrSendControl},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := CheckSendText(tt.text); !errors.Is(err, tt.want) {
				t.Errorf("CheckSendText(%q) = %v, want %v", tt.text, err, tt.want)
			}
		})
	}
}

// Typing into one's own session skips the status: mid-turn, the line is queued
// and runs once the turn ends. The input box is still read.
func TestCheckSendSelf(t *testing.T) {
	empty := InputBox{Found: true}
	tests := []struct {
		name   string
		status Status
		box    InputBox
		want   error
	}{
		{"mid-turn", StatusRunning, empty, nil},
		{"idle", StatusIdle, empty, nil},
		{"subagents running", StatusSubagentRunning, empty, nil},
		{"waiting for an approval", StatusWaitingApproval, empty, ErrSendBusy},
		{"waiting for an answer", StatusWaitingAnswer, empty, ErrSendBusy},
		{"half-typed text", StatusRunning, InputBox{Found: true, Typed: "fix"}, ErrSendHalfTyped},
		{"no prompt", StatusRunning, InputBox{}, ErrSendNoPrompt},
		{"a finished row", StatusCompleted, empty, ErrSendNoProcess},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSendSelf(Snapshot{ID: "self", Origin: OriginDeck, Status: tt.status}, tt.box)
			if !errors.Is(err, tt.want) {
				t.Errorf("CheckSendSelf() = %v, want %v", err, tt.want)
			}
		})
	}
}
