package session

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestHints(t *testing.T) {
	ctx := func(used, window int) *ContextUsage { return &ContextUsage{UsedTokens: used, WindowTokens: window} }
	apiError := Wait{Kind: WaitAPIError, Reason: "API の誤りで止まりました。何か送ると再開します"}
	command := Wait{Kind: WaitCommand, Reason: "`! gh auth login` の実行を待っています"}
	tests := []struct {
		name        string
		status      Status
		wait        Wait
		context     *ContextUsage
		compactions *int
		wantKinds   []HintKind
	}{
		{"nothing measured", StatusIdle, Wait{}, nil, nil, nil},
		{"just under the threshold", StatusIdle, Wait{}, ctx(99_999, 200_000), new(0), nil},
		{"at the threshold", StatusIdle, Wait{}, ctx(100_000, 200_000), new(0), []HintKind{HintContextFull}},
		{"compacted once with a low fill", StatusIdle, Wait{}, ctx(0, 200_000), new(1), []HintKind{HintContextFull}},
		{"full and compacted give one hint", StatusIdle, Wait{}, ctx(160_000, 200_000), new(2), []HintKind{HintContextFull}},
		{"a zero window is not full", StatusIdle, Wait{}, ctx(100_000, 0), new(0), nil},
		{"compacted with no fill measured", StatusIdle, Wait{}, nil, new(1), []HintKind{HintContextFull}},
		{"a running session gets the hint too", StatusRunning, Wait{}, ctx(150_000, 200_000), nil, []HintKind{HintContextFull}},
		{"a finished session has nothing to clear", StatusCompleted, Wait{}, ctx(150_000, 200_000), new(3), nil},
		{"an API error only", StatusWaitingAction, apiError, ctx(1_000, 200_000), new(0), []HintKind{HintAPIError}},
		{"an API error and a full context", StatusWaitingAction, apiError, ctx(150_000, 200_000), new(0), []HintKind{HintAPIError, HintContextFull}},
		{"waiting for a command is not an API error", StatusWaitingAction, command, ctx(1_000, 200_000), new(0), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Snapshot{ID: "abc", Origin: OriginDeck, Status: tt.status, WaitKind: tt.wait.Kind, StatusReason: tt.wait.Reason, Context: tt.context, Compactions: tt.compactions}
			var got []HintKind
			for _, h := range Hints(s, time.Time{}, nil) {
				got = append(got, h.Kind)
			}
			if !slices.Equal(got, tt.wantKinds) {
				t.Errorf("Hints() kinds = %v, want %v", got, tt.wantKinds)
			}
		})
	}
}

func TestHints_Message(t *testing.T) {
	tests := []struct {
		name        string
		context     *ContextUsage
		compactions *int
		want        string
	}{
		{"both figures", &ContextUsage{UsedTokens: 120_000, WindowTokens: 200_000}, new(2), "文脈が詰まっている（使用率 60%、compaction 2 回）。きりのいい所で claude-deck clear --id abc を打つ"},
		{"only compactions", nil, new(1), "文脈が詰まっている（compaction 1 回）。きりのいい所で claude-deck clear --id abc を打つ"},
		{"only the fill", &ContextUsage{UsedTokens: 150_000, WindowTokens: 200_000}, nil, "文脈が詰まっている（使用率 75%）。きりのいい所で claude-deck clear --id abc を打つ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := Hints(Snapshot{ID: "abc", Origin: OriginDeck, Status: StatusIdle, Context: tt.context, Compactions: tt.compactions}, time.Time{}, nil)
			if len(hints) != 1 || hints[0].Message != tt.want {
				t.Errorf("Hints() = %+v, want one with message %q", hints, tt.want)
			}
		})
	}
}

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
			if err := CheckSendText(tt.text); !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Errorf("CheckSendText(%q) = %v, want %v", tt.text, err, tt.want)
			}
		})
	}
}
