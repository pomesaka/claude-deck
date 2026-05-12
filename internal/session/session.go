package session

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/usage"
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

// canTransitionTo reports whether a transition from s to next is valid.
// Invalid transitions are logged but not blocked — this is a diagnostic aid,
// not a hard gate. The transition table codifies the state diagram in CLAUDE.md.
func (s Status) canTransitionTo(next Status) bool {
	if s == next {
		return true // identity transition is always allowed (idempotent)
	}
	switch s {
	case StatusIdle:
		// Idle → Running (hook: UserPromptSubmit/PreToolUse), Completed (process exit), Error (directory missing)
		return next == StatusRunning || next == StatusCompleted || next == StatusError
	case StatusRunning:
		// Running → Idle (hook: Stop/PostToolUseFailure/StopFailure), WaitingApproval, WaitingAnswer,
		//           Completed (process exit), Error
		return next == StatusIdle || next == StatusWaitingApproval || next == StatusWaitingAnswer ||
			next == StatusCompleted || next == StatusError
	case StatusWaitingApproval:
		// WaitingApproval → Running (approved), Idle (hook stop), Completed (process exit/kill)
		return next == StatusRunning || next == StatusIdle || next == StatusCompleted || next == StatusError
	case StatusWaitingAnswer:
		// WaitingAnswer → Running (answered), Idle (hook stop), Completed (process exit/kill)
		return next == StatusRunning || next == StatusIdle || next == StatusCompleted || next == StatusError
	case StatusCompleted:
		// Terminal state, but Resume resets to Idle via setStatusLocked
		return next == StatusIdle || next == StatusError
	case StatusError:
		// Terminal state, but Resume resets to Idle
		return next == StatusIdle
	case StatusUnmanaged:
		// External sessions don't transition (display-only)
		return false
	default:
		return false
	}
}

// SessionPhase represents the high-level lifecycle phase of a session.
// Status captures fine-grained state (Running/Idle/WaitingApproval/...),
// while Phase captures the coarse-grained lifecycle stage derived from
// Status + managed flag. This eliminates scattered "if managed && status != ..."
// checks across TUI and Manager code.
type SessionPhase int

const (
	// PhaseActive means a PTY process is alive and managed by the Manager.
	PhaseActive SessionPhase = iota
	// PhaseArchived means the session has finished (Completed or Error).
	PhaseArchived
	// PhaseExternal means the session was discovered from JSONL but not launched by claude-deck.
	PhaseExternal
)

func (p SessionPhase) String() string {
	switch p {
	case PhaseActive:
		return "Active"
	case PhaseArchived:
		return "Archived"
	case PhaseExternal:
		return "External"
	default:
		return "Unknown"
	}
}

// RunningProcess is the sentinel type for an active session process.
// Stored as an atomic pointer so nil/non-nil atomically signals whether a process is attached.
// The empty struct avoids per-session heap allocation while preserving the ability to
// add fields in the future without changing the atomic-pointer contract.
type RunningProcess struct{}

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

// PricingPolicy defines token pricing rates per million tokens (USD).
// This is a Value Object: immutable, compared by value, no identity.
// It captures the domain concept of "how much does usage cost" and allows
// TokenUsage to calculate its own cost without depending on infrastructure.
type PricingPolicy struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheWritePerMTok float64
	CacheReadPerMTok  float64
}

// TokenUsage tracks token consumption for a session.
type TokenUsage struct {
	InputTokens              int     `json:"input_tokens"`
	OutputTokens             int     `json:"output_tokens"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens"`
	EstimatedCostUSD         float64 `json:"estimated_cost_usd"`
}

// TotalTokens returns the sum of input and output tokens.
func (t TokenUsage) TotalTokens() int {
	return t.InputTokens + t.OutputTokens
}

// TokenUsageFromStats converts a usage.TokenStats (read from JSONL) to a
// TokenUsage Value Object. Centralises the field mapping between the two types
// so callers don't need to know the structural isomorphism.
func TokenUsageFromStats(s usage.TokenStats) TokenUsage {
	return TokenUsage{
		InputTokens:              s.InputTokens,
		OutputTokens:             s.OutputTokens,
		CacheCreationInputTokens: s.CacheCreationInputTokens,
		CacheReadInputTokens:     s.CacheReadInputTokens,
		EstimatedCostUSD:         s.EstimatedCostUSD,
	}
}

