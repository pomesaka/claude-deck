package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pomesaka/claude-deck/internal/agentruntime"
	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/jj"
	"github.com/pomesaka/claude-deck/internal/store"
	tmuxrunner "github.com/pomesaka/claude-deck/internal/tmux"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// notifyInterval はデバウンス間隔。16ms ≈ 60fps で UI を駆動する。
const notifyInterval = 16 * time.Millisecond

// Environment variables passed to every Claude Code process claude-deck starts.
const (
	// EnvSessionID carries the deck session ID; hook commands use it to find the row.
	EnvSessionID = "CLAUDE_DECK_SESSION_ID"
	// EnvCommand is the absolute path of the claude-deck binary, for the plugin's hooks.
	EnvCommand = "CLAUDE_DECK_BIN"
	// EnvDataDir pins the data directory for hook commands run inside the session.
	// WHY 環境変数で渡す: tmux のペインは TUI でなく tmux サーバーの環境を引き継ぐ。
	// 設定ファイルの場所（XDG_CONFIG_HOME）が TUI と違うと、hook が別の store に書いてしまう。
	EnvDataDir = config.EnvDataDir
)

// ManagerConfig holds configuration values used by Manager for session creation.
type ManagerConfig struct {
	DataDir               string
	AgentRuntime          agentruntime.Runtime
	ClaudeCommand         string // deprecated: use AgentRuntime
	TranscriptReader      *usage.Reader
	JJ                    *jj.Runner // jj CLI runner (nil uses default "jj")
	DefaultPermissionMode string
	MaxSessions           int
	DiscoveryDays         int
	RefreshInterval       time.Duration
	WorkspaceSymlinksFunc func(repoPath string) []string
	// TrustWorkspaceFunc tells the runtime that a workspace claude-deck created is
	// trusted, so the session does not open on a trust dialog. Nil does nothing.
	TrustWorkspaceFunc func(wsPath string) error
	// ForgetProjectsFunc removes what the runtime recorded about the directories
	// match accepts (workspaces that are gone) and returns how many records that
	// is; with dryRun it only counts. Nil does nothing.
	ForgetProjectsFunc func(match func(dir string) bool, dryRun bool) (int, error)
	// AddDirsFunc returns the --add-dir paths for the given repository.
	AddDirsFunc func(repoPath string) []string

	// DeckCommand is the absolute path of the claude-deck binary. Sessions run
	// "<DeckCommand> hook exited" when Claude Code exits, and the plugin calls it
	// for status hooks. Empty disables both (tests).
	DeckCommand string
	// PluginDir is passed to Claude Code as --plugin-dir. Empty passes nothing.
	PluginDir string

	// TmuxCommand is the tmux binary path. Defaults to "tmux" if empty.
	TmuxCommand string
	// TmuxSession is the tmux session name. Defaults to "claude-deck" if empty.
	TmuxSession string
}

// Manager coordinates multiple Claude Code sessions.
//
// The store is the source of truth for deck sessions (ADR-011). The sessions map
// is an in-memory projection of the store plus external sessions discovered from
// JSONL, which are never stored. Every operation that changes a deck session
// writes the store first and then reloads the projection.
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

	// RefreshFromJSONL の並行実行ガード
	refreshing atomic.Bool

	// reloadMu serialises Reload so that two reloads never interleave their merges.
	reloadMu sync.Mutex

	// notifyChange デバウンス用チャネル（バッファ 1 でバーストを吸収）
	notifyCh chan struct{}

	// pendingChanges はデバウンス間隔中に変更があったセッション ID を蓄積する。
	// onChange コールバック発火時にドレインされる。空の場合はブロードキャスト（全セッション更新）。
	pendingMu      sync.Mutex
	pendingChanges map[DeckSessionID]bool

	// JSONL ファイルの fsnotify 監視
	fileWatcher *usage.MultiWatcher

	// 次回 DiscoverExternalSessions の読み込み開始位置（ページネーション用）
	discoveryOffset int
}

