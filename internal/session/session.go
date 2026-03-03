package session

import (
	"sync"
	"time"

	"github.com/charmbracelet/x/vt"
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

// NeedsAttention returns true if the session requires user action.
func (s Status) NeedsAttention() bool {
	return s == StatusWaitingApproval || s == StatusWaitingAnswer
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

// Session represents a single Claude Code session.
//
// Data sources:
//   - Store (persisted as JSON): ID, Name, RepoPath, RepoName, WorkspacePath,
//     WorkspaceName, ClaudeSessionID, Status, FinishedAt, PID
//   - JSONL (Claude Code primary): Prompt, PermissionMode, StartedAt, TokenUsage
//   - Runtime only: LogLines, CurrentTool
type Session struct {
	mu sync.RWMutex

	// --- Persisted in store (claude-deck metadata) ---
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	RepoPath         string     `json:"repo_path"`
	RepoName         string     `json:"repo_name"`
	WorkspacePath    string     `json:"workspace_path"`
	WorkspaceName    string     `json:"workspace_name"`
	SubProjectDir    string     `json:"sub_project_dir,omitempty"` // リポジトリ内サブプロジェクトの相対パス
	ClaudeSessionID         string `json:"claude_session_id,omitempty"`
	PreviousClaudeSessionID string `json:"previous_claude_session_id,omitempty"` // /clear で更新される前の ID（resume フォールバック用）
	Status                  Status `json:"status"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	PID              int        `json:"pid,omitempty"`
	TerminalTitle    string     `json:"terminal_title,omitempty"` // OSC 0/2 で設定されたターミナルタイトル

	// --- Hydrated from JSONL (JSONL が最新値を上書きするが、ストアにも保存して再起動時に即表示) ---
	Prompt         string     `json:"prompt,omitempty"`
	PermissionMode string     `json:"permission_mode,omitempty"`
	StartedAt      time.Time  `json:"started_at,omitzero"`
	LastActivity   time.Time  `json:"last_activity,omitzero"`
	TokenUsage     TokenUsage `json:"token_usage,omitzero"`

	// --- Runtime fields (not persisted) ---
	LogLines        []string         `json:"-"`
	JSONLLogEntries []usage.LogEntry `json:"-"` // JSONL由来の構造化ログ
	CurrentTool     string           `json:"-"`
	ErrorMessage    string           `json:"-"` // パーサーが検知したエラー行
	managed  bool         // Manager が PTY プロセスを管理中かどうか
	emulator *vt.Emulator // PTY 出力を解釈する仮想端末 (charmbracelet/x/vt)

	// lastSpinnerTime は最後に Braille スピナーを検出した時刻。
	// Manager の定期チェックで Running → Idle 自動遷移のタイムアウト判定に使う。
	lastSpinnerTime time.Time

	// scrollback はエミュレータの ScrollUp で画面上端から消えた行の styled テキスト。
	// エミュレータは viewport サイズで動作するため画面内の行しか保持しないが、
	// ここにスクロールアウトした行を蓄積することでスクロールバックを実現する。
	scrollbackPlain  []string
	scrollbackStyled []string

	maxLogLines   int // config から設定。0 の場合はデフォルト 1000
	maxScrollback int // config から設定。0 の場合はデフォルト 2000
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
func (s *Session) setStatusLocked(status Status) {
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

// SetManaged updates whether the session has an active PTY process managed by Manager.
func (s *Session) SetManaged(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managed = v
}


// touchSpinner records the current time as the last Braille spinner detection.
func (s *Session) touchSpinner() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSpinnerTime = time.Now()
}

// spinnerIdleSince returns true if the session is Running, has previously
// detected a spinner, and the spinner has not been seen for longer than timeout.
// This is used as a fallback to transition Running → Idle when hook events
// don't arrive.
func (s *Session) spinnerIdleSince(timeout time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Status == StatusRunning &&
		!s.lastSpinnerTime.IsZero() &&
		time.Since(s.lastSpinnerTime) > timeout
}

// AddTokens updates token usage safely (incremental, from pty parser).
func (s *Session) AddTokens(input, output int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TokenUsage.InputTokens += input
	s.TokenUsage.OutputTokens += output
}

// AppendLog adds a line to the session log.
// Cursor-up シーケンスが含まれる場合、指定行数分の前の行を除去して
// LogLines が端末の表示状態を反映するようにする。
// これにより Bubble Tea のスピナー再描画がログに蓄積されるのを防ぐ。
func (s *Session) AppendLog(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.LogLines = append(s.LogLines, line)
	limit := s.maxLogLines
	if limit <= 0 {
		limit = 1000
	}
	if len(s.LogLines) > limit {
		newLines := make([]string, limit)
		copy(newLines, s.LogLines[len(s.LogLines)-limit:])
		s.LogLines = newLines
	}
	debuglog.Printf("[session:%s] AppendLog: %q", s.ID, line)

	// 仮想端末に生データを流す（Scanner が除去した \n を復元）
	if s.emulator != nil {
		s.emulator.Write([]byte(line + "\n")) //nolint:errcheck
	}

}

// GetLogs returns a copy of the PTY log lines (used for running sessions).
func (s *Session) GetLogs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.LogLines) == 0 {
		return nil
	}
	logs := make([]string, len(s.LogLines))
	copy(logs, s.LogLines)
	return logs
}

// GetStructuredLogs returns a copy of the JSONL-derived structured log entries.
func (s *Session) GetStructuredLogs() []usage.LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.JSONLLogEntries) == 0 {
		return nil
	}
	entries := make([]usage.LogEntry, len(s.JSONLLogEntries))
	copy(entries, s.JSONLLogEntries)
	return entries
}

// Snapshot is a read-only copy of session state, safe to use without locks.
type Snapshot struct {
	ID                string
	Name              string
	RepoPath          string
	RepoName          string
	WorkspacePath     string
	SubProjectDir     string
	ClaudeSessionID   string
	Status            Status
	Managed           bool
	Prompt            string
	PermissionMode    string
	StartedAt    time.Time
	LastActivity time.Time
	FinishedAt   *time.Time
	TokenUsage        TokenUsage
	CurrentTool       string
	ErrorMessage      string
	TerminalTitle     string
	Elapsed           time.Duration
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
		ID:                s.ID,
		Name:              s.Name,
		RepoPath:          s.RepoPath,
		RepoName:          s.RepoName,
		WorkspacePath:     s.WorkspacePath,
		SubProjectDir:     s.SubProjectDir,
		ClaudeSessionID:   s.ClaudeSessionID,
		Status:            s.Status,
		Managed:           s.managed,
		Prompt:            s.Prompt,
		PermissionMode:    s.PermissionMode,
		StartedAt:    s.StartedAt,
		LastActivity: s.LastActivity,
		FinishedAt:   finishedAt,
		TokenUsage:        s.TokenUsage,
		CurrentTool:       s.CurrentTool,
		ErrorMessage:      s.ErrorMessage,
		TerminalTitle:     s.TerminalTitle,
		Elapsed:           elapsed,
	}
	return snap
}

// getName returns the session name under lock for sorting.
func (s *Session) getName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Name
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
		LogLines:      make([]string, 0, 256),
	}
	s.emulator = newEmulatorWithCallbacks(s, 0, 0)
	return s
}
