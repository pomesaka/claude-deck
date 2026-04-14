package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/ghostty"
	"github.com/pomesaka/claude-deck/internal/preview"
	"github.com/pomesaka/claude-deck/internal/ratelimits"
	"github.com/pomesaka/claude-deck/internal/session"
)


// viewMode determines what the TUI is currently showing.
type viewMode int

const (
	viewDashboard viewMode = iota
	viewSelectRepo
)

// TUIBackendMode describes how the TUI interacts with the process hosting environment.
// It replaces the separate tmuxMode/splitMode boolean pair, capturing the constraint
// that splitMode implies tmuxMode as a single value.
type TUIBackendMode int

const (
	// BackendModeEmbedded is the default: PTY processes are embedded in the TUI.
	// PTY input mode is available; detail pane shows live PTY output.
	BackendModeEmbedded TUIBackendMode = iota
	// BackendModeTmux hosts processes in tmux windows.
	// PTY input mode is disabled; session switching fires tmux select-window.
	BackendModeTmux
	// BackendModeSplit combines tmux hosting with Ghostty split layout:
	// the detail pane is hidden, the list takes full width, and cursor navigation
	// drives the right tmux pane (preview window or session window).
	BackendModeSplit
)

// IsTmuxLike reports whether the backend uses tmux for process hosting.
// True for both BackendModeTmux and BackendModeSplit.
func (m TUIBackendMode) IsTmuxLike() bool { return m != BackendModeEmbedded }

// IsSplit reports whether the TUI is in Ghostty split layout mode.
func (m TUIBackendMode) IsSplit() bool { return m == BackendModeSplit }

// ptyViewCache tracks which PTY display version was last rendered into the
// ptyViewport. Used to skip redundant SetContentLines calls, avoiding the
// expensive O(n) maxLineWidth scan over all scrollback lines.
// sessionID and version must always be updated atomically (via update) to
// prevent partial-update bugs — mirroring the renderCache key+result invariant.
//
// Placed in model.go (not logrender.go) because it is a PTY viewport tracking
// concern, independent of JSONL log rendering.
type ptyViewCache struct {
	sessionID session.DeckSessionID
	version   uint64
}

// isUpToDate returns true when the cached session and version match, meaning
// the ptyViewport content is already current and SetContentLines can be skipped.
//
// Edge case: a brand-new session starts with displayVersion==0 before its first
// paint. A stale cache entry for the same sessionID would also have version==0
// (from a prior session that was deleted before ever painting). This is harmless
// because ptyViewCache.sessionID changes when the selected session changes, so
// isUpToDate returns false on every session switch regardless of version.
func (c ptyViewCache) isUpToDate(sid session.DeckSessionID, ver uint64) bool {
	return c.sessionID == sid && c.version == ver
}

// update records the session and version after SetContentLines has been called.
func (c *ptyViewCache) update(sid session.DeckSessionID, ver uint64) {
	c.sessionID = sid
	c.version = ver
}

// ptyAtomicView is the read-only PTY handle exposed to View() for the currently
// selected session. It wraps *session.Session but exposes only lock-free atomic
// accessors — methods that acquire session.mu, session.rt.mu, or display.emuMu
// must NOT appear here.
//
// This structural constraint prevents callers from accidentally reaching locked
// Session methods through the View() path, which would reintroduce the convoy
// effect that causes TUI freezes during rapid mouse scroll.
type ptyAtomicView struct {
	sess *session.Session
}

func (v *ptyAtomicView) GetPTYCursorPosition() (int, int) { return v.sess.GetPTYCursorPosition() }
func (v *ptyAtomicView) GetDisplayVersion() uint64        { return v.sess.GetDisplayVersion() }
func (v *ptyAtomicView) GetPTYDisplayLines() []string     { return v.sess.GetPTYDisplayLines() }

