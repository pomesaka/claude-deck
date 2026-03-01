package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMultiWatcher_WriteEvent(t *testing.T) {
	baseDir := t.TempDir()
	projDir := filepath.Join(baseDir, "project1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 既存ファイルを作成
	jsonlPath := filepath.Join(projDir, "sess-write-001.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mw, err := NewMultiWatcher(baseDir, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// テスト用に coalesce を短くする
	mw.coalesceInterval = 200 * time.Millisecond

	var mu sync.Mutex
	var writeEvents []FileEvent
	mw.OnWrite = func(ev FileEvent) {
		mu.Lock()
		writeEvents = append(writeEvents, ev)
		mu.Unlock()
	}
	mw.OnNewFile = func(ev FileEvent) {}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go mw.Run(ctx)

	// watcher が初期化されるのを待つ
	time.Sleep(200 * time.Millisecond)

	// ファイルに追記して Write イベントを発生させる
	f, err := os.OpenFile(jsonlPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant"}` + "\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	// coalesce interval + マージンを待つ
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(writeEvents)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for coalesced Write event")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if writeEvents[0].SessionID != "sess-write-001" {
		t.Errorf("SessionID = %q, want %q", writeEvents[0].SessionID, "sess-write-001")
	}
	if writeEvents[0].ModTime.IsZero() {
		t.Error("ModTime should not be zero")
	}
}

func TestMultiWatcher_CoalescesMultipleWrites(t *testing.T) {
	baseDir := t.TempDir()
	projDir := filepath.Join(baseDir, "project1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	jsonlPath := filepath.Join(projDir, "sess-coalesce-001.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mw, err := NewMultiWatcher(baseDir, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mw.coalesceInterval = 500 * time.Millisecond

	var mu sync.Mutex
	var writeEvents []FileEvent
	mw.OnWrite = func(ev FileEvent) {
		mu.Lock()
		writeEvents = append(writeEvents, ev)
		mu.Unlock()
	}
	mw.OnNewFile = func(ev FileEvent) {}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go mw.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	// 高頻度書き込み: 50ms 間隔で5回
	for i := 0; i < 5; i++ {
		f, err := os.OpenFile(jsonlPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(`{"type":"assistant"}` + "\n")
		f.Close()
		time.Sleep(50 * time.Millisecond)
	}

	// coalesce 2回分待つ（書き込み開始〜終了 250ms + coalesce 500ms + マージン）
	time.Sleep(1500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// 5回の Write が 1〜2回の OnWrite にコアレッシングされている
	if len(writeEvents) == 0 {
		t.Fatal("expected at least 1 coalesced Write event, got 0")
	}
	if len(writeEvents) > 3 {
		t.Errorf("expected coalesced events (<=3), got %d", len(writeEvents))
	}
}

func TestMultiWatcher_NewFileDiscovery(t *testing.T) {
	baseDir := t.TempDir()
	projDir := filepath.Join(baseDir, "project1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mw, err := NewMultiWatcher(baseDir, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var newEvents []FileEvent
	mw.OnWrite = func(ev FileEvent) {}
	mw.OnNewFile = func(ev FileEvent) {
		mu.Lock()
		newEvents = append(newEvents, ev)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go mw.Run(ctx)

	// 初回 refresh を待つ（OnNewFile は呼ばれない）
	time.Sleep(100 * time.Millisecond)

	// 新しいファイルを作成（次の refresh で発見される）
	jsonlPath := filepath.Join(projDir, "sess-new-001.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// refresh 間隔 + マージン
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(newEvents)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for OnNewFile callback")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if newEvents[0].SessionID != "sess-new-001" {
		t.Errorf("SessionID = %q, want %q", newEvents[0].SessionID, "sess-new-001")
	}
}

func TestMultiWatcher_InitialRefreshSkipsOnNewFile(t *testing.T) {
	baseDir := t.TempDir()
	projDir := filepath.Join(baseDir, "project1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 起動前に既存ファイルを作成
	for _, name := range []string{"existing-001.jsonl", "existing-002.jsonl"} {
		if err := os.WriteFile(filepath.Join(projDir, name), []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mw, err := NewMultiWatcher(baseDir, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var newEvents []FileEvent
	mw.OnWrite = func(ev FileEvent) {}
	mw.OnNewFile = func(ev FileEvent) {
		mu.Lock()
		newEvents = append(newEvents, ev)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go mw.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	cancel()

	mu.Lock()
	defer mu.Unlock()

	// 初回 refresh では OnNewFile が呼ばれないこと
	if len(newEvents) != 0 {
		t.Errorf("expected 0 OnNewFile events on initial refresh, got %d", len(newEvents))
	}
}

func TestMultiWatcher_ExcludesSubagents(t *testing.T) {
	baseDir := t.TempDir()
	projDir := filepath.Join(baseDir, "project1")
	subDir := filepath.Join(projDir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// subagents 配下にファイルを作成
	subPath := filepath.Join(subDir, "sub-001.jsonl")
	if err := os.WriteFile(subPath, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 通常のファイルも作成
	mainPath := filepath.Join(projDir, "main-001.jsonl")
	if err := os.WriteFile(mainPath, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mw, err := NewMultiWatcher(baseDir, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var newEvents []FileEvent
	mw.OnWrite = func(ev FileEvent) {}
	mw.OnNewFile = func(ev FileEvent) {
		mu.Lock()
		newEvents = append(newEvents, ev)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go mw.Run(ctx)

	// 初回（OnNewFile スキップ）+ 2回目の refresh を待つ
	time.Sleep(500 * time.Millisecond)

	// 新しいファイルを追加して次の refresh で検出させる
	newPath := filepath.Join(projDir, "new-001.jsonl")
	if err := os.WriteFile(newPath, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subPath2 := filepath.Join(subDir, "sub-002.jsonl")
	if err := os.WriteFile(subPath2, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)
	cancel()

	mu.Lock()
	defer mu.Unlock()

	// subagents のファイルは含まれず、new-001 のみ
	for _, ev := range newEvents {
		if strings.Contains(ev.Path, "subagents") {
			t.Errorf("subagent file should be excluded: %s", ev.Path)
		}
	}
}

func TestMultiWatcher_ContextCancel(t *testing.T) {
	baseDir := t.TempDir()

	mw, err := NewMultiWatcher(baseDir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mw.OnWrite = func(ev FileEvent) {}
	mw.OnNewFile = func(ev FileEvent) {}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		mw.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// OK: Run exited
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}
