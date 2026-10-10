package session

import (
	"strings"
	"sync"
	"time"
)

// Status represents the current state of a Claude Code session.
type Status int

const (
	StatusRunning Status = iota
	StatusWaitingApproval
	StatusWaitingAnswer
	StatusCompleted
	StatusError
	StatusIdle
	StatusUnmanaged // 外部セッション（claude-deck が起動していない Claude Code セッション）
	// StatusSubagentRunning: the main loop has ended its turn while subagents it
	// started in the background still run. The session works without the user.
	StatusSubagentRunning
)

func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "Running"
	case StatusWaitingApproval:
		return "Approve待ち"
	case StatusWaitingAnswer:
		return "質問待ち"
	case StatusCompleted:
		return "完了"
	case StatusError:
		return "エラー"
	case StatusIdle:
		return "アイドル"
	case StatusUnmanaged:
		return "外部"
	case StatusSubagentRunning:
		return "サブエージェント実行中"
	default:
		return "Unknown"
	}
}

// ID returns a stable lowercase ASCII identifier for use in IPC and machine-readable output.
// Unlike String(), this method is not localized and safe to compare programmatically.
func (s Status) ID() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusWaitingApproval:
		return "waiting_approval"
	case StatusWaitingAnswer:
		return "waiting_answer"
	case StatusCompleted:
		return "completed"
	case StatusError:
		return "error"
	case StatusIdle:
		return "idle"
	case StatusUnmanaged:
		return "unmanaged"
	case StatusSubagentRunning:
		return "subagent_running"
	default:
		return "unknown"
	}
}

// NeedsAttention returns true if the session requires user action.
func (s Status) NeedsAttention() bool {
	return s == StatusWaitingApproval || s == StatusWaitingAnswer
}

// IsTerminal returns true if the status is a final state (no further transitions expected).
func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusError
}

// DisplayChannel describes what data source should be used to render a
// session's detail pane. Derived from the session's current state rather
// than stored — it's a projection, not persisted data.
type DisplayChannel int

const (
	// DisplayJSONL renders structured JSONL log entries.
	// Used for completed/archived sessions.
	DisplayJSONL DisplayChannel = iota
	// DisplayTmux means the session's process is owned by tmux.
	// The user interacts directly in the tmux window; claude-deck shows no detail content.
	DisplayTmux
)

func (d DisplayChannel) String() string {
	switch d {
	case DisplayJSONL:
		return "jsonl"
	case DisplayTmux:
		return "tmux"
	default:
		return "unknown"
	}
}

// Session represents a single agent runtime session tracked by claude-deck.
//
// Data sources:
//   - Store (persisted as JSON): ID, Name, RepoPath, RepoName, WorkspacePath,
//     WorkspaceName, SessionChain, Status, FinishedAt, PID
//   - JSONL (runtime primary): Prompt, PermissionMode, StartedAt, LastActivity
//   - Runtime only: CurrentTool
type Session struct {
	mu sync.RWMutex

	// --- Persisted in store (claude-deck metadata) ---
	// Fields marked "immutable after creation" are set once by CreateSession /
	// newExternalSession and never mutated thereafter, so callers may read them
	// without holding mu. See hasManagedSessionAtWorkspaceLocked for an example.
	ID   DeckSessionID `json:"id"` // immutable after creation
	Name string        `json:"name"`
	// Alias は利用者かセッション自身が付けた表示用の名前（`claude-deck alias`）。無ければ空。
	// Name は tmux のウィンドウ名とワークスペース名を兼ねるので変えられない。
	Alias    string `json:"-"`
	RepoPath string `json:"repo_path"` // immutable after creation
	RepoName string `json:"repo_name"` // immutable after creation
	// WorkspacePath is the actual Claude Code working directory and may include a sub-project
	// subdirectory (i.e., <wsRoot>/<SubProjectDir>). WorkspaceName is the root jj workspace
	// name; the root can be reconstructed as DataDir/workspace/<encodedRepo>/<WorkspaceName>.
	// Kill removes the root via WorkspaceName, not WorkspacePath.
	// set by CreateSession/ForkSession; cleared by Kill; requires mu except where benign race is documented.
	WorkspacePath string `json:"workspace_path"`
	WorkspaceName string `json:"workspace_name"`            // set by CreateSession; cleared by Kill; requires mu except where benign race is documented
	SubProjectDir string `json:"sub_project_dir,omitempty"` // immutable after creation; relative path to sub-project
	// SessionChain は agent runtime が割り当てるセッション ID の履歴（古い順）。
	// /clear や compact など runtime 固有の reset 操作で末尾に新 ID が追加される。
	// 現在の ID は SessionChain[len-1]、旧 ID はそれ以前の要素。
	// アクセスには CurrentRuntimeID() / PriorRuntimeIDs() を使うこと。
	SessionChain []RuntimeSessionID `json:"session_chain,omitempty"`
	// ForkedFrom は、このセッションがフォークで作られたときの分岐元の runtime セッション ID。
	// 別のセッションの SessionChain の要素を指す。フォークでなければ空。immutable after creation.
	// WHY deck の ID でなく runtime の ID で持つ: 分岐元のセッションはその後 /clear で先へ進むので、
	// deck の ID だけではどの文脈から分かれたかが分からなくなる（ADR 012）。
	ForkedFrom    RuntimeSessionID `json:"forked_from,omitempty"`
	Status        Status           `json:"status"`
	FinishedAt    *time.Time       `json:"finished_at,omitempty"`
	PID           int              `json:"pid,omitempty"`
	TerminalTitle string           `json:"terminal_title,omitempty"` // OSC 0/2 で設定されたターミナルタイトル（セッション一覧表示用）
	BookmarkName  string           `json:"bookmark_name,omitempty"`  // jj の最近接ブックマーク名（セッション一覧表示用）
	// Kill 時に保存した @ / @- の change_id。Resume 時のワークスペース再作成で使う（ADR 009）。
	// 常にペアで更新すること。
	LastJJRevision       string `json:"last_jj_revision,omitempty"`
	LastJJParentRevision string `json:"last_jj_parent_revision,omitempty"`

	// --- Hydrated from JSONL (JSONL が最新値を上書きするが、ストアにも保存して再起動時に即表示) ---
	Prompt         string    `json:"prompt,omitempty"`
	PermissionMode string    `json:"permission_mode,omitempty"`
	StartedAt      time.Time `json:"started_at,omitzero"`
	LastActivity   time.Time `json:"last_activity,omitzero"`

	// --- Runtime fields (not persisted, protected by sess.mu unless noted) ---
	CurrentTool  string `json:"-"` // パーサー検出中のツール名
	ErrorMessage string `json:"-"` // 終了の理由（store の error_message）
}

