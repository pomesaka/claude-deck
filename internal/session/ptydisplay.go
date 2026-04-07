package session

import (
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/charmbracelet/x/vt"
	"github.com/pomesaka/claude-deck/internal/debuglog"
)

// PTYDisplay manages the virtual terminal emulator and display cache for
// embedded PTY sessions. It encapsulates all emulator-related state that
// was previously spread across Session fields.
//
// PTYDisplay is nil for HostExternal sessions — they have no emulator.
//
// Lock ordering: emuMu is independent of Session.mu and Session.rt.mu.
// Callers that need both must acquire emuMu first (same as the old emuMu rule).
type PTYDisplay struct {
	emuMu    sync.Mutex
	emulator *vt.Emulator

	// Written during emulator callbacks (emuMu held via emulator.Write).
	// No separate lock needed — readers also hold emuMu or use atomics.
	title            string
	scrollbackPlain  []string
	scrollbackStyled []string
	maxScrollback    int

	// Atomic display cache — lock-free reads for TUI rendering.
	displayCache        atomic.Pointer[[]string]
	cursorYHighWatermark atomic.Int32
	displayCursorX      atomic.Int32
	displayCursorY      atomic.Int32

	// Stable cursor from \033[?25h callback.
	stableCursorX       atomic.Int32
	stableCursorScreenY atomic.Int32
	stableCursorReady   atomic.Bool

	// scrollbackLen is an atomic copy of len(scrollbackStyled) for lock-free
	// cursor position calculation in CursorPosition().
	scrollbackLen atomic.Int32

	// onTitle bridges title changes back to Session.TerminalTitle.
	onTitle func(title string)

	// sessionID is used only for debug logging.
	sessionID string
}

// newPTYDisplay creates a PTYDisplay with a fresh emulator and wired callbacks.
// onTitle is called (under emuMu) whenever the OSC title changes.
func newPTYDisplay(sessionID string, cols, rows, maxScrollback int, onTitle func(string)) *PTYDisplay {
	if cols <= 0 {
		cols = defaultPTYCols
	}
	if rows <= 0 {
		rows = defaultPTYRows
	}

	d := &PTYDisplay{
		maxScrollback: maxScrollback,
		onTitle:       onTitle,
		sessionID:     sessionID,
	}

	em := vt.NewEmulator(cols, rows)

	// Drain DA1/DA2 responses to prevent blocking (unbuffered io.Pipe).
	go io.Copy(io.Discard, em) //nolint:errcheck

	em.SetCallbacks(vt.Callbacks{
		Title: func(title string) {
			// Called under emuMu (via emulator.Write).
			if !utf8.ValidString(title) {
				debuglog.Printf("[session:%s] OSC title invalid UTF-8, ignoring: %x", d.sessionID, title)
				return
			}
			clean := stripSpinnerPrefix(title)
			debuglog.Printf("[session:%s] OSC title raw=%q clean=%q", d.sessionID, title, clean)
			d.title = clean
			if d.onTitle != nil {
				d.onTitle(clean)
			}
		},
		ScrollOut: func(plain, styled string) {
			// Called under emuMu (via emulator.Write).
			limit := d.maxScrollback
			if limit <= 0 {
				limit = 2000
			}
			d.scrollbackPlain = append(d.scrollbackPlain, plain)
			d.scrollbackStyled = append(d.scrollbackStyled, styled)
			if len(d.scrollbackPlain) > limit {
				drop := len(d.scrollbackPlain) - limit
				newPlain := make([]string, limit)
				copy(newPlain, d.scrollbackPlain[drop:])
				d.scrollbackPlain = newPlain
				newStyled := make([]string, limit)
				copy(newStyled, d.scrollbackStyled[drop:])
				d.scrollbackStyled = newStyled
			}
			d.scrollbackLen.Store(int32(len(d.scrollbackStyled)))
		},
		CursorVisibility: func(visible bool) {
			// Called under emuMu (via emulator.Write).
			if !visible {
				return
			}
			pos := em.CursorPosition()
			d.stableCursorX.Store(int32(pos.X))
			d.stableCursorScreenY.Store(int32(pos.Y))
			d.stableCursorReady.Store(true)
			debuglog.Printf("[session:%s] stableCursor: x=%d screenY=%d", d.sessionID, pos.X, pos.Y)
		},
	})

	d.emulator = em
	return d
}

