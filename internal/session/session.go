package session

import (
	"strings"
	"sync"
	"time"
)

// Status is where a deck session is in its life: one of the activities while
// its process lives, or how it ended. Whether a session is a deck session at
// all is its Origin, not its Status.
type Status int

const (
	// StatusUnknown is the zero value: the store holds a status this version
	// does not know (written by a newer claude-deck). Such a row is neither live
	// nor ended, and the transitions leave it alone.
	StatusUnknown Status = iota
	StatusRunning
	StatusWaitingApproval
	StatusWaitingAnswer
	StatusIdle
	// StatusSubagentRunning: the main loop has ended its turn while subagents it
	// started in the background still run. The session works without the user.
	StatusSubagentRunning
	StatusCompleted
	StatusError
)

var statusInfo = map[Status]struct{ id, label string }{
	StatusUnknown:         {"unknown", "unknown"},
	StatusRunning:         {"running", "Running"},
	StatusWaitingApproval: {"waiting_approval", "waiting for approval"},
	StatusWaitingAnswer:   {"waiting_answer", "waiting for an answer"},
	StatusIdle:            {"idle", "idle"},
	StatusSubagentRunning: {"subagent_running", "subagents running"},
	StatusCompleted:       {"completed", "completed"},
	StatusError:           {"error", "error"},
}

func (s Status) String() string {
	if info, ok := statusInfo[s]; ok {
		return info.label
	}
	return statusInfo[StatusUnknown].label
}

// ID returns a stable lowercase ASCII identifier for use in IPC and machine-readable output.
// Unlike String(), this method is not localized and safe to compare programmatically.
// It is what the store's status column holds.
func (s Status) ID() string {
	if info, ok := statusInfo[s]; ok {
		return info.id
	}
	return statusInfo[StatusUnknown].id
}

// NeedsAttention returns true if the session requires user action.
func (s Status) NeedsAttention() bool {
	return s == StatusWaitingApproval || s == StatusWaitingAnswer
}

// IsLive reports whether the status is one of the activities of a running process.
func (s Status) IsLive() bool {
	switch s {
	case StatusRunning, StatusWaitingApproval, StatusWaitingAnswer, StatusIdle, StatusSubagentRunning:
		return true
	}
	return false
}

// IsTerminal returns true if the status is a final state (no further transitions expected).
func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusError
}

// Activity is what a live process reports about itself: through the
// deck-status hooks for Claude Code, through the JSONL for Codex. Completed and
// Error are not activities; they come from the process ending.
type Activity int

const (
	ActivityRunning Activity = iota + 1
	ActivityIdle
	ActivityWaitingApproval
	ActivityWaitingAnswer
	ActivitySubagentRunning
)

var activityStatus = map[Activity]Status{
	ActivityRunning:         StatusRunning,
	ActivityIdle:            StatusIdle,
	ActivityWaitingApproval: StatusWaitingApproval,
	ActivityWaitingAnswer:   StatusWaitingAnswer,
	ActivitySubagentRunning: StatusSubagentRunning,
}

// Status is the status a session takes on while doing a.
func (a Activity) Status() Status { return activityStatus[a] }

// ActivityFromID parses an activity by its Status ID ("running", "idle", …).
func ActivityFromID(id string) (Activity, bool) {
	for a, s := range activityStatus {
		if s.ID() == id {
			return a, true
		}
	}
	return 0, false
}

// Origin tells who started the session's process.
type Origin int

