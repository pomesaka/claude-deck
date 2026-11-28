package claudecode

import (
	"regexp"
	"strings"
)

// promptMark starts the line of Claude Code's input box.
const promptMark = "❯"

// sgr is one SGR escape (colour, weight) of a captured screen, with its parameters.
var sgr = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// TypedText returns the text typed into the input box of a Claude Code screen
// captured with its colours (tmux capture-pane -e), and whether an input box
// was found: the last line that starts with ❯ once the colours are removed.
//
// WHY 薄い字を除く: 入力欄に薄い字（SGR の 2）で出る文は次の発言の案で、利用者が打った文ではない。
// 色を外して読むと、案も書きかけに見える（Claude Code 2.1.331 の画面で確認、2026-11-28）。
// 規則は dotclaude の skills/send-to-sessions/send.ts の typedText と同じで、加えて
// 2 をほかの属性とまとめた形（ESC[2;38;5;246m）と、22 での終わりも読む。
func TypedText(screen string) (typed string, found bool) {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(sgr.ReplaceAllString(lines[i], ""), promptMark) {
			continue
		}
		// TrimSpace も NBSP を落とす。
		return strings.TrimSpace(strings.TrimPrefix(undimmed(lines[i]), promptMark)), true
	}
	return "", false
}

// undimmed is line without its escapes and without the text drawn faint.
func undimmed(line string) string {
	var b strings.Builder
	dim := false
	rest := line
	for {
		loc := sgr.FindStringSubmatchIndex(rest)
		if loc == nil {
			if !dim {
				b.WriteString(rest)
			}
			return b.String()
		}
		if !dim {
			b.WriteString(rest[:loc[0]])
		}
		dim = dimAfter(dim, rest[loc[2]:loc[3]])
		rest = rest[loc[1]:]
	}
}

// dimAfter is whether text is faint after an SGR escape with params, from dim before it.
func dimAfter(dim bool, params string) bool {
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		switch ps[i] {
		case "", "0", "22":
			dim = false
		case "2":
			dim = true
		case "38", "48", "58":
			// 色の指定の引数（5;n か 2;r;g;b）を、属性の 2 と読まない。
			if i+1 < len(ps) && ps[i+1] == "5" {
				i += 2
			} else if i+1 < len(ps) && ps[i+1] == "2" {
				i += 4
			}
		}
	}
	return dim
}
