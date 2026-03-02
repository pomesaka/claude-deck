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
// PTY は対話モード（--agent）で起動し、prompt は空。
func (m *Manager) CreateSession(ctx context.Context, repoPath string, cols, rows int) (*Session, error) {
	repoName := filepath.Base(repoPath)
	sess := NewSession(repoPath, repoName)
	sess.maxLogLines = m.config.MaxLogLines
	sess.maxScrollback = m.config.MaxScrollback

	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("リポジトリが見つかりません: %s", repoPath)
	}

	wsName := sess.Name
	wsPath := filepath.Join(m.config.DataDir, "workspace", encodePathForDir(repoPath), wsName)

	if err := jj.CreateWorkspaceAt(repoPath, wsName, wsPath); err != nil {
		return nil, fmt.Errorf("creating jj workspace: %w", err)
	}
	sess.WorkspacePath = wsPath
	sess.WorkspaceName = wsName

	proc, err := pty.Start(ctx, pty.StartOptions{
		WorkDir:        wsPath,
		Prompt:         "",
		PermissionMode: m.config.DefaultPermissionMode,
		AdditionalArgs: []string{"--agent", sess.Name},
		Env:            []string{"CLAUDE_DECK_SESSION_ID=" + sess.ID},
		Cols:           uint16(cols),
		Rows:           uint16(rows),
	}, func(line string) {
		m.handleOutput(sess, line)
	})
	if err != nil {
		_ = jj.ForgetWorkspace(repoPath, wsName)
		return nil, fmt.Errorf("starting claude code: %w", err)
	}

	sess.mu.Lock()
	sess.PID = proc.PID()
	sess.managed = true
	// PTY と同じサイズでエミュレータを再作成
	sess.emulator = newEmulatorWithCallbacks(sess, cols, rows)
	sess.mu.Unlock()

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

// handleOutput processes a line of output from a session (monitor-only).
// PTY 出力中の Braille スピナー文字を検知して Running に遷移する。
func (m *Manager) handleOutput(sess *Session, line string) {
	sess.AppendLog(line)

	if containsBrailleSpinner(line) {
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
	<-proc.Done()

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
	if m.HasActiveProcess(sessionID) {
		return fmt.Errorf("session %s already has an active process", sessionID)
	}

	m.mu.RLock()
	sess, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	sess.mu.RLock()
	csID := sess.ClaudeSessionID
	wsPath := sess.WorkspacePath
	repoPath := sess.RepoPath
	sess.mu.RUnlock()

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

	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		sess.SetErrorStatus(fmt.Sprintf("ディレクトリが見つかりません: %s", workDir))
		m.persist(sess)
		return fmt.Errorf("作業ディレクトリが見つかりません: %s", workDir)
	}

	// JSONL ストリーミングは継続する。PTY は入力・プロセス管理用で、
	// 表示は JSONL 構造化ログを優先するため。

	proc, err := pty.Start(ctx, pty.StartOptions{
		WorkDir:         workDir,
		ResumeSessionID: csID,
		Env:             []string{"CLAUDE_DECK_SESSION_ID=" + sessionID},
		Cols:            uint16(cols),
		Rows:            uint16(rows),
	}, func(line string) {
		m.handleOutput(sess, line)
	})
	if err != nil {
		return fmt.Errorf("resuming claude code: %w", err)
	}

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

	m.mu.Lock()
	m.processes[sessionID] = proc
	m.mu.Unlock()

	m.persist(sess)
	m.notifyChange()

	go m.watchProcess(sess, proc)

	return nil
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
		sess.mu.Lock()
		if sess.emulator != nil {
			sess.emulator.Resize(cols, rows)
		}
		sess.mu.Unlock()
	}
}

// GetSession returns a session by ID.
func (m *Manager) GetSession(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ListSessions returns all sessions sorted with attention-needed first, then by start time.
func (m *Manager) ListSessions() []*Session {
	m.mu.RLock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.RUnlock()

	// m.mu を解放してからソート（sortTime/getName は s.mu を取るため ABBA 回避）
	sort.Slice(list, func(i, j int) bool {
		ti, tj := list[i].sortTime(), list[j].sortTime()
		if ti.Equal(tj) {
			return list[i].getName() < list[j].getName()
		}
		return ti.Before(tj)
	})

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
