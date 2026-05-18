package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
const notifyInterval = 16 * time.Millisecond

// ManagerConfig holds configuration values used by Manager for session creation.
type ManagerConfig struct {
	DataDir               string
	ClaudeCommand         string     // claude executable path
	JJ                    *jj.Runner // jj CLI runner (nil uses default "jj")
	DefaultPermissionMode string
	MaxSessions           int
	DiscoveryDays         int
	RefreshInterval       time.Duration
	Pricing               PricingPolicy
	WorkspaceSymlinksFunc func(repoPath string) []string
	// AddDirsFunc returns the --add-dir paths for the given repository.
	AddDirsFunc func(repoPath string) []string

	// TmuxCommand is the tmux binary path. Defaults to "tmux" if empty.
	TmuxCommand string
	// TmuxSession is the tmux session name. Defaults to "claude-deck" if empty.
	TmuxSession string
}

// Manager coordinates multiple Claude Code sessions.
// The dashboard is monitor-only; manual intervention is done via Ghostty.
type Manager struct {
	mu       sync.RWMutex
	sessions map[DeckSessionID]*Session
	// backend abstracts the process hosting mechanism (tmux window, etc.).
	backend  SessionBackend
	store    *store.Store
	usage    *usage.Reader
	ctx      context.Context
	config   ManagerConfig
	onChange func(changed map[DeckSessionID]bool)

	// stream guards the active JSONL streaming goroutine.
	// See streamState in manager_jsonl.go for invariants and lock ordering.
	stream streamState

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
	m.backend = newTmuxBackend(m.ctx, runner, m.watchProcess)

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
// Assembles the CLI args for `claude` from semantic parameters, keeping backend
// implementations decoupled from claude CLI flag semantics.
//
// The four launch modes map to:
//   - resumeID != "" && forkSession  → --resume <id> --fork-session  (fork of an existing session)
//   - resumeID != "" && !forkSession → --resume <id>                 (resume an existing session)
//   - resumeID == "" && prompt != "" → -p <prompt>                   (new session with prompt)
//   - resumeID == "" && prompt == "" → (no extra flags)              (new interactive session)
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


// Launch starts a session based on the given LaunchIntent.
// This is the unified entry point for all session launch operations (New, Resume, Fork).
// Returns the session (new or existing) and any error.
func (m *Manager) Launch(ctx context.Context, intent LaunchIntent) (*Session, error) {
	switch intent.Kind {
	case LaunchNew:
		return m.CreateSession(ctx, intent.RepoPath, intent.WorkingDir, intent.WithWorkspace)
	case LaunchResume:
		if err := m.ResumeSession(ctx, intent.SessionID); err != nil {
			return nil, err
		}
		return m.GetSession(intent.SessionID), nil
	case LaunchFork:
		return m.ForkSession(ctx, intent.SessionID)
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
// 冪等性: 呼び出し元は hook 競合防止のため StartProcess 前に sess を m.sessions に登録する
// （SessionStart フックが先に発火する場合がある）。この呼び出しはブックマーク取得後に
// 同じキーで上書きするため問題ない。
// ResumeSession は既存セッションを対象とするため、このパターンを使用しない。
func (m *Manager) finalizeNewSession(sess *Session, workDir string) {
	if bookmark, err := m.jj().GetNearestBookmark(workDir); err == nil && bookmark != "" {
		// sess.mu と m.mu を同時保持しないため逐次的取得であり、順序は問わない（ABBA なし）。
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
func (m *Manager) CreateSession(ctx context.Context, repoPath string, workingDir string, withWorkspace bool) (*Session, error) {
	debuglog.Printf("[CreateSession] repoPath=%q workingDir=%q withWorkspace=%v", repoPath, workingDir, withWorkspace)
	repoName := filepath.Base(repoPath)
	sess := NewSession(repoPath, repoName)

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

	// Backend handles AttachProcess internally (PID storage, exit watcher).
	if err := m.backend.StartProcess(ctx, sess, ProcessStartOpts{
		Command: m.config.ClaudeCommand,
		WorkDir: actualWorkDir,
		Args:    buildStartArgs("", false, "", m.config.DefaultPermissionMode, additionalArgs),
		Env:     []string{"CLAUDE_DECK_SESSION_ID=" + string(sess.ID)},
	}, nil); err != nil {
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

// FocusSession makes the session's terminal visible in the tmux window.
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

// ResolveJSONLPaths returns the current JSONL file path and prior (pre-/clear) paths
// for a given deck session, in chronological order.
// Returns ("", nil) if the session is unknown or has no associated JSONL file.
func (m *Manager) ResolveJSONLPaths(sid DeckSessionID) (current string, prior []string) {
	sess := m.GetSession(sid)
	if sess == nil {
		return "", nil
	}
	sess.mu.RLock()
	csID := sess.CurrentClaudeID()
	priorIDs := sess.PriorClaudeIDs()
	sess.mu.RUnlock()

	current = m.usage.ResolveSessionPath(string(csID))
	for _, id := range priorIDs {
		if p := m.usage.ResolveSessionPath(string(id)); p != "" {
			prior = append(prior, p)
		}
	}
	return current, prior
}

// ReconcileTmux synchronises in-memory session state with the live tmux session.
// Call this after LoadExisting() on startup in tmux mode.
//
// Cases:
//  1. tmux window exists, deck session exists → backend re-attaches exit watcher (backend.Reconcile)
//  2. tmux window exists, deck session missing → backend kills orphaned window (backend.Reconcile)
//  3. deck session exists, tmux window missing → Manager marks session Completed (this function)
func (m *Manager) ReconcileTmux() {
	sessions := m.copySessionsList()
	result, err := m.backend.Reconcile(sessions)
	if err != nil {
		debuglog.Printf("[ReconcileTmux] Reconcile failed: %v", err)
		return
	}

	// Apply backend result to session state.
	for _, sess := range sessions {
		if panePID, alive := result.LivePIDs[sess.ID]; alive {
			// Case 1: window is live — attach process sentinel.
			sess.AttachProcess(panePID)
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
				// SetStatus before DetachProcess — see watchProcess comment for ordering rationale.
				sess.SetStatus(StatusCompleted)
				sess.DetachProcess()
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

	// SetStatus before DetachProcess so that the brief race window observed by
	// Snapshot() is always "terminal status, process still attached" (PhaseActive,
	// the documented case in Phase()) rather than "non-terminal, process gone".
	status := sess.GetStatus()
	if status != StatusCompleted && status != StatusError {
		sess.SetStatus(StatusCompleted)
	}
	sess.DetachProcess()

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
func (m *Manager) ResumeSession(ctx context.Context, sessionID DeckSessionID) error {
	debuglog.Printf("[ResumeSession] sessionID=%s", sessionID)
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
	sessName := sess.Name
	subProjectDir := sess.SubProjectDir
	sess.mu.RUnlock()
	debuglog.Printf("[ResumeSession] csID=%q wsPath=%q repoPath=%q", csID, wsPath, repoPath)

	if csID == "" {
		return fmt.Errorf("no Claude Code session ID available for resume")
	}

	// ワークスペースがなければ（Kill で削除済み）再作成する。
	if wsPath == "" && repoPath != "" && sessName != "" {
		newWsPath, err := m.recreateWorkspace(repoPath, sessName, subProjectDir)
		if err != nil {
			debuglog.Printf("[ResumeSession] workspace recreate failed, falling back to repo: %v", err)
			wsPath = repoPath
		} else {
			wsPath = newWsPath
			sess.mu.Lock()
			sess.WorkspaceName = sessName
			sess.WorkspacePath = newWsPath
			sess.mu.Unlock()
		}
	}

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

	sess.mu.Lock()
	sess.setStatusLocked(StatusIdle)
	sess.FinishedAt = nil // resume なので終了時刻をクリア
	sess.mu.Unlock()

	debuglog.Printf("[ResumeSession] calling backend.StartProcess")
	if err := m.backend.StartProcess(ctx, sess, ProcessStartOpts{
		Command: m.config.ClaudeCommand,
		WorkDir: workDir,
		Args:    buildStartArgs(string(csID), false, "", m.config.DefaultPermissionMode, m.buildAddDirArgs(sess.RepoPath)),
		Env:     []string{"CLAUDE_DECK_SESSION_ID=" + string(sessionID)},
	}, nil); err != nil {
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
func (m *Manager) ForkSession(ctx context.Context, sourceSessionID DeckSessionID) (*Session, error) {
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
	forkArgs := append([]string{"--agent", sess.Name}, m.buildAddDirArgs(repoPath)...)
	if err := m.backend.StartProcess(ctx, sess, ProcessStartOpts{
		Command: m.config.ClaudeCommand,
		WorkDir: actualWorkDir,
		Args:    buildStartArgs(string(srcClaudeID), true, "", m.config.DefaultPermissionMode, forkArgs),
		Env:     []string{"CLAUDE_DECK_SESSION_ID=" + string(sess.ID)},
	}, nil); err != nil {
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

	// oldSessionIDs には登録しない。deck メタデータだけ削除し JSONL は残すため、
	// 次回の DiscoverExternalSessions で外部セッションとして再発見されるのが正しい動作。
	if warnings := m.removeSessionCore(sessionID); len(warnings) > 0 {
		debuglog.Printf("[RemoveSession] cleanup warnings: %s", strings.Join(warnings, "; "))
	}
	m.notifyChange(sessionID)
	return nil
}

// DeleteSession removes a session from the manager, store, and Claude Code JSONL.
// Running sessions must be killed first.
// Returns a warning message (non-empty if any cleanup step had issues) and an error.
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

	sess.mu.RLock()
	csID := sess.CurrentClaudeID()
	wsName := sess.WorkspaceName
	repoPath := sess.RepoPath
	sess.mu.RUnlock()

	var warnings []string
	if w := m.cleanupJSONL(csID); w != "" {
		warnings = append(warnings, w)
	}
	wsRootPath := ""
	if wsName != "" && repoPath != "" {
		wsRootPath = filepath.Join(m.config.DataDir, "workspace", encodePathForDir(repoPath), wsName)
	}
	if w := m.cleanupWorkspace(repoPath, wsName, wsRootPath); w != "" {
		warnings = append(warnings, w)
	}
	warnings = append(warnings, m.removeSessionCore(sessionID)...)

	m.notifyChange(sessionID)
	return strings.Join(warnings, "; "), nil
}

// removeSessionCore performs the shared cleanup for both RemoveSession and DeleteSession:
// stops any active stream, removes from the sessions map, and deletes from the store.
// Returns any warnings encountered (typically store delete failures).
func (m *Manager) removeSessionCore(sessionID DeckSessionID) []string {
	m.stopActiveStream(sessionID)

	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if m.store != nil {
		if storeErr := m.store.Delete(string(sessionID)); storeErr != nil {
			return []string{fmt.Sprintf("ストア削除失敗: %v", storeErr)}
		}
	}
	return nil
}

// cleanupJSONL deletes Claude Code JSONL files for the given session ID.
// Returns a warning string if deletion failed, or "" on success/skip.
func (m *Manager) cleanupJSONL(csID ClaudeSessionID) string {
	if csID == "" {
		return ""
	}
	if err := m.usage.DeleteSessionFiles(string(csID)); err != nil {
		return fmt.Sprintf("JSONL削除失敗: %v", err)
	}
	return ""
}

// cleanupWorkspace runs jj workspace forget and removes the workspace directory.
// wsRootPath is the workspace root directory to delete (DataDir/workspace/<encoded>/<name>).
// It may differ from sess.WorkspacePath, which can point to a subproject subdirectory.
// Returns a warning string if any operation failed, or "" on success/skip.
func (m *Manager) cleanupWorkspace(repoPath, wsName, wsRootPath string) string {
	if wsName == "" || repoPath == "" {
		return ""
	}
	var warnings []string
	// jj ワークスペースを forget してディレクトリを削除する。
	// Kill 時にも呼ばれる（resume 時は recreateWorkspace で再作成される）。
	if err := m.jj().ForgetWorkspace(repoPath, wsName); err != nil {
		// forget 失敗でもディレクトリ削除は続行する（jj が既に forget 済みの場合など）
		warnings = append(warnings, fmt.Sprintf("workspace forget失敗: %v", err))
	}
	if wsRootPath != "" {
		// 安全ガード: DataDir/workspace/ 配下のパスのみ削除する。
		// symlink (macOS: /var → /private/var) を解決してからプレフィックスを比較する。
		resolved := wsRootPath
		if r, err := filepath.EvalSymlinks(wsRootPath); err == nil {
			resolved = r
		}
		base := filepath.Join(m.config.DataDir, "workspace") + string(filepath.Separator)
		if resolvedBase, err := filepath.EvalSymlinks(filepath.Join(m.config.DataDir, "workspace")); err == nil {
			base = resolvedBase + string(filepath.Separator)
		}
		if strings.HasPrefix(resolved, base) {
			if err := os.RemoveAll(wsRootPath); err != nil {
				warnings = append(warnings, fmt.Sprintf("workspace ディレクトリ削除失敗: %v", err))
			}
		}
	}
	return strings.Join(warnings, "; ")
}

// recreateWorkspace creates a new jj workspace for a session whose workspace was deleted.
// Returns the effective work directory (wsPath/subProjectDir if subProjectDir is set).
func (m *Manager) recreateWorkspace(repoPath, sessName, subProjectDir string) (string, error) {
	wsPath := filepath.Join(m.config.DataDir, "workspace", encodePathForDir(repoPath), sessName)
	var extraSymlinks []string
	if m.config.WorkspaceSymlinksFunc != nil {
		extraSymlinks = m.config.WorkspaceSymlinksFunc(repoPath)
	}
	debuglog.Printf("[recreateWorkspace] repoPath=%q sessName=%q wsPath=%q", repoPath, sessName, wsPath)
	if err := m.jj().CreateWorkspaceAt(repoPath, sessName, wsPath, extraSymlinks); err != nil {
		return "", fmt.Errorf("recreating jj workspace: %w", err)
	}
	if subProjectDir != "" {
		return filepath.Join(wsPath, subProjectDir), nil
	}
	return wsPath, nil
}

// Kill forcefully terminates a session and cleans up its workspace directory.
// Session metadata and Claude Code JSONL are preserved for future --resume.
func (m *Manager) Kill(sessionID DeckSessionID) error {
	m.mu.RLock()
	sess, hasSess := m.sessions[sessionID]
	m.mu.RUnlock()

	if !hasSess {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	sess.mu.RLock()
	pid := sess.PID
	wsName := sess.WorkspaceName
	repoPath := sess.RepoPath
	sess.mu.RUnlock()

	if err := m.backend.StopProcess(sessionID, pid); err != nil {
		return err
	}

	// StopProcess が SIGTERM を送った場合、watchProcess が Completed に遷移させる。
	// プロセスハンドルがない（PID フォールバック）場合は手動で遷移させる。
	//
	// DetachProcess と notifyChange も呼ぶ。呼ばないと:
	//   - process.Load() != nil のまま → DisplayChannel が DisplayTmux のまま残る
	//   - TUI が更新されず detail pane が古い表示のままになる
	// watchProcess も後から同じ処理をするが、それまでの間 TUI が不整合状態になるのを防ぐ。
	// 二重実行になるが DetachProcess / SetStatus / persist / notifyChange はすべて冪等なので問題なし。
	if !m.backend.IsActive(sessionID) {
		// SetStatus before DetachProcess — see watchProcess comment for ordering rationale.
		sess.SetStatus(StatusCompleted)
		sess.DetachProcess()
	}

	// ワークスペースディレクトリを削除して disk を回収する。
	// node_modules 等の依存ファイルがワークスペースごとに複製されるため、
	// プロセス終了時に即座にクリーンアップする。
	// resume 時は recreateWorkspace で新規ワークスペースが作られる。
	if wsName != "" && repoPath != "" {
		wsRootPath := filepath.Join(m.config.DataDir, "workspace", encodePathForDir(repoPath), wsName)
		if w := m.cleanupWorkspace(repoPath, wsName, wsRootPath); w != "" {
			debuglog.Printf("[Kill] workspace cleanup: %s", w)
		}
		sess.mu.Lock()
		sess.WorkspaceName = ""
		sess.WorkspacePath = ""
		sess.mu.Unlock()
	}

	m.persist(sess)
	m.notifyChange(sessionID)
	return nil
}

// HasActiveProcess returns true if the session has a live process.
func (m *Manager) HasActiveProcess(sessionID DeckSessionID) bool {
	return m.backend.IsActive(sessionID)
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
