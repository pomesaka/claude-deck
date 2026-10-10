package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/charmbracelet/x/ansi"
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

	header := m.renderHeader()
	sections := []string{header}
	var rows []sessionRow
	if m.mode == viewSelectRepo {
		sections = append(sections, m.repoList.View())
	} else {
		var list string
		list, rows = m.renderSessionList(m.width, m.sessionListHeight())
		sections = append(sections, list)
	}

	sections = append(sections, m.renderFooter())

	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, sections...))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.OnMouse = sessionClickHandler(rows, lipgloss.Height(header))

	return v
}

// sessionRow is where one session was drawn: lines [top, bottom) counted from
// the first line of the list box.
type sessionRow struct {
	id          session.DeckSessionID
	top, bottom int
}

// sessionClickedMsg reports a left click on a session in the list.
type sessionClickedMsg struct {
	id session.DeckSessionID
}

// sessionClickHandler turns a left click on one of rows into a sessionClickedMsg.
// listTop is the screen line of the list box's first line.
// WHY View が返すハンドラで判定する: どの行に何を描いたかを知っているのは描画だけ。Update で
// 座標から逆算すると、下寄せや枠の計算を描画と二重に持つことになる。セッションを ID で返すのは、
// 描画からクリックまでの間に一覧が並び替わっても、描かれていたセッションを指すため。
func sessionClickHandler(rows []sessionRow, listTop int) func(tea.MouseMsg) tea.Cmd {
	return func(msg tea.MouseMsg) tea.Cmd {
		click, ok := msg.(tea.MouseClickMsg)
		if !ok || click.Button != tea.MouseLeft {
			return nil
		}
		y := click.Y - listTop
		for _, row := range rows {
			if y >= row.top && y < row.bottom {
				return func() tea.Msg { return sessionClickedMsg{id: row.id} }
			}
		}
		return nil
	}
}

func (m Model) renderHeader() string {
	// Left: selected session status bar (kept open for future use when nothing is selected).
	runtimeBadge := dimStyle.Render("[" + m.config.RuntimeProvider() + "]")
	statusBar := runtimeBadge
	if m.selectedSnap != nil {
		snap := *m.selectedSnap
		icon := sessionStatusIcon(snap.Status)
		name := lipgloss.NewStyle().Foreground(colorText).Bold(true).Render(snap.Name)
		repo := dimStyle.Render("(" + snap.RepoName + ")")
		statusBar = lipgloss.JoinHorizontal(lipgloss.Top, runtimeBadge, " ", icon, " ", name, "  ", repo)
	}
	left := lipgloss.NewStyle().Padding(0, 1).Render(statusBar)

	// Right: attention badge + rate limits.
	var badge string
	if m.attentionCount > 0 {
		badge = statusApproveStyle.Render(fmt.Sprintf(" [%d asking...]", m.attentionCount))
	}
	firstLine := left
	if badge != "" {
		firstLine = lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", badge)
	}

	usage := m.renderRateLimits()
	if usage == "" {
		return firstLine
	}
	secondLine := lipgloss.NewStyle().Padding(0, 1).Render(usage)
	return lipgloss.JoinVertical(lipgloss.Left, firstLine, secondLine)
}

func (m Model) headerLineCount() int {
	if m.renderRateLimits() == "" {
		return 1
	}
	return 2
}

// sessionStatusIcon returns a colored "●" for the given status.
func sessionStatusIcon(s session.Status) string {
	switch s {
	case session.StatusRunning:
		return statusRunningStyle.Render("●")
	case session.StatusIdle:
		return statusIdleStyle.Render("●")
	case session.StatusSubagentRunning:
		return statusSubagentStyle.Render("●")
	case session.StatusWaitingApproval, session.StatusWaitingAnswer:
		return statusApproveStyle.Render("●")
	case session.StatusCompleted:
		return statusDoneStyle.Render("●")
	case session.StatusError:
		return statusErrorStyle.Render("●")
	case session.StatusUnmanaged:
		return unmanagedIconStyle.Render("●")
	default:
		return dimStyle.Render("●")
	}
}

// sessionListHeight is the outer height of the session list box: the screen
// without the header and the footer line.
func (m Model) sessionListHeight() int {
	return max(3, m.height-m.headerLineCount()-1)
}

// filterBarHeight is 1 while the filter bar is shown under the list, else 0.
func (m Model) filterBarHeight() int {
	if m.filterActive || m.filterText != "" {
		return 1
	}
	return 0
}

