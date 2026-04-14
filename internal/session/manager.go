package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/jj"
	"github.com/pomesaka/claude-deck/internal/store"
	tmuxrunner "github.com/pomesaka/claude-deck/internal/tmux"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// notifyInterval はデバウンス間隔。16ms ≈ 60fps で UI を駆動する。
// PTY 出力などのバースト時に複数の notifyChange 呼び出しを1回の onChange にまとめる。
const notifyInterval = 16 * time.Millisecond

// spinnerIdleTimeout はスピナー消失から Idle 遷移までの猶予時間。
// Claude Code の Braille スピナーは ~80ms 間隔で更新されるため、
// 3秒あれば一時的な描画の途切れで誤検知しない。
const spinnerIdleTimeout = 3 * time.Second

// BackendMode selects the process hosting backend.
type BackendMode int

const (
	// BackendPTY embeds Claude Code in an internal PTY managed by claude-deck
	// (the default mode). Output is captured, scrollback is maintained, and
	// spinner detection drives status transitions.
	BackendPTY BackendMode = iota
	// BackendTmux hosts Claude Code in a tmux window.  claude-deck manages
	// session metadata only; users see live output via `tmux attach`.
	// Near-instant session switching is achieved with tmux select-window.
	BackendTmux
)

// ManagerConfig holds configuration values used by Manager for session creation.
type ManagerConfig struct {
	DataDir               string
	ClaudeCommand         string     // claude executable path
	JJ                    *jj.Runner // jj CLI runner (nil uses default "jj")
	DefaultPermissionMode string
	MaxSessions           int
	MaxLogLines           int
	MaxScrollback         int
	DiscoveryDays         int
	RefreshInterval       time.Duration
	Pricing               PricingPolicy
	WorkspaceSymlinksFunc func(repoPath string) []string
	// AddDirsFunc returns the --add-dir paths for the given repository.
	AddDirsFunc func(repoPath string) []string

	// BackendMode selects the process hosting backend (PTY vs tmux).
	BackendMode BackendMode
	// TmuxCommand is the tmux binary path for BackendTmux mode.
	// Defaults to "tmux" if empty.
	TmuxCommand string
	// TmuxSession is the tmux session name for BackendTmux mode.
	// Defaults to "claude-deck" if empty.
	TmuxSession string
}

// Manager coordinates multiple Claude Code sessions.
// The dashboard is monitor-only; manual intervention is done via Ghostty.
type Manager struct {
	mu       sync.RWMutex
	sessions map[DeckSessionID]*Session
	// Supervisor manages PTY process lifecycle (start, stop, I/O, resize).
	// Extracted from Manager to separate process infrastructure from session domain.
	// Kept as a public field for test access; production code uses backend instead.
	Supervisor *ProcessSupervisor
	// backend abstracts the process hosting mechanism (PTY, tmux, Ghostty split, etc.).
	// Initialized in NewManager to ptyBackend backed by Supervisor.
	backend  SessionBackend
	store    *store.Store
	usage    *usage.Reader
	ctx      context.Context
	config   ManagerConfig
	onChange func(changed map[DeckSessionID]bool)

	// 詳細ペインで選択中のセッションのみストリーミング（最大1つ）
	activeStreamID     DeckSessionID
	activeStreamCancel context.CancelFunc

	// RefreshFromJSONL の並行実行ガード
	refreshing atomic.Bool

	// notifyChange デバウンス用チャネル（バッファ 1 でバーストを吸収）
	notifyCh chan struct{}

	// pendingChanges はデバウンス間隔中に変更があったセッション ID を蓄積する。
	// onChange コールバック発火時にドレインされる。空の場合はブロードキャスト（全セッション更新）。
	pendingMu      sync.Mutex
	pendingChanges map[DeckSessionID]bool

	// JSONL ファイルの fsnotify 監視
	fileWatcher *usage.MultiWatcher

	// hookProc はシングルゴルーチンで動作する SessionEnd→SessionStart ペアリングステートマシン。
	// event watcher goroutine のみが読み書きするため mu 不要。
	hookProc *hookProcessor

	// 次回 DiscoverExternalSessions の読み込み開始位置（ページネーション用）
	discoveryOffset int
}

