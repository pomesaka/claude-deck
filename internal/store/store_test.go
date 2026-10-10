package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	st, err := OpenPath(path)
	if err != nil {
		t.Fatalf("OpenPath(%q): %v", path, err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestStore_RoundTrip(t *testing.T) {
	finished := time.Unix(1_700_000_100, 5)
	closing := time.Unix(1_700_000_200, 0)
	tests := []struct {
		name string
		rec  Record
	}{
		{
			name: "zero values stay zero",
			rec:  Record{ID: "a", Status: "idle"},
		},
		{
			name: "every field set",
			rec: Record{
				ID: "b", Name: "maika-548e", RepoPath: "/repo", RepoName: "repo",
				WorkspacePath: "/ws/maika-548e/sub", WorkspaceName: "maika-548e", SubProjectDir: "sub",
				SessionChain: []string{"old", "new"}, ForkedFrom: "source", Alias: "review-pr-12", Status: "completed", FinishedAt: &finished, PID: 42,
				ErrorMessage: "boom", TerminalTitle: "title", BookmarkName: "feat/x",
				LastJJRevision: "abc", LastJJParentRevision: "def",
				Prompt: "hello", PermissionMode: "plan",
				StartedAt: time.Unix(1_700_000_000, 1), LastActivity: time.Unix(1_700_000_050, 2),
				ClosingAt: &closing, LaunchingAt: &finished,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openTestStore(t, filepath.Join(t.TempDir(), FileName))
			if err := st.Insert(tt.rec); err != nil {
				t.Fatalf("Insert: %v", err)
			}
			got, err := st.Get(tt.rec.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !reflect.DeepEqual(got, tt.rec) {
				t.Errorf("Get() =\n%+v\nwant\n%+v", got, tt.rec)
			}
		})
	}
}

// createOldDatabase creates, at path, a database with the schema as it was before
// any of addedColumns existed, holding one row (id a, name anna-8cc7).
func createOldDatabase(t *testing.T, path string) {
	t.Helper()
	oldSchema := schema
	for _, c := range addedColumns {
		line := regexp.MustCompile(`,\n\t` + c.name + ` [^\n,]*`)
		if !line.MatchString(oldSchema) {
			t.Fatalf("the schema has no line for the added column %s", c.name)
		}
		oldSchema = line.ReplaceAllString(oldSchema, "")
	}
	// WAL, as the binary that created such a database set it.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("creating old schema: %v", err)
	}
	if _, err := db.Exec("INSERT INTO sessions (id, name, status) VALUES ('a', 'anna-8cc7', 'idle')"); err != nil {
		t.Fatalf("inserting old row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// A database created before a column was added gets the column when it is
// opened, and keeps its rows.
func TestStore_MigratesAddedColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	createOldDatabase(t, path)

	st := openTestStore(t, path)
	got, err := st.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "anna-8cc7" || got.ForkedFrom != "" || got.Alias != "" {
		t.Errorf("old row = %+v, want name anna-8cc7, no ForkedFrom and no Alias", got)
	}
	if _, err := st.Update("a", func(r *Record) error {
		r.ForkedFrom = "source"
		r.Alias = "review-pr-12"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Opening again finds the column and changes nothing.
	again := openTestStore(t, path)
	got, err = again.Get("a")
	if err != nil {
		t.Fatalf("Get after reopening: %v", err)
	}
	if got.ForkedFrom != "source" || got.Alias != "review-pr-12" {
		t.Errorf("after reopening: ForkedFrom = %q, Alias = %q; want source, review-pr-12", got.ForkedFrom, got.Alias)
	}
}

func TestStore_GetMissing(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), FileName))
	if _, err := st.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := st.Update("nope", func(*Record) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(missing) error = %v, want ErrNotFound", err)
	}
}

func TestStore_InsertDuplicate(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), FileName))
	if err := st.Insert(Record{ID: "a", Status: "idle"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.Insert(Record{ID: "a", Status: "running"}); err == nil {
		t.Error("second Insert with the same ID succeeded")
	}
}

func TestStore_UpdateErrorWritesNothing(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), FileName))
	if err := st.Insert(Record{ID: "a", Status: "idle"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	abort := errors.New("abort")
	_, err := st.Update("a", func(r *Record) error {
		r.Status = "running"
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("Update error = %v, want abort", err)
	}
	got, _ := st.Get("a")
	if got.Status != "idle" {
		t.Errorf("Status = %q after aborted update, want idle", got.Status)
	}
}

// Two Store handles on the same file stand in for two processes (TUI and CLI).
// Each increments PID many times; a lost update would leave the total short.
func TestStore_ConcurrentUpdatesFromTwoHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a := openTestStore(t, path)
	b := openTestStore(t, path)
	if err := a.Insert(Record{ID: "s", Status: "idle"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	const perHandle = 50
	var wg sync.WaitGroup
	errs := make(chan error, 2*perHandle)
	for _, st := range []*Store{a, b} {
		wg.Go(func() {
			for range perHandle {
				if _, err := st.Update("s", func(r *Record) error {
					r.PID++
					return nil
				}); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Update: %v", err)
	}
	got, _ := a.Get("s")
	if got.PID != 2*perHandle {
		t.Errorf("PID = %d, want %d", got.PID, 2*perHandle)
	}
}

func TestStore_DataVersionSeesOtherHandleCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	watcher := openTestStore(t, path)
	writer := openTestStore(t, path)

	before, err := watcher.DataVersion()
	if err != nil {
		t.Fatalf("DataVersion: %v", err)
	}
	for i := range 3 {
		if err := writer.Insert(Record{ID: strconv.Itoa(i), Status: "idle"}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		after, err := watcher.DataVersion()
		if err != nil {
			t.Fatalf("DataVersion: %v", err)
		}
		if after == before {
			t.Fatalf("DataVersion did not change after commit %d", i)
		}
		before = after
	}
}

// A write through the same handle also changes DataVersion, because writes go
// through pool connections other than the dedicated version connection.
func TestStore_DataVersionSeesOwnCommits(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), FileName))
	before, _ := st.DataVersion()
	if err := st.Insert(Record{ID: "a", Status: "idle"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	after, _ := st.DataVersion()
	if after == before {
		t.Error("DataVersion did not change after own commit")
	}
}

// A database from before the token columns were dropped from the schema still
// has them. Rows are written and read without naming them.
func TestStore_IgnoresColumnsNoLongerUsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		schema,
		"ALTER TABLE sessions ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE sessions ADD COLUMN estimated_cost_usd REAL NOT NULL DEFAULT 0",
		"INSERT INTO sessions (id, name, status, input_tokens, estimated_cost_usd) VALUES ('a', 'anna-8cc7', 'idle', 120, 1.5)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st := openTestStore(t, path)
	if _, err := st.Update("a", func(r *Record) error {
		r.Alias = "review-pr-12"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := st.Insert(Record{ID: "b", Name: "emiri-78fb", Status: "idle"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := st.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "anna-8cc7" || got.Alias != "review-pr-12" {
		t.Errorf("row = %+v, want name anna-8cc7 and alias review-pr-12", got)
	}
}

// A newer claude-deck may have added a column this binary does not know (the
// TUI keeps running while the CLI is rebuilt). Writing a row must leave such a
// column as it is.
func TestStore_PutKeepsColumnsItDoesNotKnow(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	st := openTestStore(t, path)
	if err := st.Insert(Record{ID: "a", Name: "anna-8cc7", Status: "idle"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	newer, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Close()
	for _, stmt := range []string{
		"ALTER TABLE sessions ADD COLUMN added_later TEXT NOT NULL DEFAULT ''",
		"UPDATE sessions SET added_later = 'kept' WHERE id = 'a'",
	} {
		if _, err := newer.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	if _, err := st.Update("a", func(r *Record) error {
		r.Status = "running"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	var status, addedLater string
	if err := newer.QueryRow("SELECT status, added_later FROM sessions WHERE id = 'a'").Scan(&status, &addedLater); err != nil {
		t.Fatal(err)
	}
	if status != "running" || addedLater != "kept" {
		t.Errorf("status = %q, added_later = %q; want running, kept", status, addedLater)
	}
}

// Opening the store and reading it must not wait for a writer: in WAL mode a
// reader runs beside one. `claude-deck list` and every hook command open the
// store while the TUI or another hook may be in a write transaction.
func TestStore_OpenAndReadDoNotWaitForAWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writer := openTestStore(t, path)
	if err := writer.Insert(Record{ID: "a", Status: "idle"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// How long the open and the read took while the writer held its transaction.
	var took time.Duration
	err := writer.Tx(func(tx *Tx) error {
		if err := tx.Put(Record{ID: "a", Status: "running"}); err != nil {
			return err
		}
		start := time.Now()
		reader, err := OpenPath(path)
		if err != nil {
			return fmt.Errorf("OpenPath beside a writer: %w", err)
		}
		defer reader.Close()
		r, err := reader.Get("a")
		if err != nil {
			return fmt.Errorf("Get beside a writer: %w", err)
		}
		// The writer has not committed, so the reader sees the row as it was.
		if r.Status != "idle" {
			return fmt.Errorf("Status = %q, want idle", r.Status)
		}
		took = time.Since(start)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// busy_timeout is 5 seconds: an open that waited for the lock takes that long.
	if took > time.Second {
		t.Errorf("open and read took %v beside a writer", took)
	}
}

// The TUI, the CLI and the hook commands of a new version may open a database of
// the old schema at the same moment. Each sees the columns missing; only one adds them.
func TestStore_ConcurrentOpensMigrateOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	createOldDatabase(t, path)

	const openers = 8
	start := make(chan struct{})
	errs := make([]error, openers)
	var wg sync.WaitGroup
	for i := range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Each handle has its own connections, as each process does.
			st, err := OpenPath(path)
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = st.Close()
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("opener %d: %v", i, err)
		}
	}

	st := openTestStore(t, path)
	for _, c := range addedColumns {
		var n int
		if err := st.db.QueryRow("SELECT count(*) FROM pragma_table_info('sessions') WHERE name = ?", c.name).Scan(&n); err != nil {
			t.Fatalf("counting column %s: %v", c.name, err)
		}
		if n != 1 {
			t.Errorf("column %s appears %d times, want 1", c.name, n)
		}
	}
	got, err := st.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "anna-8cc7" {
		t.Errorf("Name = %q after the migration, want anna-8cc7", got.Name)
	}
}
