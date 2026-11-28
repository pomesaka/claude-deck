package claudecode

import (
	"strings"
	"testing"
	"testing/quick"
)

func TestTypedText(t *testing.T) {
	tests := []struct {
		name      string
		screen    string
		wantTyped string
		wantFound bool
	}{
		{"empty prompt", "❯ ", "", true},
		{"typed text", "❯ hello", "hello", true},
		{"a suggestion only", "❯ \x1b[2mtry this\x1b[0m", "", true},
		{"typed text and a suggestion", "❯ he\x1b[2mllo\x1b[0m", "he", true},
		{"a suggestion running to the end of the line", "❯ \x1b[2mtry this", "", true},
		{"coloured text that is not faint", "❯ \x1b[38;5;12mhi\x1b[0m", "hi", true},
		{"a coloured prompt mark", "\x1b[38;5;246m❯ \x1b[39m", "", true},
		{"an older prompt above the current one", "❯ old\n...\n❯ ", "", true},
		{"frame and status lines under the prompt", "─────\n❯ draft\n─────\n  ⏵⏵ auto mode on", "draft", true},
		{"faint joined with a colour", "❯ \x1b[2;38;5;246mtry this\x1b[0m", "", true},
		{"faint ended by 22", "❯ he\x1b[2mllo\x1b[22m", "he", true},
		{"a 256-colour index of 2 is not faint", "❯ \x1b[38;5;2mgreen\x1b[0m", "green", true},
		{"a true colour with 2 in it is not faint", "❯ \x1b[38;2;2;2;2mgrey\x1b[0m", "grey", true},
		{"no prompt", "─────\nsome output", "", false},
		{"only a no-break space after the mark", "❯ ", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typed, found := TypedText(tt.screen)
			if typed != tt.wantTyped || found != tt.wantFound {
				t.Errorf("TypedText(%q) = (%q, %v), want (%q, %v)", tt.screen, typed, found, tt.wantTyped, tt.wantFound)
			}
		})
	}
}

// A faint suggestion after the typed text never changes what was typed.
func TestTypedText_SuggestionIsNotTyped(t *testing.T) {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == '\x1b' || r == '\n' || r == '❯' {
				return -1
			}
			return r
		}, s)
	}
	f := func(typed, suggestion string) bool {
		typed, suggestion = clean(typed), clean(suggestion)
		with, _ := TypedText("❯ " + typed + "\x1b[2m" + suggestion + "\x1b[0m")
		without, _ := TypedText("❯ " + typed)
		return with == without
	}
	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
}
