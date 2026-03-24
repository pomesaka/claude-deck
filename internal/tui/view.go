package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/session"
)

// View renders the TUI.
func (m Model) View() tea.View {
	if m.quitting {
		return tea.NewView("claude-deck を終了します。\n")
	}

	// WindowSizeMsg 未受信の状態でレンダリングすると cellbuf に壊れたセルが残る
	if m.width == 0 || m.height == 0 {
		return tea.View{}
	}

	var sections []string

	sections = append(sections, m.renderHeader())
	if m.mode == viewSelectRepo {
		sections = append(sections, m.repoList.View())
	} else {
		sections = append(sections, m.renderMain())
	}

	sections = append(sections, m.renderFooter())

	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, sections...))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	// PTY 入力モード中はエミュレータのカーソル位置に TUI カーソルを配置する。
	// これにより Ghostty のカーソルが Claude の入力行に正しく表示される。
	if m.ptyInputActive && m.selectedID != "" && m.mode == viewDashboard {
		if sess := m.manager.GetSession(m.selectedID); sess != nil && m.manager.HasActiveProcess(m.selectedID) {
			cursorX, cursorDisplayRow := sess.GetPTYCursorPosition()
			_, _, ptyHeight := m.detailPaneMetrics()
			cursorViewportRow := cursorDisplayRow - m.ptyViewport.YOffset()
			if cursorViewportRow >= 0 && cursorViewportRow < ptyHeight {
				// レイアウト: header(1行) + detail枠top(1行) + viewport行
				// 列: list幅 + ペイン間スペース(1) + detail枠left(1) + padding(1) + cursorX
				const frameOverhead = 1
				available := m.width - frameOverhead
				listWidth := available * 35 / 100
				if listWidth < 20 {
					listWidth = 20
				}
				tuiCol := listWidth + 3 + cursorX
				tuiRow := 2 + cursorViewportRow
				debuglog.Printf("[cursor] cursorX=%d cursorDisplayRow=%d ptyViewportYOffset=%d cursorViewportRow=%d listWidth=%d → tuiCol=%d tuiRow=%d ptyHeight=%d",
					cursorX, cursorDisplayRow, m.ptyViewport.YOffset(), cursorViewportRow, listWidth, tuiCol, tuiRow, ptyHeight)
				c := tea.NewCursor(tuiCol, tuiRow)
				c.Shape = tea.CursorBar
				v.Cursor = c
			}
		}
	}

	return v
}

func (m Model) renderHeader() string {
	title := headerStyle.Render("🎛  claude-deck")

	var attentionCount int
	for _, s := range m.sessions {
		if s.Snapshot().Status.NeedsAttention() {
			attentionCount++
		}
	}

	infoStr := fmt.Sprintf("Sessions: %d", len(m.sessions))
	info := tokenStyle.Render(infoStr)

	var badge string
	if attentionCount > 0 {
		badge = statusApproveStyle.Render(fmt.Sprintf(" [%d asking...]", attentionCount))
	}

	right := lipgloss.JoinHorizontal(lipgloss.Top, info, badge)
	if usage := m.renderRateLimits(); usage != "" {
		right = lipgloss.JoinHorizontal(lipgloss.Top, right, "  ", usage)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, title, "  ", right)
}

func (m Model) renderMain() string {
	// Height() はボーダー込みの全体高さを設定するため、ヘッダー行(1) + フッター行(1) のみ差し引く。
	contentHeight := m.height - 2
	if contentHeight < 3 {
		contentHeight = 3
	}

	// Width() はボーダー・パディング込みの全体幅を設定するため、フレーム分の減算は不要。
	// ペイン間スペースの " " 分のみ差し引く。
	const frameOverhead = 1
	available := m.width - frameOverhead
	if available < 20 {
		available = 20
	}

	listWidth := available * 35 / 100
	if listWidth < 20 {
		listWidth = 20
	}
	detailWidth := available - listWidth
	if detailWidth < 20 {
		detailWidth = 20
	}

	list := m.renderSessionList(listWidth, contentHeight)
	detail := m.renderDetailPane(detailWidth, contentHeight)

	return lipgloss.JoinHorizontal(lipgloss.Top, list, " ", detail)
}

