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
	"github.com/pomesaka/claude-deck/internal/pty"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// notifyInterval はデバウンス間隔。16ms ≈ 60fps で UI を駆動する。
// PTY 出力などのバースト時に複数の notifyChange 呼び出しを1回の onChange にまとめる。
const notifyInterval = 16 * time.Millisecond

// spinnerIdleTimeout はスピナー消失から Idle 遷移までの猶予時間。
// Claude Code の Braille スピナーは ~80ms 間隔で更新されるため、
// 3秒あれば一時的な描画の途切れで誤検知しない。
const spinnerIdleTimeout = 3 * time.Second

// ManagerConfig holds configuration values used by Manager for session creation.
type ManagerConfig struct {
	DataDir               string
	ClaudeCommand         string     // claude executable path (passed to pty.StartOptions)
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
}

// Manager coordinates multiple Claude Code sessions.
// The dashboard is monitor-only; manual intervention is done via Ghostty.
type Manager struct {
	mu       sync.RWMutex
	sessions map[DeckSessionID]*Session
	// Supervisor manages PTY process lifecycle (start, stop, I/O, resize).
	// Extracted from Manager to separate process infrastructure from session domain.
	Supervisor *ProcessSupervisor
	store      *store.Store
	usage      *usage.Reader
	ctx        context.Context
	config     ManagerConfig
	onChange   func(changed map[DeckSessionID]bool)

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
func NewManager(ctx context.Context, st *store.Store, cfg ManagerConfig) *Manager {
	return &Manager{
		sessions:       make(map[DeckSessionID]*Session),
		Supervisor:     NewProcessSupervisor(),
		store:          st,
		usage:          usage.NewReader(""),
		ctx:            ctx,
		config:         cfg,
		notifyCh:       make(chan struct{}, 1),
		pendingChanges: make(map[DeckSessionID]bool),
		hookProc:       newHookProcessor(),
	}
}

// jj returns the configured jj Runner, falling back to a zero-value Runner
// (which defaults to "jj" executable).
func (m *Manager) jj() *jj.Runner {
	if m.config.JJ != nil {
		return m.config.JJ
	}
	return &jj.Runner{}
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

	debuglog.Printf("[CreateSession] starting pty workDir=%q", actualWorkDir)
	addDirArgs := m.buildAddDirArgs(repoPath)
	proc, err := pty.Start(ctx, pty.StartOptions{
		Command:        m.config.ClaudeCommand,
		WorkDir:        actualWorkDir,
		Prompt:         "",
		PermissionMode: m.config.DefaultPermissionMode,
		AdditionalArgs: append([]string{"--agent", sess.Name}, addDirArgs...),
		Env:            []string{"CLAUDE_DECK_SESSION_ID=" + string(sess.ID)},
		Cols:           uint16(cols),
		Rows:           uint16(rows),
	}, func(data []byte) {
		m.handleOutput(sess, data)
	})
	if err != nil {
		debuglog.Printf("[CreateSession] pty.Start failed: %v", err)
		if withWorkspace {
			_ = m.jj().ForgetWorkspace(repoPath, sess.Name)
		}
		return nil, fmt.Errorf("starting claude code: %w", err)
	}
	debuglog.Printf("[CreateSession] pty started pid=%d", proc.PID())

	sess.InitDisplay(cols, rows, m.config.MaxScrollback)

	sess.mu.Lock()
	sess.PID = proc.PID()
	sess.managed = true
	sess.mu.Unlock()

	// ワークスペースの最近接ブックマークをセッションタイトルに設定
	debuglog.Printf("[CreateSession] getting nearest bookmark for %q", actualWorkDir)
	if bookmark, err := m.jj().GetNearestBookmark(actualWorkDir); err == nil && bookmark != "" {
		debuglog.Printf("[CreateSession] bookmark=%q", bookmark)
		sess.mu.Lock()
		sess.BookmarkName = bookmark
		sess.mu.Unlock()
	} else {
		debuglog.Printf("[CreateSession] GetNearestBookmark: bookmark=%q err=%v", bookmark, err)
	}

	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	m.Supervisor.Register(sess.ID, proc)
	m.persist(sess)
	m.pruneOldSessions()
	m.notifyChange(sess.ID)

	go m.watchProcess(sess, proc)

	return sess, nil
}

// handleOutput processes a raw PTY output chunk from a session.
// Domain logic (spinner detection → Running transition) is delegated to Session.IngestPTYOutput.
func (m *Manager) handleOutput(sess *Session, data []byte) {
	sess.IngestPTYOutput(data)
	m.notifyChange(sess.ID)
}

// watchProcess monitors a session process for exit.
// ワークスペースはセッション削除時まで保持する（再開時に必要）。
// /clear 後にメッセージ未送信で終了した場合、ClaudeSessionID を旧 ID にフォールバックする。
func (m *Manager) watchProcess(sess *Session, proc *pty.Process) {
	debuglog.Printf("[watchProcess] waiting for process to exit session=%s pid=%d", sess.ID, proc.PID())
	<-proc.Done()
	debuglog.Printf("[watchProcess] process exited session=%s", sess.ID)

	sess.SetManaged(false)

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

	// PTY 起動前にエミュレータと rt フィールドをリセットする。
	// 起動後にリセットすると、起動直後の出力が古いエミュレータに流れる
	// リースウィンドウが生じるため、必ず起動前に行う。
	if sess.display != nil {
		sess.ResetDisplay(cols, rows)
	} else {
		sess.InitDisplay(cols, rows, m.config.MaxScrollback)
	}

	// rt.maxLogLines と rt.LogLines は rt.mu で保護する。
	// sess.mu との同時保持は禁止（ロック順序規則）。
	sess.rt.mu.Lock()
	sess.rt.maxLogLines = m.config.MaxLogLines
	sess.rt.LogLines = make([]string, 0, 256)
	sess.rt.mu.Unlock()

	sess.mu.Lock()
	sess.setStatusLocked(StatusIdle)
	sess.FinishedAt = nil // resume なので終了時刻をクリア
	sess.managed = true
	sess.mu.Unlock()

	debuglog.Printf("[ResumeSession] calling pty.Start")
	proc, err := pty.Start(ctx, pty.StartOptions{
		Command:         m.config.ClaudeCommand,
		WorkDir:         workDir,
		ResumeSessionID: string(csID),
		AdditionalArgs:  m.buildAddDirArgs(sess.RepoPath),
		Env:             []string{"CLAUDE_DECK_SESSION_ID=" + string(sessionID)},
		Cols:            uint16(cols),
		Rows:            uint16(rows),
	}, func(data []byte) {
		m.handleOutput(sess, data)
	})
	if err != nil {
		debuglog.Printf("[ResumeSession] pty.Start failed: %v", err)
		return fmt.Errorf("resuming claude code: %w", err)
	}
	debuglog.Printf("[ResumeSession] pty started pid=%d", proc.PID())

	sess.mu.Lock()
	sess.PID = proc.PID()
	// JSONLLogEntries は上部ログビューポートで表示に使用し続ける。
	// PTY 出力は下部の専用ビューポートに表示される。
	sess.mu.Unlock()
	debuglog.Printf("[ResumeSession] session state updated")

	m.Supervisor.Register(sessionID, proc)

	m.persist(sess)
	m.notifyChange(sessionID)
	debuglog.Printf("[ResumeSession] done, watching process")

	go m.watchProcess(sess, proc)

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
	srcWorkDir := srcSess.WorkspacePath
	srcSubProjectDir := srcSess.SubProjectDir
	if srcWorkDir == "" {
		srcWorkDir = srcSess.RepoPath
	}
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
	sess.WorkspacePath = wsPath
	sess.WorkspaceName = wsName
	sess.SubProjectDir = srcSubProjectDir

	proc, err := pty.Start(ctx, pty.StartOptions{
		Command:         m.config.ClaudeCommand,
		WorkDir:         srcWorkDir,
		ResumeSessionID: string(srcClaudeID),
		ForkSession:     true,
		AdditionalArgs:  m.buildAddDirArgs(repoPath),
		Env:             []string{"CLAUDE_DECK_SESSION_ID=" + string(sess.ID)},
		Cols:            uint16(cols),
		Rows:            uint16(rows),
	}, func(data []byte) {
		m.handleOutput(sess, data)
	})
	if err != nil {
		_ = m.jj().ForgetWorkspace(repoPath, wsName)
		return nil, fmt.Errorf("starting forked session: %w", err)
	}

	sess.InitDisplay(cols, rows, m.config.MaxScrollback)

	sess.mu.Lock()
	sess.PID = proc.PID()
	sess.managed = true
	sess.mu.Unlock()

	// ワークスペースの最近接ブックマークをセッションタイトルに設定
	if bookmark, err := m.jj().GetNearestBookmark(wsPath); err == nil && bookmark != "" {
		sess.mu.Lock()
		sess.BookmarkName = bookmark
		sess.mu.Unlock()
	}

	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	m.Supervisor.Register(sess.ID, proc)
	m.persist(sess)
	m.pruneOldSessions()
	m.notifyChange(sess.ID)

	go m.watchProcess(sess, proc)

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

	if m.Supervisor.IsAlive(sessionID) {
		return fmt.Errorf("cannot remove running session (kill it first)")
	}

	m.stopActiveStream(sessionID)

	// oldSessionIDs には登録しない。dd は deck メタデータだけ削除し JSONL は残すため、
	// 次回の DiscoverExternalSessions で外部セッションとして再発見されるのが正しい動作。
	m.Supervisor.Unregister(sessionID)
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

	if m.Supervisor.IsAlive(sessionID) {
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

	m.Supervisor.Unregister(sessionID)
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

	if err := m.Supervisor.Kill(sessionID, pid); err != nil {
		return err
	}

	// Supervisor.Kill が SIGTERM を送った場合、watchProcess が Completed に遷移させる。
	// プロセスハンドルがない場合は手動で遷移させる。
	if !m.Supervisor.IsAlive(sessionID) && m.Supervisor.Get(sessionID) == nil {
		sess.SetStatus(StatusCompleted)
		m.persist(sess)
	}
	return nil
}

// WriteToSession sends data to the PTY process of a running session.
// raw PTY 入力モードでは keyToBytes が1キー分のバイト列を返すため、
// 一括で書き込む。マルチバイト UTF-8 文字の分断を防ぐ。
func (m *Manager) WriteToSession(sessionID DeckSessionID, data []byte) error {
	return m.Supervisor.Write(sessionID, data)
}

// HasActiveProcess returns true if the session has a live PTY process.
func (m *Manager) HasActiveProcess(sessionID DeckSessionID) bool {
	return m.Supervisor.IsAlive(sessionID)
}

// ResizeSession updates the PTY process and virtual terminal emulator dimensions.
// Claude Code re-renders its Ink UI for the new size.
func (m *Manager) ResizeSession(sessionID DeckSessionID, cols, rows int) {
	debuglog.Printf("[resize] session=%s cols=%d rows=%d", sessionID, cols, rows)
	m.Supervisor.Resize(sessionID, uint16(cols), uint16(rows))

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

// ListManagedSessions returns PTY-managed sessions that are still alive.
// Only sessions with an active (not-yet-done) process are included.
func (m *Manager) ListManagedSessions() []*Session {
	activeIDs := m.Supervisor.ActiveSessionIDs()

	m.mu.RLock()
	list := make([]*Session, 0, len(activeIDs))
	for _, id := range activeIDs {
		if s, ok := m.sessions[id]; ok {
			list = append(list, s)
		}
	}
	m.mu.RUnlock()

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

// isClaudeIDClaimed returns true if the given Claude Code session ID is already
// used by another deck session (excluding excludeID).
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