// Write feeds raw PTY output to the emulator and updates the display cache.
func (d *PTYDisplay) Write(data []byte) {
	d.emuMu.Lock()
	debuglog.Printf("[session:%s] emulator.Write %d bytes hex=%x", d.sessionID, len(data), data)
	d.emulator.Write(data) //nolint:errcheck
	debuglog.Printf("[session:%s] emulator.Write done", d.sessionID)
	d.refreshDisplayCacheLocked()
	d.emuMu.Unlock()
}

// WriteLine feeds a line (with trailing newline) to the emulator.
// Test compatibility — production code uses Write.
func (d *PTYDisplay) WriteLine(line string) {
	d.emuMu.Lock()
	d.emulator.Write([]byte(line + "\n")) //nolint:errcheck
	d.refreshDisplayCacheLocked()
	d.emuMu.Unlock()
}

// Reset replaces the emulator with a fresh one at the given dimensions.
// Used by ResumeSession/ForkSession to start with a clean screen.
func (d *PTYDisplay) Reset(cols, rows int) {
	d.emuMu.Lock()
	defer d.emuMu.Unlock()

	if cols <= 0 {
		cols = defaultPTYCols
	}
	if rows <= 0 {
		rows = defaultPTYRows
	}

	// Reset cursor caches.
	d.stableCursorReady.Store(false)
	d.stableCursorX.Store(0)
	d.stableCursorScreenY.Store(0)
	d.cursorYHighWatermark.Store(0)

	// Reset scrollback.
	d.scrollbackPlain = nil
	d.scrollbackStyled = nil
	d.scrollbackLen.Store(0)
	d.title = ""

	em := vt.NewEmulator(cols, rows)
	go io.Copy(io.Discard, em) //nolint:errcheck

	em.SetCallbacks(vt.Callbacks{
		Title: func(title string) {
			if !utf8.ValidString(title) {
				debuglog.Printf("[session:%s] OSC title invalid UTF-8, ignoring: %x", d.sessionID, title)
				return
			}
			clean := stripSpinnerPrefix(title)
			debuglog.Printf("[session:%s] OSC title raw=%q clean=%q", d.sessionID, title, clean)
			d.title = clean
			if d.onTitle != nil {
				d.onTitle(clean)
			}
		},
		ScrollOut: func(plain, styled string) {
			limit := d.maxScrollback
			if limit <= 0 {
				limit = 2000
			}
			d.scrollbackPlain = append(d.scrollbackPlain, plain)
			d.scrollbackStyled = append(d.scrollbackStyled, styled)
			if len(d.scrollbackPlain) > limit {
				drop := len(d.scrollbackPlain) - limit
				newPlain := make([]string, limit)
				copy(newPlain, d.scrollbackPlain[drop:])
				d.scrollbackPlain = newPlain
				newStyled := make([]string, limit)
				copy(newStyled, d.scrollbackStyled[drop:])
				d.scrollbackStyled = newStyled
			}
			d.scrollbackLen.Store(int32(len(d.scrollbackStyled)))
		},
		CursorVisibility: func(visible bool) {
			if !visible {
				return
			}
			pos := em.CursorPosition()
			d.stableCursorX.Store(int32(pos.X))
			d.stableCursorScreenY.Store(int32(pos.Y))
			d.stableCursorReady.Store(true)
			debuglog.Printf("[session:%s] stableCursor: x=%d screenY=%d", d.sessionID, pos.X, pos.Y)
		},
	})

	d.emulator = em
}

// Resize changes the emulator dimensions.
func (d *PTYDisplay) Resize(cols, rows int) {
	d.emuMu.Lock()
	d.emulator.Resize(cols, rows)
	d.emuMu.Unlock()
}