func (m Model) renderSessionList(width, height int) string {
	style := sessionListStyle
	if !m.focusDetail {
		style = sessionListFocusedStyle
	}

	sessions := m.visibleSessions()

	// フィルタバーの高さを確保
	var filterBar string
	filterBarHeight := 0
	if m.filterActive {
		filterBar = m.filterInput.View()
		filterBarHeight = 1
	} else if m.filterText != "" {
		filterBar = dimStyle.Render("/ " + m.filterText)
		filterBarHeight = 1
	}

	if len(sessions) == 0 {
		var msg string
		if m.filterText != "" || m.filterActive {
			msg = dimStyle.Render("一致するセッションなし")
		} else {
			msg = dimStyle.Render("セッションなし。'n'で新規作成")
		}
		if filterBar != "" {
			content := lipgloss.JoinVertical(lipgloss.Left, msg, filterBar)
			return style.Width(width).Height(height).AlignVertical(lipgloss.Bottom).Render(content)
		}
		return style.Width(width).Height(height).Render(msg)
	}

	const itemHeight = 2

	// Height() はボーダー込みの外寸。コンテンツ領域はボーダー(上下各1)分を差し引く。
	// フィルタバー分も除いた利用可能高さ。
	const borderHeight = 2
	listHeight := height - borderHeight - filterBarHeight

	// スクロール範囲を算出（タブ行分を差し引く）
	offset := m.scrollOffset
	if offset < 0 {
		offset = 0
	}
	if offset >= len(sessions) {
		offset = max(0, len(sessions)-1)
	}

	availHeight := listHeight
	hasAbove := offset > 0
	if hasAbove {
		availHeight-- // 上インジケータ分
	}

	visibleCount := availHeight / itemHeight
	if visibleCount < 1 {
		visibleCount = 1
	}

	end := offset + visibleCount
	if end > len(sessions) {
		end = len(sessions)
	}

	// 下にまだあるなら、インジケータ分を確保して再計算
	if end < len(sessions) {
		revised := (availHeight - 1) / itemHeight
		if revised < 1 {
			revised = 1
		}
		end = offset + revised
		if end > len(sessions) {
			end = len(sessions)
		}
	}

	var items []string

	if hasAbove {
		items = append(items, dimStyle.Render(fmt.Sprintf("  ↑ 他%d件", offset)))
	}

	// リストペインのコンテンツ幅: border(2) + padding(2) を引く
	itemWidth := width - 4
	if itemWidth < 10 {
		itemWidth = 10
	}
	for i := offset; i < end; i++ {
		snap := sessions[i].Snapshot()
		items = append(items, renderSessionItem(snap, i == m.cursor, itemWidth))
	}

	if remaining := len(sessions) - end; remaining > 0 {
		items = append(items, dimStyle.Render(fmt.Sprintf("  ↓ 他%d件", remaining)))
	}

	if filterBar != "" {
		items = append(items, filterBar)
	}

	content := lipgloss.JoinVertical(lipgloss.Left, items...)
	// アイテムが少ない場合は下寄せで表示（AlignVertical は Height 内のコンテンツ配置を制御）
	return style.Width(width).Height(height).AlignVertical(lipgloss.Bottom).Render(content)
}

// selBg returns the style with the selected background applied when selected is true.
func selBg(s lipgloss.Style, selected bool) lipgloss.Style {
	if selected {
		return s.Background(colorBgSelected)
	}
	return s
}