// Add returns a new TokenUsage with input and output tokens incremented.
// EstimatedCostUSD is not recalculated; use ApplyJSONLTokens (which calls EstimateCost)
// for authoritative cost tracking.
func (t TokenUsage) Add(input, output int) TokenUsage {
	t.InputTokens += input
	t.OutputTokens += output
	return t
}

// EstimateCost calculates an approximate USD cost based on token usage and pricing policy.
// This places cost calculation in the domain type that best knows its own data,
// rather than in infrastructure (usage package).
func (t TokenUsage) EstimateCost(p PricingPolicy) float64 {
	cost := float64(t.InputTokens) / 1_000_000 * p.InputPerMTok
	cost += float64(t.OutputTokens) / 1_000_000 * p.OutputPerMTok
	cost += float64(t.CacheCreationInputTokens) / 1_000_000 * p.CacheWritePerMTok
	cost += float64(t.CacheReadInputTokens) / 1_000_000 * p.CacheReadPerMTok
	return cost
}

// runtimeFields holds high-frequency state that is updated by the JSONL streaming goroutine.
// rt.mu は sess.mu と独立しているため、JSONL 書き込みと TUI スナップショットが競合しない。
// Lock ordering: rt.mu と sess.mu は同時に保持しない。
type runtimeFields struct {
	mu sync.RWMutex

	JSONLLogEntries []usage.LogEntry // JSONL 由来の構造化ログ（StreamSession で更新）
}

// Session represents a single Claude Code session.
//
// Data sources:
//   - Store (persisted as JSON): ID, Name, RepoPath, RepoName, WorkspacePath,
//     WorkspaceName, SessionChain, Status, FinishedAt, PID
//   - JSONL (Claude Code primary): Prompt, PermissionMode, StartedAt, TokenUsage
//   - Runtime only: rt.JSONLLogEntries, CurrentTool
//
// Lock ordering (ABBA デッドロック防止):
//   - rt.mu: JSONL ログ専用
//   - mu:    その他全フィールド
//   - rt.mu と sess.mu は同時に保持しない
type Session struct {
	mu sync.RWMutex

	// rt は JSONL ログなど高頻度更新フィールドをまとめた struct。
	// 詳細は runtimeFields のコメントを参照。
	rt runtimeFields

	// --- Persisted in store (claude-deck metadata) ---
	// Fields marked "immutable after creation" are set once by CreateSession /
	// newExternalSession and never mutated thereafter, so callers may read them
	// without holding mu. See hasManagedSessionAtWorkspaceLocked for an example.
	ID            DeckSessionID `json:"id"`            // immutable after creation
	Name          string        `json:"name"`
	RepoPath      string        `json:"repo_path"`     // immutable after creation
	RepoName      string        `json:"repo_name"`     // immutable after creation
	// WorkspacePath is the actual Claude Code working directory and may include a sub-project
	// subdirectory (i.e., <wsRoot>/<SubProjectDir>). WorkspaceName is the root jj workspace
	// name; the root can be reconstructed as DataDir/workspace/<encodedRepo>/<WorkspaceName>.
	// Kill removes the root via WorkspaceName, not WorkspacePath.
	// set by CreateSession/ForkSession; cleared by Kill; requires mu except where benign race is documented.
	WorkspacePath string `json:"workspace_path"`
	WorkspaceName string `json:"workspace_name"` // set by CreateSession; cleared by Kill; requires mu except where benign race is documented
	SubProjectDir string `json:"sub_project_dir,omitempty"` // immutable after creation; relative path to sub-project
	// SessionChain は Claude Code が割り当てるセッション ID の履歴（古い順）。
	// /clear や compact のたびに末尾に新 ID が追加される。
	// 現在の ID は SessionChain[len-1]、旧 ID はそれ以前の要素。
	// アクセスには CurrentClaudeID() / PriorClaudeIDs() を使うこと。
	SessionChain  []ClaudeSessionID `json:"session_chain,omitempty"`
	Status        Status            `json:"status"`
	FinishedAt    *time.Time        `json:"finished_at,omitempty"`
	PID           int               `json:"pid,omitempty"`
	TerminalTitle string            `json:"terminal_title,omitempty"` // OSC 0/2 で設定されたターミナルタイトル（セッション一覧表示用）
	BookmarkName  string            `json:"bookmark_name,omitempty"`  // jj の最近接ブックマーク名（セッション一覧表示用）

	// --- Hydrated from JSONL (JSONL が最新値を上書きするが、ストアにも保存して再起動時に即表示) ---
	Prompt         string     `json:"prompt,omitempty"`
	PermissionMode string     `json:"permission_mode,omitempty"`
	StartedAt      time.Time  `json:"started_at,omitzero"`
	LastActivity   time.Time  `json:"last_activity,omitzero"`
	TokenUsage     TokenUsage `json:"token_usage,omitzero"`

	// --- Runtime fields (not persisted, protected by sess.mu unless noted) ---
	CurrentTool  string `json:"-"` // パーサー検出中のツール名
	ErrorMessage string `json:"-"` // パーサーが検知したエラー行

	// process は非 nil の間だけプロセスが生存中であることを表す。
	// nil = 停止中（Completed/Error/未起動）
	// non-nil = プロセス生存中（Backend が StartProcess で attach する）
	// AttachProcess / DetachProcess 以外では直接 Store/Swap しないこと。
	process atomic.Pointer[RunningProcess]
}