// Model is the Bubble Tea model for the TUI.
type Model struct {
	manager     *session.Manager
	config      *config.Config
	ghostty     *ghostty.Launcher
	ctx         context.Context
	backendMode TUIBackendMode

	width  int
	height int

	// Dashboard state
	sessions     []*session.Session
	cursor       int
	scrollOffset int
	selectedID  session.DeckSessionID
	layout      Layout
	logViewport viewport.Model
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

	// Quit confirmation
	confirmQuit bool

	// Vim-style key sequence state
	pendingG        bool
	pendingD        bool
	logFollow       bool // ログビューポート末尾追従モード
	ptyFollow       bool // PTY ビューポート末尾追従モード
	refreshInterval time.Duration
	lastResizeCols  int // 前回 ResizeSession に渡した幅
	lastResizeRows  int // 前回 ResizeSession に渡した高さ
	lastResizeID    session.DeckSessionID // 前回 ResizeSession に渡したセッション ID

	// Log rendering cache (JSONL structured logs)
	logCache renderCache

	// PTY viewport content tracking — skips SetContentLines when display
	// version has not changed since the last call, avoiding expensive
	// maxLineWidth O(n) computation over all scrollback lines.
	ptyViewCache ptyViewCache

	// Pre-computed view data — populated in Update(), read-only in View().
	// Eliminates all Snapshot() / GetSession() calls from the View() path,
	// making View() lock-free. Without this, View() acquires ~40-60 RLocks per
	// frame, contending with PTY writer goroutines that hold write locks for
	// OSC title updates (~12 Hz) and causing the convoy effect that freezes
	// the TUI during rapid mouse scroll.
	// viewSnaps[i] is a snapshot of visibleSessions()[i] and shares the same
	// index space as m.cursor. Sorting or filtering changes must always be
	// followed by refreshViewData() to keep this invariant.
	viewSnaps    []session.Snapshot
	selectedSnap *session.Snapshot // snapshot for the selected session (nil = no selection)
	// selectedPTY provides lock-free PTY atomic access for View() rendering.
	// It exposes only atomic accessors (GetPTYCursorPosition, GetDisplayVersion,
	// GetPTYDisplayLines) — methods that acquire any lock must NOT be called through it.
	// See ptyAtomicView for why a wrapper type is used instead of *session.Session.
	selectedPTY    *ptyAtomicView
	attentionCount int // sessions with Status.NeedsAttention() == true

	quitting bool

	// rate limits data from Claude Code statusline (Pro/Max subscribers only)
	rateLimitsStatus ratelimits.Status
}

// SessionRefreshMsg triggers a session list refresh.
// Manager の onChange コールバックからも送信されるためエクスポート。
// ChangedIDs が空の場合はブロードキャスト（全セッション更新）。
// 非空の場合は指定されたセッションのみ変更された。
type SessionRefreshMsg struct {
	ChangedIDs map[session.DeckSessionID]bool
}

// statusClearMsg clears the status message.
type statusClearMsg struct{}


// sessionCreatedMsg is sent when an async session creation completes.
type sessionCreatedMsg struct {
	sessionID session.DeckSessionID
	err       error
}

// sessionResumedMsg is sent when an async session resume completes.
type sessionResumedMsg struct {
	err error
}

// sessionForkedMsg is sent when an async session fork completes.
type sessionForkedMsg struct {
	sessionID session.DeckSessionID
	err       error
}

// ptyInputSentMsg is sent when PTY input write completes.
type ptyInputSentMsg struct {
	err error
}

// NewModel creates the initial TUI model.
// ModelOptions configures optional behaviour for the main list TUI.
type ModelOptions struct {
	// SplitMode puts the TUI into list-only mode: the detail pane is hidden and
	// cursor navigation drives the right tmux pane instead.
	// Automatically enabled when tmux mode is active and Ghostty is detected.
	SplitMode bool
}