func renderSessionItem(snap session.Snapshot, selected bool, width int) string {
	// width は sessionItemStyle の外寸（border-box）。
	// Padding(0,1) の内側がコンテンツ領域なので 2 を引く。
	const itemPadding = 2
	cw := width - itemPadding
	if cw < 4 {
		cw = 4
	}

	// selected 時にスタイル未適用の隙間（スペース、パディング）にも背景を付けるための
	// "背景のみ" スタイル。非選択時は空スタイル（何もしない）。
	bg := lipgloss.NewStyle()
	if selected {
		bg = bg.Background(colorBgSelected)
	}

	// ステータスアイコン（セッション名の前に付ける、全ステータスで幅を揃える）
	var statusIcon string
	// statusMessage: line2 末尾に表示するメッセージ（Approve待ち、エラー等）
	var statusMessage string
	switch snap.Status {
	case session.StatusRunning:
		statusIcon = selBg(statusRunningStyle, selected).Render("●")
	case session.StatusIdle:
		statusIcon = selBg(statusIdleStyle, selected).Render("●")
	case session.StatusWaitingApproval:
		statusIcon = selBg(statusApproveStyle, selected).Render("●")
	case session.StatusWaitingAnswer:
		statusIcon = selBg(statusQuestionStyle, selected).Render("●")
	case session.StatusCompleted:
		statusIcon = selBg(statusDoneStyle, selected).Render("●")
	case session.StatusError:
		statusIcon = selBg(statusErrorStyle, selected).Render("●")
		if snap.ErrorMessage != "" {
			statusMessage = selBg(statusErrorStyle, selected).Render(truncate(snap.ErrorMessage, cw-20))
		} else {
			statusMessage = selBg(statusErrorStyle, selected).Render("エラー")
		}
	case session.StatusUnmanaged:
		statusIcon = selBg(unmanagedIconStyle, selected).Render("●")
	}

	// line1: [icon] [repoPath/session 固定幅] [title 残り幅]
	iconCol := statusIcon + bg.Render(" ")
	iconWidth := lipgloss.Width(iconCol)

	// RepoPath を短縮表示（ホームディレクトリを ~ に置換）
	repoPath := snap.RepoPath
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(repoPath, home) {
		repoPath = "~" + repoPath[len(home):]
	}

	// パスを3層に分解: repoPrefix / repoName{/subProjectDir} / sessionName
	// repoName+subProjectDir を最も強調し、sessionName はやや控えめ
	repoName := snap.RepoName
	var repoPrefix string // repoName の前のパス部分（末尾 / 含む）
	if repoPath != "" && repoName != "" {
		if idx := strings.LastIndex(repoPath, repoName); idx > 0 {
			repoPrefix = repoPath[:idx]
		}
	}

	// 強調部分: repoName + subProjectDir
	emphasized := repoName
	if snap.SubProjectDir != "" {
		emphasized += "/" + snap.SubProjectDir
	}

	// パスカラム: 全体幅の50%を固定確保し、truncateLeft で末尾を残す
	pathWidth := (cw - iconWidth) / 2
	if pathWidth < 10 {
		pathWidth = 10
	}

	// フルパス組み立て: prefix + emphasized + / + sessionName
	fullPath := repoPrefix + emphasized + "/" + snap.Name
	if repoPath == "" {
		fullPath = snap.Name
	}
	truncated := truncateLeft(fullPath, pathWidth)

	// truncate 後の文字列をスタイル適用
	// セッション名（末尾）→ 強調部分（中間）→ プレフィックス（先頭）の順でマッチ
	emphStyle := selBg(lipgloss.NewStyle().Foreground(colorPrimary).Bold(true), selected)
	nameStyle := selBg(lipgloss.NewStyle().Foreground(colorSecondary), selected)
	dim := selBg(dimStyle, selected)

	var pathCol string
	if lastSlash := strings.LastIndex(truncated, "/"); lastSlash >= 0 {
		sessionPart := truncated[lastSlash+1:]
		beforeSession := truncated[:lastSlash+1]
		// beforeSession 内で強調部分を探す
		if empIdx := strings.Index(beforeSession, repoName); empIdx >= 0 {
			prefix := beforeSession[:empIdx]
			empPart := beforeSession[empIdx:]
			pathCol = dim.Render(prefix) + emphStyle.Render(empPart) + nameStyle.Render(sessionPart)
		} else {
			// truncateLeft で prefix が切られた場合、全体を強調+セッション名
			pathCol = emphStyle.Render(beforeSession) + nameStyle.Render(sessionPart)
		}
	} else {
		pathCol = nameStyle.Render(truncated)
	}
	pathCol = padRightBg(pathCol, pathWidth, bg)

	// タイトルカラム: BookmarkName を優先、なければ TerminalTitle にフォールバック
	titleWidth := cw - iconWidth - pathWidth - 1
	var titleCol string
	displayTitle := snap.BookmarkName
	if displayTitle == "" {
		displayTitle = snap.TerminalTitle
	}
	if displayTitle != "" && titleWidth > 4 {
		titleCol = bg.Render(" ") + selBg(lipgloss.NewStyle().Foreground(colorText), selected).Render(truncate(displayTitle, titleWidth))
	}

	line1 := padRightBg(iconCol+pathCol+titleCol, cw, bg)

	// line2
	const timeWidth = 14
	lastAct := padRightBg(dim.Render(formatTimeCompact(snap)), timeWidth, bg)
	const costWidth = 7
	cost := padRightBg(selBg(tokenStyle, selected).Render(fmt.Sprintf("$%.2f", snap.TokenUsage.EstimatedCostUSD)), costWidth, bg)
	tokens := dim.Render(formatTokens(snap.TokenUsage.InputTokens, snap.TokenUsage.OutputTokens))

	// line2: インデント(icon幅) + 時間 + コスト + トークン + [メッセージ]
	indent := bg.Render(strings.Repeat(" ", iconWidth))
	sp := bg.Render(" ")
	var line2 string
	if statusMessage != "" {
		line2 = indent + lastAct + sp + cost + sp + tokens + sp + statusMessage
	} else {
		line2 = indent + lastAct + sp + cost + sp + tokens
	}
	line2 = padRightBg(line2, cw, bg)

	content := lipgloss.JoinVertical(lipgloss.Left, line1, line2)

	style := sessionItemStyle
	if selected {
		style = sessionItemSelectedStyle
	}
	// width は border-box 外寸。Padding(0,1) 込みで width セル幅にする。
	return style.Width(width).Render(content)
}

