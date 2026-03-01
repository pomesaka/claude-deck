package session

import (
	"strings"
)

// isFullWidthSeparator detects a line consisting entirely of horizontal
// line-drawing characters (─, ━) or ASCII dashes, spanning a significant width.
// These are the borders Claude Code renders around its input area.
// NOTE: プレーンテキスト（ANSI 除去済み）の行に対して使うこと。
func isFullWidthSeparator(line string) bool {
	trimmed := strings.TrimSpace(line)
	n := 0
	for _, r := range trimmed {
		switch r {
		case '─', '━', '-', '=':
			n++
		default:
			return false
		}
	}
	return n >= 20
}

// chromeStartIndex returns the line index where Claude Code's bottom chrome
// (input area, status bar, tab titles) begins. Returns len(lines) if no
// chrome is found — meaning all lines are content.
//
// Uses plain-text lines (no ANSI) for separator detection.
func chromeStartIndex(plainLines []string) int {
	if len(plainLines) == 0 {
		return 0
	}

	start := len(plainLines) - 10
	if start < 0 {
		start = 0
	}
	for i := start; i < len(plainLines); i++ {
		if isFullWidthSeparator(plainLines[i]) {
			return i
		}
	}

	return len(plainLines)
}

// filterBottomChrome removes Claude Code's TUI chrome from plain-text lines.
// Convenience wrapper around chromeStartIndex for tests and simple use cases.
func filterBottomChrome(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	idx := chromeStartIndex(lines)
	if idx == 0 {
		return nil
	}
	return lines[:idx]
}
