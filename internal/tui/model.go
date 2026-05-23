package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
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

// BackendMode describes the TUI layout mode.
// Kept as an interface / extension point for future backend implementations.
type BackendMode int

const (
	// BackendModeSplit combines tmux hosting with Ghostty split layout:
	// the list takes full width and cursor navigation drives the right tmux pane
	// (preview window or session window).
	BackendModeSplit BackendMode = iota
)

// IsSplit reports whether the TUI is in Ghostty split layout mode.
func (m BackendMode) IsSplit() bool { return m == BackendModeSplit }

// Model is the Bubble Tea model for the TUI.
type Model struct {
	manager     *session.Manager
	config      *config.Config
	ghostty     *ghostty.Launcher
	ctx         context.Context
	backendMode BackendMode

	width  int
	height int

	// Dashboard state
	sessions     []*session.Session
	cursor       int
	scrollOffset int
	selectedID   session.DeckSessionID

	// View state
	mode     viewMode
	repoList list.Model

	// Session filter
	filterInput  textinput.Model
	filterActive bool   // フィルタ入力中
	filterText   string // 確定済みフィルタ

	// Status bar
	statusMsg string

	// Quit state
	confirmQuit bool
	quitting    bool

	// In-flight async operation flags — prevent duplicate key presses from
	// launching concurrent operations while a background Cmd is running.
	killing bool // true while sessionKilledMsg is in flight

	// Vim-style key sequence state
	pendingG        bool
	refreshInterval time.Duration

	// Pre-computed view data — populated in Update(), read-only in View().
	// Eliminates all Snapshot() / GetSession() calls from the View() path,
	// making View() lock-free.
	// viewSnaps[i] is a snapshot of visibleSessions()[i] and shares the same
	// index space as m.cursor. Sorting or filtering changes must always be
	// followed by refreshViewData() to keep this invariant.
	viewSnaps      []session.Snapshot
	selectedSnap   *session.Snapshot // snapshot for the selected session (nil = no selection)
	attentionCount int               // sessions with Status.NeedsAttention() == true

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

// sessionKilledMsg is sent when an async session kill completes.
// No sessionID field needed: kill does not create a new session.
type sessionKilledMsg struct {
	err error
}

// NewModel creates the initial TUI model.
// ModelOptions configures optional behaviour for the main list TUI.
type ModelOptions struct {
	// SplitMode は将来の拡張ポイントとして保持。現在は非 Split レイアウトの実装を削除済みのため、
	// true/false いずれも BackendModeSplit として動作する。
	// main.go は Ghostty 検出結果を渡しており、将来の別 backend 追加時に参照される。
	SplitMode bool
}

func NewModel(mgr *session.Manager, cfg *config.Config, ctx context.Context, opt ModelOptions) Model {
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

	// opt.SplitMode は将来の拡張ポイント。現在は非 Split 実装を削除済みのため
	// 値に関わらず BackendModeSplit を使用する。
	_ = opt.SplitMode
	m := Model{
		manager:         mgr,
		config:          cfg,
		ghostty:         ghostty.NewLauncher(cfg.Ghostty.Command),
		ctx:             ctx,
		repoList:        rl,
		filterInput:     fi,
		refreshInterval: refreshInterval,
		backendMode:     BackendModeSplit,
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

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// repoList はヘッダー(1) + フッター(1) を除いた全画面サイズを使う
		m.repoList.SetSize(msg.Width, msg.Height-2)
		m.ensureCursorVisible()

	case SessionRefreshMsg:
		debuglog.Printf("[tui] SessionRefreshMsg received changedIDs=%d", len(msg.ChangedIDs))
		cmds = append(cmds, m.refreshSessions()...)
		// 選択中セッションの状態が変わった場合、preview spec を最新化する。
		// kill 時の DisplayTmux → DisplayJSONL 遷移などがリアルタイムに preview に届く。
		if m.selectedID != "" {
			if len(msg.ChangedIDs) == 0 || msg.ChangedIDs[m.selectedID] {
				if spec := m.buildPreviewSpec(); spec.Display != "" {
					if err := preview.WriteSpec(m.config.DataDir, spec); err != nil {
						debuglog.Printf("[previewSpec] write: %v", err)
					}
				}
			}
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
			// m.selectedID を refreshSessions() 前に設定するため updateSelected() で
			// idChanged=false になる。switchRightPane で明示的に右ペインを切替する。
			cmds = append(cmds, m.switchRightPane(msg.sessionID, true))
		}
		cmds = append(cmds, clearStatusCmd())

	case sessionResumedMsg:
		if msg.err != nil {
			m.statusMsg = "再開エラー: " + msg.err.Error()
		} else {
			m.statusMsg = "セッションを再開しました"
			// ResumeSession が完了した時点でセッションは managed=true になっており
			// DisplayTmux に遷移済み。switchRightPane が FocusSession + FocusRight を発行する。
			cmds = append(cmds, m.switchRightPane(m.selectedID, true))
		}
		cmds = append(cmds, clearStatusCmd())

	case sessionForkedMsg:
		if msg.err != nil {
			m.statusMsg = "フォークエラー: " + msg.err.Error()
		} else {
			m.statusMsg = "セッションをフォークしました"
			m.selectedID = msg.sessionID
			cmds = append(cmds, m.refreshSessions()...)
			// sessionCreatedMsg と同じ理由で明示的に切替する。
			cmds = append(cmds, m.switchRightPane(msg.sessionID, true))
		}
		cmds = append(cmds, clearStatusCmd())

	case sessionKilledMsg:
		m.killing = false
		if msg.err != nil {
			// Kill() が StopProcess エラーで早期 return した場合、notifyChange は呼ばれず
			// SessionRefreshMsg も届かない。セッション状態は変化していないため refreshSessions は不要。
			m.statusMsg = "終了エラー: " + msg.err.Error()
		} else {
			m.statusMsg = "セッションを終了しました (workspace削除済)"
			// Kill() 末尾の notifyChange がデバウンスループ経由で SessionRefreshMsg を発行するため
			// refreshSessions の明示呼び出しは不要。
		}
		cmds = append(cmds, clearStatusCmd())

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
			sessionCreatedMsg, sessionResumedMsg, sessionForkedMsg, sessionKilledMsg, repoListMsg,
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
// Call order: must be called AFTER updateSelected() so that m.cursor and
// m.selectedID are already final — viewSnaps is indexed by m.cursor.
//
// selectedSnap is maintained separately by updateSelected() → refreshSelectedSnap().
// refreshViewData() does NOT update selectedSnap to avoid a redundant Snapshot()
// call in the refreshSessions() → updateSelected() → refreshSelectedSnap() → refreshViewData() path.
func (m *Model) refreshViewData() {
	// attention count — scans all sessions (not just visible) so the indicator
	// reflects the global state even when a filter is active.
	count := 0
	for _, s := range m.sessions {
		if s.GetStatus().NeedsAttention() {
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

// refreshSelectedSnap updates selectedSnap for the current selectedID.
// Called from updateSelected() on cursor movement — a fast partial update that avoids
// re-scanning all sessions (unlike the full refreshViewData).
func (m *Model) refreshSelectedSnap() {
	if m.selectedID != "" {
		if sess := m.manager.GetSession(m.selectedID); sess != nil {
			snap := sess.Snapshot()
			m.selectedSnap = &snap
			return
		}
	}
	m.selectedSnap = nil
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
		// 選択中のセッションだけ JSONL ストリーミングを開始
		m.manager.StreamSession(m.selectedID)
	}
	// 常に最新 snap を取得する。選択 ID が同じでも display channel が変わる場合
	// （kill → Completed: DisplayTmux → DisplayJSONL）があるため。
	m.refreshSelectedSnap()

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
	if !idChanged && newDisplay == session.DisplayJSONL && oldDisplay != session.DisplayJSONL {
		m.manager.StreamSession(sid)
	}

	// カーソル移動(idChanged)またはセッション状態変化(display変化)で
	// 右ペインを最新の状態に同期する。focusRight=false なのでカーソル移動では
	// Ghostty のペインフォーカスは移動しない（ユーザーはリストを閲覧中）。
	if idChanged || newDisplay != oldDisplay {
		return []tea.Cmd{m.switchRightPane(sid, false)}
	}
	return nil
}

// buildPreviewSpec builds a PreviewSpec from the current selection for IPC to the
// preview subprocess. Returns an empty spec (Display=="") when there is no selection.
func (m *Model) buildPreviewSpec() preview.PreviewSpec {
	if m.selectedSnap == nil {
		return preview.PreviewSpec{}
	}
	return m.buildPreviewSpecFromSnap(*m.selectedSnap)
}

// buildPreviewSpecFromSnap builds a PreviewSpec from the given snapshot.
// JSONL path resolution happens here so the preview subprocess needs no Manager.
func (m *Model) buildPreviewSpecFromSnap(snap session.Snapshot) preview.PreviewSpec {
	jsonlPath, priorPaths := m.manager.ResolveJSONLPaths(snap.ID)
	return preview.PreviewSpec{
		DeckSessionID:    snap.ID,
		Name:             snap.Name,
		RepoName:         snap.RepoName,
		WorkspacePath:    snap.WorkspacePath,
		RuntimeSessionID: snap.RuntimeSessionID,
		PriorRuntimeIDs:  snap.PriorRuntimeIDs,
		ClaudeSessionID:  snap.RuntimeSessionID,
		PriorClaudeIDs:   snap.PriorRuntimeIDs,
		ClearCount:       snap.ClearCount,
		Status:           snap.Status.ID(),
		Display:          snap.Display.String(),
		CurrentTool:      snap.CurrentTool,
		ErrorMessage:     snap.ErrorMessage,
		NeedsAttention:   snap.Status.NeedsAttention(),
		JSONLPath:        jsonlPath,
		PriorJSONLPaths:  priorPaths,
	}
}

// switchRightPane は右ペイン制御を tea.Cmd として返す。
// Update() のメッセージハンドラから発行する明示的なユーザー操作（Enter/n/r/f）に使う。
//
// 副作用: split モードかつ DisplayJSONL の場合、preview.WriteSpec でプレビュープロセスへ
// IPC 書き込みを行う（選択セッションの PreviewSpec をファイルに書き出す）。
//
// display と spec は Update フレーム内（Cmd 生成時）でキャプチャする。
// bubbletea は Cmd を別 goroutine で実行するため、後からカーソルが移動しても
// 生成時点のセッションに対する仕様が書き出される（順序性の保証）。
//
// sid は selectedID とは限らない（sessionCreatedMsg/sessionForkedMsg は新規セッションの ID を渡す）。
// そのため selectedSnap ではなく GetSession(sid) で直接取得する。
func (m *Model) switchRightPane(sid session.DeckSessionID, focusRight bool) tea.Cmd {
	// Update フレーム内で display と spec を確定する。
	// GetSession(sid) から直接 Snapshot を取得することで sid と selectedID が異なる場合
	// （sessionCreatedMsg / sessionForkedMsg など）でも正しいセッションの spec が書き出される。
	var display session.DisplayChannel
	var spec preview.PreviewSpec
	if sess := m.manager.GetSession(sid); sess != nil {
		snap := sess.Snapshot()
		display = snap.Display
		if display != session.DisplayTmux {
			spec = m.buildPreviewSpecFromSnap(snap)
		}
	}
	dataDir := m.config.DataDir
	mgr := m.manager
	return func() tea.Msg {
		if display == session.DisplayTmux {
			_ = mgr.FocusSession(sid)
			if focusRight {
				_ = ghostty.FocusRight()
			}
			return nil
		}
		if err := preview.WriteSpec(dataDir, spec); err != nil {
			debuglog.Printf("[switchRightPane] preview IPC: %v", err)
		}
		_ = mgr.FocusPreviewWindow()
		if focusRight {
			_ = ghostty.FocusRight()
		}
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
