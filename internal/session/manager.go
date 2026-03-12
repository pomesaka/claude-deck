package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/hooks"
	"github.com/pomesaka/claude-deck/internal/jj"
	"github.com/pomesaka/claude-deck/internal/pty"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// notifyInterval はデバウンス間隔。16ms ≈ 60fps で UI を駆動する。
const notifyInterval = 16 * time.Millisecond

// spinnerIdleTimeout はスピナー消失から Idle 遷移までの猶予時間。
// Claude Code の Braille スピナーは ~80ms 間隔で更新されるため、
// 3秒あれば一時的な描画の途切れで誤検知しない。
const spinnerIdleTimeout = 3 * time.Second

// ManagerConfig holds configuration values used by Manager for session creation.
type ManagerConfig struct {
	DataDir               string
	DefaultPermissionMode string
	MaxSessions           int
	MaxLogLines           int
	MaxScrollback         int
	DiscoveryDays         int
	RefreshInterval       time.Duration
	WorkspaceSymlinksFunc func(repoPath string) []string
}

// Manager coordinates multiple Claude Code sessions.
// The dashboard is monitor-only; manual intervention is done via Ghostty.
type Manager struct {
	mu        sync.RWMutex
	sessions  map[string]*Session
	processes map[string]*pty.Process
	store *store.Store
	usage     *usage.Reader
	ctx       context.Context
	config    ManagerConfig
	onChange  func()

	// 詳細ペインで選択中のセッションのみストリーミング（最大1つ）
	activeStreamID     string
	activeStreamCancel context.CancelFunc

	// RefreshFromJSONL の並行実行ガード
	refreshing atomic.Bool

	// notifyChange デバウンス用 dirty flag
	dirty atomic.Bool

	// JSONL ファイルの fsnotify 監視
	fileWatcher *usage.MultiWatcher

	// SessionEnd→SessionStart ペアリング: ClaudeDeckSessionID をキーにして
	// 直前の SessionEnd イベントを保持。セッションごとに独立してペアリングするため
	// 複数セッションの並行 /clear でも混同しない。
	pendingEndEvents map[string]*hooks.Event

	// handleHookEvent で ClaudeSessionID が更新された際の旧 ID を記録。
	// DiscoverExternalSessions / handleNewFile で重複インポートを防ぐ。
	oldSessionIDs map[string]bool

	// 次回 DiscoverExternalSessions の読み込み開始位置（ページネーション用）
	discoveryOffset int
}

// NewManager creates a new session manager.
// ctx is used as the parent context for log streaming goroutines.
func NewManager(ctx context.Context, st *store.Store, cfg ManagerConfig) *Manager {
	return &Manager{
		sessions:  make(map[string]*Session),
		processes: make(map[string]*pty.Process),
		store: st,
		usage:     usage.NewReader(""),
		ctx:       ctx,
		config:    cfg,
	}
}

// SetOnChange registers a callback for session state changes.
func (m *Manager) SetOnChange(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = fn
}

func (m *Manager) notifyChange() {
	m.dirty.Store(true)
}

// StartNotifyLoop polls the dirty flag at notifyInterval and fires onChange.
// 高頻度の PTY 出力を吸収し、最大 ~60fps で UI 更新を通知する。
func (m *Manager) StartNotifyLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(notifyInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if m.dirty.CompareAndSwap(true, false) {
					m.mu.RLock()
					fn := m.onChange
					m.mu.RUnlock()
					if fn != nil {
						fn()
					}
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
						m.notifyChange()
					}
				}
			}
		}
	}()
}

