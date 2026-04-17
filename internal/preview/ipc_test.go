package preview

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/session"
)

func TestWriteReadSpec(t *testing.T) {
	tests := []struct {
		name string
		spec PreviewSpec
	}{
		{
			name: "empty spec (no selection)",
			spec: PreviewSpec{},
		},
		{
			name: "basic spec",
			spec: PreviewSpec{
				DeckSessionID:   session.DeckSessionID("abc123"),
				Name:            "my-session",
				RepoName:        "myrepo",
				WorkspacePath:   "/home/user/ws",
				ClaudeSessionID: session.ClaudeSessionID("uuid-abc"),
				ClearCount:      2,
				Status:          "完了",
				Display:         "jsonl",
				CurrentTool:     "Edit",
				ErrorMessage:    "",
				NeedsAttention:  false,
				JSONLPath:       "/home/.claude/projects/abc.jsonl",
				PriorJSONLPaths: []string{"/home/.claude/projects/old.jsonl"},
				PriorClaudeIDs:  []session.ClaudeSessionID{"old-uuid"},
			},
		},
		{
			name: "spec with needs attention",
			spec: PreviewSpec{
				DeckSessionID:  session.DeckSessionID("deadbeef"),
				Status:         "Approve待ち",
				Display:        "none",
				NeedsAttention: true,
			},
		},
	}

	dir := t.TempDir()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := WriteSpec(dir, tc.spec); err != nil {
				t.Fatalf("WriteSpec: %v", err)
			}
			got, err := ReadSpec(dir)
			if err != nil {
				t.Fatalf("ReadSpec: %v", err)
			}
			if got.DeckSessionID != tc.spec.DeckSessionID {
				t.Errorf("DeckSessionID: got %q, want %q", got.DeckSessionID, tc.spec.DeckSessionID)
			}
			if got.Display != tc.spec.Display {
				t.Errorf("Display: got %q, want %q", got.Display, tc.spec.Display)
			}
			if got.JSONLPath != tc.spec.JSONLPath {
				t.Errorf("JSONLPath: got %q, want %q", got.JSONLPath, tc.spec.JSONLPath)
			}
			if got.NeedsAttention != tc.spec.NeedsAttention {
				t.Errorf("NeedsAttention: got %v, want %v", got.NeedsAttention, tc.spec.NeedsAttention)
			}
		})
	}
}

func TestReadSpecMissingFile(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadSpec(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Display != "" {
		t.Errorf("expected empty spec, got Display=%q", got.Display)
	}
}

func TestWatchSpec(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan PreviewSpec, 4)
	if err := WatchSpec(ctx, dir, func(spec PreviewSpec) {
		received <- spec
	}); err != nil {
		t.Fatalf("WatchSpec: %v", err)
	}

	// Give the watcher goroutine a moment to start.
	time.Sleep(50 * time.Millisecond)

	want := PreviewSpec{
		DeckSessionID: session.DeckSessionID("deadbeef1234"),
		Display:       "jsonl",
		Name:          "test-session",
	}
	if err := WriteSpec(dir, want); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}

	select {
	case got := <-received:
		if got.DeckSessionID != want.DeckSessionID {
			t.Errorf("got DeckSessionID=%q, want %q", got.DeckSessionID, want.DeckSessionID)
		}
		if got.Display != want.Display {
			t.Errorf("got Display=%q, want %q", got.Display, want.Display)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for spec change")
	}
}

func TestSelectionPath(t *testing.T) {
	dir := "/tmp/testdata"
	got := selectionPath(dir)
	want := dir + "/" + selectionFileName
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriteSpecAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSpec(dir, PreviewSpec{DeckSessionID: "abc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(selectionPath(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Error("expected .tmp file to be removed after atomic rename")
	}
}