func (m Model) renderDetailPane(width, height int) string {
	style := detailPaneStyle
	if m.focusDetail {
		style = detailPaneFocusedStyle
	}

	if m.selectedID == "" {
		return style.Width(width).Height(height).Render(dimStyle.Render("セッションを選択してください"))
	}

	sess := m.manager.GetSession(m.selectedID)
	if sess == nil {
		return style.Width(width).Height(height).Render(dimStyle.Render("セッションが見つかりません"))
	}

	snap := sess.Snapshot()
	var sections []string

	// Width() はボーダー・パディング込みなので、コンテンツ幅は border(2) + padding(2) を引く
	innerWidth := width - 4
	if innerWidth < 10 {
		innerWidth = 10
	}

	hasActiveProcess := m.selectedID != "" && m.manager.HasActiveProcess(m.selectedID)

	if hasActiveProcess {
		// アクティブプロセス → PTY 全画面表示（ヘッダー非表示）
		sections = append(sections, m.ptyViewport.View())

		// 入力ステータス行
		separator := dimStyle.Render(strings.Repeat("─", innerWidth))
		if m.ptyInputActive {
			inputLine := inputPromptStyle.Render("  PTY 直接入力中 (Ctrl+D で終了)")
			sections = append(sections, separator, inputLine)
		} else {
			var placeholder string
			if !m.ptyFollow {
				placeholder = dimStyle.Render("  Enter で入力モード / G で最新に戻る")
			} else {
				placeholder = dimStyle.Render("  Enter で入力モード開始")
			}
			sections = append(sections, separator, placeholder)
		}
	} else {
		// 完了済み → ヘッダー + JSONL ログ表示
		sections = append(sections, titleStyle.Render(truncate(fmt.Sprintf("📋 %s (%s)", snap.Name, snap.RepoName), innerWidth)))
		sections = append(sections, dimStyle.Render(truncate(fmt.Sprintf("   パス: %s", snap.WorkspacePath), innerWidth)))
		sections = append(sections, dimStyle.Render(truncate(fmt.Sprintf("   ID: %s  Claude: %s", snap.ID, snap.ClaudeSessionID), innerWidth)))

		if snap.CurrentTool != "" {
			sections = append(sections, statusRunningStyle.Render(truncate(fmt.Sprintf("   🔧 %s", snap.CurrentTool), innerWidth)))
		}

		if snap.Status.NeedsAttention() {
			sections = append(sections, statusApproveStyle.Render(truncate("   👆 Enter で再開", innerWidth)))
		}

		if snap.Status == session.StatusError && snap.ErrorMessage != "" {
			sections = append(sections, statusErrorStyle.Render(truncate("   ✗ "+snap.ErrorMessage, innerWidth)))
		}

		sections = append(sections, "")
		sections = append(sections, m.logViewport.View())
	}

	content := lipgloss.JoinVertical(lipgloss.Left, sections...)
	return style.Width(width).Height(height).Render(content)
}

