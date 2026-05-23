package tui

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/session"
)

// handleKey processes keyboard input and dispatches to the appropriate handler.
func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	switch key {
	case "ctrl+c":
		m.confirmQuit = true
		m.statusMsg = "終了しますか? (y/n)"
		return nil
	case "ctrl+z":
		m.quitting = true
		return tea.Quit
	}

	// 終了確認中: y で終了、それ以外でキャンセル
	if m.confirmQuit {
		m.confirmQuit = false
		if key == "y" || key == "Y" {
			m.quitting = true
			return tea.Quit
		}
		m.statusMsg = ""
		return nil
	}

	switch m.mode {
	case viewSelectRepo:
		return m.handleRepoSelectKey(msg)
	case viewDashboard:
		return m.handleDashboardKey(msg)
	}

	return nil
}

// handleDashboardKey processes keys in dashboard mode.
func (m *Model) handleDashboardKey(msg tea.KeyPressMsg) tea.Cmd {
	if m.filterActive {
		return m.handleFilterKey(msg)
	}

	// Vim-style multi-key sequences: gg
	if m.pendingG {
		m.pendingG = false
		if msg.String() == "g" {
			m.cursor = 0
			cmds := m.updateSelected()
			m.ensureCursorVisible()
			return tea.Batch(cmds...)
		}
		// Not gg — fall through to normal handling
	}

	return m.handleListKey(msg)
}

// handleFilterKey processes keys while the filter input is active.
func (m *Model) handleFilterKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		m.filterText = m.filterInput.Value()
		m.filterActive = false
		m.filterInput.Blur()
	case "esc":
		// 入力破棄: 前回の確定フィルタに戻す
		m.filterInput.SetValue(m.filterText)
		m.filterActive = false
		m.filterInput.Blur()
	default:
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return tea.Batch(append(m.clampCursorToVisible(), cmd)...)
	}
	return tea.Batch(m.clampCursorToVisible()...)
}

// handleListKey processes navigation and command keys in the session list.
func (m *Model) handleListKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	var cmds []tea.Cmd
	switch key {
	case "j", "down":
		if m.cursor < len(m.visibleSessions())-1 {
			m.cursor++
			cmds = append(cmds, m.updateSelected()...)
			m.ensureCursorVisible()
		}

	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
			cmds = append(cmds, m.updateSelected()...)
			m.ensureCursorVisible()
		}

	case "G":
		visible := m.visibleSessions()
		if len(visible) > 0 {
			m.cursor = len(visible) - 1
			cmds = append(cmds, m.updateSelected()...)
			m.ensureCursorVisible()
		}

	case "g":
		m.pendingG = true

	case "tab":
		// 次の approve/answer 待ちセッションへジャンプ
		if idx := m.findNextAttentionSession(); idx >= 0 {
			m.cursor = idx
			cmds = append(cmds, m.updateSelected()...)
			m.ensureCursorVisible()
			// tab はユーザーが介入しに行く意図なので focusRight=true で
			// Ghostty の右ペイン（tmux client）にフォーカスを移す。
			cmds = append(cmds, m.switchRightPane(m.selectedID, true))
			return tea.Batch(cmds...)
		}

	case "n":
		return m.startNewSession()

	case "enter":
		debuglog.Printf("[key:enter] selectedID=%q", m.selectedID)
		// 実行中セッション: tmux ウィンドウを前面に出す。
		// 完了済みセッション: resume。
		if m.selectedDisplayChannel() == session.DisplayTmux {
			return m.switchRightPane(m.selectedID, true)
		}
		return m.resumeSelected()

	case "r":
		return m.resumeSelected()

	case m.config.Keybinds.Fork:
		return m.forkSelected()

	case "R":
		return tea.ClearScreen

	case "x":
		return m.killSelected()

	case m.config.Keybinds.OpenTerm:
		return m.openTerminal()

	case "/":
		m.filterActive = true
		m.filterInput.Focus()
		return nil

	case "esc":
		// フィルタ適用中 → フィルタ解除
		if m.filterText != "" {
			m.filterText = ""
			m.filterInput.SetValue("")
			return tea.Batch(m.clampCursorToVisible()...)
		}

	case "?":
		m.statusMsg = "j/k:移動 gg/G:先頭/末尾 /:フィルタ tab:要注意 n:新規 Enter/r:再開 f:フォーク t:ターミナル x:終了 R:再描画 C-c:quit"
		return clearStatusCmd()
	}

	return tea.Batch(cmds...)
}

// clampCursorToVisible ensures the cursor index is within the visible session list,
// then updates the selected session and scroll position.
// Returns any tea.Cmd produced by updateSelected (e.g. switchRightPane in tmux mode).
func (m *Model) clampCursorToVisible() []tea.Cmd {
	visible := m.visibleSessions()
	if m.cursor >= len(visible) {
		m.cursor = max(0, len(visible)-1)
	}
	cmds := m.updateSelected()
	m.ensureCursorVisible()
	// フィルタ変化後に viewSnaps を最新化する。refreshSessions は SessionRefreshMsg で
	// のみ呼ばれるため、キー入力だけでは viewSnaps が古いまま View() に渡ってしまう。
	m.refreshViewData()
	return cmds
}