// NewManager creates a new session manager.
// ctx is used as the parent context for log streaming goroutines.
// The backend is selected from cfg.BackendMode (default: BackendPTY).
func NewManager(ctx context.Context, st *store.Store, cfg ManagerConfig) *Manager {
	m := &Manager{
		sessions:       make(map[DeckSessionID]*Session),
		store:          st,
		usage:          usage.NewReader(""),
		ctx:            ctx,
		config:         cfg,
		notifyCh:       make(chan struct{}, 1),
		pendingChanges: make(map[DeckSessionID]bool),
		hookProc:       newHookProcessor(),
	}

	switch cfg.BackendMode {
	case BackendTmux:
		runner := &tmuxrunner.Runner{
			Command:     cfg.TmuxCommand,
			SessionName: cfg.TmuxSession,
		}
		// Auto-create the tmux session if it doesn't exist yet.
		// Status bar is hidden so the tmux client shows only Claude Code output.
		if !runner.HasSession() {
			if err := runner.NewSession(); err != nil {
				debuglog.Printf("[NewManager] tmux new-session failed: %v", err)
			} else {
				runner.ApplyDefaultOptions() // マウスホイールでターミナル履歴を遡れるようにする
			}
		}
		m.backend = newTmuxBackend(runner, m.watchProcess)

	default: // BackendPTY
		sup := NewProcessSupervisor()
		m.Supervisor = sup
		// Wire ptyBackend with the exit callback. watchProcess handles all post-exit
		// domain logic; the backend only needs to fire it after <-proc.Done().
		m.backend = newPTYBackend(sup, m.watchProcess)
	}

	return m
}

// jj returns the configured jj Runner, falling back to a zero-value Runner
// (which defaults to "jj" executable).
func (m *Manager) jj() *jj.Runner {
	if m.config.JJ != nil {
		return m.config.JJ
	}
	return &jj.Runner{}
}

// buildStartArgs pre-assembles the CLI arg list for a Claude Code process start.
// It mirrors the arg construction that pty.Start performs from its StartOptions fields,
// allowing the SessionBackend interface to accept a plain []string instead of a
// PTY-specific typed struct. This keeps backend implementations decoupled from
// claude CLI flag semantics.
func buildStartArgs(resumeID string, forkSession bool, prompt, permMode string, additionalArgs []string) []string {
	var args []string
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
		if forkSession {
			args = append(args, "--fork-session")
		}
	} else if prompt != "" {
		args = append(args, "-p", prompt)
	}
	if permMode != "" {
		args = append(args, "--permission-mode", permMode)
	}
	return append(args, additionalArgs...)
}

// SetOnChange registers a callback for session state changes.
// The callback receives a map of session IDs that changed since the last call.
// An empty map means a broad change (e.g. discovery) that may affect all sessions.
func (m *Manager) SetOnChange(fn func(changed map[DeckSessionID]bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = fn
}

// notifyChange signals that session state has changed.
// sessionIDs identifies which sessions changed. If empty, the change is broad
// (e.g. discovery) and consumers should refresh everything.
func (m *Manager) notifyChange(sessionIDs ...DeckSessionID) {
	if len(sessionIDs) > 0 {
		m.pendingMu.Lock()
		for _, id := range sessionIDs {
			m.pendingChanges[id] = true
		}
		m.pendingMu.Unlock()
	}
	select {
	case m.notifyCh <- struct{}{}:
	default: // already pending; coalesce into the buffered signal
	}
}

// drainPendingChanges returns and clears the accumulated set of changed session IDs.
// An empty map means at least one broad (non-session-specific) change occurred.
func (m *Manager) drainPendingChanges() map[DeckSessionID]bool {
	m.pendingMu.Lock()
	changes := m.pendingChanges
	m.pendingChanges = make(map[DeckSessionID]bool)
	m.pendingMu.Unlock()
	return changes
}

// StartNotifyLoop fires onChange whenever notifyChange is called, debounced to
// at most ~60fps. バースト時は notifyCh（バッファ 1）が信号を吸収し、
// debounce window 内の追加信号をドレインしてから onChange を一度だけ呼ぶ。
// ticker ポーリングと異なりアイドル時は goroutine がスリープし CPU を消費しない。
func (m *Manager) StartNotifyLoop(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.notifyCh:
				// debounce: drain additional signals within one frame window
				timer := time.NewTimer(notifyInterval)
			drain:
				for {
					select {
					case <-m.notifyCh:
					case <-timer.C:
						break drain
					case <-ctx.Done():
						timer.Stop()
						return
					}
				}
				changes := m.drainPendingChanges()
				m.mu.RLock()
				fn := m.onChange
				m.mu.RUnlock()
				if fn != nil {
					fn(changes)
				}
			}
		}
	}()
}

// StartSpinnerIdleLoop periodically checks managed sessions for spinner timeout.
// Braille スピナーが spinnerIdleTimeout 以上検出されていない Running セッションを
// 自動的に Idle に遷移させる。フックイベントが届かない場合のフォールバック。
func (m *Manager) StartSpinnerIdleLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, sess := range m.ListManagedSessions() {
					if sess.spinnerIdleSince(spinnerIdleTimeout) {
						debuglog.Printf("[spinnerIdle] session %s: spinner timeout, transitioning to Idle", sess.ID)
						sess.SetStatus(StatusIdle)
						m.notifyChange(sess.ID)
					}
				}
			}
		}
	}()
}

