package tui

import (
	"fmt"
	"os"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/session"
)

// handleKey processes keyboard input and dispatches to the appropriate handler.
func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	// PTY 入力モード中は Ctrl+C を含む全キーを PTY に転送する。
	// claude-deck の終了は入力モード外での Ctrl+C で行う。
	if m.ptyInputActive {
		return m.handlePTYInputKey(msg)
	}

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

	// TODO: config.toml の keybind 設定に対応する
	// split mode ではレイアウトサイクルは不要（常にリスト表示のみ）
	if msg.String() == "ctrl+e" && !m.backendMode.IsSplit() {
		m.layout.CycleMode()
		m.syncLogViewport()
		return nil
	}

	display := m.selectedDisplayChannel()

	// Vim-style multi-key sequences: gg, dd
	if m.pendingG {
		m.pendingG = false
		if msg.String() == "g" {
			if m.layout.IsDetailFocused() {
				m.viewportGotoTop(display)
			} else {
				m.cursor = 0
				cmds := m.updateSelected()
				m.ensureCursorVisible()
				return tea.Batch(cmds...)
			}
			return nil
		}
		// Not gg — fall through to normal handling
	}
	// split mode ではリストが常にフォーカスされる（detail ペインは右の tmux ウィンドウ）
	if !m.backendMode.IsSplit() && m.layout.IsDetailFocused() {
		if cmd, handled := m.handleDetailPaneKey(msg, display); handled {
			return cmd
		}
	}

	return m.handleListKey(msg, display)
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

// handleDetailPaneKey processes scroll keys when the detail pane is focused.
// Returns (cmd, true) if the key was handled, (nil, false) to fall through.
func (m *Model) handleDetailPaneKey(msg tea.KeyPressMsg, display session.DisplayChannel) (tea.Cmd, bool) {
	switch msg.String() {
	case "G":
		m.viewportGotoBottom(display)
		return nil, true
	case "g":
		m.pendingG = true
		return nil, true
	case "h", "left":
		m.layout.FocusList()
		return nil, true
	case "ctrl+b":
		// 半ページ下スクロール（ctrl+u と上下ペア）
		m.viewportHalfPageDown(display)
		return nil, true
	case "ctrl+u":
		// 半ページ上スクロール
		m.viewportHalfPageUp(display)
		return nil, true
	case "j", "down", "k", "up", "pgup", "pgdown", "f", "b", "u":
		return m.viewportUpdate(msg, display), true
	}
	// Other keys (n, enter, r, x, dd, tab, ?) fall through to list handling
	return nil, false
}

