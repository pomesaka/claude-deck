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
	key := msg.String()

	// フィルタ入力中のキー処理
	if m.filterActive {
		switch key {
		case "enter":
			m.filterText = m.filterInput.Value()
			m.filterActive = false
			m.filterInput.Blur()
			// フィルタ確定後、カーソルを範囲内に収める
			visible := m.visibleSessions()
			if m.cursor >= len(visible) {
				m.cursor = max(0, len(visible)-1)
			}
			m.updateSelected()
			m.ensureCursorVisible()
		case "esc":
			// 入力破棄: 前回の確定フィルタに戻す
			m.filterInput.SetValue(m.filterText)
			m.filterActive = false
			m.filterInput.Blur()
			visible := m.visibleSessions()
			if m.cursor >= len(visible) {
				m.cursor = max(0, len(visible)-1)
			}
			m.updateSelected()
			m.ensureCursorVisible()
		default:
			// キー入力を textinput に反映してからカーソル再計算
			var cmd tea.Cmd
			m.filterInput, cmd = m.filterInput.Update(msg)
			visible := m.visibleSessions()
			if m.cursor >= len(visible) {
				m.cursor = max(0, len(visible)-1)
			}
			m.updateSelected()
			m.ensureCursorVisible()
			return cmd
		}
		return nil
	}

	// アクティブプロセスの有無でスクロール対象を切り替え
	hasActiveProcess := m.selectedID != "" && m.manager.HasActiveProcess(m.selectedID)

	// Vim-style multi-key sequences: gg, dd
	if m.pendingG {
		m.pendingG = false
		if key == "g" {
			if m.focusDetail {
				// gg → scroll to top
				if hasActiveProcess {
					m.ptyViewport.GotoTop()
					m.ptyFollow = false
				} else {
					m.logViewport.GotoTop()
					m.logFollow = false
				}
			} else {
				// gg → go to top of list
				m.cursor = 0
				m.updateSelected()
				m.ensureCursorVisible()
			}
			return nil
		}
		// Not gg — fall through to normal handling
	}
	if m.pendingD {
		m.pendingD = false
		switch key {
		case "d":
			return m.deleteSelected()
		case "D":
			return m.removeSelected()
		}
		// Not dd or dD — fall through to normal handling
	}

	// Detail pane focused: viewport handles scroll keys
	if m.focusDetail {
		switch key {
		case "G":
			if hasActiveProcess {
				m.ptyFollow = true
				m.ptyViewport.GotoBottom()
			} else {
				m.logFollow = true
				m.logViewport.GotoBottom()
			}
			return nil
		case "g":
			m.pendingG = true
			return nil
		case "h", "left":
			m.focusDetail = false
			return nil
		case "ctrl+b":
			// 半ページ下スクロール
			if hasActiveProcess {
				m.ptyViewport.HalfPageDown()
				m.ptyFollow = m.ptyViewport.AtBottom()
			} else {
				m.logViewport.HalfPageDown()
				m.logFollow = m.logViewport.AtBottom()
			}
			return nil
		case "ctrl+u":
			// 半ページ上スクロール
			if hasActiveProcess {
				m.ptyViewport.HalfPageUp()
				m.ptyFollow = false
			} else {
				m.logViewport.HalfPageUp()
				m.logFollow = false
			}
			return nil
		case "j", "down", "k", "up", "pgup", "pgdown", "f", "b", "u":
			var cmd tea.Cmd
			if hasActiveProcess {
				m.ptyViewport, cmd = m.ptyViewport.Update(msg)
				m.ptyFollow = m.ptyViewport.AtBottom()
			} else {
				m.logViewport, cmd = m.logViewport.Update(msg)
				m.logFollow = m.logViewport.AtBottom()
			}
			if cmd != nil {
				return cmd
			}
			return nil
		}
		// Other keys (q, n, enter, r, x, dd, tab, ?) fall through to common handling
	}

	switch key {
	case "j", "down":
		if m.cursor < len(m.visibleSessions())-1 {
			m.cursor++
			m.updateSelected()
			m.ensureCursorVisible()
		}

	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
			m.updateSelected()
			m.ensureCursorVisible()
		}

	case "G":
		// G → go to bottom of list
		visible := m.visibleSessions()
		if len(visible) > 0 {
			m.cursor = len(visible) - 1
			m.updateSelected()
			m.ensureCursorVisible()
		}

	case "g":
		// First g press — wait for second g
		m.pendingG = true

	case "h", "left":
		m.focusDetail = false

	case "l", "right":
		m.focusDetail = true

	case "tab":
		if !m.focusDetail {
			// リストフォーカス時: 次の approve/answer 待ちセッションへジャンプして PTY 入力開始
			if idx := m.findNextAttentionSession(); idx >= 0 {
				m.cursor = idx
				m.updateSelected()
				m.ensureCursorVisible()
				m.focusDetail = true
				m.ptyInputActive = true
				m.syncLogViewport()
				return nil
			}
		}
		m.focusDetail = !m.focusDetail

	case "n":
		return m.startNewSession()

	case "enter", "i":
		// 生きた PTY プロセスあり → 詳細ペインに切り替えて PTY 直接入力モード開始
		debuglog.Printf("[key:%s] selectedID=%q hasActiveProcess=%v", key, m.selectedID, m.selectedID != "" && m.manager.HasActiveProcess(m.selectedID))
		if m.selectedID != "" && m.manager.HasActiveProcess(m.selectedID) {
			debuglog.Printf("[key:%s] activating PTY input mode", key)
			m.focusDetail = true
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

	case "d":
		m.pendingD = true

	case m.config.Keybinds.OpenTerm:
		return m.openTerminal()

	case "/":
		if !m.focusDetail {
			m.filterActive = true
			m.filterInput.Focus()
			return nil
		}

	case "esc":
		// フィルタ適用中 → フィルタ解除
		if m.filterText != "" {
			m.filterText = ""
			m.filterInput.SetValue("")
			// フィルタ解除後、カーソルを範囲内に収める
			visible := m.visibleSessions()
			if m.cursor >= len(visible) {
				m.cursor = max(0, len(visible)-1)
			}
			m.updateSelected()
			m.ensureCursorVisible()
			return nil
		}

	case "?":
		m.statusMsg = "h/l:ペイン切替 j/k:移動 gg/G:先頭/末尾 /:フィルタ C-b/C-u:半頁 n:新規 Enter/i:入力/再開 r:再開 f:フォーク t:ターミナル R:再描画 dd:削除 dD:deckのみ削除 x:終了 C-c:quit"
		return clearStatusCmd()
	}

	return nil
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
	m.focusDetail = false
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
	cols, _, rows := m.detailPaneMetrics()
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
	cols, _, rows := m.detailPaneMetrics()
	return func() tea.Msg {
		newSess, err := mgr.ForkSession(ctx, id, cols, rows)
		var newID string
		if newSess != nil {
			newID = newSess.ID
		}
		return sessionForkedMsg{sessionID: newID, err: err}
	}
}