// Launch starts a session based on the given LaunchIntent.
// This is the unified entry point for all session launch operations (New, Resume, Fork).
// Returns the session (new or existing) and any error.
func (m *Manager) Launch(ctx context.Context, intent LaunchIntent) (*Session, error) {
	switch intent.Kind {
	case LaunchNew:
		return m.CreateSession(ctx, intent.RepoPath, intent.WorkingDir, intent.WithWorkspace, intent.Cols, intent.Rows)
	case LaunchResume:
		if err := m.ResumeSession(ctx, intent.SessionID, intent.Cols, intent.Rows); err != nil {
			return nil, err
		}
		return m.GetSession(intent.SessionID), nil
	case LaunchFork:
		return m.ForkSession(ctx, intent.SessionID, intent.Cols, intent.Rows)
	default:
		return nil, fmt.Errorf("unknown launch kind: %v", intent.Kind)
	}
}

// computeActualWorkDir は wsPath と subProjectDir からプロセスの作業ディレクトリを算出する。
// subProjectDir が空のときは wsPath をそのまま返す。
func computeActualWorkDir(wsPath, subProjectDir string) string {
	if subProjectDir == "" {
		return wsPath
	}
	return filepath.Join(wsPath, subProjectDir)
}

// finalizeNewSession は新規セッション（CreateSession / ForkSession）の共通後処理。
// jj ブックマークをセッション名として設定し、sessions マップへ登録、永続化、通知を行う。
//
// sessions マップへの登録は StartProcess 前にも行われているが（SessionStart フック競合対策）、
// ここでも冪等に上書きすることで bookmark 等の後処理フィールドを確実に反映する。
// ResumeSession は既存セッションを対象とするためこのパターンは不要。
func (m *Manager) finalizeNewSession(sess *Session, workDir string) {
	if bookmark, err := m.jj().GetNearestBookmark(workDir); err == nil && bookmark != "" {
		sess.mu.Lock()
		sess.BookmarkName = bookmark
		sess.mu.Unlock()
	}
	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()
	m.persist(sess)
	m.pruneOldSessions()
	m.notifyChange(sess.ID)
}

// CreateSession creates and starts a new Claude Code session.
// repoPath は .jj のあるリポジトリルート、workingDir は claude を起動するディレクトリ（サブプロジェクト対応）。
// withWorkspace が true なら jj workspace を作成して隔離環境で起動する。
func (m *Manager) CreateSession(ctx context.Context, repoPath string, workingDir string, withWorkspace bool, cols, rows int) (*Session, error) {
	debuglog.Printf("[CreateSession] repoPath=%q workingDir=%q withWorkspace=%v cols=%d rows=%d", repoPath, workingDir, withWorkspace, cols, rows)
	repoName := filepath.Base(repoPath)
	sess := NewSession(repoPath, repoName)
	sess.rt.maxLogLines = m.config.MaxLogLines

	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("リポジトリが見つかりません: %s", repoPath)
	}

	var actualWorkDir string
	if withWorkspace {
		wsName := sess.Name
		wsPath := filepath.Join(m.config.DataDir, "workspace", encodePathForDir(repoPath), wsName)

		var extraSymlinks []string
		if m.config.WorkspaceSymlinksFunc != nil {
			extraSymlinks = m.config.WorkspaceSymlinksFunc(repoPath)
		}
		debuglog.Printf("[CreateSession] creating jj workspace name=%q path=%q", wsName, wsPath)
		if err := m.jj().CreateWorkspaceAt(repoPath, wsName, wsPath, extraSymlinks); err != nil {
			return nil, fmt.Errorf("creating jj workspace: %w", err)
		}
		debuglog.Printf("[CreateSession] jj workspace created")
		sess.WorkspaceName = wsName

		// サブプロジェクト対応: workingDir がリポジトリルートと異なる場合、
		// ワークスペース内の対応サブディレクトリを作業ディレクトリにする
		relPath, err := filepath.Rel(repoPath, workingDir)
		if err != nil || relPath == "." {
			actualWorkDir = wsPath
		} else {
			actualWorkDir = filepath.Join(wsPath, relPath)
			sess.SubProjectDir = relPath
		}
		sess.WorkspacePath = actualWorkDir
	} else {
		// ワークスペースなし → workingDir をそのまま使用
		actualWorkDir = workingDir
		sess.WorkspacePath = actualWorkDir
		if relPath, err := filepath.Rel(repoPath, workingDir); err == nil && relPath != "." {
			sess.SubProjectDir = relPath
		}
	}

	debuglog.Printf("[CreateSession] starting process workDir=%q", actualWorkDir)
	addDirArgs := m.buildAddDirArgs(repoPath)
	additionalArgs := append([]string{"--agent", sess.Name}, addDirArgs...)

	// StartProcess より前に m.sessions に登録する。
	// tmux/PTY プロセスが起動してすぐ SessionStart フックを発火することがあり、
	// event watcher が m.sessions を参照するタイミングとの競合を防ぐため。
	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	// Backend handles AttachProcess internally (display creation, PID storage).
	if err := m.backend.StartProcess(ctx, sess, ProcessStartOpts{
		Command:       m.config.ClaudeCommand,
		WorkDir:       actualWorkDir,
		Args:          buildStartArgs("", false, "", m.config.DefaultPermissionMode, additionalArgs),
		Env:           []string{"CLAUDE_DECK_SESSION_ID=" + string(sess.ID)},
		Cols:          uint16(cols),
		Rows:          uint16(rows),
		MaxScrollback: m.config.MaxScrollback,
	}, func(data []byte) {
		m.handleOutput(sess, data)
	}); err != nil {
		debuglog.Printf("[CreateSession] StartProcess failed: %v", err)
		m.mu.Lock()
		delete(m.sessions, sess.ID)
		m.mu.Unlock()
		if withWorkspace {
			_ = m.jj().ForgetWorkspace(repoPath, sess.Name)
		}
		return nil, fmt.Errorf("starting claude code: %w", err)
	}
	debuglog.Printf("[CreateSession] process started")

	// backend.StartProcess already registered the process and wired the exit watcher.
	// finalizeNewSession も m.sessions[sess.ID] = sess を呼ぶが冪等なので問題なし。
	m.finalizeNewSession(sess, actualWorkDir)

	return sess, nil
}