// NewManager creates a new session manager.
// ctx is used as the parent context for log streaming goroutines.
func NewManager(ctx context.Context, st *store.Store, cfg ManagerConfig) *Manager {
	m := &Manager{
		sessions:       make(map[DeckSessionID]*Session),
		store:          st,
		usage:          cfg.TranscriptReader,
		ctx:            ctx,
		config:         cfg,
		notifyCh:       make(chan struct{}, 1),
		pendingChanges: make(map[DeckSessionID]bool),
	}
	if m.usage == nil {
		m.usage = usage.NewReader("")
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
	m.backend = newTmuxBackend(runner)

	return m
}

// jjRunner returns the configured jj Runner, falling back to a zero-value Runner
// (which defaults to "jj" executable).
func (c ManagerConfig) jjRunner() *jj.Runner {
	if c.JJ != nil {
		return c.JJ
	}
	return &jj.Runner{}
}

func (m *Manager) jj() *jj.Runner { return m.config.jjRunner() }

// ws returns the workspaces of this Manager's data directory.
// WHY 毎回組み立てる: テストが Manager を作った後で config の関数を差し替える。
func (m *Manager) ws() workspaces { return newWorkspaces(m.config) }

func (m *Manager) runtime() agentruntime.Runtime {
	if m.config.AgentRuntime != nil {
		return m.config.AgentRuntime
	}
	return agentruntime.ClaudeRuntime{Command: m.config.ClaudeCommand}
}

// startSpec builds the runtime command for one launch. Every launch mode goes through
// here so that the session name, the plugin dir and --add-dir are passed the same way.
func (m *Manager) startSpec(mode agentruntime.LaunchMode, runtimeID RuntimeSessionID, workDir, name, repoPath string) agentruntime.StartSpec {
	return m.runtime().StartSpec(agentruntime.StartRequest{
		Mode:           mode,
		SessionID:      string(runtimeID),
		WorkDir:        workDir,
		SessionName:    name,
		PermissionMode: m.config.DefaultPermissionMode,
		PluginDir:      m.config.PluginDir,
		AdditionalArgs: m.buildAddDirArgs(repoPath),
	})
}

// processOpts assembles the ProcessStartOpts shared by every launch mode.
func (m *Manager) processOpts(sessionID DeckSessionID, workDir string, spec agentruntime.StartSpec) ProcessStartOpts {
	env := []string{EnvSessionID + "=" + string(sessionID), EnvDataDir + "=" + m.config.DataDir}
	var onExit []string
	if m.config.DeckCommand != "" {
		env = append(env, EnvCommand+"="+m.config.DeckCommand)
		onExit = []string{m.config.DeckCommand, "hook", "exited", "--session", string(sessionID)}
	}
	return ProcessStartOpts{
		Command: spec.Command,
		WorkDir: workDir,
		Args:    spec.Args,
		Env:     env,
		OnExit:  onExit,
	}
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

// computeActualWorkDir は wsPath と subProjectDir からプロセスの作業ディレクトリを算出する。
// subProjectDir が空のときは wsPath をそのまま返す。
func computeActualWorkDir(wsPath, subProjectDir string) string {
	if subProjectDir == "" {
		return wsPath
	}
	return filepath.Join(wsPath, subProjectDir)
}

// startNewSession is the shared tail of CreateSession and ForkSession.
//
// The row is inserted before the process starts: the process's hooks and the
// TUI's orphan-window cleanup both look the session up in the store, and must
// find it as soon as the window exists. LaunchingAt stays set until the process
// has started, so other processes neither mark the row exited for lacking a
// window nor close it under the starting process.
func (m *Manager) startNewSession(sess *Session, workDir string, spec agentruntime.StartSpec) error {
	// WHY 起動前に確かめる: tmux は存在しない -c のディレクトリを指定されてもエラーにせず、
	// ホームディレクトリでウィンドウを開く（tmux 3.6a で確認）。確かめないと Claude Code が
	// 意図しない場所で動き始める。サブプロジェクトのディレクトリが、ワークスペースを作った
	// リポジトリに含まれていないときに起きる（外側に別の jj リポジトリがある場合など）。
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return fmt.Errorf("作業ディレクトリが見つかりません: %s", workDir)
	}
	sess.mu.RLock()
	rec := sess.recordLocked()
	sess.mu.RUnlock()
	beginLaunch(&rec, time.Now())
	if err := m.store.Insert(rec); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	pid, err := m.backend.StartProcess(sess.ID, m.processOpts(sess.ID, workDir, spec))
	if err != nil {
		if derr := m.store.Delete(string(sess.ID)); derr != nil {
			debuglog.Printf("[startNewSession] store delete after failed start: %v", derr)
		}
		return fmt.Errorf("starting claude code: %w", err)
	}

	bookmark, _ := m.jj().GetNearestBookmark(workDir)
	if _, err := m.store.Update(string(sess.ID), func(r *store.Record) error {
		finishLaunch(r, pid)
		if bookmark != "" {
			r.BookmarkName = bookmark
		}
		return nil
	}); err != nil {
		debuglog.Printf("[startNewSession] recording pid: %v", err)
	}
	m.pruneOldSessions()
	m.Reload()
	return nil
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
		wsPath, err := m.ws().create(repoPath, wsName, jj.WorkspaceOptions{})
		if err != nil {
			return nil, fmt.Errorf("creating jj workspace: %w", err)
		}
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
	spec := m.startSpec(agentruntime.LaunchNew, "", actualWorkDir, sess.Name, repoPath)
	if err := m.startNewSession(sess, actualWorkDir, spec); err != nil {
		if withWorkspace {
			m.ws().discard(repoPath, sess.Name)
		}
		return nil, err
	}
	debuglog.Printf("[CreateSession] process started")
	return m.GetSession(sess.ID), nil
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
	csID := sess.CurrentRuntimeID()
	priorIDs := sess.PriorRuntimeIDs()
	sess.mu.RUnlock()

	current = m.usage.ResolveSessionPath(string(csID))
	for _, id := range priorIDs {
		if p := m.usage.ResolveSessionPath(string(id)); p != "" {
			prior = append(prior, p)
		}
	}
	return current, prior
}

// markVanished marks the session exited if, read again inside the transaction,
// it is still unfinished with no close or launch in progress (see vanished).
// Callers decide that the window is gone from a window list taken before; the
// re-read catches a launch that started in between.
func markVanished(st *store.Store, ur *usage.Reader, sessionID DeckSessionID) error {
	return st.Tx(func(tx *store.Tx) error {
		r, err := tx.Get(string(sessionID))
		if err != nil {
			return err
		}
		now := time.Now()
		if !vanished(r, now) {
			return nil
		}
		others, err := tx.List()
		if err != nil {
			return err
		}
		applyExited(&r, others, ur.HasConversation, now)
		return tx.Put(r)
	})
}

// MarkExited records that the session's process has ended, without a Manager.
// See applyExited for the rules.
func MarkExited(st *store.Store, ur *usage.Reader, sessionID DeckSessionID) error {
	return st.Tx(func(tx *store.Tx) error {
		r, err := tx.Get(string(sessionID))
		if err != nil {
			return err
		}
		others, err := tx.List()
		if err != nil {
			return err
		}
		applyExited(&r, others, ur.HasConversation, time.Now())
		return tx.Put(r)
	})
}

// ResumeSession resumes a completed Claude Code session using --resume.
func (m *Manager) ResumeSession(ctx context.Context, sessionID DeckSessionID) error {
	debuglog.Printf("[ResumeSession] sessionID=%s", sessionID)
	if m.backend.IsActive(sessionID) {
		debuglog.Printf("[ResumeSession] already has active process")
		return fmt.Errorf("session %s already has an active process", sessionID)
	}

	if err := m.adoptExternal(sessionID); err != nil {
		return err
	}

	// beginResume moves the row out of the finished state, so a second resume
	// from another process fails here instead of starting a second window.
	rec, err := m.store.Update(string(sessionID), func(r *store.Record) error {
		return beginResume(r, time.Now())
	})
	if err != nil {
		return err
	}
	csID := ""
	if len(rec.SessionChain) > 0 {
		csID = rec.SessionChain[len(rec.SessionChain)-1]
	}
	debuglog.Printf("[ResumeSession] csID=%q wsPath=%q repoPath=%q atRev=%q parentRev=%q",
		csID, rec.WorkspacePath, rec.RepoPath, rec.LastJJRevision, rec.LastJJParentRevision)

	// fail puts the row back into a finished state when the process could not start.
	fail := func(cause error, markError bool) error {
		if _, err := m.store.Update(string(sessionID), func(r *store.Record) error {
			now := time.Now()
			abortResume(r, now)
			if markError {
				setError(r, cause.Error(), now)
			}
			return nil
		}); err != nil {
			debuglog.Printf("[ResumeSession] restoring status: %v", err)
		}
		m.Reload()
		return cause
	}

	if csID == "" {
		return fail(errors.New("no Claude Code session ID available for resume"), false)
	}

	// ワークスペースがなければ（Kill で削除済み）再作成する。
	// Kill 時に保存した @ / @- の change_id を渡す（ADR 009）。
	// CreateWorkspaceAt が jj edit <@> → jj new <@-> → jj new trunk() の順で試みる。
	wsPath := rec.WorkspacePath
	if wsPath == "" && rec.RepoPath != "" && rec.Name != "" {
		newWsPath, err := m.ws().recreate(rec.RepoPath, rec.Name, rec.SubProjectDir, rec.LastJJRevision, rec.LastJJParentRevision)
		if err != nil {
			debuglog.Printf("[ResumeSession] workspace recreate failed, falling back to repo: %v", err)
			wsPath = rec.RepoPath
		} else {
			wsPath = newWsPath
			if _, err := m.store.Update(string(sessionID), func(r *store.Record) error {
				r.WorkspaceName = rec.Name
				r.WorkspacePath = newWsPath
				return nil
			}); err != nil {
				debuglog.Printf("[ResumeSession] recording workspace: %v", err)
			}
		}
	}

	workDir := wsPath
	if workDir == "" {
		workDir = rec.RepoPath
	}
	if workDir == "" {
		return fail(fmt.Errorf("no work directory available for session %s", sessionID), false)
	}
	debuglog.Printf("[ResumeSession] workDir=%q", workDir)

	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		debuglog.Printf("[ResumeSession] workDir does not exist: %s", workDir)
		return fail(fmt.Errorf("ディレクトリが見つかりません: %s", workDir), true)
	}

	spec := m.startSpec(agentruntime.LaunchResume, RuntimeSessionID(csID), workDir, rec.Name, rec.RepoPath)
	pid, err := m.backend.StartProcess(sessionID, m.processOpts(sessionID, workDir, spec))
	if err != nil {
		debuglog.Printf("[ResumeSession] StartProcess failed: %v", err)
		return fail(fmt.Errorf("resuming claude code: %w", err), false)
	}
	if _, err := m.store.Update(string(sessionID), func(r *store.Record) error {
		finishLaunch(r, pid)
		return nil
	}); err != nil {
		debuglog.Printf("[ResumeSession] recording pid: %v", err)
	}
	m.Reload()
	debuglog.Printf("[ResumeSession] done")
	return nil
}