// handleListKey processes navigation and command keys in the session list.
func (m *Model) handleListKey(msg tea.KeyPressMsg, display session.DisplayChannel) tea.Cmd {
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

	case "h", "left":
		m.layout.FocusList()

	case "l", "right":
		// split mode では l キーは無効（detail ペインは右の tmux ウィンドウ）
		if !m.backendMode.IsSplit() {
			m.layout.FocusDetail()
		}

	case "tab":
		if !m.layout.IsDetailFocused() {
			// リストフォーカス時: 次の approve/answer 待ちセッションへジャンプ
			if idx := m.findNextAttentionSession(); idx >= 0 {
				m.cursor = idx
				cmds = append(cmds, m.updateSelected()...)
				m.ensureCursorVisible()
				m.layout.FocusDetail()
				if m.backendMode.IsTmuxLike() {
					// tmux mode: updateSelected が返す switchRightPane(false) は tmux window 選択のみ。
					// tab はユーザーが介入しに行く意図なので Enter と同様に focusRight=true で
					// Ghostty の右ペイン（tmux client）にフォーカスを移す。
					cmds = append(cmds, m.switchRightPane(m.selectedID, true))
				} else {
					m.ptyInputActive = true
				}
				m.syncLogViewport()
				return tea.Batch(cmds...)
			}
		}
		m.layout.ToggleFocus()

	case "n":
		return m.startNewSession()

	case "enter", "i":
		// 生きた PTY プロセスあり → 詳細ペインに切り替えて PTY 直接入力モード開始
		debuglog.Printf("[key:%s] selectedID=%q display=%v tmuxMode=%v", key, m.selectedID, display, m.backendMode.IsTmuxLike())
		if m.backendMode.IsTmuxLike() {
			// tmux mode: focus the window in the tmux client.
			// Completed sessions pressed with Enter → resume in a new tmux window.
			if display == session.DisplayNone {
				// 実行中セッション: tmux ウィンドウを前面に出し、
				// Ghostty の右ペイン（tmux client）にフォーカスを移す。
				return m.switchRightPane(m.selectedID, true)
			}
			if key == "enter" {
				return m.resumeSelected()
			}
			return nil
		}
		if display == session.DisplayPTY {
			debuglog.Printf("[key:%s] activating PTY input mode", key)
			m.layout.FocusDetail()
			m.ptyInputActive = true
			m.syncLogViewport()
			debuglog.Printf("[key:%s] PTY input mode activated", key)
			return nil
		}
		if key == "enter" {
			debuglog.Printf("[key:enter] no active process, calling resumeSelected")
			return m.resumeSelected()
		}

	case "r":
		return m.resumeSelected()

	case m.config.Keybinds.Fork:
		return m.forkSelected()

	case "R":
		m.syncLogViewport()
		return tea.ClearScreen

	case "x":
		return m.killSelected()

	case m.config.Keybinds.OpenTerm:
		return m.openTerminal()

	case "/":
		if !m.layout.IsDetailFocused() {
			m.filterActive = true
			m.filterInput.Focus()
			return nil
		}

	case "esc":
		// フィルタ適用中 → フィルタ解除
		if m.filterText != "" {
			m.filterText = ""
			m.filterInput.SetValue("")
			return tea.Batch(m.clampCursorToVisible()...)
		}

	case "?":
		m.statusMsg = "h/l:ペイン切替 j/k:移動 gg/G:先頭/末尾 /:フィルタ C-b/C-u:半頁 n:新規 Enter/i:入力/再開 r:再開 f:フォーク t:ターミナル R:再描画 dd:削除 dD:deckのみ削除 x:終了 C-c:quit"
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
	return cmds
}

// viewportGotoTop scrolls the active viewport to the top.
func (m *Model) viewportGotoTop(display session.DisplayChannel) {
	if display == session.DisplayPTY {
		m.ptyViewport.GotoTop()
		m.ptyFollow = false
	} else {
		m.logViewport.GotoTop()
		m.logFollow = false
	}
}

// viewportGotoBottom scrolls the active viewport to the bottom and enables follow mode.
func (m *Model) viewportGotoBottom(display session.DisplayChannel) {
	if display == session.DisplayPTY {
		m.ptyFollow = true
		m.ptyViewport.GotoBottom()
	} else {
		m.logFollow = true
		m.logViewport.GotoBottom()
	}
}

// viewportHalfPageUp scrolls the active viewport half a page up.
func (m *Model) viewportHalfPageUp(display session.DisplayChannel) {
	if display == session.DisplayPTY {
		m.ptyViewport.HalfPageUp()
		m.ptyFollow = false
	} else {
		m.logViewport.HalfPageUp()
		m.logFollow = false
	}
}

// viewportHalfPageDown scrolls the active viewport half a page down.
func (m *Model) viewportHalfPageDown(display session.DisplayChannel) {
	if display == session.DisplayPTY {
		m.ptyViewport.HalfPageDown()
		m.ptyFollow = m.ptyViewport.AtBottom()
	} else {
		m.logViewport.HalfPageDown()
		m.logFollow = m.logViewport.AtBottom()
	}
}

// viewportUpdate forwards a scroll key to the active viewport and updates follow state.
func (m *Model) viewportUpdate(msg tea.KeyPressMsg, display session.DisplayChannel) tea.Cmd {
	var cmd tea.Cmd
	if display == session.DisplayPTY {
		m.ptyViewport, cmd = m.ptyViewport.Update(msg)
		m.ptyFollow = m.ptyViewport.AtBottom()
	} else {
		m.logViewport, cmd = m.logViewport.Update(msg)
		m.logFollow = m.logViewport.AtBottom()
	}
	return cmd
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

// handlePTYInputKey processes keys when PTY input mode is active.
// キーイベントを直接 PTY stdin に転送し、Claude Code の Ink UI をそのまま操作する。
// Ctrl+D で入力モード終了。Ctrl+C は PTY に転送（プロセス中断用）。
func (m *Model) handlePTYInputKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	if key == "ctrl+d" {
		m.deactivatePTYInput()
		return nil
	}

	data := keyToBytes(msg)
	if len(data) == 0 {
		debuglog.Printf("[pty-input] key=%q → no bytes (skipped)", key)
		return nil
	}
	debuglog.Printf("[pty-input] key=%q → %x (%d bytes)", key, data, len(data))

	mgr := m.manager
	sid := m.selectedID
	return func() tea.Msg {
		err := mgr.WriteToSession(sid, data)
		return ptyInputSentMsg{err: err}
	}
}