const (
	sessionItemHeight = 2 // lines per session in the list
	listBorderHeight  = 2 // top and bottom border of the list box
)

// listWindow is the part of the session list drawn in the list box.
type listWindow struct {
	start, end int // sessions [start, end) are drawn
	// hasAbove and hasBelow tell whether the one-line "↑ 他N件" / "↓ 他N件"
	// indicators are drawn.
	hasAbove, hasBelow bool
}

// sessionListWindow decides which of n sessions fit in a list box of the given
// outer height when scrolled to offset.
func sessionListWindow(height, filterBarHeight, offset, n int) listWindow {
	// Height() はボーダー込みの外寸。コンテンツ領域はボーダー(上下各1)分を差し引く。
	// フィルタバー分も除いた利用可能高さ。
	availHeight := height - listBorderHeight - filterBarHeight

	offset = max(0, offset)
	if offset >= n {
		offset = max(0, n-1)
	}
	w := listWindow{start: offset, hasAbove: offset > 0}
	if w.hasAbove {
		availHeight-- // 上インジケータ分
	}

	w.end = min(n, offset+max(1, availHeight/sessionItemHeight))
	// 下にまだあるなら、インジケータ分を確保して再計算
	if w.end < n {
		w.hasBelow = true
		w.end = min(n, offset+max(1, (availHeight-1)/sessionItemHeight))
	}
	return w
}

// renderSessionList draws the list box and reports where each session is in it.
func (m Model) renderSessionList(width, height int) (string, []sessionRow) {
	style := sessionListStyle

	// m.viewSnaps は Update() 内で事前計算済み。View() でのロック取得を避けるため
	// visibleSessions() は呼ばず、キャッシュ済みスナップショットを直接参照する。
	snaps := m.viewSnaps

	var filterBar string
	if m.filterActive {
		filterBar = m.filterInput.View()
	} else if m.filterText != "" {
		filterBar = dimStyle.Render("/ " + m.filterText)
	}

	if len(snaps) == 0 {
		var msg string
		if m.filterText != "" || m.filterActive {
			msg = dimStyle.Render("一致するセッションなし")
		} else {
			msg = dimStyle.Render("セッションなし。'n'で新規作成")
		}
		if filterBar != "" {
			content := lipgloss.JoinVertical(lipgloss.Left, msg, filterBar)
			return style.Width(width).Height(height).AlignVertical(lipgloss.Bottom).Render(content), nil
		}
		return style.Width(width).Height(height).Render(msg), nil
	}

	win := sessionListWindow(height, m.filterBarHeight(), m.scrollOffset, len(snaps))

	var items []string

	if win.hasAbove {
		items = append(items, dimStyle.Render(fmt.Sprintf("  ↑ 他%d件", win.start)))
	}

	// リストペインのコンテンツ幅: border(2) + padding(2) を引く
	itemWidth := width - 4
	if itemWidth < 10 {
		itemWidth = 10
	}
	// rows は content の先頭からの行で記録し、最後に箱の中での位置へずらす。
	var rows []sessionRow
	line := 0
	for _, item := range items {
		line += lipgloss.Height(item)
	}
	for i := win.start; i < win.end; i++ {
		item := renderSessionItem(snaps[i], i == m.cursor, itemWidth)
		h := lipgloss.Height(item)
		rows = append(rows, sessionRow{id: snaps[i].ID, top: line, bottom: line + h})
		line += h
		items = append(items, item)
	}

	if remaining := len(snaps) - win.end; remaining > 0 {
		items = append(items, dimStyle.Render(fmt.Sprintf("  ↓ 他%d件", remaining)))
	}

	if filterBar != "" {
		items = append(items, filterBar)
	}

	content := lipgloss.JoinVertical(lipgloss.Left, items...)
	// アイテムが少ない場合は下寄せで表示（AlignVertical は Height 内のコンテンツ配置を制御）
	box := style.Width(width).Height(height).AlignVertical(lipgloss.Bottom).Render(content)

	// 下寄せなので、content の最終行は下の枠のすぐ上にある。
	const bottomBorder = 1
	contentTop := lipgloss.Height(box) - bottomBorder - lipgloss.Height(content)
	for i := range rows {
		rows[i].top += contentTop
		rows[i].bottom += contentTop
	}
	return box, rows
}

