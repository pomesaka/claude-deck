// Package ratelimits watches the rate-limits.json file written by the claude-deck
// statusline script and provides Claude.ai subscription rate limit data.
//
// Data flow:
//  1. Claude Code invokes DataDir/statusline.sh after each assistant message
//  2. The script writes {rate_limits: ...} JSON to DataDir/rate-limits.json
//  3. Watch() detects the write via fsnotify and calls onUpdate
//
// Rate limit data is only available for Pro/Max subscribers and only after the
// first API response in a session; callers must handle the zero Status gracefully.
package ratelimits

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pomesaka/claude-deck/internal/debuglog"
)

// Window holds usage data for a single rate limit window.
type Window struct {
	// UsedPct is the usage percentage (0–100).
	UsedPct float64
	// ResetsAt is when this window resets.
	ResetsAt time.Time
}

// Status holds rate limit data from the Claude Code statusline JSON.
type Status struct {
	FiveHour          Window
	FiveHourAvailable bool
	SevenDay          Window
	SevenDayAvailable bool
}

// rateLimitsFile mirrors the JSON written by the statusline script.
type rateLimitsFile struct {
	RateLimits *struct {
		FiveHour *struct {
			UsedPct  float64 `json:"used_percentage"`
			ResetsAt int64   `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			UsedPct  float64 `json:"used_percentage"`
			ResetsAt int64   `json:"resets_at"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
}

const rateLimitsFileName = "rate-limits.json"

// RateLimitsFilePath returns the path to the rate limits file under dataDir.
func RateLimitsFilePath(dataDir string) string {
	return filepath.Join(dataDir, rateLimitsFileName)
}

// Load reads the rate limits file and returns its parsed Status.
// Returns a zero Status if the file is absent or unparseable.
func Load(dataDir string) Status {
	data, err := os.ReadFile(RateLimitsFilePath(dataDir))
	if err != nil {
		return Status{}
	}
	return parse(data)
}

func parse(data []byte) Status {
	var f rateLimitsFile
	if err := json.Unmarshal(data, &f); err != nil || f.RateLimits == nil {
		return Status{}
	}
	var s Status
	if w := f.RateLimits.FiveHour; w != nil {
		s.FiveHour = Window{UsedPct: w.UsedPct, ResetsAt: time.Unix(w.ResetsAt, 0)}
		s.FiveHourAvailable = true
	}
	if w := f.RateLimits.SevenDay; w != nil {
		s.SevenDay = Window{UsedPct: w.UsedPct, ResetsAt: time.Unix(w.ResetsAt, 0)}
		s.SevenDayAvailable = true
	}
	return s
}

// Watch monitors DataDir/rate-limits.json via fsnotify and calls onUpdate whenever
// the file is written with valid rate limit data. Blocks in a background goroutine
// until ctx is cancelled.
//
// If the file does not exist yet, the parent directory is watched instead and
// monitoring switches to the file once it appears.
func Watch(ctx context.Context, dataDir string, onUpdate func(Status)) error {
	path := RateLimitsFilePath(dataDir)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}

	// If the file doesn't exist yet, watch the dir until the file appears.
	watchTarget := path
	if _, err := os.Stat(path); os.IsNotExist(err) {
		watchTarget = dataDir
	}

	if err := watcher.Add(watchTarget); err != nil {
		watcher.Close()
		return fmt.Errorf("watching %s: %w", watchTarget, err)
	}

	go func() {
		defer watcher.Close()
		watchingDir := watchTarget == dataDir

		for {
			select {
			case <-ctx.Done():
				return

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				isOurFile := filepath.Clean(event.Name) == filepath.Clean(path)

				// If we're watching the dir, switch to the file once it appears.
				// Remove the dir watch to prevent duplicate events from both targets.
				if watchingDir && isOurFile && (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)) {
					if err := watcher.Add(path); err == nil {
						_ = watcher.Remove(dataDir)
						watchingDir = false
						debuglog.Printf("[ratelimits] switched to watching file %s", path)
					}
				}

				// The statusline script writes atomically via tmp→mv, so a Create event
				// on the target path means the file is already fully written and safe to read.
				if isOurFile && (event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) {
					s := Load(dataDir)
					if s.FiveHourAvailable || s.SevenDayAvailable {
						debuglog.Printf("[ratelimits] updated: 5h=%.0f%% 7d=%.0f%%",
							s.FiveHour.UsedPct, s.SevenDay.UsedPct)
						onUpdate(s)
					}
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				debuglog.Printf("[ratelimits] watcher error: %v", err)
			}
		}
	}()

	return nil
}