// killSelected terminates the currently selected session.
func (m *Model) killSelected() tea.Cmd {
	if m.selectedID == "" {
		return nil
	}
	if err := m.manager.Kill(m.selectedID); err != nil {
		m.statusMsg = fmt.Sprintf("終了エラー: %v", err)
	} else {
		m.statusMsg = "セッションを終了しました"
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

// removeSelected removes the deck session metadata only (keeps Claude JSONL).
func (m *Model) removeSelected() tea.Cmd {
	if m.selectedID == "" {
		return nil
	}
	if err := m.manager.RemoveSession(m.selectedID); err != nil {
		m.statusMsg = fmt.Sprintf("削除エラー: %v", err)
	} else {
		m.statusMsg = "deckセッションを削除しました"
		m.refreshSessions()
	}
	return clearStatusCmd()
}

// deleteSelected removes the session including Claude Code JSONL files.
func (m *Model) deleteSelected() tea.Cmd {
	if m.selectedID == "" {
		return nil
	}
	warning, err := m.manager.DeleteSession(m.selectedID)
	if err != nil {
		m.statusMsg = fmt.Sprintf("削除エラー: %v", err)
	} else {
		if warning != "" {
			m.statusMsg = fmt.Sprintf("完全削除しました (%s)", warning)
		} else {
			m.statusMsg = "セッションを完全削除しました (JSONL含む)"
		}
		// selectedID を空にすると refreshSessions が末尾にカーソルを飛ばすため、
		// 削除済み ID をそのまま残す。refreshSessions がリストを再取得し、
		// 見つからない場合は cursor をクランプして隣のセッションを選択する。
		m.refreshSessions()
	}
	return clearStatusCmd()
}