// handleOutput processes a raw PTY output chunk from a session.
// Domain logic (spinner detection → Running transition) is delegated to Session.IngestPTYOutput.
func (m *Manager) handleOutput(sess *Session, data []byte) {
	sess.IngestPTYOutput(data)
	m.notifyChange(sess.ID)
}

// IsTmuxMode returns true when the manager uses the tmux process backend.
// The TUI uses this to skip PTY viewport updates and adjust key bindings.
func (m *Manager) IsTmuxMode() bool {
	return m.config.BackendMode == BackendTmux
}

// FocusSession makes the session's terminal visible in the hosting environment.
// In tmux mode this calls tmux select-window (~0ms); in PTY mode it is a no-op.
func (m *Manager) FocusSession(sessionID DeckSessionID) error {
	return m.backend.Focus(sessionID)
}

// EnsurePreviewWindow creates the preview subprocess window if it does not exist.
// Delegates to the backend — only tmuxBackend creates a real window.
func (m *Manager) EnsurePreviewWindow() error {
	return m.backend.EnsurePreview()
}

// FocusPreviewWindow switches the display to the preview window.
// Delegates to the backend — only tmuxBackend has an effect.
func (m *Manager) FocusPreviewWindow() error {
	return m.backend.FocusPreview()
}

// KillPreviewWindow destroys the preview window.
// Delegates to the backend — only tmuxBackend has an effect.
func (m *Manager) KillPreviewWindow() error {
	return m.backend.KillPreview()
}

// ReconcileTmux synchronises in-memory session state with the live tmux session.
// Call this after LoadExisting() on startup in tmux mode.
//
// Three cases handled by the backend:
//  1. tmux window exists, deck session exists → backend re-attaches exit watcher
//  2. tmux window exists, deck session missing → backend kills orphaned window
//  3. deck session exists, tmux window missing → Manager marks session Completed
func (m *Manager) ReconcileTmux() {
	if m.config.BackendMode != BackendTmux {
		return
	}

	sessions := m.copySessionsList()
	result, err := m.backend.Reconcile(sessions)
	if err != nil {
		debuglog.Printf("[ReconcileTmux] Reconcile failed: %v", err)
		return
	}

	// Apply backend result to session state.
	for _, sess := range sessions {
		if panePID, alive := result.LivePIDs[sess.ID]; alive {
			// Case 1: window is live — attach as External (tmux owns the PTY).
			sess.AttachProcess(panePID, nil)
			// LoadExisting marks sessions Completed when the stored PID is stale
			// (old pane PID from the previous run no longer exists). If the tmux
			// window is actually alive, reset to Idle so the session shows as
			// active and status hooks can drive transitions from here.
			if status := sess.GetStatus(); status == StatusCompleted || status == StatusError {
				debuglog.Printf("[ReconcileTmux] window alive but status=%s, resetting to Idle session=%s", status, sess.ID)
				sess.SetStatus(StatusIdle)
				m.persist(sess)
			}
		} else {
			// Case 3: window gone — mark Completed if not already terminal.
			// StatusUnmanaged（外部発見セッション）は tmux が管理しないため skip する。
			// ReconcileTmux が Unmanaged → Completed に変換するとストアに重複が蓄積する。
			status := sess.GetStatus()
			if status != StatusCompleted && status != StatusError && status != StatusUnmanaged {
				debuglog.Printf("[ReconcileTmux] window gone, marking Completed session=%s", sess.ID)
				sess.DetachProcess()
				sess.SetStatus(StatusCompleted)
				m.persist(sess)
			}
		}
	}
}