const (
	// OriginDeck: claude-deck started it. The session has a store row.
	OriginDeck Origin = iota
	// OriginExternal: found in the runtime's transcripts, started elsewhere.
	// Kept in memory only; resuming it adopts it as a deck session.
	OriginExternal
)

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
//   - Store (SQLite deck.db): ID, Name, RepoPath, RepoName, Isolation,
//     SessionChain, Status, FinishedAt, PID
//   - JSONL (runtime primary): Prompt, PermissionMode, StartedAt, LastActivity
//   - Runtime only: CurrentTool
type Session struct {
	mu sync.RWMutex

	// --- Persisted in store (claude-deck metadata) ---
	// Fields marked "immutable after creation" are set once by CreateSession /
	// newExternalSession and never mutated thereafter, so callers may read them
	// without holding mu. See hasManagedSessionAtWorkspaceLocked for an example.
	ID DeckSessionID `json:"id"` // immutable after creation
	// Origin is OriginExternal for a session found in the transcripts, until it
	// is adopted. Not in the store: only deck sessions have rows.
	Origin Origin `json:"-"`
	Name   string `json:"name"`
	// Alias は利用者かセッション自身が付けた表示用の名前（`claude-deck alias`）。無ければ空。
	// WHY NOT Name を書き換える: Name は jj ワークスペースの名前とディレクトリ名、Claude Code の --name を兼ねる。
	// 再開はこの名前でワークスペースを作り直し、gc はこの名前で持ち主を判定する。
	Alias string `json:"-"`
	// Pinned は一覧の一番下の組に置くかどうか（TUI の p）。
	Pinned   bool   `json:"-"`
	RepoPath string `json:"repo_path"` // immutable after creation
	RepoName string `json:"repo_name"` // immutable after creation
	// Isolation is where the process runs: directly in a directory, or in the
	// session's jj workspace. Set by CreateSession/ForkSession, changed by Kill
	// and ResumeSession through the store.
	Isolation     Isolation `json:"-"`
	SubProjectDir string    `json:"sub_project_dir,omitempty"` // immutable after creation; relative path to sub-project
	// SessionChain は agent runtime が割り当てるセッション ID の履歴（古い順）。
	// /clear や compact など runtime 固有の reset 操作で末尾に新 ID が追加される。
	// 現在の ID は SessionChain[len-1]、旧 ID はそれ以前の要素。
	// アクセスには CurrentRuntimeID() / PriorRuntimeIDs() を使うこと。
	SessionChain []RuntimeSessionID `json:"session_chain,omitempty"`
	// ForkedFrom は、このセッションがフォークで作られたときの分岐元の runtime セッション ID。
	// 別のセッションの SessionChain の要素を指す。フォークでなければ空。immutable after creation.
	// WHY deck の ID でなく runtime の ID で持つ: 分岐元のセッションはその後 /clear で先へ進むので、
	// deck の ID だけではどの文脈から分かれたかが分からなくなる。
	// WHY NOT JSONL から復元する: フォーク先の JSONL に分岐元の ID は残らない（2026-10-09、Claude Code 2.1.295 で
	// ~/.claude/projects の 48 ファイルを確認）。
	ForkedFrom    RuntimeSessionID `json:"forked_from,omitempty"`
	Status        Status           `json:"status"`
	FinishedAt    *time.Time       `json:"finished_at,omitempty"`
	PID           int              `json:"pid,omitempty"`
	TerminalTitle string           `json:"terminal_title,omitempty"` // OSC 0/2 で設定されたターミナルタイトル（セッション一覧表示用）
	BookmarkName  string           `json:"bookmark_name,omitempty"`  // jj の最近接ブックマーク名（セッション一覧表示用）

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
	return hasProcess(s.Origin, s.Status)
}

// hasProcess is the one rule for whether claude-deck has a live process for a
// session: it started it, and the store says it is doing something.
func hasProcess(origin Origin, status Status) bool {
	return origin == OriginDeck && status.IsLive()
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
	Pinned           bool
	RepoPath         string
	RepoName         string
	Isolation        Isolation
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
	Origin         Origin
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

// HasProcess reports whether claude-deck has a live process for the session.
func (s Snapshot) HasProcess() bool { return hasProcess(s.Origin, s.Status) }

// CanResume reports whether r starts the session: an external session is
// adopted, a finished deck session is started again.
func (s Snapshot) CanResume() bool { return s.Origin == OriginExternal || s.Status.IsTerminal() }

// DisplayName returns the alias, or the session name when no alias was given.
func (s Snapshot) DisplayName() string {
	if s.Alias != "" {
		return s.Alias
	}
	return s.Name
}

// WorkDir returns the effective working directory for this session: where its
// process runs, or the repository while its workspace is removed.
func (s Snapshot) WorkDir() string {
	return s.Isolation.workDirOr(s.RepoPath)
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
		Pinned:           s.Pinned,
		RepoPath:         s.RepoPath,
		RepoName:         s.RepoName,
		Isolation:        s.Isolation,
		SubProjectDir:    s.SubProjectDir,
		RuntimeSessionID: s.CurrentRuntimeID(),
		PriorRuntimeIDs:  s.PriorRuntimeIDs(),
		ForkedFrom:       s.ForkedFrom,
		ClearCount:       max(0, len(s.SessionChain)-1),
		Display:          s.displayChannelLocked(),
		Origin:           s.Origin,
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
	return sortTimeOf(s.LastActivity, s.FinishedAt, s.StartedAt)
}

// sortTimeOf is the latest of the given times, ignoring the missing ones.
// Sessions in memory and rows in the store are ordered by it.
//
// WHY 最も新しい時刻: store の LastActivity を書くのは TUI の正常終了時（PersistAll）
// だけで、古いまま残ることがある。それを優先すると、今日閉じたセッションが prune で
// 古いものとして消される。FinishedAt と StartedAt は store を書く遷移が入れる。
func sortTimeOf(lastActivity time.Time, finishedAt *time.Time, startedAt time.Time) time.Time {
	t := startedAt
	if lastActivity.After(t) {
		t = lastActivity
	}
	if finishedAt != nil && finishedAt.After(t) {
		t = *finishedAt
	}
	return t
}

func (s *Session) isPinned() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Pinned
}

// sortGroup returns a numeric priority for status-based sorting.
// Lower values appear at the top of the list (least important).
// Higher values appear at the bottom (most important, closest to user's eyes).
//
//	0: 外部セッション / Completed / Error / Unknown（非アクティブ）
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