// displayChannel returns the appropriate display data source for this session.
// Reads process via atomic load; mu need not be held.
func (s *Session) displayChannel() DisplayChannel {
	if s.process.Load() == nil {
		return DisplayJSONL // no active process → show structured logs
	}
	return DisplayTmux // tmux owns the process → user interacts via external terminal
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

// SetStatus updates the session status safely.
func (s *Session) SetStatus(status Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStatusLocked(status)
}

// setStatusLocked updates status under an already-held write lock.
// FinishedAt は Completed/Error 時のみ自動設定される。
// 不正な遷移はデバッグログに記録するが、ブロックはしない（診断用）。
func (s *Session) setStatusLocked(status Status) {
	if !s.Status.canTransitionTo(status) {
		debuglog.Printf("[session:%s] unexpected transition %s → %s", s.ID, s.Status, status)
	}
	s.Status = status
	if status == StatusCompleted || status == StatusError {
		now := time.Now()
		s.FinishedAt = &now
	}
}

// SetErrorStatus updates the session to error state with a reason message.
func (s *Session) SetErrorStatus(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStatusLocked(StatusError)
	s.ErrorMessage = msg
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

// AttachProcess records that a process has started for this session.
// Called by backends (tmuxBackend) before the exit-watcher goroutine starts.
// Must NOT be called with mu held — this method acquires mu internally,
// so calling with mu already held would self-deadlock.
func (s *Session) AttachProcess(pid int) {
	s.mu.Lock()
	if pid > 0 {
		s.PID = pid
	}
	// Store process sentinel under mu so PID and process pointer are set atomically.
	// A concurrent Snapshot() or DetachProcess() cannot observe PID≠0 with process=nil.
	s.process.Store(&RunningProcess{})
	s.mu.Unlock()
}

// reconcileStatusFromStore corrects the session status when loaded from the store.
// ストア復元直後に呼び出し、保存時に実行中だったセッションが実際には死んでいる場合に
// StatusCompleted へ補正し FinishedAt を記録する。
// StatusRunning / WaitingApproval / WaitingAnswer / Idle のいずれかで PID が生存していなければ補正し、
// true を返す。補正が不要なときは false を返す。
//
// LoadExisting と SyncNewFromStore の両方から呼ばれる共通ロジック。
// ワーキングディレクトリの存在確認など起動時固有の補正は呼び出し側で行うこと。
//
// 前提: 呼び出し時点で s は他の goroutine に公開されていない（m.sessions への登録前の
// deserialize 直後のローカル変数）。そのため s.mu を取得せずにフィールドを直書きしている。
// 登録済みセッションに対して呼ぶのは禁止。
func (s *Session) reconcileStatusFromStore() bool {
	switch s.Status {
	case StatusRunning, StatusWaitingApproval, StatusWaitingAnswer:
		if !isProcessAlive(s.PID) {
			s.Status = StatusCompleted
			now := time.Now()
			s.FinishedAt = &now
			return true
		}
	case StatusIdle:
		// PID=0 は「一度も起動していない」を意味するため Completed に補正しない。
		// PID が設定されていてプロセスが死んでいる場合のみ補正する。
		if s.PID > 0 && !isProcessAlive(s.PID) {
			s.Status = StatusCompleted
			now := time.Now()
			s.FinishedAt = &now
			return true
		}
	}
	return false
}

// DetachProcess clears the running process context.
// Called by Manager when a process exits.
// mu is not required: atomic.Pointer.Store provides the necessary atomicity.
// Unlike AttachProcess (which updates both PID and process under mu for consistency),
// DetachProcess only clears the process sentinel — PID is intentionally left intact
// for post-exit identification.
func (s *Session) DetachProcess() {
	s.process.Store(nil)
}

// IsProcessAlive reports whether a tmux process is currently attached to this session.
// Thread-safe: uses atomic load, no lock required.
func (s *Session) IsProcessAlive() bool {
	return s.process.Load() != nil
}

// AddTokens updates token usage safely.
func (s *Session) AddTokens(input, output int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TokenUsage = s.TokenUsage.Add(input, output)
}

// GetStructuredLogs returns a copy of the JSONL-derived structured log entries.
func (s *Session) GetStructuredLogs() []usage.LogEntry {
	s.rt.mu.RLock()
	defer s.rt.mu.RUnlock()
	if len(s.rt.JSONLLogEntries) == 0 {
		return nil
	}
	entries := make([]usage.LogEntry, len(s.rt.JSONLLogEntries))
	copy(entries, s.rt.JSONLLogEntries)
	return entries
}

// Snapshot is a read-only copy of session state, safe to use without locks.
type Snapshot struct {
	ID              DeckSessionID
	Name            string
	RepoPath        string
	RepoName        string
	WorkspacePath   string
	SubProjectDir   string
	ClaudeSessionID ClaudeSessionID
	// PriorClaudeIDs contains all historical Claude Code session IDs except the current one,
	// in chronological order. Populated from SessionChain[:-1].
	PriorClaudeIDs []ClaudeSessionID
	// ClearCount is the number of /clear (or compact) operations performed in
	// this session. 0 means the original session; 1 means cleared once, etc.
	// Derived from len(SessionChain) - 1.
	ClearCount int
	// HasProcess is true while a process is attached to this session.
	// It becomes false when watchProcess detects exit and clears Session.process.
	// Used by Phase() to distinguish "terminal status but process still running"
	// (rare race window) from "truly finished".
	HasProcess bool
	Display    DisplayChannel
	Status         Status
	Prompt         string
	PermissionMode string
	StartedAt      time.Time
	LastActivity   time.Time
	FinishedAt     *time.Time
	TokenUsage     TokenUsage
	CurrentTool    string
	ErrorMessage   string
	TerminalTitle  string
	BookmarkName   string
	Elapsed        time.Duration
}

// Phase returns the high-level lifecycle phase derived from Status and HasProcess.
// This is always consistent with the snapshot's other fields — no separate Phase
// field is stored, eliminating the risk of stale derived state.
//
//   - StatusUnmanaged           → PhaseExternal  (external/discovered sessions)
//   - IsTerminal() && !HasProcess → PhaseArchived (finished; no process attached)
//   - otherwise                 → PhaseActive
//
// HasProcess correctly handles the rare race where Status is terminal but watchProcess
// hasn't detached the process yet — such sessions remain PhaseActive.
func (s Snapshot) Phase() SessionPhase {
	if s.Status == StatusUnmanaged {
		return PhaseExternal
	}
	if s.Status.IsTerminal() && !s.HasProcess {
		return PhaseArchived
	}
	return PhaseActive
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
		ID:              s.ID,
		Name:            s.Name,
		RepoPath:        s.RepoPath,
		RepoName:        s.RepoName,
		WorkspacePath:   s.WorkspacePath,
		SubProjectDir:   s.SubProjectDir,
		ClaudeSessionID: s.CurrentClaudeID(),
		PriorClaudeIDs:  s.PriorClaudeIDs(),
		ClearCount:      max(0, len(s.SessionChain)-1),
		HasProcess:      s.process.Load() != nil,
		Display:         s.displayChannel(),
		Status:          s.Status,
		Prompt:          s.Prompt,
		PermissionMode:  s.PermissionMode,
		StartedAt:       s.StartedAt,
		LastActivity:    s.LastActivity,
		FinishedAt:      finishedAt,
		TokenUsage:      s.TokenUsage,
		CurrentTool:     s.CurrentTool,
		ErrorMessage:    s.ErrorMessage,
		TerminalTitle:   s.TerminalTitle,
		BookmarkName:    s.BookmarkName,
		Elapsed:         elapsed,
	}
	return snap
}

// CurrentClaudeID returns the active Claude Code session ID, or "" if none.
// Must be called with mu held (at least for reading), or use Snapshot.ClaudeSessionID.
func (s *Session) CurrentClaudeID() ClaudeSessionID {
	if len(s.SessionChain) == 0 {
		return ""
	}
	return s.SessionChain[len(s.SessionChain)-1]
}

// ChainIDs returns a copy of all Claude Code session IDs in this session's chain,
// from oldest to newest. The last element is the current active ID.
// Thread-safe; acquires mu for reading.
func (s *Session) ChainIDs() []ClaudeSessionID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]ClaudeSessionID, len(s.SessionChain))
	copy(ids, s.SessionChain)
	return ids
}