// watchProcess is called by the SessionBackend after a process exits.
// ワークスペースはセッション削除時まで保持する（再開時に必要）。
// /clear 後にメッセージ未送信で終了した場合、ClaudeSessionID を旧 ID にフォールバックする。
//
// The backend is responsible for blocking until the process exits before calling
// this method, so there is no <-proc.Done() here — that keeps the signature
// backend-agnostic (ptyBackend and tmuxBackend share the same onExit type).
func (m *Manager) watchProcess(sess *Session) {
	debuglog.Printf("[watchProcess] process exited session=%s", sess.ID)

	sess.DetachProcess()

	status := sess.GetStatus()
	if status != StatusCompleted && status != StatusError {
		sess.SetStatus(StatusCompleted)
	}

	// /clear 後にメッセージを送らず終了した場合、新 ID の JSONL は空。
	// resume 不可能なので chain の末尾をポップして旧 ID にフォールバックする。
	sess.mu.RLock()
	chain := make([]ClaudeSessionID, len(sess.SessionChain))
	copy(chain, sess.SessionChain)
	sess.mu.RUnlock()

	if len(chain) > 1 {
		csID := chain[len(chain)-1]
		prevCSID := chain[len(chain)-2]
		if !m.usage.HasConversation(string(csID)) {
			// 旧 ID が別の deck セッションに既に紐付いている場合は revert しない。
			// Discovery が旧 ID を外部セッションとしてインポート済みの場合に
			// 2つの deck セッションが同じ ClaudeSessionID を持つのを防ぐ。
			if m.isClaudeIDClaimed(prevCSID, sess.ID) {
				debuglog.Printf("[watchProcess] session %s: empty JSONL for %s, but %s is claimed by another session, not reverting",
					sess.ID, csID, prevCSID)
			} else {
				debuglog.Printf("[watchProcess] session %s: empty JSONL for %s, reverting to %s",
					sess.ID, csID, prevCSID)
				sess.mu.Lock()
				sess.popChainLocked()
				sess.mu.Unlock()
			}
		}
	}

	m.persist(sess)
	m.notifyChange(sess.ID)
}