// CreateSession creates and starts a new Claude Code session.
// repoPath は .jj のあるリポジトリルート、workingDir は claude を起動するディレクトリ（サブプロジェクト対応）。
// withWorkspace が true なら jj workspace を作成して隔離環境で起動する。
func (m *Manager) CreateSession(ctx context.Context, repoPath string, workingDir string, withWorkspace bool, cols, rows int) (*Session, error) {
	debuglog.Printf("[CreateSession] repoPath=%q workingDir=%q withWorkspace=%v cols=%d rows=%d", repoPath, workingDir, withWorkspace, cols, rows)
	repoName := filepath.Base(repoPath)
	sess := NewSession(repoPath, repoName)
	sess.maxLogLines = m.config.MaxLogLines
	sess.maxScrollback = m.config.MaxScrollback

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
		if err := jj.CreateWorkspaceAt(repoPath, wsName, wsPath, extraSymlinks); err != nil {
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
	proc, err := pty.Start(ctx, pty.StartOptions{
		WorkDir:        actualWorkDir,
		Prompt:         "",
		PermissionMode: m.config.DefaultPermissionMode,
		AdditionalArgs: []string{"--agent", sess.Name},
		Env:            []string{"CLAUDE_DECK_SESSION_ID=" + sess.ID},
		Cols:           uint16(cols),
		Rows:           uint16(rows),
	}, func(data []byte) {
		m.handleOutput(sess, data)
	})
	if err != nil {
		debuglog.Printf("[CreateSession] pty.Start failed: %v", err)
		if withWorkspace {
			_ = jj.ForgetWorkspace(repoPath, sess.Name)
		}
		return nil, fmt.Errorf("starting claude code: %w", err)
	}
	debuglog.Printf("[CreateSession] pty started pid=%d", proc.PID())

	sess.mu.Lock()
	sess.PID = proc.PID()
	sess.managed = true
	// PTY と同じサイズでエミュレータを再作成
	sess.emulator = newEmulatorWithCallbacks(sess, cols, rows)
	sess.mu.Unlock()

	// ワークスペースの最近接ブックマークをセッションタイトルに設定
	debuglog.Printf("[CreateSession] getting nearest bookmark for %q", actualWorkDir)
	if bookmark, err := jj.GetNearestBookmark(actualWorkDir); err == nil && bookmark != "" {
		debuglog.Printf("[CreateSession] bookmark=%q", bookmark)
		sess.mu.Lock()
		sess.BookmarkName = bookmark
		sess.mu.Unlock()
	} else {
		debuglog.Printf("[CreateSession] GetNearestBookmark: bookmark=%q err=%v", bookmark, err)
	}

	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.processes[sess.ID] = proc
	m.mu.Unlock()

	m.persist(sess)
	m.pruneOldSessions()
	m.notifyChange()

	go m.watchProcess(sess, proc)

	return sess, nil
}

// handleOutput processes a raw PTY output chunk from a session (monitor-only).
// PTY 出力中の Braille スピナー文字を検知して Running に遷移する。
func (m *Manager) handleOutput(sess *Session, data []byte) {
	sess.AppendRaw(data)

	if containsBrailleSpinner(string(data)) {
		sess.touchSpinner()
		status := sess.GetStatus()
		if status != StatusRunning && status != StatusCompleted && status != StatusError {
			sess.SetStatus(StatusRunning)
		}
	}

	m.notifyChange()
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
	// resume 不可能なので旧 ID にフォールバックする。
	sess.mu.RLock()
	csID := sess.ClaudeSessionID
	prevCSID := sess.PreviousClaudeSessionID
	sess.mu.RUnlock()

	if prevCSID != "" && !m.usage.HasConversation(csID) {
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
			sess.ClaudeSessionID = prevCSID
			sess.PreviousClaudeSessionID = ""
			sess.mu.Unlock()
		}
	}

	m.persist(sess)
	m.notifyChange()
}