func NewModel(mgr *session.Manager, cfg *config.Config, ctx context.Context, opts ...ModelOptions) Model {
	var opt ModelOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
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

	var backendMode TUIBackendMode
	if opt.SplitMode {
		backendMode = BackendModeSplit
	} else if mgr.IsTmuxMode() {
		backendMode = BackendModeTmux
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
		backendMode:     backendMode,
	}

	m.refreshSessions() // cmds discarded — bubbletea event loop not yet running
	// カーソル初期位置を最下部（最新セッション）に設定
	if len(m.sessions) > 0 {
		m.cursor = len(m.sessions) - 1
		m.updateSelected() // cmds discarded — bubbletea event loop not yet running
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

// RateLimitsUpdatedMsg is sent when the rate-limits.json file is updated by the
// claude-deck statusline script. main.go injects this via p.Send().
type RateLimitsUpdatedMsg struct {
	Status ratelimits.Status
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

	case tea.PasteMsg:
		if m.ptyInputActive && msg.Content != "" {
			data := []byte(msg.Content)
			mgr := m.manager
			sid := m.selectedID
			cmds = append(cmds, func() tea.Msg {
				err := mgr.WriteToSession(sid, data)
				return ptyInputSentMsg{err: err}
			})
		}

	case tea.MouseWheelMsg:
		if m.layout.IsDetailFocused() && m.mode == viewDashboard {
			var cmd tea.Cmd
			if m.selectedDisplayChannel() == session.DisplayPTY {
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
		debuglog.Printf("[tui] SessionRefreshMsg received changedIDs=%d", len(msg.ChangedIDs))
		cmds = append(cmds, m.refreshSessions()...)
		// 選択中のセッションが変更対象に含まれるか、ブロードキャストの場合のみ viewport 更新
		if len(msg.ChangedIDs) == 0 || msg.ChangedIDs[m.selectedID] {
			m.syncLogViewport()
		}

	case metadataTickMsg:
		debuglog.Printf("[tui] metadataTickMsg (event loop alive)")
		go m.manager.RefreshFromJSONL()
		cmds = append(cmds, metadataTickCmd(m.refreshInterval))

	case RateLimitsUpdatedMsg:
		m.rateLimitsStatus = msg.Status

	case sessionCreatedMsg:
		if msg.err != nil {
			m.statusMsg = "セッション作成エラー: " + msg.err.Error()
		} else {
			m.statusMsg = "新規セッションを作成しました"
			m.selectedID = msg.sessionID
			cmds = append(cmds, m.refreshSessions()...)
			m.layout.FocusDetail()
			if !m.backendMode.IsTmuxLike() {
				m.ptyInputActive = true
			} else {
				// m.selectedID を refreshSessions() 前に設定するため updateSelected() で
				// idChanged=false になる。switchRightPane で明示的に右ペインを切替する。
				cmds = append(cmds, m.switchRightPane(msg.sessionID, true))
			}
		}
		cmds = append(cmds, clearStatusCmd())

	case sessionResumedMsg:
		if msg.err != nil {
			m.statusMsg = "再開エラー: " + msg.err.Error()
		} else {
			m.statusMsg = "セッションを再開しました"
			m.layout.FocusDetail()
			if !m.backendMode.IsTmuxLike() {
				m.ptyInputActive = true
			} else {
				// ResumeSession が完了した時点でセッションは managed=true になっており
				// DisplayNone に遷移済み。switchRightPane が FocusSession + FocusRight を発行する。
				cmds = append(cmds, m.switchRightPane(m.selectedID, m.backendMode.IsSplit()))
			}
		}
		cmds = append(cmds, clearStatusCmd())

	case sessionForkedMsg:
		if msg.err != nil {
			m.statusMsg = "フォークエラー: " + msg.err.Error()
		} else {
			m.statusMsg = "セッションをフォークしました"
			m.selectedID = msg.sessionID
			m.refreshSessions()
			m.layout.FocusDetail()
			if !m.backendMode.IsTmuxLike() {
				m.ptyInputActive = true
			} else {
				// sessionCreatedMsg と同じ理由で明示的に切替する。
				cmds = append(cmds, m.switchRightPane(msg.sessionID, true))
			}
		}
		cmds = append(cmds, clearStatusCmd())

	case ptyInputSentMsg:
		debuglog.Printf("[tui] ptyInputSentMsg err=%v", msg.err)
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
		// KeyPressMsg は handleRepoSelectKey で処理済み。
		// カスタムメッセージを list.Model に渡すとフィルタ状態がリセットされるため、
		// bubbletea 内部メッセージ（WindowSize, マウス, カーソル点滅等）のみ委譲する。
		switch msg.(type) {
		case tea.KeyPressMsg,
			metadataTickMsg, SessionRefreshMsg, statusClearMsg,
			sessionCreatedMsg, sessionResumedMsg, sessionForkedMsg, ptyInputSentMsg, repoListMsg,
			RateLimitsUpdatedMsg:
			// skip: 処理済み or repoList に無関係
		default:
			var cmd tea.Cmd
			m.repoList, cmd = m.repoList.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func clearStatusCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return statusClearMsg{}
	})
}


func (m *Model) refreshSessions() []tea.Cmd {
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
	cmds := m.updateSelected()
	m.ensureCursorVisible()
	// refreshViewData は updateSelected 内で選択が変わった場合だけ部分更新される。
	// ここで全体を更新してセッションリスト・attention count も同期させる。
	m.refreshViewData()
	return cmds
}

// refreshViewData pre-computes all session data that View() needs.
// Called from Update() whenever session data changes (SessionRefreshMsg etc.).
// By caching Snapshot values here (with locks, in Update goroutine),
// View() can read plain struct fields without acquiring any locks.
//
// Invariant: viewSnaps[i] == visibleSessions()[i].Snapshot(), so viewSnaps
// shares the same index space as m.cursor. Always call refreshViewData() after
// any change to session order, filter text, or the visible session set.
//
// Note: selectedSnap/selectedPTY are updated by updateSelected() → refreshSelectedSnap()
// whenever the selection changes. refreshViewData() intentionally does NOT call
// refreshSelectedSnap() again to avoid the double-call when coming through
// refreshSessions() → updateSelected() → refreshSelectedSnap() → refreshViewData().
func (m *Model) refreshViewData() {
	// attention count — iterate all sessions
	count := 0
	for _, s := range m.sessions {
		if s.Snapshot().Status.NeedsAttention() {
			count++
		}
	}
	m.attentionCount = count

	// snapshots for visible sessions — shares index space with m.cursor
	visible := m.visibleSessions()
	snaps := make([]session.Snapshot, len(visible))
	for i, s := range visible {
		snaps[i] = s.Snapshot()
	}
	m.viewSnaps = snaps
}

// refreshSelectedSnap updates selectedSnap and selectedPTY for the current selectedID.
// Called from updateSelected() on cursor movement — a fast partial update that avoids
// re-scanning all sessions (unlike the full refreshViewData).
func (m *Model) refreshSelectedSnap() {
	if m.selectedID != "" {
		if sess := m.manager.GetSession(m.selectedID); sess != nil {
			snap := sess.Snapshot()
			m.selectedSnap = &snap
			m.selectedPTY = &ptyAtomicView{sess: sess}
			return
		}
	}
	m.selectedSnap = nil
	m.selectedPTY = nil
}

// selectedDisplayChannel returns the DisplayChannel for the currently selected session.
// Uses the pre-computed selectedSnap to avoid lock acquisition in the Update path.
func (m *Model) selectedDisplayChannel() session.DisplayChannel {
	if m.selectedSnap != nil {
		return m.selectedSnap.Display
	}
	return session.DisplayJSONL
}

// visibleSessions returns sessions filtered by filterText.
// filterText が空なら全セッション、非空なら RepoPath/Name に対して case-insensitive 部分一致でフィルタ。
// Note: this is called from refreshViewData() (Update path, locks acceptable).
// View() uses m.viewSnaps instead and never calls this directly.
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
		// Session.MatchesFilter takes a targeted RLock on RepoPath+Name only,
		// avoiding a full Snapshot() call just for filter matching.
		if s.MatchesFilter(lower) {
			result = append(result, s)
		}
	}
	return result
}

// updateSelected updates m.selectedID based on the current cursor position, triggers
// JSONL streaming, and returns any tea.Cmd for right-pane switching.
// The caller must append the returned cmds to its own cmd batch.
func (m *Model) updateSelected() []tea.Cmd {
	oldID := m.selectedID
	oldDisplay := session.DisplayJSONL
	if m.selectedSnap != nil {
		oldDisplay = m.selectedSnap.Display
	}

	visible := m.visibleSessions()
	if m.cursor >= 0 && m.cursor < len(visible) {
		m.selectedID = visible[m.cursor].ID
	} else {
		m.selectedID = ""
	}

	idChanged := m.selectedID != oldID
	if idChanged {
		m.logFollow = true
		m.ptyFollow = true
		// 選択中のセッションだけ JSONL ストリーミングを開始
		m.manager.StreamSession(m.selectedID)
	}
	// 常に最新 snap を取得する。選択 ID が同じでも display channel が変わる場合
	// （kill → Completed: DisplayPTY/DisplayNone → DisplayJSONL）があるため。
	m.refreshSelectedSnap()
	if idChanged {
		m.syncLogViewport()
	}

	if m.selectedID == "" {
		return nil
	}

	sid := m.selectedID
	snap := m.selectedSnap

	newDisplay := session.DisplayJSONL
	if snap != nil {
		newDisplay = snap.Display
	}

	// DisplayJSONL に遷移したとき（同一セッションのまま kill → Completed など）は
	// StreamSession を再トリガーする。idChanged=true のケースは上で既に呼んでいるので
	// idChanged=false のケースだけ対象にする。
	// 理由: idChanged=true のときに StreamSession を呼んでいる段階では selectedSnap が
	// まだ更新前（refreshSelectedSnap より前）なので、display 遷移は判定できない。
	// また logFollow を true にして最新ログへ自動スクロールさせる。
	if !idChanged && newDisplay == session.DisplayJSONL && oldDisplay != session.DisplayJSONL {
		m.logFollow = true
		m.manager.StreamSession(sid)
	}

	// tmux/split mode: カーソル移動(idChanged)またはセッション状態変化(display変化)で
	// 右ペインを最新の状態に同期する。focusRight=false なのでカーソル移動では
	// Ghostty のペインフォーカスは移動しない（ユーザーはリストを閲覧中）。
	if m.backendMode.IsTmuxLike() && (idChanged || (m.backendMode.IsSplit() && newDisplay != oldDisplay)) {
		return []tea.Cmd{m.switchRightPane(sid, false)}
	}
	return nil
}

// doSwitchRightPane は右ペイン制御のコアロジック。goroutine または tea.Cmd から呼ぶ。
//
// focusRight の意味でペイン切替先を決定する:
//
//   - focusRight=false (カーソル移動): 常に __preview__ (JSONL viewer) を表示する。
//     稼働中・停止中を問わず、マウスホイールで会話履歴をスクロールして見られる。
//
//   - focusRight=true (Enter/新規/再開/fork) かつ稼働中 (DisplayNone): ライブ端末を表示し
//     Ghostty のフォーカスを右ペインに移す（Claude Code と直接対話するため）。
//
//   - focusRight=true かつ非稼働: __preview__ を表示（ライブ端末が存在しない）。
//
// split でない tmux mode では __preview__ が存在しないため、稼働中セッションのみ
// tmux select-window を発行する（従来通り）。
func (m *Model) doSwitchRightPane(sid session.DeckSessionID, focusRight bool) {
	if !m.backendMode.IsTmuxLike() {
		return
	}

	var display session.DisplayChannel
	if sess := m.manager.GetSession(sid); sess != nil {
		display = sess.Snapshot().Display
	}

	if m.backendMode.IsSplit() {
		if display == session.DisplayNone {
			// 稼働中セッション: カーソル移動・明示操作いずれも実際の tmux ウィンドウを表示。
			// __preview__ は DisplayNone の場合にヘッダー数行のみとなり大部分が黒くなるため。
			// focusRight=true（Enter/新規/再開）のときのみ Ghostty フォーカスを右ペインに移す。
			_ = m.manager.FocusSession(sid)
			if focusRight {
				_ = ghostty.FocusRight()
			}
			return
		}
		// 非稼働セッション → JSONL プレビュー（会話履歴をスクロールして見られる）
		if err := preview.WriteSelection(m.config.DataDir, sid); err != nil {
			debuglog.Printf("[switchRightPane] preview IPC: %v", err)
		}
		_ = m.manager.FocusPreviewWindow()
		if focusRight {
			_ = ghostty.FocusRight()
		}
		return
	}

	// split なし tmux mode: 稼働中セッションのウィンドウに切替
	if display == session.DisplayNone {
		_ = m.manager.FocusSession(sid)
	}
}

// switchRightPane は doSwitchRightPane を tea.Cmd として返す。
// Update() のメッセージハンドラから発行する明示的なユーザー操作（Enter/n/r/f）に使う。
func (m *Model) switchRightPane(sid session.DeckSessionID, focusRight bool) tea.Cmd {
	if !m.backendMode.IsTmuxLike() {
		return nil
	}
	return func() tea.Msg {
		m.doSwitchRightPane(sid, focusRight)
		return nil
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