// ResumeSession resumes a completed Claude Code session using --resume.
func (m *Manager) ResumeSession(ctx context.Context, sessionID DeckSessionID, cols, rows int) error {
	debuglog.Printf("[ResumeSession] sessionID=%s cols=%d rows=%d", sessionID, cols, rows)
	if m.HasActiveProcess(sessionID) {
		debuglog.Printf("[ResumeSession] already has active process")
		return fmt.Errorf("session %s already has an active process", sessionID)
	}

	m.mu.RLock()
	sess, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		debuglog.Printf("[ResumeSession] session not found")
		return fmt.Errorf("session not found: %s", sessionID)
	}

	sess.mu.RLock()
	csID := sess.CurrentClaudeID()
	wsPath := sess.WorkspacePath
	repoPath := sess.RepoPath
	sess.mu.RUnlock()
	debuglog.Printf("[ResumeSession] csID=%q wsPath=%q repoPath=%q", csID, wsPath, repoPath)

	if csID == "" {
		return fmt.Errorf("no Claude Code session ID available for resume")
	}

	// Determine work directory: prefer workspace, fall back to repo
	workDir := wsPath
	if workDir == "" {
		workDir = repoPath
	}
	if workDir == "" {
		return fmt.Errorf("no work directory available for session %s", sessionID)
	}
	debuglog.Printf("[ResumeSession] workDir=%q", workDir)

	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		debuglog.Printf("[ResumeSession] workDir does not exist: %s", workDir)
		sess.SetErrorStatus(fmt.Sprintf("ディレクトリが見つかりません: %s", workDir))
		m.persist(sess)
		return fmt.Errorf("作業ディレクトリが見つかりません: %s", workDir)
	}

	// JSONL ストリーミングは継続する。PTY は入力・プロセス管理用で、
	// 表示は JSONL 構造化ログを優先するため。

	// rt.maxLogLines と rt.LogLines は rt.mu で保護する。
	// sess.mu との同時保持は禁止（ロック順序規則）。
	sess.rt.mu.Lock()
	sess.rt.maxLogLines = m.config.MaxLogLines
	sess.rt.LogLines = make([]string, 0, 256)
	sess.rt.mu.Unlock()

	sess.mu.Lock()
	sess.setStatusLocked(StatusIdle)
	sess.FinishedAt = nil // resume なので終了時刻をクリア
	sess.mu.Unlock()

	// Backend handles display creation and AttachProcess internally.
	// For ptyBackend: display is attached before the output goroutine starts.
	debuglog.Printf("[ResumeSession] calling backend.StartProcess")
	if err := m.backend.StartProcess(ctx, sess, ProcessStartOpts{
		Command:       m.config.ClaudeCommand,
		WorkDir:       workDir,
		Args:          buildStartArgs(string(csID), false, "", "", m.buildAddDirArgs(sess.RepoPath)),
		Env:           []string{"CLAUDE_DECK_SESSION_ID=" + string(sessionID)},
		Cols:          uint16(cols),
		Rows:          uint16(rows),
		MaxScrollback: m.config.MaxScrollback,
	}, func(data []byte) {
		m.handleOutput(sess, data)
	}); err != nil {
		debuglog.Printf("[ResumeSession] StartProcess failed: %v", err)
		return fmt.Errorf("resuming claude code: %w", err)
	}
	debuglog.Printf("[ResumeSession] session state updated")

	// backend.StartProcess already registered the process and wired the exit watcher.
	m.persist(sess)
	m.notifyChange(sessionID)
	debuglog.Printf("[ResumeSession] done, watching process")

	return nil
}

// ForkSession creates a new session that forks from an existing session's conversation.
// Uses claude --resume <sourceClaudeSessionID> --fork-session to inherit conversation
// history while creating a new Claude Code session ID and JSONL file.
func (m *Manager) ForkSession(ctx context.Context, sourceSessionID DeckSessionID, cols, rows int) (*Session, error) {
	m.mu.RLock()
	srcSess, ok := m.sessions[sourceSessionID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found: %s", sourceSessionID)
	}

	srcSess.mu.RLock()
	srcClaudeID := srcSess.CurrentClaudeID()
	repoPath := srcSess.RepoPath
	srcSubProjectDir := srcSess.SubProjectDir
	srcSess.mu.RUnlock()

	if srcClaudeID == "" {
		return nil, fmt.Errorf("ソースセッションに ClaudeSessionID がありません")
	}

	if repoPath == "" {
		return nil, fmt.Errorf("ソースセッションにリポジトリパスがありません")
	}

	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("リポジトリが見つかりません: %s", repoPath)
	}

	repoName := filepath.Base(repoPath)
	sess := NewSession(repoPath, repoName)
	sess.rt.maxLogLines = m.config.MaxLogLines

	wsName := sess.Name
	wsPath := filepath.Join(m.config.DataDir, "workspace", encodePathForDir(repoPath), wsName)

	var extraSymlinks []string
	if m.config.WorkspaceSymlinksFunc != nil {
		extraSymlinks = m.config.WorkspaceSymlinksFunc(repoPath)
	}
	if err := m.jj().CreateWorkspaceAt(repoPath, wsName, wsPath, extraSymlinks); err != nil {
		return nil, fmt.Errorf("creating jj workspace: %w", err)
	}
	// サブプロジェクト対応: ソースが wsPath/subProject で動いていた場合、
	// フォーク先の新ワークスペースでも同じサブディレクトリを使う。
	actualWorkDir := computeActualWorkDir(wsPath, srcSubProjectDir)
	sess.WorkspacePath = actualWorkDir
	sess.WorkspaceName = wsName
	sess.SubProjectDir = srcSubProjectDir

	// StartProcess より前に m.sessions に登録する（CreateSession と同じ競合対策）。
	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	// Backend handles AttachProcess internally.
	if err := m.backend.StartProcess(ctx, sess, ProcessStartOpts{
		Command:       m.config.ClaudeCommand,
		WorkDir:       actualWorkDir,
		Args:          buildStartArgs(string(srcClaudeID), true, "", "", m.buildAddDirArgs(repoPath)),
		Env:           []string{"CLAUDE_DECK_SESSION_ID=" + string(sess.ID)},
		Cols:          uint16(cols),
		Rows:          uint16(rows),
		MaxScrollback: m.config.MaxScrollback,
	}, func(data []byte) {
		m.handleOutput(sess, data)
	}); err != nil {
		m.mu.Lock()
		delete(m.sessions, sess.ID)
		m.mu.Unlock()
		_ = m.jj().ForgetWorkspace(repoPath, wsName)
		return nil, fmt.Errorf("starting forked session: %w", err)
	}

	// backend.StartProcess already registered the process and wired the exit watcher.
	m.finalizeNewSession(sess, actualWorkDir)

	return sess, nil
}