// hasProcessLocked reports whether the session has a running process.
// WHY Status から導く: プロセスを起動・終了させるのは CLI やペイン内の終了コマンドなど TUI 以外の
// プロセスのこともあり、TUI はそれを store のステータスでしか知り得ない。
// Caller must hold s.mu (read).
func (s *Session) hasProcessLocked() bool {
	return !s.Status.IsTerminal() && s.Status != StatusUnmanaged
}

// displayChannelLocked returns the appropriate display data source for this session.
// Caller must hold s.mu (read).
func (s *Session) displayChannelLocked() DisplayChannel {
	if s.hasProcessLocked() {
		return DisplayTmux // tmux owns the process → user interacts via external terminal
	}
	return DisplayJSONL // no active process → show structured logs
}

// Elapsed returns the duration since the session started.
func (s *Session) Elapsed() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.FinishedAt != nil {
		if !s.StartedAt.IsZero() {
			return s.FinishedAt.Sub(s.StartedAt)
		}
		return 0
	}
	if s.StartedAt.IsZero() {
		return 0
	}
	return time.Since(s.StartedAt)
}

// GetStatus returns the current session status safely.
func (s *Session) GetStatus() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Status
}

// SetCurrentTool updates the current tool name safely.
func (s *Session) SetCurrentTool(tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CurrentTool = tool
}

// IsProcessAlive reports whether the session has a running process.
func (s *Session) IsProcessAlive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasProcessLocked()
}

// Snapshot is a read-only copy of session state, safe to use without locks.
type Snapshot struct {
	ID   DeckSessionID
	Name string
	// Alias is the label given with `claude-deck alias`, or "".
	Alias            string
	RepoPath         string
	RepoName         string
	WorkspacePath    string
	SubProjectDir    string
	RuntimeSessionID RuntimeSessionID
	// RuntimeSessionIDs contains all historical runtime session IDs except the current one,
	// in chronological order. Populated from SessionChain[:-1].
	PriorRuntimeIDs []RuntimeSessionID
	// ForkedFrom is the runtime session ID this session was forked from, or "".
	ForkedFrom RuntimeSessionID
	// ClearCount is the number of /clear (or compact) operations performed in
	// this session. 0 means the original session; 1 means cleared once, etc.
	// Derived from len(SessionChain) - 1.
	ClearCount     int
	Display        DisplayChannel
	Status         Status
	Prompt         string
	PermissionMode string
	StartedAt      time.Time
	LastActivity   time.Time
	FinishedAt     *time.Time
	CurrentTool    string
	ErrorMessage   string
	TerminalTitle  string
	BookmarkName   string
	Elapsed        time.Duration
}

// DisplayName returns the alias, or the session name when no alias was given.
func (s Snapshot) DisplayName() string {
	if s.Alias != "" {
		return s.Alias
	}
	return s.Name
}

// WorkDir returns the effective working directory for this session.
// WorkspacePath があればそれを、なければ RepoPath をフォールバックとして返す。
func (s Snapshot) WorkDir() string {
	if s.WorkspacePath != "" {
		return s.WorkspacePath
	}
	return s.RepoPath
}

