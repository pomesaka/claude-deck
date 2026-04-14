package preview

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/session"
)

func TestWriteReadSelection(t *testing.T) {
	tests := []struct {
		name      string
		sessionID session.DeckSessionID
	}{
		{"normal ID", "abc123def456"},
		{"long hex ID", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"},
		{"whitespace-trimmed", "abc  def"}, // TrimSpace only affects leading/trailing
	}

	dir := t.TempDir()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := WriteSelection(dir, tc.sessionID); err != nil {
				t.Fatalf("WriteSelection: %v", err)
			}
			got, err := ReadSelection(dir)
			if err != nil {
				t.Fatalf("ReadSelection: %v", err)
			}
			// WriteSelection writes raw bytes; ReadSelection trims whitespace.
			want := session.DeckSessionID(string(tc.sessionID))
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestReadSelectionMissingFile(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadSelection(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty ID, got %q", got)
	}
}

func TestWatchSelection(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan session.DeckSessionID, 4)
	if err := WatchSelection(ctx, dir, func(id session.DeckSessionID) {
		received <- id
	}); err != nil {
		t.Fatalf("WatchSelection: %v", err)
	}

	// Give the watcher goroutine a moment to start.
	time.Sleep(50 * time.Millisecond)

	want := session.DeckSessionID("deadbeef1234")
	if err := WriteSelection(dir, want); err != nil {
		t.Fatalf("WriteSelection: %v", err)
	}

	select {
	case got := <-received:
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for selection change")
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

func TestWriteSelectionAtomic(t *testing.T) {
	// Verify that .tmp file does not persist after a successful write.
	dir := t.TempDir()
	if err := WriteSelection(dir, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(selectionPath(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Error("expected .tmp file to be removed after atomic rename")
	}
}