// adoptExternal stores an external session (discovered from JSONL, memory only)
// as a finished deck session, so it can be resumed like one. No-op for deck sessions.
func (m *Manager) adoptExternal(sessionID DeckSessionID) error {
	sess := m.GetSession(sessionID)
	if sess == nil {
		return nil
	}
	sess.mu.RLock()
	status := sess.Status
	rec := sess.recordLocked()
	sess.mu.RUnlock()
	if status != StatusUnmanaged {
		return nil
	}
	markAdopted(&rec, time.Now())
	return m.store.Tx(func(tx *store.Tx) error {
		if _, err := tx.Get(rec.ID); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return tx.Put(rec)
	})
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
	srcClaudeID := srcSess.CurrentRuntimeID()
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
	wsPath, err := m.ws().create(repoPath, wsName, jj.WorkspaceOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating jj workspace: %w", err)
	}
	// サブプロジェクト対応: ソースが wsPath/subProject で動いていた場合、
	// フォーク先の新ワークスペースでも同じサブディレクトリを使う。
	actualWorkDir := computeActualWorkDir(wsPath, srcSubProjectDir)
	sess.WorkspacePath = actualWorkDir
	sess.WorkspaceName = wsName
	sess.SubProjectDir = srcSubProjectDir
	sess.ForkedFrom = srcClaudeID

	spec := m.startSpec(agentruntime.LaunchFork, srcClaudeID, actualWorkDir, sess.Name, repoPath)
	if err := m.startNewSession(sess, actualWorkDir, spec); err != nil {
		m.ws().discard(repoPath, wsName)
		return nil, fmt.Errorf("starting forked session: %w", err)
	}
	return m.GetSession(sess.ID), nil
}