// Snapshot returns a consistent, lock-free copy of the session state.
func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var elapsed time.Duration
	if s.FinishedAt != nil {
		if !s.StartedAt.IsZero() {
			elapsed = s.FinishedAt.Sub(s.StartedAt)
		}
	} else if !s.StartedAt.IsZero() {
		elapsed = time.Since(s.StartedAt)
	}

	// FinishedAt はポインタなのでディープコピーする
	var finishedAt *time.Time
	if s.FinishedAt != nil {
		t := *s.FinishedAt
		finishedAt = &t
	}

	snap := Snapshot{
		ID:               s.ID,
		Name:             s.Name,
		Alias:            s.Alias,
		RepoPath:         s.RepoPath,
		RepoName:         s.RepoName,
		WorkspacePath:    s.WorkspacePath,
		SubProjectDir:    s.SubProjectDir,
		RuntimeSessionID: s.CurrentRuntimeID(),
		PriorRuntimeIDs:  s.PriorRuntimeIDs(),
		ForkedFrom:       s.ForkedFrom,
		ClearCount:       max(0, len(s.SessionChain)-1),
		Display:          s.displayChannelLocked(),
		Status:           s.Status,
		Prompt:           s.Prompt,
		PermissionMode:   s.PermissionMode,
		StartedAt:        s.StartedAt,
		LastActivity:     s.LastActivity,
		FinishedAt:       finishedAt,
		CurrentTool:      s.CurrentTool,
		ErrorMessage:     s.ErrorMessage,
		TerminalTitle:    s.TerminalTitle,
		BookmarkName:     s.BookmarkName,
		Elapsed:          elapsed,
	}
	return snap
}

// CurrentRuntimeID returns the active runtime session ID, or "" if none.
// Must be called with mu held (at least for reading), or use Snapshot.RuntimeSessionID.
func (s *Session) CurrentRuntimeID() RuntimeSessionID {
	if len(s.SessionChain) == 0 {
		return ""
	}
	return s.SessionChain[len(s.SessionChain)-1]
}

// ChainIDs returns a copy of all runtime session IDs in this session's chain,
// from oldest to newest. The last element is the current active ID.
// Thread-safe; acquires mu for reading.
func (s *Session) ChainIDs() []RuntimeSessionID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]RuntimeSessionID, len(s.SessionChain))
	copy(ids, s.SessionChain)
	return ids
}

// PriorRuntimeIDs returns all historical runtime session IDs excluding the current one.
// Returns nil if there is no history.
// Must be called with mu held for reading (RLock is sufficient).
// Calling from within an already-held RLock is safe: sync.RWMutex allows multiple
// concurrent RLock holders. Snapshot() calls this while holding mu.RLock().
func (s *Session) PriorRuntimeIDs() []RuntimeSessionID {
	if len(s.SessionChain) <= 1 {
		return nil
	}
	prior := make([]RuntimeSessionID, len(s.SessionChain)-1)
	copy(prior, s.SessionChain[:len(s.SessionChain)-1])
	return prior
}

// getName returns the session name under lock for sorting.
func (s *Session) getName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Name
}

// MatchesFilter reports whether the session matches the given filter text.
// Matching is case-insensitive substring search over "repoPath/name alias".
// An empty text always matches (no filter applied).
// Uses a targeted RLock on RepoPath+Name only, avoiding a full Snapshot() call.
func (s *Session) MatchesFilter(text string) bool {
	if text == "" {
		return true
	}
	s.mu.RLock()
	target := strings.ToLower(s.RepoPath + "/" + s.Name + " " + s.Alias)
	s.mu.RUnlock()
	return strings.Contains(target, text)
}

// sortTime returns the best available timestamp for chronological sorting.
func (s *Session) sortTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.LastActivity.IsZero() {
		return s.LastActivity
	}
	if s.FinishedAt != nil {
		return *s.FinishedAt
	}
	return s.StartedAt
}

// sortGroup returns a numeric priority for status-based sorting.
// Lower values appear at the top of the list (least important).
// Higher values appear at the bottom (most important, closest to user's eyes).
//
//	0: Unmanaged / Completed / Error（非アクティブ）
//	1: Idle
//	2: Running / SubagentRunning
//	3: WaitingApproval / WaitingAnswer（要手動介入）
func (s *Session) sortGroup() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch s.Status {
	case StatusWaitingApproval, StatusWaitingAnswer:
		return 3
	case StatusRunning, StatusSubagentRunning:
		return 2
	case StatusIdle:
		return 1
	default:
		return 0
	}
}

// NewSession creates a new session with the given parameters.
func NewSession(repoPath, repoName string) *Session {
	s := &Session{
		ID:            GenerateSessionID(),
		Name:          GenerateWorkspaceName(),
		RepoPath:      repoPath,
		RepoName:      repoName,
		TerminalTitle: "New Session",
		Status:        StatusIdle,
		StartedAt:     time.Now(),
	}
	return s
}