// RemoveSession removes a deck session from the manager and store, but keeps
// Claude Code JSONL files and jj workspace intact. Use for cleaning up duplicate
// deck sessions without losing Claude Code data.
func (m *Manager) RemoveSession(sessionID DeckSessionID) error {
	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if m.backend.IsActive(sessionID) {
		return fmt.Errorf("cannot remove running session (kill it first)")
	}

	m.stopActiveStream(sessionID)

	// oldSessionIDs には登録しない。dd は deck メタデータだけ削除し JSONL は残すため、
	// 次回の DiscoverExternalSessions で外部セッションとして再発見されるのが正しい動作。
	//
	// Supervisor は PTY モードでのみ非 nil。tmux モードでは backend がプロセスを
	// 管理するため Supervisor を持たない。
	if m.Supervisor != nil {
		m.Supervisor.Unregister(sessionID)
	}
	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if m.store != nil {
		_ = m.store.Delete(string(sessionID))
	}

	m.notifyChange(sessionID)
	return nil
}

// DeleteSession removes a session from the manager, store, and Claude Code JSONL.
// Running sessions must be killed first.
// Returns a warning message (non-empty if JSONL cleanup had issues) and an error.
func (m *Manager) DeleteSession(sessionID DeckSessionID) (warning string, err error) {
	m.mu.RLock()
	sess, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("session not found: %s", sessionID)
	}

	if m.backend.IsActive(sessionID) {
		return "", fmt.Errorf("cannot delete running session (kill it first)")
	}

	m.stopActiveStream(sessionID)

	// Claude Code の JSONL ファイルも削除
	sess.mu.RLock()
	csID := sess.CurrentClaudeID()
	sess.mu.RUnlock()

	if csID != "" {
		if jsonlErr := m.usage.DeleteSessionFiles(string(csID)); jsonlErr != nil {
			warning = fmt.Sprintf("JSONL削除失敗: %v", jsonlErr)
		}
	}

	sess.mu.RLock()
	wsName := sess.WorkspaceName
	repoPath := sess.RepoPath
	sess.mu.RUnlock()

	// jj ワークスペースを forget（削除時のみ。プロセス終了時は再開用に保持する）
	if wsName != "" && repoPath != "" {
		if wsErr := m.jj().ForgetWorkspace(repoPath, wsName); wsErr != nil {
			msg := fmt.Sprintf("workspace forget失敗: %v", wsErr)
			if warning != "" {
				warning += "; " + msg
			} else {
				warning = msg
			}
		}
	}

	// Supervisor は PTY モードでのみ非 nil。tmux モードでは backend がプロセスを
	// 管理するため Supervisor を持たない。
	if m.Supervisor != nil {
		m.Supervisor.Unregister(sessionID)
	}
	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if m.store != nil {
		if storeErr := m.store.Delete(string(sessionID)); storeErr != nil {
			msg := fmt.Sprintf("ストア削除失敗: %v", storeErr)
			if warning != "" {
				warning += "; " + msg
			} else {
				warning = msg
			}
		}
	}

	m.notifyChange(sessionID)
	return warning, nil
}

