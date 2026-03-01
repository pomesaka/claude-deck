package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/sandbox/claude-deck/internal/config"
	"github.com/pomesaka/sandbox/claude-deck/internal/debuglog"
	"github.com/pomesaka/sandbox/claude-deck/internal/ghostty"
	"github.com/pomesaka/sandbox/claude-deck/internal/session"
)


// viewMode determines what the TUI is currently showing.
type viewMode int

const (
	viewDashboard viewMode = iota
	viewSelectRepo
)

// Model is the Bubble Tea model for the TUI.
type Model struct {
	manager *session.Manager
	config  *config.Config
	ghostty *ghostty.Launcher
	ctx     context.Context

	width  int
	height int

	// Dashboard state
	sessions     []*session.Session
	cursor       int
	scrollOffset int
	selectedID   string
	focusDetail  bool
	logViewport  viewport.Model
	ptyViewport  viewport.Model // PTY リアルタイム出力用

	// View state
	mode           viewMode
	repoList       list.Model
	ptyInputActive bool // PTY 直接入力モード中（キーイベントを PTY に転送）

	// Session filter
	filterInput  textinput.Model
	filterActive bool   // フィルタ入力中
	filterText   string // 確定済みフィルタ

	// Status bar
	statusMsg string

	// Vim-style key sequence state
	pendingG        bool
	pendingD        bool
	logFollow       bool // ログビューポート末尾追従モード
	ptyFollow       bool // PTY ビューポート末尾追従モード
	refreshInterval time.Duration

	// Log rendering cache (JSONL structured logs)
	logCache renderCache

	quitting bool
}

// SessionRefreshMsg triggers a session list refresh.
// Manager の onChange コールバックからも送信されるためエクスポート。
type SessionRefreshMsg struct{}

// statusClearMsg clears the status message.
type statusClearMsg struct{}

// sessionCreatedMsg is sent when an async session creation completes.
type sessionCreatedMsg struct {
	sessionID string
	err       error
}

// sessionResumedMsg is sent when an async session resume completes.
type sessionResumedMsg struct {
	err error
}

// ptyInputSentMsg is sent when PTY input write completes.
type ptyInputSentMsg struct {
	err error
}

// NewModel creates the initial TUI model.
func NewModel(mgr *session.Manager, cfg *config.Config, ctx context.Context) Model {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(10))
	vp.SetContent("")

	pvp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(3))
	pvp.SetContent("")

	delegate := newRepoDelegate()
	rl := list.New(nil, delegate, 80, 24)
	rl.Title = "リポジトリ選択"
	rl.SetShowStatusBar(true)
	rl.SetFilteringEnabled(true)
	rl.SetStatusBarItemName("repo", "repos")
	rl.DisableQuitKeybindings()

	// 最初から Filtering 状態にするので、自前で Enter/Esc を処理する。
	// list.Model 側のフィルタ関連キーバインドを無効化。
	rl.KeyMap.AcceptWhileFiltering.SetEnabled(false)
	rl.KeyMap.CancelWhileFiltering.SetEnabled(false)
	rl.KeyMap.Filter.SetEnabled(false)

	fi := textinput.New()
	fi.Prompt = "/ "
	fi.Placeholder = "filter..."

	refreshInterval, _ := time.ParseDuration(cfg.Session.RefreshInterval)
	if refreshInterval <= 0 {
		refreshInterval = 5 * time.Second
	}

	m := Model{
		manager:         mgr,
		config:          cfg,
		ghostty:         ghostty.NewLauncher(cfg.Ghostty.Command),
		ctx:             ctx,
		repoList:        rl,
		logViewport:     vp,
		ptyViewport:     pvp,
		filterInput:     fi,
		logFollow:       true,
		ptyFollow:       true,
		refreshInterval: refreshInterval,
	}

	m.refreshSessions()
	// カーソル初期位置を最下部（最新セッション）に設定
	if len(m.sessions) > 0 {
		m.cursor = len(m.sessions) - 1
		m.updateSelected()
		m.ensureCursorVisible()
	}
	return m
}

// metadataTickMsg triggers periodic JSONL metadata refresh (low frequency).
type metadataTickMsg struct{}

func metadataTickCmd(interval time.Duration) tea.Cmd {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return metadataTickMsg{}
	})
}