func (m Model) renderFooter() string {
	var helpText string
	if m.mode == viewSelectRepo {
		helpText = "Enter:ワークスペース作成+起動 C-Enter:直接起動 Esc:戻る"
	} else if m.ptyInputActive {
		helpText = "PTY 直接入力中 / Ctrl+D:終了"
	} else if m.filterActive {
		helpText = "Enter:確定 Esc:キャンセル"
	} else if m.filterText != "" {
		helpText = fmt.Sprintf("フィルタ: %s / Esc:解除 ?:ヘルプ", m.filterText)
	} else {
		helpText = "h/l:ペイン切替 j/k:移動 gg/G:先頭/末尾 /:フィルタ n:新規 Enter/i:入力/再開 r:再開 f:フォーク t:ターミナル R:再描画 dd:削除 x:終了 C-c:quit"
	}
	left := dimStyle.Render(helpText)

	var right string
	if m.statusMsg != "" {
		right = statusApproveStyle.Render(m.statusMsg)
	}

	footer := lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
	return footerStyle.Render(footer)
}

const usageGaugeWidth = 10

// renderRateLimits renders gauge bars for Claude.ai rate limit windows.
// Data is provided by the claude-deck statusline script via rate-limits.json.
// Returns empty string when no data is available (API users, before first response).
func (m Model) renderRateLimits() string {
	s := m.rateLimitsStatus
	var parts []string

	if s.FiveHourAvailable {
		parts = append(parts, renderUsageGauge("5h", s.FiveHour.UsedPct, s.FiveHour.ResetsAt, "#06B6D4"))
	}
	if s.SevenDayAvailable {
		parts = append(parts, renderUsageGauge("7d", s.SevenDay.UsedPct, s.SevenDay.ResetsAt, "#F59E0B"))
	}

	return strings.Join(parts, "  ")
}

