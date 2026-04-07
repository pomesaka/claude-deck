package session

import (
	"bytes"
	"sync"
	"sync/atomic"
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
		// Idle → Running (spinner), Completed (process exit), Error (directory missing)
		return next == StatusRunning || next == StatusCompleted || next == StatusError
	case StatusRunning:
		// Running → Idle (spinner timeout / hook stop), WaitingApproval, WaitingAnswer,
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

// runtimeFields holds PTY-related state that changes at high frequency.
//
// mu は sess.mu と独立しているため、PTY 出力ゴルーチン（LogLines 書き込み）と
// TUI スナップショット（sess.mu で ID/Status などを読む）が競合しない。
//
// Lock ordering:
//   - emuMu → rt.mu（AppendRaw: emulator.Write 後に LogLines 更新）
//   - emuMu → sess.mu（ScrollOut コールバック内で scrollback/TerminalTitle 更新）
//   - rt.mu と sess.mu は同時に保持しない
type runtimeFields struct {
	mu sync.RWMutex

	LogLines        []string         // PTY ログ行（AppendRaw で追記）
	JSONLLogEntries []usage.LogEntry // JSONL 由来の構造化ログ（StreamSession で更新）
	lastSpinnerTime time.Time        // Braille スピナー最終検出時刻
	maxLogLines     int              // config から設定。0 の場合はデフォルト 1000
}

// Session represents a single Claude Code session.
//
// Data sources:
//   - Store (persisted as JSON): ID, Name, RepoPath, RepoName, WorkspacePath,
//     WorkspaceName, SessionChain, Status, FinishedAt, PID
//   - JSONL (Claude Code primary): Prompt, PermissionMode, StartedAt, TokenUsage
//   - Runtime only: rt.LogLines, CurrentTool
//
// Lock ordering (ABBA デッドロック防止):
//   - emuMu: emulator 読み書き専用（Write / String / Render / CursorPosition）
//   - rt.mu:  PTY ログ・JSONL ログ専用（emuMu の後に取得）
//   - mu:     その他全フィールド（emuMu の後、rt.mu とは独立）
type Session struct {
	mu    sync.RWMutex
	emuMu sync.Mutex // emulator 専用。mu/rt.mu より先に取得すること

	// rt は PTY ログなど高頻度更新フィールドをまとめた struct。
	// 詳細は runtimeFields のコメントを参照。
	rt runtimeFields

	// --- Persisted in store (claude-deck metadata) ---
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	RepoPath         string     `json:"repo_path"`
	RepoName         string     `json:"repo_name"`
	WorkspacePath    string     `json:"workspace_path"`
	WorkspaceName    string     `json:"workspace_name"`
	SubProjectDir    string     `json:"sub_project_dir,omitempty"` // リポジトリ内サブプロジェクトの相対パス
	// SessionChain は Claude Code が割り当てるセッション ID の履歴（古い順）。
	// /clear や compact のたびに末尾に新 ID が追加される。
	// 現在の ID は SessionChain[len-1]、旧 ID はそれ以前の要素。
	// アクセスには CurrentClaudeID() / PriorClaudeIDs() を使うこと。
	SessionChain []string `json:"session_chain,omitempty"`
	Status       Status   `json:"status"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	PID              int        `json:"pid,omitempty"`
	TerminalTitle    string     `json:"terminal_title,omitempty"` // OSC 0/2 で設定されたターミナルタイトル（PTY表示フィルタ用）
	BookmarkName     string     `json:"bookmark_name,omitempty"`  // jj の最近接ブックマーク名（セッション一覧表示用）

	// --- Hydrated from JSONL (JSONL が最新値を上書きするが、ストアにも保存して再起動時に即表示) ---
	Prompt         string     `json:"prompt,omitempty"`
	PermissionMode string     `json:"permission_mode,omitempty"`
	StartedAt      time.Time  `json:"started_at,omitzero"`
	LastActivity   time.Time  `json:"last_activity,omitzero"`
	TokenUsage     TokenUsage `json:"token_usage,omitzero"`

	// --- Runtime fields (not persisted, protected by sess.mu) ---
	CurrentTool  string `json:"-"` // パーサー検出中のツール名
	ErrorMessage string `json:"-"` // パーサーが検知したエラー行
	managed      bool               // Manager が PTY プロセスを管理中かどうか
	emulator     *vt.Emulator       // PTY 出力を解釈する仮想端末 (charmbracelet/x/vt)

	// displayCache は emulator.Write() 完了後に毎回更新される表示キャッシュ。
	displayCache atomic.Pointer[[]string]

	// cursorYHighWatermark は観測した cursorY の最大値を保持する単調増加カウンタ。
	// Ink は再描画時にカーソルを上に移動してから下に描画するため、描画途中に cursorY が
	// 一時的に下がり表示行数が縮小してちらつく。cursorY の縮小を常に抑制することで
	// フレーム描画中のちらつきを防ぐ。emulator リセット時（newEmulatorWithCallbacks）のみ 0 に戻る。
	// buildDisplayLines が trailing blank を除去するため、値が大きいままでも余分な空行は表示されない。
	cursorYHighWatermark atomic.Int32

	// displayCursorX/Y はエミュレータカーソルの表示座標（displayCache 内の行番号と列番号）。
	// TUI でカーソルを正確に配置するために refreshDisplayCacheLocked 内で更新される。
	// stableCursorReady が true の場合は stableCursor* が優先される。
	displayCursorX atomic.Int32
	displayCursorY atomic.Int32

	// stableCursor* は \033[?25h（カーソル表示）コールバックで設定される確定カーソル位置。
	// Ink の描画フレーム終了時に発火するため、refreshDisplayCacheLocked より精度が高い。
	// stableCursorScreenY はエミュレータのスクリーン行（scrollback を含まない 0-indexed）。
	stableCursorX       atomic.Int32
	stableCursorScreenY atomic.Int32
	stableCursorReady   atomic.Bool

	// scrollback はエミュレータの ScrollUp で画面上端から消えた行の styled テキスト。
	// エミュレータは viewport サイズで動作するため画面内の行しか保持しないが、
	// ここにスクロールアウトした行を蓄積することでスクロールバックを実現する。
	// emuMu 保持中（ScrollOut コールバック）に書き込まれるため sess.mu で保護する。
	scrollbackPlain  []string
	scrollbackStyled []string

	maxScrollback int // config から設定。0 の場合はデフォルト 2000
}

// Phase returns the high-level lifecycle phase derived from Status and managed flag.
// Must be called with mu held (at least for reading), or use Snapshot.Phase.
func (s *Session) phaseLocked() SessionPhase {
	if s.Status == StatusUnmanaged {
		return PhaseExternal
	}
	if s.Status.IsTerminal() && !s.managed {
		return PhaseArchived
	}
	return PhaseActive
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

// SetManaged updates whether the session has an active PTY process managed by Manager.
func (s *Session) SetManaged(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managed = v
}


// touchSpinner records the current time as the last Braille spinner detection.
func (s *Session) touchSpinner() {
	s.rt.mu.Lock()
	s.rt.lastSpinnerTime = time.Now()
	s.rt.mu.Unlock()
}

// spinnerIdleSince returns true if the session is Running, has previously
// detected a spinner, and the spinner has not been seen for longer than timeout.
// This is used as a fallback to transition Running → Idle when hook events
// don't arrive.
//
// rt.mu と sess.mu を同時に保持しない（ロック順序規則）ため、2回に分けて読む。
func (s *Session) spinnerIdleSince(timeout time.Duration) bool {
	s.mu.RLock()
	status := s.Status
	s.mu.RUnlock()
	if status != StatusRunning {
		return false
	}
	s.rt.mu.RLock()
	t := s.rt.lastSpinnerTime
	s.rt.mu.RUnlock()
	return !t.IsZero() && time.Since(t) > timeout
}

// AddTokens updates token usage safely (incremental, from pty parser).
func (s *Session) AddTokens(input, output int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TokenUsage.InputTokens += input
	s.TokenUsage.OutputTokens += output
}

// AppendRaw feeds a raw PTY output chunk to the virtual terminal emulator and
// appends any newline-delimited lines to LogLines.
// bufio.Scanner が除去していた \n を復元する必要はなく、生バイトをそのまま渡す。
// これにより \n なしで画面更新するインタラクティブな TUI も正しく処理できる。
//
// lock 順: emuMu（emulator.Write） → mu（LogLines 更新）
// emulator.Write 中に ScrollOut/Title コールバックが mu.Lock() を取るため、
// mu.Lock() を保持したまま emulator.Write を呼ぶと Snapshot/GetPTYDisplayLines と
// デッドロックする。
func (s *Session) AppendRaw(data []byte) {
	// Step 1: emulator に生バイトを流す（emuMu 保持、mu は未保持）
	// emulator.Write() 完了後に displayCache を更新する。これにより GetPTYDisplayLines が
	// emuMu を待たずにキャッシュを返せるため、Bubble Tea イベントループのブロックを防ぐ。
	s.emuMu.Lock()
	if s.emulator != nil {
		debuglog.Printf("[session:%s] emulator.Write %d bytes hex=%x", s.ID, len(data), data)
		s.emulator.Write(data) //nolint:errcheck
		debuglog.Printf("[session:%s] emulator.Write done", s.ID)
		s.refreshDisplayCacheLocked()
	}
	s.emuMu.Unlock()

	// Step 2: LogLines を更新（rt.mu 保持）
	s.rt.mu.Lock()
	limit := s.rt.maxLogLines
	if limit <= 0 {
		limit = 1000
	}
	for _, part := range bytes.Split(data, []byte{'\n'}) {
		line := string(bytes.TrimRight(part, "\r"))
		if line != "" {
			s.rt.LogLines = append(s.rt.LogLines, line)
		}
	}
	if len(s.rt.LogLines) > limit {
		newLines := make([]string, limit)
		copy(newLines, s.rt.LogLines[len(s.rt.LogLines)-limit:])
		s.rt.LogLines = newLines
	}
	s.rt.mu.Unlock()
}

// AppendLog adds a single line to the session log and feeds it to the emulator.
// テスト互換性のため残す。プロダクションコードは AppendRaw を使う。
// lock 順は AppendRaw と同じ: emuMu → mu。
func (s *Session) AppendLog(line string) {
	// Step 1: emulator に書く（emuMu 保持）
	s.emuMu.Lock()
	if s.emulator != nil {
		s.emulator.Write([]byte(line + "\n")) //nolint:errcheck
		s.refreshDisplayCacheLocked()
	}
	s.emuMu.Unlock()

	// Step 2: LogLines を更新（rt.mu 保持）
	s.rt.mu.Lock()
	s.rt.LogLines = append(s.rt.LogLines, line)
	limit := s.rt.maxLogLines
	if limit <= 0 {
		limit = 1000
	}
	if len(s.rt.LogLines) > limit {
		newLines := make([]string, limit)
		copy(newLines, s.rt.LogLines[len(s.rt.LogLines)-limit:])
		s.rt.LogLines = newLines
	}
	s.rt.mu.Unlock()
}

// GetLogs returns a copy of the PTY log lines (used for running sessions).
func (s *Session) GetLogs() []string {
	s.rt.mu.RLock()
	defer s.rt.mu.RUnlock()
	if len(s.rt.LogLines) == 0 {
		return nil
	}
	logs := make([]string, len(s.rt.LogLines))
	copy(logs, s.rt.LogLines)
	return logs
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
	ID                string
	Name              string
	RepoPath          string
	RepoName          string
	WorkspacePath     string
	SubProjectDir     string
	ClaudeSessionID   string
	// ClearCount is the number of /clear (or compact) operations performed in
	// this session. 0 means the original session; 1 means cleared once, etc.
	// Derived from len(SessionChain) - 1.
	ClearCount        int
	Phase             SessionPhase
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
	BookmarkName      string
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
		ClaudeSessionID:   s.CurrentClaudeID(),
		ClearCount:        max(0, len(s.SessionChain)-1),
		Phase:             s.phaseLocked(),
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
		BookmarkName:      s.BookmarkName,
		Elapsed:           elapsed,
	}
	return snap
}

// CurrentClaudeID returns the active Claude Code session ID, or "" if none.
// Must be called with mu held (at least for reading), or use Snapshot.ClaudeSessionID.
func (s *Session) CurrentClaudeID() string {
	if len(s.SessionChain) == 0 {
		return ""
	}
	return s.SessionChain[len(s.SessionChain)-1]
}

// PriorClaudeIDs returns all historical Claude Code session IDs excluding the current one.
// Returns nil if there is no history. Must be called with mu held for reading.
func (s *Session) PriorClaudeIDs() []string {
	if len(s.SessionChain) <= 1 {
		return nil
	}
	prior := make([]string, len(s.SessionChain)-1)
	copy(prior, s.SessionChain[:len(s.SessionChain)-1])
	return prior
}

// appendToChainLocked appends newID to SessionChain under an already-held write lock.
// No-op if newID is empty or already the current (last) ID.
func (s *Session) appendToChainLocked(newID string) {
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
	s.rt.LogLines = make([]string, 0, 256)
	s.emulator = newEmulatorWithCallbacks(s, 0, 0)
	return s
}
