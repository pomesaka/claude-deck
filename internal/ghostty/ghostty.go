package ghostty

import (
	"fmt"
	"os/exec"
)

// Launcher handles opening Ghostty terminal windows.
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
// title が非空の場合、ウィンドウタイトルを設定する。
func (l *Launcher) Open(workDir string, title string) error {
	args := []string{"--working-directory=" + workDir}
	if title != "" {
		args = append(args, "--title="+title)
	}
	cmd := exec.Command(l.command, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching ghostty: %w", err)
	}
	// Detach from the process
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}