// Kill forcefully terminates a session and cleans up its workspace directory.
// Session metadata and Claude Code JSONL are preserved for future --resume.
//
// ClosingAt in the store guards against the TUI and the CLI closing the same
// session at once (both would try to remove the same workspace).
func (m *Manager) Kill(sessionID DeckSessionID) error {
	rec, err := m.store.Update(string(sessionID), func(r *store.Record) error {
		return beginClose(r, time.Now())
	})
	if err != nil {
		return err
	}
	// endClose clears ClosingAt and applies fn in one transaction.
	endClose := func(fn func(tx *store.Tx, r *store.Record) error) error {
		err := m.store.Tx(func(tx *store.Tx) error {
			r, err := tx.Get(string(sessionID))
			if err != nil {
				return err
			}
			r.ClosingAt = nil
			if fn != nil {
				if err := fn(tx, &r); err != nil {
					return err
				}
			}
			return tx.Put(r)
		})
		m.Reload()
		return err
	}

	// kill-window はペインごと終了させるので、ペイン内の終了コマンド（hook exited）は動かない。
	// 終了の記録はここで行う。
	if err := m.backend.StopProcess(sessionID, rec.PID); err != nil {
		if cerr := endClose(nil); cerr != nil {
			debuglog.Printf("[Kill] clearing closing flag: %v", cerr)
		}
		return err
	}

	// ワークスペースを消してディスクを回収する（node_modules などがワークスペースごとに複製される）。
	// resume のときに作り直す。
	var atRev, parentRev string
	removeWorkspace := rec.WorkspaceName != "" && rec.RepoPath != ""
	if removeWorkspace {
		atRev, parentRev = m.ws().remove(rec.RepoPath, rec.WorkspaceName)
	}

	return endClose(func(tx *store.Tx, r *store.Record) error {
		others, err := tx.List()
		if err != nil {
			return err
		}
		applyExited(r, others, m.usage.HasConversation, time.Now())
		if removeWorkspace {
			recordWorkspaceRemoved(r, atRev, parentRev)
		}
		return nil
	})
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
	list := m.copySessionsList()
	sortSessions(list)
	return list
}