// Lines returns the cached display lines. Non-blocking (atomic read).
func (d *PTYDisplay) Lines() []string {
	if p := d.displayCache.Load(); p != nil {
		return *p
	}
	return nil
}

// CursorPosition returns the cursor's position within Lines().
// X is the terminal column (0-indexed), Y is the line index in Lines().
func (d *PTYDisplay) CursorPosition() (x, y int) {
	if d.stableCursorReady.Load() {
		screenY := int(d.stableCursorScreenY.Load())
		scrollback := int(d.scrollbackLen.Load())
		return int(d.stableCursorX.Load()), scrollback + screenY
	}
	return int(d.displayCursorX.Load()), int(d.displayCursorY.Load())
}

// refreshDisplayCacheLocked must be called with emuMu held.
func (d *PTYDisplay) refreshDisplayCacheLocked() {
	plain := d.emulator.String()
	styled := d.emulator.Render()
	cursor := d.emulator.CursorPosition()
	cursorY := cursor.Y

	// High-watermark: prevent flicker during Ink redraws.
	if prevHW := int(d.cursorYHighWatermark.Load()); cursorY < prevHW {
		cursorY = prevHW
	} else {
		d.cursorYHighWatermark.Store(int32(cursorY))
	}

	lines := buildDisplayLines(plain, styled, cursorY, d.title, d.scrollbackStyled, d.sessionID)
	d.displayCache.Store(&lines)

	displayRow := int32(max(len(lines)-1, 0))
	d.displayCursorX.Store(int32(cursor.X))
	d.displayCursorY.Store(displayRow)
}

// buildDisplayLines constructs display lines from emulator snapshot.
// Pure function — no locks.
func buildDisplayLines(plain, styled string, cursorY int, title string, scrollbackStyled []string, sessionID string) []string {
	if plain == "" {
		return nil
	}

	plainLines := strings.Split(plain, "\n")
	styledLines := strings.Split(styled, "\n")

	limit := cursorY + 1
	if limit < len(plainLines) {
		plainLines = plainLines[:limit]
	}
	if limit < len(styledLines) {
		styledLines = styledLines[:limit]
	}

	// Trim trailing blank lines.
	for len(plainLines) > 0 && strings.TrimRight(plainLines[len(plainLines)-1], " ") == "" {
		plainLines = plainLines[:len(plainLines)-1]
	}
	if len(plainLines) == 0 {
		return nil
	}
	if len(styledLines) > len(plainLines) {
		styledLines = styledLines[:len(plainLines)]
	}

	// Remove Ink tab title line from bottom of screen.
	if title != "" {
		const scanRange = 8
		scanStart := max(0, len(plainLines)-scanRange)
		for i := len(plainLines) - 1; i >= scanStart; i-- {
			line := strings.TrimSpace(plainLines[i])
			if line == "" {
				continue
			}
			if strings.Contains(line, title) || (len(line) >= 4 && strings.Contains(title, line)) {
				debuglog.Printf("[session:%s] title filter: removed line[%d] %q (title=%q)", sessionID, i, line, title)
				plainLines = append(plainLines[:i], plainLines[i+1:]...)
				if i < len(styledLines) {
					styledLines = append(styledLines[:i], styledLines[i+1:]...)
				}
			}
		}
		for len(plainLines) > 0 && strings.TrimRight(plainLines[len(plainLines)-1], " ") == "" {
			plainLines = plainLines[:len(plainLines)-1]
		}
		if len(styledLines) > len(plainLines) {
			styledLines = styledLines[:len(plainLines)]
		}
	}

	// Combine scrollback + screen.
	var result []string
	if len(scrollbackStyled) > 0 {
		result = make([]string, 0, len(scrollbackStyled)+len(styledLines))
		result = append(result, scrollbackStyled...)
		result = append(result, styledLines...)
	} else {
		result = styledLines
	}
	debuglog.Printf("[session:%s] buildDisplayLines: %d lines (%d scrollback + %d screen, cursorY=%d)",
		sessionID, len(result), len(scrollbackStyled), len(styledLines), cursorY)
	return result
}