// selBg returns the style with the selected background applied when selected is true.
func selBg(s lipgloss.Style, selected bool) lipgloss.Style {
	if selected {
		return s.Background(colorBgSelected)
	}
	return s
}

// renderWorkDir renders where the session works, as "<path to the repository>/<repository>[/<sub project>]",
// cut on the left to width. The home directory is shown as "~".
// The repository and the sub project are emphasized; the path before them is dimmed.
func renderWorkDir(snap session.Snapshot, width int, dim, emph lipgloss.Style) string {
	repoPath := snap.RepoPath
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(repoPath, home) {
		repoPath = "~" + repoPath[len(home):]
	}
	if repoPath == "" {
		return dim.Render(truncateLeft(snap.WorkDir(), width))
	}
	full := repoPath
	if snap.SubProjectDir != "" {
		full += "/" + snap.SubProjectDir
	}
	// 強調するのは、リポジトリ名から後ろ（サブプロジェクトを含む）。
	emphLen := len([]rune(snap.RepoName))
	if snap.SubProjectDir != "" {
		emphLen += 1 + len([]rune(snap.SubProjectDir))
	}
	if !strings.HasSuffix(repoPath, snap.RepoName) {
		emphLen = 0
	}
	runes := []rune(truncateLeft(full, width))
	split := max(0, len(runes)-emphLen)
	return dim.Render(string(runes[:split])) + emph.Render(string(runes[split:]))
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
	// statusMessage: line2 末尾に表示するメッセージ（エラーの理由）
	var statusMessage string
	switch snap.Status {
	case session.StatusRunning:
		statusIcon = selBg(statusRunningStyle, selected).Render("●")
	case session.StatusIdle:
		statusIcon = selBg(statusIdleStyle, selected).Render("●")
	case session.StatusSubagentRunning:
		statusIcon = selBg(statusSubagentStyle, selected).Render("●")
	case session.StatusWaitingApproval:
		statusIcon = selBg(statusApproveStyle, selected).Render("●")
	case session.StatusWaitingAnswer:
		statusIcon = selBg(statusQuestionStyle, selected).Render("●")
	case session.StatusCompleted:
		statusIcon = selBg(statusDoneStyle, selected).Render("●")
	case session.StatusError:
		statusIcon = selBg(statusErrorStyle, selected).Render("●")
		statusMessage = snap.ErrorMessage
		if statusMessage == "" {
			statusMessage = "エラー"
		}
	case session.StatusUnmanaged:
		statusIcon = selBg(unmanagedIconStyle, selected).Render("●")
	}

	// line1: [icon] 作業ディレクトリ@bookmark ……… セッション名（右寄せ）
	iconCol := statusIcon + bg.Render(" ")
	iconWidth := lipgloss.Width(iconCol)

	emphStyle := selBg(lipgloss.NewStyle().Foreground(colorPrimary).Bold(true), selected)
	nameStyle := selBg(lipgloss.NewStyle().Foreground(colorSecondary), selected)
	dim := selBg(dimStyle, selected)
	sp := bg.Render(" ")

	// 幅の配分: セッション名とブックマークは切らずに出し、作業ディレクトリを先に省略する。
	// WHY: 作業ディレクトリは左を省略しても末尾（リポジトリ名）で見分けがつく。ブックマークと
	// セッション名は、末尾が切れると別のものと区別できなくなる。
	// 作業ディレクトリが minWorkDirWidth を下回るときだけ、ブックマークを切る。
	const minWorkDirWidth = 10
	avail := cw - iconWidth
	nameCol := nameStyle.Render(truncate(snap.Name, avail-1))
	avail -= lipgloss.Width(nameCol) + 1 // 名前の前に最低 1 桁あける
	var bookmarkCol string
	if room := avail - 1 - minWorkDirWidth; snap.BookmarkName != "" && room > 0 {
		bookmarkCol = dim.Render("@") + selBg(lipgloss.NewStyle().Foreground(colorText), selected).Render(truncate(snap.BookmarkName, room))
		avail -= lipgloss.Width(bookmarkCol)
	}
	line1 := joinEnds(iconCol+renderWorkDir(snap, avail, dim, emphStyle)+bookmarkCol, nameCol, cw, bg)

	// line2: インデント(icon幅) + 起動時間 + 最終更新 + [メッセージ] ……… [エイリアス]（右寄せ）
	const (
		uptimeWidth = 6  // "23h59m" / "99d23h"
		timeWidth   = 11 // "01/02 15:04"
	)
	indent := bg.Render(strings.Repeat(" ", iconWidth))
	uptime := padRightBg(nameStyle.Render(formatUptime(snap)), uptimeWidth, bg)
	lastAct := padRightBg(dim.Render(formatLastActivity(snap)), timeWidth, bg)
	left2 := indent + uptime + sp + lastAct

	var aliasCol string
	if snap.Alias != "" {
		alias := truncate(snap.Alias, cw-lipgloss.Width(left2)-1)
		aliasCol = selBg(lipgloss.NewStyle().Foreground(colorText).Bold(true), selected).Render(alias)
	}
	// メッセージは、日時とエイリアスのあいだに残った幅に収める。
	rest := cw - lipgloss.Width(left2) - 1
	if aliasCol != "" {
		rest -= lipgloss.Width(aliasCol) + 1
	}
	if statusMessage != "" && rest > 4 {
		left2 += sp + selBg(statusErrorStyle, selected).Render(truncate(statusMessage, rest))
	}
	line2 := joinEnds(left2, aliasCol, cw, bg)

	content := lipgloss.JoinVertical(lipgloss.Left, line1, line2)

	style := sessionItemStyle
	if selected {
		style = sessionItemSelectedStyle
	}
	// width は border-box 外寸。Padding(0,1) 込みで width セル幅にする。
	return style.Width(width).Render(content)
}

