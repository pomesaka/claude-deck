package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/agentruntime"
	"github.com/pomesaka/claude-deck/internal/jj"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// fakeBackend stands in for tmux. Each started session gets a "window" with a PID.
type fakeBackend struct {
	mu      sync.Mutex
	windows map[DeckSessionID]int
	nextPID int
	started []ProcessStartOpts
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{windows: map[DeckSessionID]int{}, nextPID: 1000}
}

func (b *fakeBackend) StartProcess(id DeckSessionID, opts ProcessStartOpts) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextPID++
	b.windows[id] = b.nextPID
	b.started = append(b.started, opts)
	return b.nextPID, nil
}

func (b *fakeBackend) StopProcess(id DeckSessionID, _ int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.windows, id)
	return nil
}

func (b *fakeBackend) IsActive(id DeckSessionID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.windows[id]
	return ok
}

func (b *fakeBackend) LiveSessions() (map[DeckSessionID]int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[DeckSessionID]int, len(b.windows))
	for id, pid := range b.windows {
		out[id] = pid
	}
	return out, nil
}

func (b *fakeBackend) KillWindows(ids []DeckSessionID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range ids {
		delete(b.windows, id)
	}
	return nil
}

func (b *fakeBackend) Focus(DeckSessionID) error { return nil }
func (b *fakeBackend) EnsurePreview() error      { return nil }
func (b *fakeBackend) FocusPreview() error       { return nil }
func (b *fakeBackend) KillPreview() error        { return nil }

// newTestManager builds a Manager on a temporary store and a fake backend,
// without touching tmux or the user's data directory.
func newTestManager(t *testing.T) (*Manager, *fakeBackend) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	be := newFakeBackend()
	m := &Manager{
		sessions:       make(map[DeckSessionID]*Session),
		store:          st,
		backend:        be,
		usage:          usage.NewReader(t.TempDir()),
		ctx:            context.Background(),
		config:         ManagerConfig{DataDir: dataDir, MaxSessions: 30, ClaudeCommand: "claude"},
		notifyCh:       make(chan struct{}, 1),
		pendingChanges: make(map[DeckSessionID]bool),
	}
	return m, be
}

