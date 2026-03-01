package jj

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Command is the jj executable path. Override from config before use.
var Command = "jj"

// CreateWorkspaceAt creates a new jj workspace at the specified path.
// The parent directory is created automatically if it doesn't exist.
func CreateWorkspaceAt(repoPath, name, wsPath string) error {
	if err := os.MkdirAll(filepath.Dir(wsPath), 0o755); err != nil {
		return fmt.Errorf("creating workspace parent dir: %w", err)
	}

	cmd := exec.Command(Command, "workspace", "add", "--name", name, wsPath)
	cmd.Dir = repoPath

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jj workspace add: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return nil
}

// ForgetWorkspace removes a jj workspace.
func ForgetWorkspace(repoPath, name string) error {
	cmd := exec.Command(Command, "workspace", "forget", name)
	cmd.Dir = repoPath

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jj workspace forget: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return nil
}

// ListWorkspaces returns the list of current jj workspaces.
func ListWorkspaces(repoPath string) ([]string, error) {
	cmd := exec.Command(Command, "workspace", "list")
	cmd.Dir = repoPath

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("jj workspace list: %s: %w", strings.TrimSpace(string(output)), err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var workspaces []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			// jj workspace list format: "name: <commit-id>"
			parts := strings.SplitN(line, ":", 2)
			workspaces = append(workspaces, strings.TrimSpace(parts[0]))
		}
	}

	return workspaces, nil
}

// WorkspaceStatus returns a brief status of the workspace (changed files count, etc.).
func WorkspaceStatus(wsPath string) (string, error) {
	cmd := exec.Command(Command, "status")
	cmd.Dir = wsPath

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("jj status: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return strings.TrimSpace(string(output)), nil
}