func (m Model) renderFooter() string {
	// When a status message is active, show it exclusively — no help text.
	if m.statusMsg != "" {
		return footerStyle.Render(statusApproveStyle.Render(m.statusMsg))
	}

	var helpText string
	if m.mode == viewSelectRepo {
		helpText = "Enter:ワークスペース作成+起動 C-Enter:直接起動 Esc:戻る"
	} else if m.filterActive {
		helpText = "Enter:確定 Esc:キャンセル"
	} else if m.filterText != "" {
		helpText = fmt.Sprintf("フィルタ: %s / Esc:解除 ?:ヘルプ", m.filterText)
	} else {
		helpText = "j/k:移動 gg/G:先頭/末尾 /:フィルタ n:新規 Enter:入力 r:再開 f:フォーク t:ターミナル R:再描画 x:終了 C-c:quit"
	}
	return footerStyle.Render(dimStyle.Render(helpText))
}

const usageGaugeWidth = 10

// renderRateLimits renders gauge bars for Claude.ai rate limit windows.
// Data is what the sessions reported to rate-limits.json (package ratelimits).
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
	h := int(d.Hours()) // intentional truncation: fractional hours accounted for in mins
	mins := int(d.Minutes()) % 60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", mins)
	case h < 24:
		return fmt.Sprintf("%dh%dm", h, mins)
	default:
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	}
}

// formatUptime formats how long the session has been running: from its start to
// now, or to when it finished. "-" when the start is unknown.
func formatUptime(snap session.Snapshot) string {
	if snap.StartedAt.IsZero() {
		return "-"
	}
	return formatDuration(snap.Elapsed)
}

// formatLastActivity formats when the session was last active, falling back to
// when it finished and then to when it started. "-" when none is known.
func formatLastActivity(snap session.Snapshot) string {
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

// truncate cuts s to at most maxLen terminal cells, ending it with "…".
// WHY 文字数でなく表示幅で切る: 全角文字は 2 桁を使う。文字数で切ると行が幅を超えて折り返し、
// 1 セッション 2 行の前提が崩れる。
func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	return ansi.Truncate(s, maxLen, "…")
}

// truncateLeft cuts s from the left to at most maxLen terminal cells, keeping
// the trailing (more important) part.
// e.g. "~/github.com/org/repo/session" → "…org/repo/session"
func truncateLeft(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	over := lipgloss.Width(s) - maxLen
	if over <= 0 {
		return s
	}
	return ansi.TruncateLeft(s, over+1, "…")
}

// joinEnds puts left at the start and right at the end of a line w cells wide,
// filling the gap with spaces in the bg style. With an empty right it only pads.
// The caller makes sure the two fit; when they do not, they are joined with one space.
func joinEnds(left, right string, w int, bg lipgloss.Style) string {
	if right == "" {
		return padRightBg(left, w, bg)
	}
	gap := max(1, w-lipgloss.Width(left)-lipgloss.Width(right))
	return left + bg.Render(strings.Repeat(" ", gap)) + right
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