// findNextAttentionSession returns the index of the next session that needs
// user attention (approve/answer 待ち), searching forward from the current
// cursor with wrap-around. Returns -1 if none found.
func (m *Model) findNextAttentionSession() int {
	visible := m.visibleSessions()
	n := len(visible)
	if n == 0 {
		return -1
	}
	for i := 1; i <= n; i++ {
		idx := (m.cursor + i) % n
		if visible[idx].GetStatus().NeedsAttention() {
			return idx
		}
	}
	return -1
}

// handleRepoSelectKey processes keys in repo selector mode.
// 常にフィルタリング状態のため、Enter/Esc/カーソル移動は自前で処理し、
// それ以外は list.Model に委譲する（Update() 末尾でフィルタ入力を処理）。
// Filtering 状態では list 内部が CursorUp/CursorDown を無効化するため、
// カーソル操作は公開メソッドを直接呼ぶ。
func (m *Model) handleRepoSelectKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	switch key {
	case "esc":
		m.mode = viewDashboard
		return nil

	case "enter", "ctrl+enter":
		item := m.repoList.SelectedItem()
		if item == nil {
			return nil
		}
		ri, ok := item.(repoItem)
		if !ok {
			return nil
		}
		withWorkspace := key == "enter"
		return m.selectRepo(ri, withWorkspace)

	case "tab", "down", "ctrl+n":
		m.repoList.CursorDown()
		return nil
	case "shift+tab", "up", "ctrl+p":
		m.repoList.CursorUp()
		return nil
	}

	// その他のキーは list.Update に委譲（フィルタ入力等）
	var cmd tea.Cmd
	m.repoList, cmd = m.repoList.Update(msg)
	return cmd
}

// resumeSelected resumes the currently selected completed session.
func (m *Model) resumeSelected() tea.Cmd {
	if m.selectedID == "" {
		return nil
	}
	sess := m.manager.GetSession(m.selectedID)
	if sess == nil {
		return nil
	}
	status := sess.GetStatus()
	if status != session.StatusCompleted && status != session.StatusError && status != session.StatusUnmanaged {
		m.statusMsg = "実行中のセッションは再開できません"
		return clearStatusCmd()
	}

	m.statusMsg = "セッション再開中..."
	mgr := m.manager
	ctx := m.ctx
	id := m.selectedID
	return func() tea.Msg {
		err := mgr.ResumeSession(ctx, id)
		return sessionResumedMsg{err: err}
	}
}

// forkSelected creates a new session forking from the selected session's conversation.
func (m *Model) forkSelected() tea.Cmd {
	if m.selectedID == "" {
		return nil
	}
	sess := m.manager.GetSession(m.selectedID)
	if sess == nil {
		return nil
	}

	snap := sess.Snapshot()
	if snap.RuntimeSessionID == "" {
		m.statusMsg = "RuntimeSessionID がないためフォークできません"
		return clearStatusCmd()
	}

	m.statusMsg = "セッションフォーク中..."
	mgr := m.manager
	ctx := m.ctx
	id := m.selectedID
	return func() tea.Msg {
		newSess, err := mgr.ForkSession(ctx, id)
		var newID session.DeckSessionID
		if newSess != nil {
			newID = newSess.ID
		}
		return sessionForkedMsg{sessionID: newID, err: err}
	}
}

// killSelected terminates the currently selected session and cleans up its workspace.
// Runs asynchronously via tea.Cmd so large workspace deletions (e.g. node_modules) do not block the UI.
//
// Unlike resumeSelected/forkSelected (which complete quickly and are guarded at the Manager
// layer by status checks), cleanupWorkspace can take many seconds on large directories.
// m.killing prevents concurrent workspace deletions while one is already in flight.
func (m *Model) killSelected() tea.Cmd {
	if m.selectedID == "" || m.killing {
		return nil
	}
	sid := m.selectedID
	mgr := m.manager
	m.killing = true
	m.statusMsg = "セッション終了中..."
	return func() tea.Msg {
		return sessionKilledMsg{err: mgr.Kill(sid)}
	}
}

// openTerminal opens a new Ghostty terminal in the selected session's working directory.
func (m *Model) openTerminal() tea.Cmd {
	if m.selectedID == "" {
		return nil
	}
	sess := m.manager.GetSession(m.selectedID)
	if sess == nil {
		return nil
	}
	snap := sess.Snapshot()
	workDir := snap.WorkDir()
	if workDir == "" {
		m.statusMsg = "作業ディレクトリが不明です"
		return clearStatusCmd()
	}
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		m.statusMsg = fmt.Sprintf("ディレクトリが見つかりません: %s", workDir)
		return clearStatusCmd()
	}
	ghosttyTitle := snap.BookmarkName
	if ghosttyTitle == "" {
		ghosttyTitle = snap.TerminalTitle
	}
	if err := m.ghostty.Open(workDir, ghosttyTitle); err != nil {
		m.statusMsg = fmt.Sprintf("ターミナル起動エラー: %v", err)
		return clearStatusCmd()
	}
	m.statusMsg = fmt.Sprintf("ターミナルを開きました: %s", workDir)
	return clearStatusCmd()
}
