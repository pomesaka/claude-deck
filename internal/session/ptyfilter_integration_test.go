package session

import (
	"strings"
	"testing"
)

// TestPTYDisplayIncludesFullScreen は AppendLog → GetPTYDisplayLines の実フローで
// Claude Code の入力エリアやステータスバーを含む全画面が返されることを検証する。
// raw PTY 入力モードではユーザーが直接 Claude Code の UI を操作するため、
// 下部クロームのフィルタリングは行わない。
func TestPTYDisplayIncludesFullScreen(t *testing.T) {
	s := NewSession("/tmp/test", "test")

	// Ink の再描画フレームをシミュレート（ESC シーケンス付き）
	tuiLines := []string{
		"\x1b[H\x1b[2J",                                         // cursor home + clear screen
		"\x1b[1m⏺ Running tool: Bash\x1b[0m",                    // bold tool name
		"  \x1b[33m⠹\x1b[0m go test ./...",                      // spinner with color
		"\x1b[90m────────────────────────────────────────\x1b[0m", // separator (colored)
		"❯\u00a0",                           // prompt
		"\x1b[90m────────────────────────────────────────\x1b[0m", // separator (colored)
		"\x1b[2m  Opus 4.6 │ $0.382\x1b[0m",                     // status bar
	}
	for _, line := range tuiLines {
		s.AppendLog(line)
	}

	lines := s.GetPTYDisplayLines()
	t.Logf("Full screen: %d lines", len(lines))
	for i, l := range lines {
		t.Logf("  [%d] %q", i, l)
	}

	// 全画面が返されること（プロンプト・ステータスバー含む）
	foundPrompt := false
	foundStatus := false
	foundSpinner := false
	for _, l := range lines {
		if strings.Contains(l, "❯") {
			foundPrompt = true
		}
		if strings.Contains(l, "Opus 4.6") {
			foundStatus = true
		}
		if strings.Contains(l, "go test") {
			foundSpinner = true
		}
	}
	if !foundPrompt {
		t.Error("prompt ❯ should be visible (no chrome filtering)")
	}
	if !foundStatus {
		t.Error("status bar should be visible (no chrome filtering)")
	}
	if !foundSpinner {
		t.Error("spinner line 'go test' should be visible")
	}
}