func (m *Model) deactivatePTYInput() {
	m.ptyInputActive = false
	m.layout.FocusList()
	m.syncLogViewport()
}

// keyToBytes converts a bubbletea KeyPressMsg to the corresponding byte sequence
// that should be sent to a PTY.
// bubbletea v2 の KeyPressMsg.String() は修飾キーを正しく反映するが、
// tea.Key(msg) への変換で Mod ビットが落ちるケースがあるため、
// 修飾キー付きの特殊キーは String() ベースで判定する。
func keyToBytes(msg tea.KeyPressMsg) []byte {
	k := tea.Key(msg)
	s := msg.String()

	// 修飾キー付き特殊キー（String() ベースで判定）
	switch s {
	case "shift+tab":
		return []byte{0x1b, '[', 'Z'}
	}

	// Ctrl+A..Z → \x01..\x1a
	if k.Mod&tea.ModCtrl != 0 && k.Code >= 'a' && k.Code <= 'z' {
		return []byte{byte(k.Code - 'a' + 1)}
	}

	// Special keys
	switch k.Code {
	case tea.KeyEnter:
		return []byte{'\r'}
	case tea.KeyTab:
		return []byte{'\t'}
	case tea.KeyBackspace:
		return []byte{0x7f}
	case tea.KeyEscape:
		return []byte{0x1b}
	case tea.KeySpace:
		return []byte{0x20}
	case tea.KeyUp:
		return []byte{0x1b, '[', 'A'}
	case tea.KeyDown:
		return []byte{0x1b, '[', 'B'}
	case tea.KeyRight:
		return []byte{0x1b, '[', 'C'}
	case tea.KeyLeft:
		return []byte{0x1b, '[', 'D'}
	case tea.KeyHome:
		return []byte{0x1b, '[', 'H'}
	case tea.KeyEnd:
		return []byte{0x1b, '[', 'F'}
	case tea.KeyDelete:
		return []byte{0x1b, '[', '3', '~'}
	case tea.KeyPgUp:
		return []byte{0x1b, '[', '5', '~'}
	case tea.KeyPgDown:
		return []byte{0x1b, '[', '6', '~'}
	}

	// Printable characters (including multibyte UTF-8)
	if k.Text != "" {
		return []byte(k.Text)
	}

	// Single rune fallback (e.g. plain letter keys without Text)
	if k.Code > 0 {
		var buf [utf8.UTFMax]byte
		n := utf8.EncodeRune(buf[:], k.Code)
		return buf[:n]
	}

	return nil
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
	cols, _, rows, _ := m.detailPaneMetrics()
	return func() tea.Msg {
		err := mgr.ResumeSession(ctx, id, cols, rows)
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
	if snap.ClaudeSessionID == "" {
		m.statusMsg = "ClaudeSessionID がないためフォークできません"
		return clearStatusCmd()
	}

	m.statusMsg = "セッションフォーク中..."
	mgr := m.manager
	ctx := m.ctx
	id := m.selectedID
	cols, _, rows, _ := m.detailPaneMetrics()
	return func() tea.Msg {
		newSess, err := mgr.ForkSession(ctx, id, cols, rows)
		var newID session.DeckSessionID
		if newSess != nil {
			newID = newSess.ID
		}
		return sessionForkedMsg{sessionID: newID, err: err}
	}
}

// killSelected terminates the currently selected session and cleans up its workspace.
func (m *Model) killSelected() tea.Cmd {
	if m.selectedID == "" {
		return nil
	}
	if err := m.manager.Kill(m.selectedID); err != nil {
		m.statusMsg = fmt.Sprintf("終了エラー: %v", err)
	} else {
		m.statusMsg = "セッションを終了しました (workspace削除済)"
	}
	return clearStatusCmd()
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