// ResumeSession resumes a completed Claude Code session using --resume.
func (m *Manager) ResumeSession(ctx context.Context, sessionID string, cols, rows int) error {
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
	csID := sess.ClaudeSessionID
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

	debuglog.Printf("[ResumeSession] calling pty.Start")
	proc, err := pty.Start(ctx, pty.StartOptions{
		WorkDir:         workDir,
		ResumeSessionID: csID,
		Env:             []string{"CLAUDE_DECK_SESSION_ID=" + sessionID},
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
	sess.setStatusLocked(StatusIdle)
	sess.FinishedAt = nil // resume なので終了時刻をクリア
	sess.PID = proc.PID()
	sess.managed = true
	sess.maxLogLines = m.config.MaxLogLines
	sess.maxScrollback = m.config.MaxScrollback
	sess.LogLines = make([]string, 0, 256)
	sess.emulator = newEmulatorWithCallbacks(sess, cols, rows)
	// JSONLLogEntries は上部ログビューポートで表示に使用し続ける。
	// PTY 出力は下部の専用ビューポートに表示される。
	sess.mu.Unlock()
	debuglog.Printf("[ResumeSession] session state updated")

	m.mu.Lock()
	m.processes[sessionID] = proc
	m.mu.Unlock()

	m.persist(sess)
	m.notifyChange()
	debuglog.Printf("[ResumeSession] done, watching process")

	go m.watchProcess(sess, proc)

	return nil
}

// ForkSession creates a new session that forks from an existing session's conversation.
// Uses claude --resume <sourceClaudeSessionID> --fork-session to inherit conversation
// history while creating a new Claude Code session ID and JSONL file.
func (m *Manager) ForkSession(ctx context.Context, sourceSessionID string, cols, rows int) (*Session, error) {
	m.mu.RLock()
	srcSess, ok := m.sessions[sourceSessionID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found: %s", sourceSessionID)
	}

	srcSess.mu.RLock()
	srcClaudeID := srcSess.ClaudeSessionID
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
	sess.maxLogLines = m.config.MaxLogLines
	sess.maxScrollback = m.config.MaxScrollback

	wsName := sess.Name
	wsPath := filepath.Join(m.config.DataDir, "workspace", encodePathForDir(repoPath), wsName)

	var extraSymlinks []string
	if m.config.WorkspaceSymlinksFunc != nil {
		extraSymlinks = m.config.WorkspaceSymlinksFunc(repoPath)
	}
	if err := jj.CreateWorkspaceAt(repoPath, wsName, wsPath, extraSymlinks); err != nil {
		return nil, fmt.Errorf("creating jj workspace: %w", err)
	}
	sess.WorkspacePath = wsPath
	sess.WorkspaceName = wsName
	sess.SubProjectDir = srcSubProjectDir

	proc, err := pty.Start(ctx, pty.StartOptions{
		WorkDir:         srcWorkDir,
		ResumeSessionID: srcClaudeID,
		ForkSession:     true,
		Env:             []string{"CLAUDE_DECK_SESSION_ID=" + sess.ID},
		Cols:            uint16(cols),
		Rows:            uint16(rows),
	}, func(data []byte) {
		m.handleOutput(sess, data)
	})
	if err != nil {
		_ = jj.ForgetWorkspace(repoPath, wsName)
		return nil, fmt.Errorf("starting forked session: %w", err)
	}

	sess.mu.Lock()
	sess.PID = proc.PID()
	sess.managed = true
	sess.emulator = newEmulatorWithCallbacks(sess, cols, rows)
	sess.mu.Unlock()

	// ワークスペースの最近接ブックマークをセッションタイトルに設定
	if bookmark, err := jj.GetNearestBookmark(wsPath); err == nil && bookmark != "" {
		sess.mu.Lock()
		sess.BookmarkName = bookmark
		sess.mu.Unlock()
	}

	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.processes[sess.ID] = proc
	m.mu.Unlock()

	m.persist(sess)
	m.pruneOldSessions()
	m.notifyChange()

	go m.watchProcess(sess, proc)

	return sess, nil
}

// RemoveSession removes a deck session from the manager and store, but keeps
// Claude Code JSONL files and jj workspace intact. Use for cleaning up duplicate
// deck sessions without losing Claude Code data.
func (m *Manager) RemoveSession(sessionID string) error {
	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	_, hasProc := m.processes[sessionID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if hasProc && m.HasActiveProcess(sessionID) {
		return fmt.Errorf("cannot remove running session (kill it first)")
	}

	m.stopActiveStream(sessionID)

	// oldSessionIDs には登録しない。dd は deck メタデータだけ削除し JSONL は残すため、
	// 次回の DiscoverExternalSessions で外部セッションとして再発見されるのが正しい動作。
	m.mu.Lock()
	delete(m.sessions, sessionID)
	delete(m.processes, sessionID)
	m.mu.Unlock()

	if m.store != nil {
		_ = m.store.Delete(sessionID)
	}

	m.notifyChange()
	return nil
}

// DeleteSession removes a session from the manager, store, and Claude Code JSONL.
// Running sessions must be killed first.
// Returns a warning message (non-empty if JSONL cleanup had issues) and an error.
func (m *Manager) DeleteSession(sessionID string) (warning string, err error) {
	m.mu.RLock()
	sess, ok := m.sessions[sessionID]
	_, hasProc := m.processes[sessionID]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("session not found: %s", sessionID)
	}

	status := sess.GetStatus()
	if hasProc && status != StatusCompleted && status != StatusError && status != StatusUnmanaged {
		return "", fmt.Errorf("cannot delete running session (kill it first)")
	}

	m.stopActiveStream(sessionID)

	// Claude Code の JSONL ファイルも削除
	sess.mu.RLock()
	csID := sess.ClaudeSessionID
	sess.mu.RUnlock()

	if csID != "" {
		if jsonlErr := m.usage.DeleteSessionFiles(csID); jsonlErr != nil {
			warning = fmt.Sprintf("JSONL削除失敗: %v", jsonlErr)
		}
	}

	sess.mu.RLock()
	wsName := sess.WorkspaceName
	repoPath := sess.RepoPath
	sess.mu.RUnlock()

	// jj ワークスペースを forget（削除時のみ。プロセス終了時は再開用に保持する）
	if wsName != "" && repoPath != "" {
		if wsErr := jj.ForgetWorkspace(repoPath, wsName); wsErr != nil {
			msg := fmt.Sprintf("workspace forget失敗: %v", wsErr)
			if warning != "" {
				warning += "; " + msg
			} else {
				warning = msg
			}
		}
	}

	m.mu.Lock()
	delete(m.sessions, sessionID)
	delete(m.processes, sessionID)
	m.mu.Unlock()

	if m.store != nil {
		if storeErr := m.store.Delete(sessionID); storeErr != nil {
			msg := fmt.Sprintf("ストア削除失敗: %v", storeErr)
			if warning != "" {
				warning += "; " + msg
			} else {
				warning = msg
			}
		}
	}

	m.notifyChange()
	return warning, nil
}

// Kill forcefully terminates a session.
func (m *Manager) Kill(sessionID string) error {
	m.mu.RLock()
	proc, hasProc := m.processes[sessionID]
	sess, hasSess := m.sessions[sessionID]
	m.mu.RUnlock()

	if !hasSess {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if hasProc {
		return proc.Kill()
	}

	// プロセスハンドルなし（前回起動時のセッションがストアから復元された場合など）。
	// PID が残っていれば SIGTERM で終了を試みる。
	sess.mu.RLock()
	pid := sess.PID
	sess.mu.RUnlock()

	if pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}

	sess.SetStatus(StatusCompleted)
	m.persist(sess)
	return nil
}

// WriteToSession sends data to the PTY process of a running session.
// raw PTY 入力モードでは keyToBytes が1キー分のバイト列を返すため、
// 一括で書き込む。マルチバイト UTF-8 文字の分断を防ぐ。
func (m *Manager) WriteToSession(sessionID string, data []byte) error {
	m.mu.RLock()
	proc, ok := m.processes[sessionID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no active process for session %s", sessionID)
	}
	if _, err := proc.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// HasActiveProcess returns true if the session has a live PTY process.
func (m *Manager) HasActiveProcess(sessionID string) bool {
	m.mu.RLock()
	proc, ok := m.processes[sessionID]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case <-proc.Done():
		return false
	default:
		return true
	}
}

// ResizeSession updates the PTY process and virtual terminal emulator dimensions.
// Claude Code re-renders its Ink UI for the new size.
func (m *Manager) ResizeSession(sessionID string, cols, rows int) {
	debuglog.Printf("[resize] session=%s cols=%d rows=%d", sessionID, cols, rows)
	m.mu.RLock()
	proc, ok := m.processes[sessionID]
	m.mu.RUnlock()
	if ok {
		proc.Resize(uint16(cols), uint16(rows)) //nolint:errcheck
	}

	if sess := m.GetSession(sessionID); sess != nil {
		// emuMu を使う。mu ではなく emuMu がエミュレータ操作専用ロック。
		// mu を使うと AppendRaw（emuMu）と並行実行されデータレースになる。
		sess.emuMu.Lock()
		if sess.emulator != nil {
			sess.emulator.Resize(cols, rows)
		}
		sess.emuMu.Unlock()
	}
}

// GetSession returns a session by ID.
func (m *Manager) GetSession(id string) *Session {
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
	m.mu.RLock()
	list := make([]*Session, 0, len(m.processes))
	for id, proc := range m.processes {
		// non-blocking check: if Done channel is closed, the process has exited
		select {
		case <-proc.Done():
			continue
		default:
		}
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
func (m *Manager) isClaudeIDClaimed(claudeSessionID, excludeID string) bool {
	for _, s := range m.copySessionsList() {
		if s.ID == excludeID {
			continue
		}
		s.mu.RLock()
		csID := s.ClaudeSessionID
		s.mu.RUnlock()
		if csID == claudeSessionID {
			return true
		}
	}
	return false
}