// Kill forcefully terminates a session.
func (m *Manager) Kill(sessionID DeckSessionID) error {
	m.mu.RLock()
	sess, hasSess := m.sessions[sessionID]
	m.mu.RUnlock()

	if !hasSess {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	sess.mu.RLock()
	pid := sess.PID
	sess.mu.RUnlock()

	if err := m.backend.StopProcess(sessionID, pid); err != nil {
		return err
	}

	// StopProcess が SIGTERM を送った場合、watchProcess が Completed に遷移させる。
	// プロセスハンドルがない（PID フォールバック）場合は手動で遷移させる。
	//
	// DetachProcess と notifyChange も呼ぶ。呼ばないと:
	//   - process.Load() != nil のまま → DisplayChannel が DisplayNone/DisplayPTY のまま残る
	//   - TUI が更新されず detail pane が古い表示のままになる
	// watchProcess も後から同じ処理をするが、それまでの間 TUI が不整合状態になるのを防ぐ。
	if !m.backend.IsActive(sessionID) && (m.Supervisor == nil || m.Supervisor.Get(sessionID) == nil) {
		sess.DetachProcess()
		sess.SetStatus(StatusCompleted)
		m.persist(sess)
		m.notifyChange(sessionID)
	}
	return nil
}

// WriteToSession sends data to the PTY process of a running session.
// raw PTY 入力モードでは keyToBytes が1キー分のバイト列を返すため、
// 一括で書き込む。マルチバイト UTF-8 文字の分断を防ぐ。
func (m *Manager) WriteToSession(sessionID DeckSessionID, data []byte) error {
	return m.backend.WriteInput(sessionID, data)
}

// HasActiveProcess returns true if the session has a live PTY process.
func (m *Manager) HasActiveProcess(sessionID DeckSessionID) bool {
	return m.backend.IsActive(sessionID)
}

// ResizeSession updates the PTY process and virtual terminal emulator dimensions.
// Claude Code re-renders its Ink UI for the new size.
func (m *Manager) ResizeSession(sessionID DeckSessionID, cols, rows int) {
	debuglog.Printf("[resize] session=%s cols=%d rows=%d", sessionID, cols, rows)
	m.backend.Resize(sessionID, uint16(cols), uint16(rows))

	if sess := m.GetSession(sessionID); sess != nil {
		sess.ResizeDisplay(cols, rows)
	}
}

// GetSession returns a session by ID.
func (m *Manager) GetSession(id DeckSessionID) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ListSessions returns all sessions sorted by status group, then by last activity (newest first).
// Group order (top→bottom): Unmanaged/Completed/Error → Idle → Running → WaitingApproval/Answer.
func (m *Manager) ListSessions() []*Session {
	m.mu.RLock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.RUnlock()

	// ソートキーを事前計算（比較ごとのロック取得を排除）
	// sort.Slice は list 内の要素をスワップするが、別配列の keys はスワップしないため
	// キーと要素がずれる。session とキーをペアにした構造体をソートする。
	type sortItem struct {
		session *Session
		group   int
		t       time.Time
		name    string
	}
	items := make([]sortItem, len(list))
	for i, s := range list {
		items[i] = sortItem{
			session: s,
			group:   s.sortGroup(),
			t:       s.sortTime(),
			name:    s.getName(),
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].group != items[j].group {
			return items[i].group < items[j].group
		}
		if items[i].t.Equal(items[j].t) {
			return items[i].name < items[j].name
		}
		return items[i].t.Before(items[j].t)
	})
	for i, item := range items {
		list[i] = item.session
	}

	return list
}

// ListManagedSessions returns sessions that currently have an active process.
// The check is delegated to the backend so this works for both PTY and tmux modes.
// m.mu を解放してから IsActive を呼ぶ（tmux モードでは外部コマンド実行になるため
// ロック保持中の長時間ブロックを避ける）。
func (m *Manager) ListManagedSessions() []*Session {
	candidates := m.copySessionsList()
	list := make([]*Session, 0, len(candidates))
	for _, s := range candidates {
		if m.backend.IsActive(s.ID) {
			list = append(list, s)
		}
	}

	sort.Slice(list, func(i, j int) bool {
		ti, tj := list[i].sortTime(), list[j].sortTime()
		if ti.Equal(tj) {
			return list[i].getName() < list[j].getName()
		}
		return ti.Before(tj)
	})

	return list
}

// copySessionsList returns a snapshot of the sessions slice under m.mu.
// m.mu → s.mu のロック順序を守るため、先に sessions リストをコピーしてから
// 個別の Session フィールドにアクセスするパターンで使う。
func (m *Manager) copySessionsList() []*Session {
	m.mu.RLock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.RUnlock()
	return list
}

// buildAddDirArgs returns --add-dir flag pairs for the given repository path.
func (m *Manager) buildAddDirArgs(repoPath string) []string {
	if m.config.AddDirsFunc == nil {
		return nil
	}
	dirs := m.config.AddDirsFunc(repoPath)
	if len(dirs) == 0 {
		return nil
	}
	args := make([]string, 0, len(dirs)*2)
	for _, d := range dirs {
		args = append(args, "--add-dir", d)
	}
	return args
}

// isClaudeIDClaimed returns true if the given Claude Code session ID is already
// used by another deck session (excluding excludeID).
func (m *Manager) isClaudeIDClaimed(claudeSessionID ClaudeSessionID, excludeID DeckSessionID) bool {
	for _, s := range m.copySessionsList() {
		if s.ID == excludeID {
			continue
		}
		s.mu.RLock()
		csID := s.CurrentClaudeID()
		s.mu.RUnlock()
		if csID == claudeSessionID {
			return true
		}
	}
	return false
}