// PriorClaudeIDs returns all historical Claude Code session IDs excluding the current one.
// Returns nil if there is no history.
// Must be called with mu held for reading (RLock is sufficient).
// Calling from within an already-held RLock is safe: sync.RWMutex allows multiple
// concurrent RLock holders. Snapshot() calls this while holding mu.RLock().
func (s *Session) PriorClaudeIDs() []ClaudeSessionID {
	if len(s.SessionChain) <= 1 {
		return nil
	}
	prior := make([]ClaudeSessionID, len(s.SessionChain)-1)
	copy(prior, s.SessionChain[:len(s.SessionChain)-1])
	return prior
}

// appendToChainLocked appends newID to SessionChain under an already-held write lock.
// No-op if newID is empty or already the current (last) ID.
func (s *Session) appendToChainLocked(newID ClaudeSessionID) {
	if newID == "" {
		return
	}
	if s.CurrentClaudeID() == newID {
		return
	}
	s.SessionChain = append(s.SessionChain, newID)
}

// popChainLocked removes the last entry from SessionChain under an already-held write lock.
// Used to revert a /clear when the new session has no conversation.
func (s *Session) popChainLocked() {
	if len(s.SessionChain) > 0 {
		s.SessionChain = s.SessionChain[:len(s.SessionChain)-1]
	}
}

// getName returns the session name under lock for sorting.
func (s *Session) getName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Name
}

// MatchesFilter reports whether the session matches the given filter text.
// Matching is case-insensitive substring search over "repoPath/name".
// An empty text always matches (no filter applied).
// Uses a targeted RLock on RepoPath+Name only, avoiding a full Snapshot() call.
func (s *Session) MatchesFilter(text string) bool {
	if text == "" {
		return true
	}
	s.mu.RLock()
	target := strings.ToLower(s.RepoPath + "/" + s.Name)
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
//	2: Running
//	3: WaitingApproval / WaitingAnswer（要手動介入）
func (s *Session) sortGroup() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch s.Status {
	case StatusWaitingApproval, StatusWaitingAnswer:
		return 3
	case StatusRunning:
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