// ListStored returns the deck sessions in the store, in the same order as
// ListSessions, without a Manager (no tmux, no JSONL). Used by `claude-deck list`.
func ListStored(st *store.Store) ([]Snapshot, error) {
	recs, err := st.List()
	if err != nil {
		return nil, err
	}
	list := make([]*Session, len(recs))
	for i, r := range recs {
		list[i] = newSessionFromRecord(r)
	}
	sortSessions(list)
	snaps := make([]Snapshot, len(list))
	for i, s := range list {
		snaps[i] = s.Snapshot()
	}
	return snaps, nil
}

// sortSessions sorts by status group, then by last activity (newest last).
func sortSessions(list []*Session) {
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
}

// FindSession looks up a session by deck session ID, falling back to its name.
// Name は一意性が保証されない（外部セッションは ClaudeSessionID の先頭 8 文字など）ため、
// 複数一致したときは推測で 1 つを選ばずエラーにする。
func (m *Manager) FindSession(key string) (*Session, error) {
	if sess := m.GetSession(DeckSessionID(key)); sess != nil {
		return sess, nil
	}
	var matches []*Session
	for _, s := range m.copySessionsList() {
		if s.getName() == key {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("session not found: %s", key)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, s := range matches {
			ids[i] = string(s.ID)
		}
		return nil, fmt.Errorf("session name %q is ambiguous; use one of the IDs: %s", key, strings.Join(ids, ", "))
	}
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
