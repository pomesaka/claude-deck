package ghostty

import (
	"fmt"
	"os/exec"
	"runtime"
)

// Launcher handles opening Ghostty terminal tabs/windows.
type Launcher struct {
	command string
}

// NewLauncher creates a new Ghostty launcher with the specified command.
func NewLauncher(command string) *Launcher {
	if command == "" {
		command = "ghostty"
	}
	return &Launcher{command: command}
}

// Open launches a new Ghostty terminal in the specified directory.
// macOS では `open -a Ghostty <dir>` で既存ウィンドウにタブとして開く。
// それ以外では ghostty CLI を直接起動する。
func (l *Launcher) Open(workDir string, title string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		// macOS: `open -a` は既存 Ghostty インスタンスに新規タブとして開く
		cmd = exec.Command("open", "-a", "Ghostty", workDir)
	} else {
		args := []string{"--working-directory=" + workDir}
		if title != "" {
			args = append(args, "--title="+title)
		}
		cmd = exec.Command(l.command, args...)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching ghostty: %w", err)
	}
	// Detach from the process
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}