// Init returns the initial command.
func (m Model) Init() tea.Cmd {
	return metadataTickCmd(m.refreshInterval)
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		cmd := m.handleKey(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case tea.MouseWheelMsg:
		if m.focusDetail && m.mode == viewDashboard {
			var cmd tea.Cmd
			if m.selectedID != "" && m.manager.HasActiveProcess(m.selectedID) {
				m.ptyViewport, cmd = m.ptyViewport.Update(msg)
				m.ptyFollow = m.ptyViewport.AtBottom()
			} else {
				m.logViewport, cmd = m.logViewport.Update(msg)
				m.logFollow = m.logViewport.AtBottom()
			}
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// repoList はヘッダー(1) + フッター(1) を除いた全画面サイズを使う
		m.repoList.SetSize(msg.Width, msg.Height-2)
		m.ensureCursorVisible()
		m.syncLogViewport()

	case SessionRefreshMsg:
		debuglog.Printf("[tui] SessionRefreshMsg received")
		m.refreshSessions()
		m.syncLogViewport()

	case metadataTickMsg:
		go m.manager.RefreshFromJSONL()
		cmds = append(cmds, metadataTickCmd(m.refreshInterval))

	case sessionCreatedMsg:
		if msg.err != nil {
			m.statusMsg = "セッション作成エラー: " + msg.err.Error()
		} else {
			m.statusMsg = "新規セッションを作成しました"
			m.selectedID = msg.sessionID
			m.refreshSessions()
			m.focusDetail = true
			m.ptyInputActive = true
		}
		cmds = append(cmds, clearStatusCmd())

	case sessionResumedMsg:
		if msg.err != nil {
			m.statusMsg = "再開エラー: " + msg.err.Error()
		} else {
			m.statusMsg = "セッションを再開しました"
			m.focusDetail = true
			m.ptyInputActive = true
		}
		cmds = append(cmds, clearStatusCmd())

	case ptyInputSentMsg:
		if msg.err != nil {
			m.statusMsg = "PTY送信エラー: " + msg.err.Error()
			cmds = append(cmds, clearStatusCmd())
		}

	case repoListMsg:
		if msg.err != nil {
			m.statusMsg = "リポジトリ検索エラー: " + msg.err.Error()
			m.mode = viewDashboard
			cmds = append(cmds, clearStatusCmd())
		} else {
			items := make([]list.Item, len(msg.repos))
			for i, r := range msg.repos {
				items[i] = repoItem(r)
			}
			m.repoList.SetItems(items)
			// SetFilterText で filteredItems を全アイテムで同期的に初期化してから、
			// SetFilterState(Filtering) で入力モードに切り替える。
			// （SetFilterState 単体では filteredItems が空のままになるため）
			m.repoList.SetFilterText("")
			m.repoList.SetFilterState(list.Filtering)
			m.statusMsg = ""
		}

	case statusClearMsg:
		m.statusMsg = ""
	}

	// filterActive 中のキー入力は handleDashboardKey 内で filterInput.Update を呼ぶ。
	// ここではカーソル点滅等の非キーメッセージのみ textinput に渡す。
	if m.filterActive {
		if _, isKey := msg.(tea.KeyPressMsg); !isKey {
			var cmd tea.Cmd
			m.filterInput, cmd = m.filterInput.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	if m.mode == viewSelectRepo {
		var cmd tea.Cmd
		m.repoList, cmd = m.repoList.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func clearStatusCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return statusClearMsg{}
	})
}

func (m *Model) refreshSessions() {
	m.sessions = m.manager.ListSessions()

	visible := m.visibleSessions()

	// リスト再ソート後、選択中のセッションにカーソルを追従させる
	if m.selectedID != "" {
		for i, s := range visible {
			if s.ID == m.selectedID {
				m.cursor = i
				break
			}
		}
	} else if len(visible) > 0 {
		// 初期表示: 一番下（最新）のセッションにカーソルを置く
		m.cursor = len(visible) - 1
	}

	if m.cursor >= len(visible) {
		m.cursor = max(0, len(visible)-1)
	}
	m.updateSelected()
	m.ensureCursorVisible()
}

// visibleSessions returns sessions filtered by filterText.
// filterText が空なら全セッション、非空なら RepoName/Name に対して case-insensitive 部分一致でフィルタ。
func (m *Model) visibleSessions() []*session.Session {
	ft := m.filterText
	if m.filterActive {
		ft = m.filterInput.Value()
	}
	if ft == "" {
		return m.sessions
	}
	lower := strings.ToLower(ft)
	var result []*session.Session
	for _, s := range m.sessions {
		snap := s.Snapshot()
		target := strings.ToLower(snap.RepoPath + "/" + snap.Name)
		if strings.Contains(target, lower) {
			result = append(result, s)
		}
	}
	return result
}

func (m *Model) updateSelected() {
	oldID := m.selectedID
	visible := m.visibleSessions()
	if m.cursor >= 0 && m.cursor < len(visible) {
		m.selectedID = visible[m.cursor].ID
	} else {
		m.selectedID = ""
	}
	if m.selectedID != oldID {
		m.logFollow = true
		m.ptyFollow = true
		// 選択中のセッションだけ JSONL ストリーミングを開始
		m.manager.StreamSession(m.selectedID)
		m.syncLogViewport()
	}
}

// ensureCursorVisible adjusts scrollOffset so the cursor is within the visible window.
func (m *Model) ensureCursorVisible() {
	contentHeight := m.height - 4
	if contentHeight < 3 {
		contentHeight = 3
	}
	// フィルタバー表示中は1行分差し引く
	if m.filterActive || m.filterText != "" {
		contentHeight--
	}
	const itemHeight = 2
	// インジケータ分(上下各1行)を控えめに確保
	visibleCount := (contentHeight - 2) / itemHeight
	if visibleCount < 1 {
		visibleCount = 1
	}

	if m.cursor < m.scrollOffset {
		m.scrollOffset = m.cursor
	}
	if m.cursor >= m.scrollOffset+visibleCount {
		m.scrollOffset = m.cursor - visibleCount + 1
	}
	// ウィンドウが広がったときに不要な空白を残さない
	maxScroll := len(m.visibleSessions()) - visibleCount
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scrollOffset > maxScroll {
		m.scrollOffset = maxScroll
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
}