// renderUsageGauge renders a labeled gauge bar with reset countdown:
// `label ○○●●●●●●●● 80% 4h30m`
// used is 0–100 (usage percentage). filled dots represent consumed portion.
// resetsAt zero value omits the countdown.
func renderUsageGauge(label string, used float64, resetsAt time.Time, hexColor string) string {
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	filled := int(used/100*usageGaugeWidth + 0.5)
	bar := strings.Repeat("●", filled) + strings.Repeat("○", usageGaugeWidth-filled)
	gaugeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(hexColor))
	gauge := gaugeStyle.Render(fmt.Sprintf("%s %.0f%%", bar, used))

	countdown := ""
	if !resetsAt.IsZero() {
		remaining := time.Until(resetsAt)
		if remaining > 0 {
			countdown = " " + dimStyle.Render(formatDuration(remaining))
		}
	}

	return dimStyle.Render(label+" ") + gauge + countdown
}

// formatDuration formats a duration as a compact human-readable countdown.
// Examples: "42m", "4h30m", "1d20h"
func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours()) // intentional truncation: fractional hours accounted for in m
	m := int(d.Minutes()) % 60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case h < 24:
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	}
}

func formatTimeCompact(snap session.Snapshot) string {
	t := snap.LastActivity
	if t.IsZero() && snap.FinishedAt != nil {
		t = *snap.FinishedAt
	}
	if t.IsZero() {
		t = snap.StartedAt
	}
	if t.IsZero() {
		return "-"
	}
	return t.Format("01/02 15:04")
}

// formatCompact formats a number in compact form: 0, 1, 999, 1.2k, 12k, 123k, 1.2M etc.
func formatCompact(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10_000:
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dK", n/1000)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	default:
		return fmt.Sprintf("%dM", n/1_000_000)
	}
}

// formatTokens formats token counts as "N/M".
func formatTokens(in, out int) string {
	return fmt.Sprintf("%s/%s", formatCompact(in), formatCompact(out))
}

func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}

// truncateLeft truncates from the left, keeping the trailing (more important) part.
// e.g. "~/github.com/org/repo/session" → "…org/repo/session"
func truncateLeft(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return "…" + string(runes[len(runes)-maxLen+1:])
}

// padRightBg pads a (possibly styled) string to exactly w cell-width with trailing spaces,
// applying the given style to the padding. 選択行でパディング部分にも背景色を付けるために使う。
// bg が空スタイルの場合は素のスペースと同等。
func padRightBg(s string, w int, bg lipgloss.Style) string {
	cur := lipgloss.Width(s)
	if cur >= w {
		return s
	}
	return s + bg.Render(strings.Repeat(" ", w-cur))
}

// detailPaneMetrics calculates the inner width, log height, and PTY viewport height for the detail pane.
// PTY 出力がある場合は下部 20% を ptyViewport に割り当て、残りを logViewport に使う。
func (m *Model) detailPaneMetrics() (innerWidth, logHeight, ptyHeight int) {
	// Width() はボーダー・パディング込みの全体幅を設定するため、フレーム分の減算は不要。
	// ペイン間スペースの " " 分のみ差し引く。
	const frameOverhead = 1
	available := m.width - frameOverhead
	if available < 20 {
		available = 20
	}
	listWidth := available * 35 / 100
	if listWidth < 20 {
		listWidth = 20
	}
	detailWidth := available - listWidth
	if detailWidth < 20 {
		detailWidth = 20
	}
	// Width() はボーダー・パディング込みなので、コンテンツ幅は border(2) + padding(2) を引く
	innerWidth = detailWidth - 4
	if innerWidth < 10 {
		innerWidth = 10
	}

	// Height() はボーダー込みの全体高さを設定するため、ヘッダー行(1) + フッター行(1) のみ差し引く。
	contentHeight := m.height - 2
	if contentHeight < 3 {
		contentHeight = 3
	}

	// アクティブプロセス判定
	hasActiveProcess := false
	if m.selectedID != "" {
		hasActiveProcess = m.manager.HasActiveProcess(m.selectedID)
	}

	// contentHeight はペインの全体高さ（ボーダー込み）。コンテンツ領域はボーダー(上下各1)分を引く。
	availableLines := contentHeight - 2

	if hasActiveProcess {
		// アクティブプロセスあり → PTY 全画面表示
		// ヘッダー非表示、入力ステータス行(separator + line = 2)のみ確保
		availableLines -= 2
		logHeight = 0
		ptyHeight = availableLines
	} else {
		// 完了済みセッション → ヘッダー + JSONL ログ表示
		headerLines := 4 // タイトル + パス + ID行 + 空行
		if m.selectedID != "" {
			if sess := m.manager.GetSession(m.selectedID); sess != nil {
				snap := sess.Snapshot()
				if snap.CurrentTool != "" {
					headerLines++
				}
				if snap.Status.NeedsAttention() {
					headerLines++
				}
				if snap.Status == session.StatusError && snap.ErrorMessage != "" {
					headerLines++
				}
			}
		}
		availableLines -= headerLines
		logHeight = availableLines
		ptyHeight = 0
	}

	if ptyHeight < 0 {
		ptyHeight = 0
	}
	if logHeight < 0 {
		logHeight = 0
	}
	return
}

