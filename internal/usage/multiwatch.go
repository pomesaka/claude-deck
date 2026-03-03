package usage

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pomesaka/claude-deck/internal/debuglog"
)

// FileEvent represents a filesystem event for a JSONL session file.
type FileEvent struct {
	SessionID string
	Path      string
	ModTime   time.Time
}

// MultiWatcher watches the most recently modified JSONL files for Write events
// using fsnotify, and periodically refreshes the watch list via glob + mtime sort.
//
// Write イベントは即時発火せず pending に蓄積し、coalesceInterval 間隔で
// 一括 stat → OnWrite する。これにより高頻度書き込みでも UI 更新が安定する。
type MultiWatcher struct {
	baseDir          string
	watcher          *fsnotify.Watcher
	watched          map[string]bool      // paths currently being watched
	pending          map[string]struct{}   // paths with pending Write events
	OnWrite          func(FileEvent)      // called on coalesced Write events
	OnNewFile        func(FileEvent)      // called when a new file enters the watch list
	refreshInterval  time.Duration        // watch list re-glob interval
	coalesceInterval time.Duration        // Write event coalesce window
	maxFiles         int
	initialized      bool // true after first refreshWatchList completes
}

// NewMultiWatcher creates a MultiWatcher that watches JSONL files under baseDir.
// refreshInterval controls how often the watch list is re-evaluated via glob.
func NewMultiWatcher(baseDir string, refreshInterval time.Duration) (*MultiWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &MultiWatcher{
		baseDir:          baseDir,
		watcher:          w,
		watched:          make(map[string]bool),
		pending:          make(map[string]struct{}),
		refreshInterval:  refreshInterval,
		coalesceInterval: 2 * time.Second,
		maxFiles:         30,
	}, nil
}

// Run starts the event loop. It blocks until ctx is cancelled.
// The caller should invoke this in a goroutine.
func (mw *MultiWatcher) Run(ctx context.Context) {
	defer mw.watcher.Close()

	// 初回はウォッチリスト構築のみ。OnNewFile はスキップして
	// DiscoverExternalSessions との競合を避ける。
	mw.refreshWatchList()
	mw.initialized = true

	refreshTicker := time.NewTicker(mw.refreshInterval)
	defer refreshTicker.Stop()

	coalesceTicker := time.NewTicker(mw.coalesceInterval)
	defer coalesceTicker.Stop()

	debuglog.Printf("[multiwatch] Run started, watching %d files", len(mw.watched))

	for {
		select {
		case <-ctx.Done():
			debuglog.Printf("[multiwatch] Run stopping: ctx cancelled")
			return

		case event, ok := <-mw.watcher.Events:
			if !ok {
				debuglog.Printf("[multiwatch] Run stopping: Events channel closed")
				return
			}
			mw.handleEvent(event)

		case err, ok := <-mw.watcher.Errors:
			if !ok {
				debuglog.Printf("[multiwatch] Run stopping: Errors channel closed")
				return
			}
			debuglog.Printf("[multiwatch] fsnotify error: %v", err)

		case <-coalesceTicker.C:
			mw.flushPending()

		case <-refreshTicker.C:
			mw.refreshWatchList()
		}
	}
}

// handleEvent records a Write event as pending (stat は flushPending で一括実行).
func (mw *MultiWatcher) handleEvent(event fsnotify.Event) {
	path := event.Name
	debuglog.Printf("[multiwatch] event op=%s path=%s", event.Op, filepath.Base(path))

	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		delete(mw.watched, path)
		delete(mw.pending, path)
		return
	}

	if event.Has(fsnotify.Write) {
		mw.pending[path] = struct{}{}
	}
}

// flushPending processes all coalesced Write events.
// 各ファイルを stat して最新の mtime を取得し、OnWrite を呼ぶ。
func (mw *MultiWatcher) flushPending() {
	if len(mw.pending) == 0 {
		return
	}

	for path := range mw.pending {
		fi, err := os.Stat(path)
		if err != nil {
			delete(mw.pending, path)
			continue
		}

		sessionID := sessionIDFromPath(path)
		if mw.OnWrite != nil {
			mw.OnWrite(FileEvent{
				SessionID: sessionID,
				Path:      path,
				ModTime:   fi.ModTime(),
			})
		}
	}

	clear(mw.pending)
}

// refreshWatchList re-globs the baseDir, sorts by mtime, and adds/removes watches
// so only the top N files are watched.
// initialized == true の場合のみ、新規ファイルで OnNewFile を呼ぶ。
func (mw *MultiWatcher) refreshWatchList() {
	jsonlFiles, _ := filepath.Glob(filepath.Join(mw.baseDir, "*", "*.jsonl"))

	type fileEntry struct {
		path  string
		mtime time.Time
	}

	var entries []fileEntry
	for _, path := range jsonlFiles {
		// subagents パスは除外
		if isSubagentPath(path) {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		entries = append(entries, fileEntry{path: path, mtime: fi.ModTime()})
	}

	// mtime 降順
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mtime.After(entries[j].mtime)
	})

	if len(entries) > mw.maxFiles {
		entries = entries[:mw.maxFiles]
	}

	// 新しいウォッチ対象を差分計算
	desired := make(map[string]bool, len(entries))
	for _, e := range entries {
		desired[e.path] = true
	}

	// 不要になったパスの監視を解除
	for path := range mw.watched {
		if !desired[path] {
			_ = mw.watcher.Remove(path)
			delete(mw.watched, path)
		}
	}

	// 新規パスを追加
	for _, e := range entries {
		if mw.watched[e.path] {
			continue
		}
		if err := mw.watcher.Add(e.path); err != nil {
			continue
		}
		mw.watched[e.path] = true

		// 初回 refresh 時は OnNewFile をスキップ。
		// 初回のセッション追加は DiscoverExternalSessions に任せる。
		if mw.initialized && mw.OnNewFile != nil {
			sessionID := sessionIDFromPath(e.path)
			mw.OnNewFile(FileEvent{
				SessionID: sessionID,
				Path:      e.path,
				ModTime:   e.mtime,
			})
		}
	}
}
