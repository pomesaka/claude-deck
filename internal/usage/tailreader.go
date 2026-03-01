package usage

import (
	"context"
	"io"
	"os"

	"github.com/fsnotify/fsnotify"
)

// TailReader implements io.Reader over a file, blocking on EOF
// until new data is written (detected via fsnotify).
type TailReader struct {
	f       *os.File
	watcher *fsnotify.Watcher
	ctx     context.Context
}

// NewTailReader opens path for tail-style streaming.
// If offset > 0, seeks to that position before reading.
// Caller must call Close when done.
func NewTailReader(ctx context.Context, path string, offset int64) (*TailReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		f.Close()
		return nil, err
	}

	if err := w.Add(path); err != nil {
		w.Close()
		f.Close()
		return nil, err
	}

	return &TailReader{f: f, watcher: w, ctx: ctx}, nil
}

// Read implements io.Reader. Blocks on EOF until the file grows.
func (r *TailReader) Read(p []byte) (int, error) {
	for {
		n, err := r.f.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			select {
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			case _, ok := <-r.watcher.Events:
				if !ok {
					return 0, io.EOF
				}
				continue
			case werr, ok := <-r.watcher.Errors:
				if !ok {
					return 0, io.EOF
				}
				if werr != nil {
					return 0, werr
				}
				continue
			}
		}
		return n, err
	}
}

// Close releases the watcher and file handle.
func (r *TailReader) Close() error {
	_ = r.watcher.Close()
	return r.f.Close()
}