// syncLogViewport updates both viewport contents and dimensions from the selected session's logs.
// アクティブプロセスあり → ptyViewport に PTY 全画面表示
// 完了済みセッション → logViewport に JSONL ログ表示
func (m *Model) syncLogViewport() {
	// WindowSizeMsg 前は正しいサイズが分からないのでスキップ
	if m.width == 0 {
		return
	}
	debuglog.Printf("[syncLogViewport] selectedID=%q ptyInputActive=%v focusDetail=%v", m.selectedID, m.ptyInputActive, m.focusDetail)
	innerWidth, logHeight, ptyHeight := m.detailPaneMetrics()

	m.logViewport.SetWidth(innerWidth)
	m.logViewport.SetHeight(logHeight)
	m.ptyViewport.SetWidth(innerWidth)
	m.ptyViewport.SetHeight(ptyHeight)

	// PTY プロセスとエミュレータのサイズをビューポートに同期（寸法変更時のみ）
	if ptyHeight > 0 && m.selectedID != "" &&
		(m.selectedID != m.lastResizeID || innerWidth != m.lastResizeCols || ptyHeight != m.lastResizeRows) {
		m.manager.ResizeSession(m.selectedID, innerWidth, ptyHeight)
		m.lastResizeID = m.selectedID
		m.lastResizeCols = innerWidth
		m.lastResizeRows = ptyHeight
	}

	if m.selectedID == "" {
		m.logViewport.SetContent("")
		m.ptyViewport.SetContent("")
		return
	}
	sess := m.manager.GetSession(m.selectedID)
	if sess == nil {
		m.logViewport.SetContent("")
		m.ptyViewport.SetContent("")
		return
	}

	if ptyHeight > 0 {
		// ── アクティブプロセス: PTY 全画面 ──
		debuglog.Printf("[syncLogViewport] calling GetPTYDisplayLines")
		lines := sess.GetPTYDisplayLines()
		debuglog.Printf("[syncLogViewport] GetPTYDisplayLines returned %d lines", len(lines))
		if len(lines) > 0 {
			m.ptyViewport.SetContent(strings.Join(lines, "\n"))
		} else {
			m.ptyViewport.SetContent(dimStyle.Render("(PTY 出力待ち)"))
		}
		if m.ptyFollow {
			m.ptyViewport.GotoBottom()
		}
		m.logViewport.SetContent("")
	} else {
		// ── 完了済み: JSONL ログ表示 ──
		entries := sess.GetStructuredLogs()
		if len(entries) > 0 {
			rendered := RenderLogs(entries, innerWidth, &m.logCache)
			m.logViewport.SetContent(rendered)
		} else {
			m.logViewport.SetContent(dimStyle.Render("(出力なし)"))
		}
		if m.logFollow {
			m.logViewport.GotoBottom()
		}
		m.ptyViewport.SetContent("")
	}
}
