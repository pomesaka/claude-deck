package tui

import (
	"context"
	"strings"
	"sync/atomic"
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

// sessionManager is what the TUI needs from session.Manager. Tests pass a fake
// to drive Update through the async results without tmux, jj, or a store.
type sessionManager interface {
	GetSession(id session.DeckSessionID) *session.Session
	ListSessions() []*session.Session
	RefreshFromJSONL()
	ResolveJSONLPaths(id session.DeckSessionID) (current string, prior []string)
	CreateSession(ctx context.Context, repoPath, workingDir string, withWorkspace bool, prompt string) (*session.Session, error)
	ForkSession(ctx context.Context, source session.DeckSessionID, prompt string) (*session.Session, error)
	ResumeSession(ctx context.Context, id session.DeckSessionID) error
	Kill(id session.DeckSessionID) (session.CloseResult, error)
	SetPinned(id session.DeckSessionID, pinned bool) error
}

// Model is the Bubble Tea model for the TUI.
type Model struct {
	manager sessionManager
	config  *config.Config
	ghostty *ghostty.Launcher
	ctx     context.Context

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
	// repoItems is the last discovery result. The `n` selector shows it at once
	// and replaces it when the background discovery started by `n` finishes.
	repoItems []repoItem
	// repoDiscovering is true while a discoverRepos Cmd is in flight, so that
	// pressing `n` again does not stack fd runs (each walks the whole home dir).
	repoDiscovering bool
	// discoverRepos runs the candidate search. Tests replace it to avoid fd.
	discoverRepos func() tea.Msg

	// Session filter
	filterInput  textinput.Model
	filterActive bool   // フィルタ入力中
	filterText   string // 確定済みフィルタ

	// Status bar
	statusMsg string
	// statusGen counts the status messages shown. A clear tick carries the
	// generation of the message it was set for, so it never clears a later one.
	statusGen uint64

	// Quit state
	prompt   modalPrompt
	quitting bool

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

	// rate limits the sessions reported (Pro/Max subscribers only)
	rateLimitsStatus ratelimits.Status

	// rightPaneGeneration is shared with pane-switch Cmds so stale switches cannot
	// overwrite the latest selected session after rapid cursor movement.
	rightPaneGeneration *atomic.Uint64
	// rightPane runs the switches one at a time. Nil in tests that build Model
	// directly; the switches are then dropped.
	rightPane *rightPaneWorker
}

// SessionRefreshMsg triggers a session list refresh.
// Manager の onChange コールバックからも送信されるためエクスポート。
// ChangedIDs が空の場合はブロードキャスト（全セッション更新）。
// 非空の場合は指定されたセッションのみ変更された。
type SessionRefreshMsg struct {
	ChangedIDs map[session.DeckSessionID]bool
}

// statusClearMsg clears the status message shown as generation gen.
type statusClearMsg struct{ gen uint64 }

// modalPrompt is a question that takes the next key. The footer shows it
// instead of the status message, so no status update or clear can hide it.
type modalPrompt int

const (
	promptNone modalPrompt = iota
	promptQuit
)

// sessionCreatedMsg is sent when an async session creation completes.
type sessionCreatedMsg struct {
	sessionID session.DeckSessionID
	err       error
}

// sessionResumedMsg is sent when an async session resume completes.
type sessionResumedMsg struct {
	sessionID session.DeckSessionID
	err       error
}

// sessionForkedMsg is sent when an async session fork completes.
type sessionForkedMsg struct {
	sessionID session.DeckSessionID
	err       error
}

// sessionKilledMsg is sent when an async session kill completes.
// No sessionID field needed: kill does not create a new session.
type sessionKilledMsg struct {
	result session.CloseResult
	err    error
}

// pinToggledMsg is sent when an async pin or unpin completes. The list itself
// is refreshed through WatchStore, so only the error is carried.
type pinToggledMsg struct {
	err error
}

// NewModel creates the initial TUI model.
func NewModel(mgr *session.Manager, cfg *config.Config, ctx context.Context) Model {
	m := newModel(mgr, cfg, ctx)
	dataDir := cfg.DataDir
	m.rightPane = newRightPaneWorker(ctx, m.rightPaneGeneration, rightPaneOps{
		focusSession: mgr.FocusSession,
		writeSpec:    func(spec preview.PreviewSpec) error { return preview.WriteSpec(dataDir, spec) },
		focusPreview: mgr.FocusPreviewWindow,
		focusRight:   ghostty.FocusRight,
	})
	m.discoverRepos = discoverRepos(cfg)
	return m
}

// newModel builds the model without the side effects that need the real
// Manager (right pane worker, repository discovery). Tests start from here and
// set those fields to fakes.
func newModel(mgr sessionManager, cfg *config.Config, ctx context.Context) Model {
	fi := textinput.New()
	fi.Prompt = "/ "
	fi.Placeholder = "filter..."

	refreshInterval, _ := time.ParseDuration(cfg.Session.RefreshInterval)
	if refreshInterval <= 0 {
		refreshInterval = 5 * time.Second
	}

	m := Model{
		manager:             mgr,
		config:              cfg,
		ghostty:             ghostty.NewLauncher(cfg.Ghostty.Command),
		ctx:                 ctx,
		repoList:            newRepoList(),
		filterInput:         fi,
		refreshInterval:     refreshInterval,
		rightPaneGeneration: &atomic.Uint64{},
		// Init が起動時の探索を始める。Init は値レシーバで印を残せないので、ここで立てる。
		repoDiscovering: true,
		discoverRepos:   func() tea.Msg { return nil },
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

// RateLimitsUpdatedMsg is sent when a session's report updates the
// rate-limits.json file. main.go injects this via p.Send().
type RateLimitsUpdatedMsg struct {
	Status ratelimits.Status
}

type rightPaneTarget int

const (
	rightPaneNone rightPaneTarget = iota
	rightPanePreview
	rightPaneTmux
)

type rightPaneSwitch struct {
	SessionID  session.DeckSessionID
	FocusRight bool
	Target     rightPaneTarget
	Spec       preview.PreviewSpec
	Generation uint64
}

// Init returns the initial command.
func (m Model) Init() tea.Cmd {
	// 最初の n で数秒待たないよう、起動時に候補を探しておく。
	return tea.Batch(metadataTickCmd(m.refreshInterval), m.discoverRepos)
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

	case sessionClickedMsg:
		if cmd := m.handleSessionClick(msg.id); cmd != nil {
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
					m.rightPane.submitSpec(spec, m.currentRightPaneGeneration())
				}
			}
		}

	case metadataTickMsg:
		debuglog.Printf("[tui] metadataTickMsg (event loop alive)")
		go m.manager.RefreshFromJSONL()
		cmds = append(cmds, metadataTickCmd(m.refreshInterval))

	case RateLimitsUpdatedMsg:
		m.rateLimitsStatus = msg.Status
		// レート制限の行が出るとヘッダーが 1 行増え、一覧の窓が狭まる。
		m.ensureCursorVisible()

	case sessionCreatedMsg:
		if msg.err != nil {
			cmds = append(cmds, m.flash("Create failed: "+msg.err.Error()))
		} else {
			cmds = append(cmds, m.flash("Session created"))
			cmds = append(cmds, m.focusStartedSession(msg.sessionID)...)
		}

	case sessionResumedMsg:
		if msg.err != nil {
			cmds = append(cmds, m.flash("Resume failed: "+msg.err.Error()))
		} else {
			cmds = append(cmds, m.flash("Session resumed"))
			cmds = append(cmds, m.focusStartedSession(msg.sessionID)...)
		}

	case sessionForkedMsg:
		if msg.err != nil {
			cmds = append(cmds, m.flash("Fork failed: "+msg.err.Error()))
		} else {
			cmds = append(cmds, m.flash("Session forked"))
			cmds = append(cmds, m.focusStartedSession(msg.sessionID)...)
		}

	case sessionKilledMsg:
		m.killing = false
		// refreshSessions は呼ばない: Kill は store を書いた後に Reload し、その notifyChange が
		// SessionRefreshMsg を届ける。StopProcess が失敗したときも同じ。beginClose で失敗したときは何も書いていない。
		switch {
		case msg.err != nil:
			cmds = append(cmds, m.flash("Close failed: "+msg.err.Error()))
		case msg.result.WorkspaceKept != "":
			// WHY パスを出さない: 左ペインは狭く、長い文はワークスペースを残したことまで切れる。
			// パスは claude-deck list の work_dir で分かる。
			cmds = append(cmds, m.flash("Closed. Workspace kept: jj could not snapshot it"))
		default:
			cmds = append(cmds, m.flash("Session closed"))
		}

	case pinToggledMsg:
		if msg.err != nil {
			cmds = append(cmds, m.flash("Pin failed: "+msg.err.Error()))
		}

	case repoListMsg:
		if cmd := m.applyRepoList(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case statusClearMsg:
		if msg.gen == m.statusGen {
			m.statusMsg = ""
		}
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
			sessionCreatedMsg, sessionResumedMsg, sessionForkedMsg, sessionKilledMsg, pinToggledMsg, repoListMsg,
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

// statusDuration is how long a flash message stays.
const statusDuration = 3 * time.Second

// flash shows text in the status line and returns the Cmd that clears it
// after statusDuration, unless another message has been shown by then.
func (m *Model) flash(text string) tea.Cmd {
	m.setStatus(text)
	gen := m.statusGen
	return tea.Tick(statusDuration, func(time.Time) tea.Msg { return statusClearMsg{gen: gen} })
}

// setStatus shows text until the next status message. Used for progress
// ("…中") that the result of the operation replaces.
func (m *Model) setStatus(text string) {
	m.statusGen++
	m.statusMsg = text
}

// focusStartedSession selects a session that was just created, resumed or
// forked, and shows it on the right pane.
// WHY フィルタを解除する: 新しいセッションがフィルタで隠れると、一覧の選択はカーソルの行に
// 戻る一方で右ペインは新しいセッションを出し、Enter や x がカーソル側に効く。
// WHY 再開でもカーソルを移す: 再開を待つ間にカーソルを動かしていても、右ペインに出るのは
// 再開したセッションなので、選択もそれに合わせる。
func (m *Model) focusStartedSession(id session.DeckSessionID) []tea.Cmd {
	m.filterActive = false
	m.filterText = ""
	m.filterInput.SetValue("")
	m.filterInput.Blur()
	m.selectedID = id
	cmds := m.refreshSessions()
	// tmux window は作成済み。作成直後は JSONL hydrate や snapshot 更新順の影響で Display が
	// 一時的に JSONL に見えることがあるため、明示操作では tmux を直接選ぶ。
	m.switchRightPaneToTmux(id, true)
	return cmds
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

// updateSelected updates m.selectedID based on the current cursor position and
// returns any tea.Cmd for right-pane switching.
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

	// カーソル移動(idChanged)またはセッション状態変化(display変化)で
	// 右ペインを最新の状態に同期する。focusRight=false なのでカーソル移動では
	// Ghostty のペインフォーカスは移動しない（ユーザーはリストを閲覧中）。
	if idChanged || newDisplay != oldDisplay {
		m.switchRightPane(sid, false)
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
		Name:             snap.DisplayName(),
		RepoName:         snap.RepoName,
		WorkspacePath:    snap.WorkDir(),
		RuntimeSessionID: snap.RuntimeSessionID,
		ClearCount:       snap.ClearCount,
		Status:           snap.Status.ID(),
		Display:          snap.Display.String(),
		CurrentTool:      snap.CurrentTool,
		ErrorMessage:     snap.ErrorMessage,
		StatusReason:     snap.StatusReason,
		NeedsAttention:   snap.Status.NeedsAttention(),
		TranscriptLayout: m.config.RuntimeProvider(),
		JSONLPath:        jsonlPath,
		PriorJSONLPaths:  priorPaths,
	}
}

// switchRightPane は右ペインの切り替えを worker（rightpane.go）に渡す。
// カーソル移動と明示的なユーザー操作（Enter/n/r/f）から使う。
//
// 副作用: split モードでは DisplayTmux の場合は tmux window を、DisplayJSONL の場合は
// preview window を右ペインへ表示する。focusRight は Ghostty のキーボードフォーカス移動だけを制御する。
//
// display と spec は switchRightPane 呼び出し時点でキャプチャする。
//
// sid は selectedID とは限らない（sessionCreatedMsg/sessionForkedMsg は新規セッションの ID を渡す）。
// そのため selectedSnap ではなく GetSession(sid) で直接取得する。
func (m *Model) switchRightPane(sid session.DeckSessionID, focusRight bool) {
	m.executeRightPaneSwitch(m.buildRightPaneSwitch(sid, focusRight))
}

func (m *Model) switchRightPaneToTmux(sid session.DeckSessionID, focusRight bool) {
	sw := rightPaneSwitch{
		SessionID:  sid,
		FocusRight: focusRight,
		Target:     rightPaneTmux,
		Generation: m.nextRightPaneGeneration(),
	}
	m.executeRightPaneSwitch(sw)
}

func (m *Model) buildRightPaneSwitch(sid session.DeckSessionID, focusRight bool) rightPaneSwitch {
	// Update フレーム内で display と spec を確定する。
	// GetSession(sid) から直接 Snapshot を取得することで sid と selectedID が異なる場合
	// （sessionCreatedMsg / sessionForkedMsg など）でも正しいセッションの spec が書き出される。
	sw := rightPaneSwitch{SessionID: sid, FocusRight: focusRight}
	if sess := m.manager.GetSession(sid); sess != nil {
		sw = m.buildRightPaneSwitchFromSnap(sess.Snapshot(), focusRight)
	} else {
		sw.Generation = m.nextRightPaneGeneration()
	}
	return sw
}

func (m *Model) buildRightPaneSwitchFromSnap(snap session.Snapshot, focusRight bool) rightPaneSwitch {
	sw := rightPaneSwitch{
		SessionID:  snap.ID,
		FocusRight: focusRight,
		Target:     rightPaneTargetForDisplay(snap.Display),
		Generation: m.nextRightPaneGeneration(),
	}
	if sw.Target == rightPanePreview {
		sw.Spec = m.buildPreviewSpecFromSnap(snap)
	}
	return sw
}

// rightPaneTargetForDisplay は、カーソル移動でも明示的な操作でも、右ペインに出す先を Display だけで決める。
// WHY NOT 実行中のセッションを preview の JSONL で見せる: preview は Display が tmux のときログを流さない
// （tui/preview.go）ので、実行中のセッションが空の画面になる。
func rightPaneTargetForDisplay(display session.DisplayChannel) rightPaneTarget {
	if display == session.DisplayTmux {
		return rightPaneTmux
	}
	return rightPanePreview
}

func (m *Model) executeRightPaneSwitch(sw rightPaneSwitch) {
	if m.shouldApplyRightPaneSwitch(sw) {
		m.rightPane.submit(sw)
	}
}

func (m *Model) shouldApplyRightPaneSwitch(sw rightPaneSwitch) bool {
	if sw.Target == rightPaneNone {
		return false
	}
	if !sw.FocusRight && sw.SessionID != m.selectedID {
		return false
	}
	return true
}

// currentRightPaneGeneration is the generation of the latest switch. A spec
// rewrite carries it so that a switch requested afterwards supersedes it.
func (m *Model) currentRightPaneGeneration() uint64 {
	if m.rightPaneGeneration == nil {
		return 0
	}
	return m.rightPaneGeneration.Load()
}

func (m *Model) nextRightPaneGeneration() uint64 {
	if m.rightPaneGeneration == nil {
		m.rightPaneGeneration = &atomic.Uint64{}
	}
	return m.rightPaneGeneration.Add(1)
}

// ensureCursorVisible adjusts scrollOffset so the cursor is within the visible window.
func (m *Model) ensureCursorVisible() {
	m.scrollOffset = scrollToShow(m.sessionListHeight(), m.filterBarHeight(), m.scrollOffset, m.cursor, len(m.visibleSessions()))
}

// scrollToShow returns the scroll offset that puts cursor inside the window
// sessionListWindow draws, moving offset as little as possible.
// WHY sessionListWindow を使う: 見える行数をここで別に見積もると、ヘッダーの行数
// （レート制限の行）やインジケータの有無で View とずれ、カーソルの行が描かれなくなる。
func scrollToShow(height, filterBarHeight, offset, cursor, n int) int {
	if n == 0 {
		return 0
	}
	cursor = min(max(cursor, 0), n-1)
	offset = min(max(offset, 0), n-1)
	if cursor < offset {
		offset = cursor
	}
	for cursor >= sessionListWindow(height, filterBarHeight, offset, n).end {
		offset++
	}
	// ウィンドウが広がったときに、下に空白を残さない。
	for offset > 0 {
		w := sessionListWindow(height, filterBarHeight, offset-1, n)
		if w.end < n || cursor >= w.end {
			break
		}
		offset--
	}
	return offset
}
