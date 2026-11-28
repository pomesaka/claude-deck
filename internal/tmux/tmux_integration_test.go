package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startServer runs a tmux server of the test's own (tmux -L) with one session,
// and kills it when the test ends.
func startServer(t *testing.T) *Runner {
	t.Helper()
	if _, err := exec.LookPath(DefaultCommand); err != nil {
		t.Skip("tmux is not installed")
	}
	r := &Runner{Socket: fmt.Sprintf("cdtest-%d-%d", os.Getpid(), time.Now().UnixNano()), SessionName: "test"}
	// 端末の幅を決めておく。既定の 80 桁で足りるが、環境の TMUX を引き継がないよう -L で分けている。
	if _, err := r.run("new-session", "-d", "-s", r.sess(), "-x", "120", "-y", "20", "sleep 600"); err != nil {
		t.Fatalf("starting tmux: %v", err)
	}
	t.Cleanup(func() { _, _ = r.run("kill-server") })
	return r
}

// waitFor polls cond until it holds or two seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunner_CapturePaneKeepsColours(t *testing.T) {
	r := startServer(t)
	if err := r.NewWindow("w", WindowOpts{Command: `printf 'x \033[2msuggest\033[0m\n'; sleep 600`}); err != nil {
		t.Fatal(err)
	}
	var screen string
	waitFor(t, "the window to draw", func() bool {
		screen, _ = r.CapturePane("w")
		return strings.Contains(screen, "suggest")
	})
	if !strings.Contains(screen, "\x1b[2msuggest") {
		t.Errorf("CapturePane() = %q, want the faint escape kept", screen)
	}
}

func TestRunner_SendLine(t *testing.T) {
	r := startServer(t)
	out := filepath.Join(t.TempDir(), "out")
	if err := r.NewWindow("w", WindowOpts{Command: fmt.Sprintf(`read l; printf %%s "$l" > %q; sleep 600`, out)}); err != nil {
		t.Fatal(err)
	}
	// "Enter" in the text must be typed, not pressed.
	if err := r.SendLine("w", "/clear Enter"); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	var got []byte
	waitFor(t, "the line to arrive", func() bool {
		got, _ = os.ReadFile(out)
		return len(got) > 0
	})
	if string(got) != "/clear Enter" {
		t.Errorf("the window read %q, want %q", got, "/clear Enter")
	}
}

func TestRunner_MissingWindow(t *testing.T) {
	r := startServer(t)
	if _, err := r.CapturePane("nowhere"); err == nil {
		t.Error("CapturePane of a missing window succeeded")
	}
	if err := r.SendLine("nowhere", "/clear"); err == nil {
		t.Error("SendLine to a missing window succeeded")
	}
}