// otherProcess opens a second handle on the manager's store, standing in for
// the CLI or a hook command running in another process.
func otherProcess(t *testing.T, m *Manager) *store.Store {
	t.Helper()
	st, err := store.Open(m.config.DataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustGet(t *testing.T, st *store.Store, id DeckSessionID) store.Record {
	t.Helper()
	r, err := st.Get(string(id))
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return r
}

func createPlainSession(t *testing.T, m *Manager) *Session {
	t.Helper()
	dir := t.TempDir()
	sess, err := m.CreateSession(context.Background(), dir, dir, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sess
}

func TestCreateSession_WritesStoreBeforeReturning(t *testing.T) {
	m, be := newTestManager(t)
	m.config.DeckCommand = "/opt/claude-deck"
	m.config.PluginDir = "/data/plugin"

	sess := createPlainSession(t, m)

	r := mustGet(t, m.store, sess.ID)
	if r.Status != StatusIdle.ID() {
		t.Errorf("Status = %q, want idle", r.Status)
	}
	if r.PID != be.windows[sess.ID] || r.PID == 0 {
		t.Errorf("PID = %d, want the window PID %d", r.PID, be.windows[sess.ID])
	}
	if !sess.IsProcessAlive() {
		t.Error("in-memory session has no process after create")
	}

	opts := be.started[0]
	wantExit := []string{"/opt/claude-deck", "hook", "exited", "--session", string(sess.ID)}
	if !slices.Equal(opts.OnExit, wantExit) {
		t.Errorf("OnExit = %v, want %v", opts.OnExit, wantExit)
	}
	for _, want := range []string{
		EnvSessionID + "=" + string(sess.ID),
		EnvDataDir + "=" + m.config.DataDir,
		EnvCommand + "=/opt/claude-deck",
	} {
		if !slices.Contains(opts.Env, want) {
			t.Errorf("Env %v lacks %q", opts.Env, want)
		}
	}
	if !slices.Contains(opts.Args, "--plugin-dir") {
		t.Errorf("Args %v lack --plugin-dir", opts.Args)
	}
}

func TestCreateSession_MissingWorkDir(t *testing.T) {
	m, be := newTestManager(t)
	repo := t.TempDir()
	missing := filepath.Join(repo, "not-there")

	if _, err := m.CreateSession(context.Background(), repo, missing, false); err == nil {
		t.Fatal("CreateSession in a missing directory succeeded")
	}
	if len(be.started) != 0 {
		t.Errorf("a process was started: %+v", be.started)
	}
	if recs, _ := m.store.List(); len(recs) != 0 {
		t.Errorf("store has rows after the refused create: %+v", recs)
	}
}

func TestProcessOpts_WithoutDeckCommand(t *testing.T) {
	m, _ := newTestManager(t)
	opts := m.processOpts("abc", "/w", agentruntime.StartSpec{Command: "claude"})
	if opts.OnExit != nil {
		t.Errorf("OnExit = %v, want nil when DeckCommand is empty", opts.OnExit)
	}
	for _, e := range opts.Env {
		if strings.HasPrefix(e, EnvCommand+"=") {
			t.Errorf("Env has %q although DeckCommand is empty", e)
		}
	}
}

func TestKill(t *testing.T) {
	m, be := newTestManager(t)
	sess := createPlainSession(t, m)

	if err := m.Kill(sess.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	r := mustGet(t, m.store, sess.ID)
	if r.Status != StatusCompleted.ID() || r.FinishedAt == nil || r.ClosingAt != nil {
		t.Errorf("after Kill: status=%q finished=%v closing=%v", r.Status, r.FinishedAt, r.ClosingAt)
	}
	if be.IsActive(sess.ID) {
		t.Error("window still alive after Kill")
	}
	if got := m.GetSession(sess.ID).GetStatus(); got != StatusCompleted {
		t.Errorf("in-memory status = %v, want Completed", got)
	}
}

// A session closed earlier has no workspace left; closing it again must not
// drop the revisions the first close saved for resume (ADR 009).
func TestKill_AlreadyClosedKeepsSavedRevisions(t *testing.T) {
	m, _ := newTestManager(t)
	finished := time.Now().Add(-time.Hour)
	if err := m.store.Insert(store.Record{
		ID: "closed", Name: "abcd1234", RepoPath: t.TempDir(),
		Status: StatusCompleted.ID(), FinishedAt: &finished,
		LastJJRevision: "at-rev", LastJJParentRevision: "parent-rev",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := m.Kill("closed"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	r := mustGet(t, m.store, "closed")
	if r.LastJJRevision != "at-rev" || r.LastJJParentRevision != "parent-rev" {
		t.Errorf("revisions = %q %q, want at-rev parent-rev", r.LastJJRevision, r.LastJJParentRevision)
	}
	if r.Status != StatusCompleted.ID() || !r.FinishedAt.Equal(finished) || r.ClosingAt != nil {
		t.Errorf("after second Kill: status=%q finished=%v closing=%v", r.Status, r.FinishedAt, r.ClosingAt)
	}
}

func TestKill_RefusedWhileAnotherProcessCloses(t *testing.T) {
	m, be := newTestManager(t)
	sess := createPlainSession(t, m)

	other := otherProcess(t, m)
	if _, err := other.Update(string(sess.ID), func(r *store.Record) error {
		return beginClose(r, time.Now())
	}); err != nil {
		t.Fatalf("beginClose from other process: %v", err)
	}

	if err := m.Kill(sess.ID); !errors.Is(err, ErrClosing) {
		t.Fatalf("Kill error = %v, want ErrClosing", err)
	}
	if !be.IsActive(sess.ID) {
		t.Error("refused Kill still stopped the window")
	}
}

func TestResumeSession(t *testing.T) {
	m, be := newTestManager(t)
	sess := createPlainSession(t, m)
	if err := RecordSessionStart(m.store, sess.ID, "claude-1", SourceStartup); err != nil {
		t.Fatalf("RecordSessionStart: %v", err)
	}
	if err := m.Kill(sess.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	if err := m.ResumeSession(context.Background(), sess.ID); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	r := mustGet(t, m.store, sess.ID)
	if r.Status != StatusIdle.ID() || r.FinishedAt != nil || r.PID != be.windows[sess.ID] {
		t.Errorf("after resume: status=%q finished=%v pid=%d window pid=%d", r.Status, r.FinishedAt, r.PID, be.windows[sess.ID])
	}
	if args := be.started[len(be.started)-1].Args; !slices.Contains(args, "claude-1") {
		t.Errorf("resume args %v lack the Claude session ID", args)
	}

	// The row is no longer finished, so a second resume (e.g. from another process) fails.
	delete(be.windows, sess.ID)
	if err := m.ResumeSession(context.Background(), sess.ID); err == nil {
		t.Error("second resume of a running session succeeded")
	}
}

func TestResumeSession_WithoutClaudeIDRestoresCompleted(t *testing.T) {
	m, _ := newTestManager(t)
	sess := createPlainSession(t, m)
	if err := m.Kill(sess.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := m.ResumeSession(context.Background(), sess.ID); err == nil {
		t.Fatal("ResumeSession without a Claude session ID succeeded")
	}
	if r := mustGet(t, m.store, sess.ID); r.Status != StatusCompleted.ID() {
		t.Errorf("Status = %q after failed resume, want completed", r.Status)
	}
}

func TestResumeSession_FailsWhenWorkspaceCannotBeRecreated(t *testing.T) {
	m, be := newTestManager(t)
	m.config.JJ = &jj.Runner{Command: "false"} // every jj command fails
	finished := time.Now()
	// A session whose workspace a close has removed.
	rec := store.Record{
		ID: "s1", Name: "shoko-0001", RepoPath: t.TempDir(),
		Status: StatusCompleted.ID(), FinishedAt: &finished,
		SessionChain: []string{"claude-1"},
	}
	if err := m.store.Tx(func(tx *store.Tx) error { return tx.Put(rec) }); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := m.ResumeSession(context.Background(), "s1"); err == nil {
		t.Fatal("ResumeSession succeeded without a workspace")
	}
	if len(be.started) != 0 {
		t.Errorf("started a process in %q", be.started[0].WorkDir)
	}
	r := mustGet(t, m.store, "s1")
	if r.Status != StatusError.ID() || r.ErrorMessage == "" || r.WorkspacePath != "" {
		t.Errorf("after failed resume: status=%q error=%q workspace=%q", r.Status, r.ErrorMessage, r.WorkspacePath)
	}
}

func TestResumeSession_AdoptsExternalSession(t *testing.T) {
	m, be := newTestManager(t)
	dir := t.TempDir()
	ext := &Session{
		ID: "ext", Name: "abcd1234", RepoPath: dir, WorkspacePath: dir,
		Status: StatusUnmanaged, SessionChain: []ClaudeSessionID{"claude-ext"},
	}
	m.mu.Lock()
	m.sessions[ext.ID] = ext
	m.mu.Unlock()

	if err := m.ResumeSession(context.Background(), "ext"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	r := mustGet(t, m.store, "ext")
	if r.Status != StatusIdle.ID() || r.PID != be.windows["ext"] {
		t.Errorf("adopted session: status=%q pid=%d", r.Status, r.PID)
	}
	if got := m.GetSession("ext").GetStatus(); got != StatusIdle {
		t.Errorf("in-memory status = %v, want Idle", got)
	}
}

func TestReload_FollowsOtherProcesses(t *testing.T) {
	m, _ := newTestManager(t)
	sess := createPlainSession(t, m)
	other := otherProcess(t, m)

	// A discovered external session for the Claude session the deck session is about to report.
	ext := &Session{ID: "ext", Status: StatusUnmanaged, SessionChain: []ClaudeSessionID{"claude-1"}}
	m.mu.Lock()
	m.sessions[ext.ID] = ext
	m.mu.Unlock()

	if err := RecordSessionStart(other, sess.ID, "claude-1", SourceStartup); err != nil {
		t.Fatalf("RecordSessionStart: %v", err)
	}
	if err := RecordHookStatus(other, sess.ID, StatusWaitingApproval); err != nil {
		t.Fatalf("RecordHookStatus: %v", err)
	}
	m.Reload()

	snap := m.GetSession(sess.ID).Snapshot()
	if snap.Status != StatusWaitingApproval || snap.RuntimeSessionID != "claude-1" {
		t.Errorf("after reload: status=%v claude=%q", snap.Status, snap.RuntimeSessionID)
	}
	if m.GetSession("ext") != nil {
		t.Error("external session for a Claude ID now owned by a deck session was kept")
	}

	if err := other.Delete(string(sess.ID)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m.Reload()
	if m.GetSession(sess.ID) != nil {
		t.Error("session deleted by another process is still in memory")
	}
}

func TestReload_KeepsProjectedFields(t *testing.T) {
	m, _ := newTestManager(t)
	sess := createPlainSession(t, m)
	sess.ApplyFileActivity(time.Unix(1_800_000_000, 0))

	if err := RecordHookStatus(otherProcess(t, m), sess.ID, StatusRunning); err != nil {
		t.Fatalf("RecordHookStatus: %v", err)
	}
	m.Reload()

	snap := sess.Snapshot()
	if !snap.LastActivity.Equal(time.Unix(1_800_000_000, 0)) {
		t.Errorf("LastActivity = %v, overwritten by the store", snap.LastActivity)
	}
	if snap.Status != StatusRunning {
		t.Errorf("Status = %v, want Running", snap.Status)
	}
}

func TestReconcileTmux(t *testing.T) {
	m, be := newTestManager(t)
	finished := time.Now().Add(-time.Hour)
	launching := time.Now()
	staleLaunch := time.Now().Add(-launchTimeout - time.Second)
	for _, r := range []store.Record{
		{ID: "stale-finished", Status: StatusCompleted.ID(), FinishedAt: &finished},
		{ID: "gone-running", Status: StatusRunning.ID(), PID: 7},
		// A CLI in another process may be launching while the TUI starts.
		{ID: "launching", Status: StatusIdle.ID(), LaunchingAt: &launching},
		{ID: "launch-crashed", Status: StatusIdle.ID(), LaunchingAt: &staleLaunch},
		{ID: "alive-idle", Status: StatusIdle.ID(), PID: 8},
	} {
		if err := m.store.Insert(r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	be.windows["stale-finished"] = 11
	be.windows["alive-idle"] = 8
	be.windows["orphan"] = 12

	m.ReconcileTmux()

	tests := []struct {
		id         DeckSessionID
		wantStatus Status
	}{
		{"stale-finished", StatusIdle},
		{"gone-running", StatusCompleted},
		{"launching", StatusIdle},
		{"launch-crashed", StatusCompleted},
		{"alive-idle", StatusIdle},
	}
	for _, tt := range tests {
		t.Run(string(tt.id), func(t *testing.T) {
			if got := m.GetSession(tt.id).GetStatus(); got != tt.wantStatus {
				t.Errorf("status = %v, want %v", got, tt.wantStatus)
			}
		})
	}
	if r := mustGet(t, m.store, "stale-finished"); r.PID != 11 || r.FinishedAt != nil {
		t.Errorf("stale-finished: pid=%d finished=%v", r.PID, r.FinishedAt)
	}
	if be.IsActive("orphan") {
		t.Error("orphan window was not killed")
	}
}

func TestMarkVanishedSessions(t *testing.T) {
	m, be := newTestManager(t)
	now := time.Now()
	stale := now.Add(-3 * time.Minute)
	for _, r := range []store.Record{
		{ID: "gone", Status: StatusRunning.ID(), PID: 7},
		{ID: "gone-without-pid", Status: StatusIdle.ID()},
		{ID: "launching", Status: StatusIdle.ID(), LaunchingAt: &now},
		{ID: "launch-crashed", Status: StatusIdle.ID(), LaunchingAt: &stale},
		{ID: "closing", Status: StatusIdle.ID(), PID: 9, ClosingAt: &now},
		{ID: "close-crashed", Status: StatusIdle.ID(), PID: 9, ClosingAt: &stale},
		{ID: "alive", Status: StatusIdle.ID(), PID: 10},
	} {
		if err := m.store.Insert(r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	be.windows["alive"] = 10

	m.markVanishedSessions()

	tests := []struct {
		id   DeckSessionID
		want string
	}{
		{"gone", StatusCompleted.ID()},
		{"gone-without-pid", StatusCompleted.ID()},
		{"launching", StatusIdle.ID()},
		{"launch-crashed", StatusCompleted.ID()},
		{"closing", StatusIdle.ID()},
		{"close-crashed", StatusCompleted.ID()},
		{"alive", StatusIdle.ID()},
	}
	for _, tt := range tests {
		t.Run(string(tt.id), func(t *testing.T) {
			if got := mustGet(t, m.store, tt.id).Status; got != tt.want {
				t.Errorf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

// The window list is taken before the transaction. A launch that claims the row
// in between must not be marked exited.
func TestMarkVanished_RereadsTheRow(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.store.Insert(store.Record{ID: "s", Status: StatusCompleted.ID()}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Another process resumes the session after this process decided the window is gone.
	if _, err := otherProcess(t, m).Update("s", func(r *store.Record) error {
		return beginResume(r, time.Now())
	}); err != nil {
		t.Fatalf("beginResume: %v", err)
	}
	if err := markVanished(m.store, m.usage, "s"); err != nil {
		t.Fatalf("markVanished: %v", err)
	}
	if got := mustGet(t, m.store, "s").Status; got != StatusIdle.ID() {
		t.Errorf("status = %q, want idle (launch in progress)", got)
	}
}

func TestPruneOldSessions(t *testing.T) {
	m, _ := newTestManager(t)
	m.config.MaxSessions = 1
	at := func(sec int64) time.Time { return time.Unix(1_700_000_000+sec, 0) }
	now := time.Now()
	for _, r := range []store.Record{
		{ID: "newest", Status: StatusCompleted.ID(), LastActivity: at(9)},
		{ID: "old-finished", Status: StatusCompleted.ID(), LastActivity: at(1)},
		{ID: "old-closing", Status: StatusCompleted.ID(), LastActivity: at(1), ClosingAt: &now},
		{ID: "old-running", Status: StatusRunning.ID(), LastActivity: at(0)},
	} {
		if err := m.store.Insert(r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	m.pruneOldSessions()

	recs, _ := m.store.List()
	var ids []string
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	if want := []string{"newest", "old-closing", "old-running"}; !slices.Equal(ids, want) {
		t.Errorf("remaining = %v, want %v", ids, want)
	}
}

// A pruned session cannot be resumed, so its workspace and the runtime's records
// about it go with the row.
func TestPruneOldSessions_DiscardsWorkspace(t *testing.T) {
	const snapshot = "log --no-graph --color=never -r @ -T change_id"
	const snapshotParent = "log --no-graph --color=never -r @- -T change_id"

	tests := []struct {
		name          string
		workspaceName string // the pruned row's WorkspaceName
		newestName    string // the Name of the row that stays
		jjLogFails    bool
		wantDir       bool
		wantJJ        []string
		wantForgotten bool
	}{
		{
			name:          "session that still has its workspace",
			workspaceName: "ws",
			newestName:    "other",
			wantDir:       false,
			wantJJ:        []string{snapshot, snapshotParent, "--ignore-working-copy workspace forget ws"},
			wantForgotten: true,
		},
		{
			name:          "closed session: the workspace is already gone from the row",
			workspaceName: "",
			newestName:    "other",
			wantDir:       true,
			wantJJ:        nil,
			wantForgotten: true,
		},
		{
			name:          "a remaining row has the same workspace",
			workspaceName: "ws",
			newestName:    "ws",
			wantDir:       true,
			wantJJ:        nil,
			wantForgotten: false,
		},
		{
			name:          "snapshot fails: the workspace is kept",
			workspaceName: "ws",
			newestName:    "other",
			jjLogFails:    true,
			wantDir:       true,
			wantJJ:        []string{snapshot},
			wantForgotten: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			m.config.MaxSessions = 1

			jjCalls := useFakeJJ(t, m, tt.jjLogFails)

			repo := t.TempDir()
			wsRoot := m.ws().root(repo, "ws")
			if err := os.MkdirAll(wsRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			// The workspace and what is below it are forgotten, a sibling is not.
			forgotten := false
			m.config.ForgetProjectsFunc = func(match func(dir string) bool, dryRun bool) (int, error) {
				forgotten = match(wsRoot) && match(filepath.Join(wsRoot, "sub")) && !match(wsRoot+"x") && !dryRun
				return 0, nil
			}
			at := func(sec int64) time.Time { return time.Unix(1_700_000_000+sec, 0) }
			for _, r := range []store.Record{
				{ID: "newest", Name: tt.newestName, RepoPath: repo, Status: StatusCompleted.ID(), LastActivity: at(9)},
				{ID: "old", Name: "ws", RepoPath: repo, WorkspaceName: tt.workspaceName, Status: StatusCompleted.ID(), LastActivity: at(1)},
			} {
				if err := m.store.Insert(r); err != nil {
					t.Fatalf("Insert: %v", err)
				}
			}

			m.pruneOldSessions()

			if _, err := m.store.Get("old"); err == nil {
				t.Error("old row was not pruned")
			}
			_, statErr := os.Stat(wsRoot)
			if gotDir := statErr == nil; gotDir != tt.wantDir {
				t.Errorf("workspace directory exists = %v, want %v", gotDir, tt.wantDir)
			}
			if gotJJ := jjCalls(); !slices.Equal(gotJJ, tt.wantJJ) {
				t.Errorf("jj calls = %q, want %q", gotJJ, tt.wantJJ)
			}
			if forgotten != tt.wantForgotten {
				t.Errorf("forgotten = %v, want %v", forgotten, tt.wantForgotten)
			}
		})
	}
}

// useFakeJJ makes m run a jj that only records its arguments, creates the
// directory for `jj workspace add --name NAME PATH`, and fails `jj log` when
// logFails is set. The returned function reads the calls so far.
func useFakeJJ(t *testing.T, m *Manager, logFails bool) func() []string {
	t.Helper()
	jjLog := filepath.Join(t.TempDir(), "jj.log")
	fakeJJ := filepath.Join(t.TempDir(), "jj")
	script := "#!/bin/sh\necho \"$*\" >> \"$FAKE_JJ_LOG\"\n" +
		"if [ \"$1\" = workspace ] && [ \"$2\" = add ]; then mkdir -p \"$5\"; fi\n" +
		"if [ \"$1\" = log ]; then exit \"$FAKE_JJ_LOG_EXIT\"; fi\n"
	if err := os.WriteFile(fakeJJ, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_JJ_LOG", jjLog)
	t.Setenv("FAKE_JJ_LOG_EXIT", "0")
	if logFails {
		t.Setenv("FAKE_JJ_LOG_EXIT", "1")
	}
	m.config.JJ = &jj.Runner{Command: fakeJJ}
	return func() []string {
		data, err := os.ReadFile(jjLog)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// A fork records the context it left from, so the session tree can attach it
// there even after the source session moves on with /clear.
func TestForkSession_RecordsForkedFrom(t *testing.T) {
	m, _ := newTestManager(t)
	useFakeJJ(t, m, false)
	src := createPlainSession(t, m)
	if err := RecordSessionStart(m.store, src.ID, "ctx-1", SourceStartup); err != nil {
		t.Fatalf("RecordSessionStart: %v", err)
	}
	m.Reload()

	fork, err := m.ForkSession(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if err := RecordSessionStart(m.store, src.ID, "ctx-2", SourceClear); err != nil {
		t.Fatalf("RecordSessionStart: %v", err)
	}
	m.Reload()

	if got := mustGet(t, m.store, fork.ID).ForkedFrom; got != "ctx-1" {
		t.Errorf("store ForkedFrom = %q, want ctx-1", got)
	}
	if got := m.GetSession(fork.ID).Snapshot().ForkedFrom; got != "ctx-1" {
		t.Errorf("snapshot ForkedFrom = %q, want ctx-1", got)
	}
	if got := mustGet(t, m.store, src.ID).ForkedFrom; got != "" {
		t.Errorf("source ForkedFrom = %q, want empty", got)
	}
}

func TestCollectGarbage(t *testing.T) {
	const snapshot = "log --no-graph --color=never -r @ -T change_id"
	const snapshotParent = "log --no-graph --color=never -r @- -T change_id"
	const forgetOrphan = "--ignore-working-copy workspace forget orphan"
	old := time.Now().Add(-2 * time.Hour)

	tests := []struct {
		name       string
		dryRun     bool
		jjLogFails bool
		// wantLeft are the workspace names still on disk afterwards.
		wantLeft     []string
		wantReported []string
		wantJJ       []string
		// wantForgotten are the project records the runtime is asked to drop.
		wantForgotten []string
	}{
		{
			name:          "removes workspaces without a row",
			wantLeft:      []string{"fresh", "owned"},
			wantReported:  []string{"orphan", "plain"},
			wantJJ:        []string{snapshot, snapshotParent, forgetOrphan},
			wantForgotten: []string{"gone", "gone/sub", "orphan", "plain"},
		},
		{
			name:          "dry run removes nothing",
			dryRun:        true,
			wantLeft:      []string{"fresh", "orphan", "owned", "plain"},
			wantReported:  []string{"orphan", "plain"},
			wantJJ:        nil,
			wantForgotten: []string{"gone", "gone/sub", "orphan", "plain"},
		},
		{
			name:          "snapshot fails: the workspace is removed anyway",
			jjLogFails:    true,
			wantLeft:      []string{"fresh", "owned"},
			wantReported:  []string{"orphan", "plain"},
			wantJJ:        []string{snapshot, forgetOrphan},
			wantForgotten: []string{"gone", "gone/sub", "orphan", "plain"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			jjCalls := useFakeJJ(t, m, tt.jjLogFails)

			repo := t.TempDir()
			repoDir := filepath.Dir(m.ws().root(repo, "x"))
			// owned: a closed session's row has this name. orphan: a jj workspace with no row.
			// plain: no row and not a jj workspace. fresh: no row yet, just created.
			for _, name := range []string{"owned", "orphan", "plain", "fresh"} {
				if err := os.MkdirAll(filepath.Join(repoDir, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(repoDir, "orphan", ".jj"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"owned", "orphan", "plain"} {
				if err := os.Chtimes(filepath.Join(repoDir, name), old, old); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.store.Insert(store.Record{ID: "s", Name: "owned", RepoPath: repo, Status: StatusCompleted.ID()}); err != nil {
				t.Fatalf("Insert: %v", err)
			}

			// Records the runtime holds: for each workspace, for one that is already
			// gone (and a directory below it), and for directories outside the workspaces.
			candidates := []string{"fresh", "gone", "gone/sub", "orphan", "owned", "plain"}
			var forgotten []string
			var forgetDryRun bool
			m.config.ForgetProjectsFunc = func(match func(dir string) bool, dryRun bool) (int, error) {
				forgetDryRun = dryRun
				for _, c := range candidates {
					if match(filepath.Join(repoDir, c)) {
						forgotten = append(forgotten, c)
					}
				}
				if match(repo) || match(repoDir) || match(m.config.DataDir) {
					t.Error("a directory that is not a workspace matched")
				}
				return len(forgotten), nil
			}

			report, err := CollectGarbage(m.store, m.config, tt.dryRun)
			if err != nil {
				t.Fatalf("CollectGarbage: %v", err)
			}

			entries, err := os.ReadDir(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			var left []string
			for _, e := range entries {
				left = append(left, e.Name())
			}
			if !slices.Equal(left, tt.wantLeft) {
				t.Errorf("left on disk = %v, want %v", left, tt.wantLeft)
			}
			var reported []string
			for _, ws := range report.Workspaces {
				reported = append(reported, filepath.Base(ws.Path))
				if ws.Warning != "" {
					t.Errorf("%s: warning %q", ws.Path, ws.Warning)
				}
			}
			if !slices.Equal(reported, tt.wantReported) {
				t.Errorf("reported = %v, want %v", reported, tt.wantReported)
			}
			if gotJJ := jjCalls(); !slices.Equal(gotJJ, tt.wantJJ) {
				t.Errorf("jj calls = %q, want %q", gotJJ, tt.wantJJ)
			}
			if !slices.Equal(forgotten, tt.wantForgotten) {
				t.Errorf("forgotten = %v, want %v", forgotten, tt.wantForgotten)
			}
			if report.ForgottenProjects != len(tt.wantForgotten) || report.DryRun != tt.dryRun || forgetDryRun != tt.dryRun {
				t.Errorf("report = %+v (forget dryRun %v), want %d forgotten, dry run %v", report, forgetDryRun, len(tt.wantForgotten), tt.dryRun)
			}
		})
	}
}

func TestPersistAll(t *testing.T) {
	m, _ := newTestManager(t)
	sess := createPlainSession(t, m)
	// The process that created the session wrote a bookmark after this TUI loaded the row.
	if _, err := otherProcess(t, m).Update(string(sess.ID), func(r *store.Record) error {
		r.BookmarkName = "feat/x"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sess.mu.Lock()
	sess.BookmarkName = ""
	sess.mu.Unlock()
	sess.ApplyFileActivity(time.Unix(1_800_000_000, 0))

	m.PersistAll()

	r := mustGet(t, m.store, sess.ID)
	if !r.LastActivity.Equal(time.Unix(1_800_000_000, 0)) {
		t.Errorf("LastActivity = %v, want the in-memory value", r.LastActivity)
	}
	if r.BookmarkName != "feat/x" {
		t.Errorf("BookmarkName = %q, overwritten by the empty in-memory value", r.BookmarkName)
	}
}

func TestKill_RefusedWhileLaunching(t *testing.T) {
	m, _ := newTestManager(t)
	now := time.Now()
	if err := m.store.Insert(store.Record{ID: "s", Status: StatusIdle.ID(), LaunchingAt: &now}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	m.Reload()
	if err := m.Kill("s"); !errors.Is(err, ErrLaunching) {
		t.Errorf("Kill error = %v, want ErrLaunching", err)
	}
}

func TestWatchStore_ReloadsOnOtherProcessWrite(t *testing.T) {
	m, _ := newTestManager(t)
	sess := createPlainSession(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.WatchStore(ctx)

	if err := RecordHookStatus(otherProcess(t, m), sess.ID, StatusRunning); err != nil {
		t.Fatalf("RecordHookStatus: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for sess.GetStatus() != StatusRunning {
		if time.Now().After(deadline) {
			t.Fatal("WatchStore did not pick up the other process's write")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLoadExisting_DeletedDirectory(t *testing.T) {
	m, _ := newTestManager(t)
	finishedAt := time.Unix(1_700_000_000, 0)
	deletedPath := filepath.Join(t.TempDir(), "nonexistent-workspace")
	existingPath := t.TempDir()
	for _, r := range []store.Record{
		{ID: "deleted-dir", RepoPath: "/repo", WorkspacePath: deletedPath, Status: StatusCompleted.ID(), FinishedAt: &finishedAt},
		{ID: "existing-dir", RepoPath: "/repo", WorkspacePath: existingPath, Status: StatusCompleted.ID(), FinishedAt: &finishedAt},
	} {
		if err := m.store.Insert(r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	if err := m.LoadExisting(); err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}

	got := m.GetSession("deleted-dir").Snapshot()
	if got.Status != StatusError || !strings.Contains(got.ErrorMessage, deletedPath) {
		t.Errorf("deleted-dir: status=%v message=%q", got.Status, got.ErrorMessage)
	}
	// FinishedAt は元の値を保持（上書きしない）
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finishedAt) {
		t.Errorf("FinishedAt = %v, want the original %v", got.FinishedAt, finishedAt)
	}
	if s := m.GetSession("existing-dir").GetStatus(); s != StatusCompleted {
		t.Errorf("existing-dir status = %v, want Completed", s)
	}
}

func TestLoadExisting_RemovesDuplicatesAndPrunesFinished(t *testing.T) {
	m, _ := newTestManager(t)
	m.config.MaxSessions = 2
	at := func(sec int64) time.Time { return time.Unix(1_700_000_000+sec, 0) }
	for _, r := range []store.Record{
		{ID: "long-chain", SessionChain: []string{"a", "b"}, Status: StatusCompleted.ID(), LastActivity: at(1)},
		{ID: "dup-of-a", SessionChain: []string{"a"}, Status: StatusCompleted.ID(), LastActivity: at(5)},
		{ID: "newest", SessionChain: []string{"c"}, Status: StatusCompleted.ID(), LastActivity: at(9)},
		{ID: "old-finished", SessionChain: []string{"d"}, Status: StatusCompleted.ID(), LastActivity: at(0)},
		{ID: "old-running", SessionChain: []string{"e"}, Status: StatusRunning.ID(), PID: 1, LastActivity: at(0)},
	} {
		if err := m.store.Insert(r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	if err := m.LoadExisting(); err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}

	recs, _ := m.store.List()
	var ids []string
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	want := []string{"long-chain", "newest", "old-running"}
	if !slices.Equal(ids, want) {
		t.Errorf("remaining sessions = %v, want %v", ids, want)
	}
}

func TestOpenStore_MigratesLegacyJSON(t *testing.T) {
	dataDir := t.TempDir()
	legacy := legacySessionsDir(dataDir)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"deck1.json":  `{"id":"deck1","name":"anna-1234","repo_path":"/repo","session_chain":["c1","c2"],"status":3,"pid":42}`,
		"ext.json":    `{"id":"ext","name":"ext","status":6}`,
		"broken.json": `{`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st, err := OpenStore(dataDir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	recs, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("migrated %d sessions, want 1 (external and broken ones skipped): %+v", len(recs), recs)
	}
	r := recs[0]
	if r.ID != "deck1" || r.Status != StatusCompleted.ID() || !slices.Equal(r.SessionChain, []string{"c1", "c2"}) || r.PID != 42 {
		t.Errorf("migrated record = %+v", r)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy directory still at %s (err=%v)", legacy, err)
	}
}
