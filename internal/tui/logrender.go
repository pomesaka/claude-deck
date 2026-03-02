package tui

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"

	"github.com/pomesaka/claude-deck/internal/usage"
)

// renderCache caches rendered output keyed by (entries hash + width).
type renderCache struct {
	key    string
	result string
}

// RenderLogs renders structured log entries into a styled string.
func RenderLogs(entries []usage.LogEntry, width int, cache *renderCache) string {
	key := cacheKey(entries, width)
	if cache != nil && cache.key == key {
		return cache.result
	}

	var sb strings.Builder
	for i, e := range entries {
		if i > 0 {
			sb.WriteString("\n")
		}

		indent := ""
		w := width
		if e.Depth > 0 {
			indent = strings.Repeat("  ", e.Depth) + "│ "
			// 表示幅: 2*depth(スペース) + 2("│ " = 1幅 + 1スペース)
			w -= 2*e.Depth + 2
			if w < 20 {
				w = 20
			}
		}

		var line string
		switch e.Kind {
		case usage.LogEntryUser:
			line = renderUserEntry(e, w)
		case usage.LogEntryText:
			line = renderTextEntry(e, w)
		case usage.LogEntryToolUse:
			line = renderToolEntry(e, w)
		case usage.LogEntryThinking:
			line = renderThinkingEntry(e)
		case usage.LogEntryDiff:
			line = renderDiffEntry(e, w)
		}

		if indent != "" {
			// 各行にインデントを追加
			for j, l := range strings.Split(line, "\n") {
				if j > 0 {
					sb.WriteString("\n")
				}
				if l != "" {
					sb.WriteString(dimStyle.Render(indent) + l)
				}
			}
		} else {
			sb.WriteString(line)
		}
	}

	result := sb.String()
	if cache != nil {
		cache.key = key
		cache.result = result
	}
	return result
}

func renderUserEntry(e usage.LogEntry, width int) string {
	text := truncate(e.Text, width)
	return userMessageStyle.Width(width).Render(text) + "\n"
}

func renderTextEntry(e usage.LogEntry, width int) string {
	// マークダウン構文がなければ glamour をスキップ（パフォーマンス）
	if !hasMarkdownSyntax(e.Text) {
		return e.Text + "\n"
	}

	rendered, err := renderMarkdown(e.Text, width)
	if err != nil {
		return e.Text + "\n"
	}
	// glamour の末尾改行を1つだけ残す（後続エントリとの間に空行）
	return strings.TrimRight(rendered, "\n") + "\n"
}

func renderDiffEntry(e usage.LogEntry, width int) string {
	var sb strings.Builder
	for _, line := range strings.Split(e.Text, "\n") {
		if len(line) == 0 {
			continue
		}
		rendered := truncate(line, width)
		switch line[0] {
		case '+':
			sb.WriteString(diffAddStyle.Render(rendered))
		case '-':
			sb.WriteString(diffDelStyle.Render(rendered))
		case '@':
			sb.WriteString(diffHunkStyle.Render(rendered))
		default:
			sb.WriteString(dimStyle.Render(rendered))
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func renderThinkingEntry(e usage.LogEntry) string {
	verb := e.Text
	if verb == "" {
		verb = "Thinking"
	}
	return thinkingStyle.Render("💭 " + verb + "...")
}

func renderToolEntry(e usage.LogEntry, width int) string {
	// result の有無でアイコンを切り替え
	var icon string
	if e.HasResult {
		icon = toolDoneStyle.Render("✓")
	} else {
		icon = toolPendingStyle.Render("●")
	}

	name := toolNameStyle.Render(e.ToolName)

	if e.ToolDetail != "" {
		// icon + space + name + "(" + detail + ")" のサイズ計算
		overhead := 2 + len(e.ToolName) + 2 // icon+space, name, parens
		maxDetail := width - overhead
		if maxDetail < 5 {
			maxDetail = 5
		}
		detail := truncate(e.ToolDetail, maxDetail)
		return fmt.Sprintf("%s %s(%s)", icon, name, dimStyle.Render(detail))
	}
	return fmt.Sprintf("%s %s", icon, name)
}

// hasMarkdownSyntax does a cheap check for common markdown patterns.
func hasMarkdownSyntax(text string) bool {
	for _, prefix := range []string{"# ", "## ", "### ", "```", "- ", "* ", "> ", "1. "} {
		if strings.Contains(text, prefix) {
			return true
		}
	}
	if strings.Contains(text, "**") || strings.Contains(text, "`") {
		return true
	}
	return false
}

// glamour renderer cache — 幅が変わったときだけ再生成する
var (
	cachedRenderer      *glamour.TermRenderer
	cachedRendererWidth int
)

// renderMarkdown renders markdown text using glamour with a dark style.
func renderMarkdown(text string, width int) (string, error) {
	if cachedRenderer == nil || cachedRendererWidth != width {
		r, err := glamour.NewTermRenderer(
			glamour.WithWordWrap(width),
			glamour.WithStylePath("dark"),
		)
		if err != nil {
			return "", err
		}
		cachedRenderer = r
		cachedRendererWidth = width
	}
	return cachedRenderer.Render(text)
}

// cacheKey computes a hash over entries and width for cache invalidation.
func cacheKey(entries []usage.LogEntry, width int) string {
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%d|%s|%s|%s|%v|%d\n", e.Kind, e.Text, e.ToolName, e.ToolDetail, e.HasResult, e.Depth)
	}
	fmt.Fprintf(h, "w=%d", width)
	return fmt.Sprintf("%x", h.Sum(nil))
}
