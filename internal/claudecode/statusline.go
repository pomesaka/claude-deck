package claudecode

import (
	json "encoding/json/v2"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// statuslineScriptContent returns the shell script that Claude Code invokes for
// each statusline update. The script extracts rate_limits from the JSON sent via
// stdin and writes it atomically to dataDir/rate-limits.json for claude-deck to read.
//
// Paths are embedded directly (no shell variables) so the script works even if
// the XDG environment differs between Claude Code's launch context and claude-deck.
//
// If prevCmd is non-empty, the script chains to it and passes its stdout through,
// preserving any existing statusLine display output.
//
// Returns an error if any path contains a single quote, which cannot be safely
// embedded in a single-quoted shell argument.
func statuslineScriptContent(dataDir, prevCmd string) (string, error) {
	out := filepath.Join(dataDir, "rate-limits.json")
	tmp := out + ".tmp"

	for _, p := range []string{out, tmp, prevCmd} {
		if strings.Contains(p, "'") {
			return "", fmt.Errorf("path contains single quote and cannot be safely embedded in shell script: %q", p)
		}
	}

	// %%s in fmt.Sprintf becomes %s in the script (used by shell printf).
	script := fmt.Sprintf(`#!/bin/sh
# claude-deck statusline wrapper — managed by claude-deck, do not edit
input=$(cat)
printf '%%s' "$input" | jq -c '{rate_limits: .rate_limits}' \
  > '%s' 2>/dev/null \
  && mv '%s' '%s' 2>/dev/null
`, tmp, tmp, out)

	if prevCmd != "" {
		// Chain to the previously configured statusLine command so its display
		// output (shown in Claude Code's status bar) is preserved.
		script += fmt.Sprintf("printf '%%s' \"$input\" | '%s'\n", prevCmd)
	}
	return script, nil
}

// statusLineConfig mirrors the {"type":"command","command":"..."} object that
// Claude Code expects under the "statusLine" key in settings.json.
type statusLineConfig struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// SetupStatuslineHook writes the statusline wrapper script to dataDir and
// registers it in ~/.claude/settings.json under the "statusLine" key
// (camelCase, object form — the format recognised by Claude Code ≥ 2.1.80).
//
// If settings.json already has a "statusLine" command that is NOT our script,
// the existing command is embedded in our wrapper so its display output is
// preserved (chain mode).  If it already points to our script, this is a no-op.
//
// The legacy lowercase "statusline" string key (written by older claude-deck
// versions) is removed if present.
func SetupStatuslineHook(dataDir string) error {
	scriptPath := filepath.Clean(filepath.Join(dataDir, "statusline.sh"))

	settPath, err := claudeSettingsPath()
	if err != nil {
		return fmt.Errorf("resolving settings path: %w", err)
	}

	data, err := os.ReadFile(settPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading settings: %w", err)
	}

	var settings map[string]jsontext.Value
	if len(data) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parsing settings: %w", err)
		}
	}
	if settings == nil {
		settings = make(map[string]jsontext.Value)
	}

	// Parse existing statusLine config once; use filepath.Clean for comparison
	// to tolerate trailing slashes, symlinks, and similar path variations.
	var existingCmd string
	if existing, ok := settings["statusLine"]; ok {
		var sl statusLineConfig
		if json.Unmarshal(existing, &sl) == nil && sl.Type == "command" {
			existingCmd = sl.Command
		}
	}
	if filepath.Clean(expandHome(existingCmd)) == scriptPath {
		return nil // already configured to our script
	}

	// Chain to the existing command (if any) to preserve its display output.
	// filepath.Clean normalises the path before embedding it in the script.
	var prevCmd string
	if existingCmd != "" {
		prevCmd = filepath.Clean(expandHome(existingCmd))
	}

	// Write (or refresh) the wrapper script.
	content, err := statuslineScriptContent(dataDir, prevCmd)
	if err != nil {
		return fmt.Errorf("building statusline script: %w", err)
	}
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing statusline script: %w", err)
	}

	// Register under the correct camelCase key.
	cfgJSON, err := json.Marshal(statusLineConfig{Type: "command", Command: scriptPath})
	if err != nil {
		return fmt.Errorf("marshaling statusLine config: %w", err)
	}
	settings["statusLine"] = jsontext.Value(cfgJSON)

	// Remove the legacy lowercase key written by older versions.
	delete(settings, "statusline")

	out, err := json.Marshal(settings, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(settPath), 0o755); err != nil {
		return fmt.Errorf("creating settings dir: %w", err)
	}
	settTmp := settPath + ".tmp"
	if err := os.WriteFile(settTmp, out, 0o644); err != nil {
		return fmt.Errorf("writing settings tmp: %w", err)
	}
	if err := os.Rename(settTmp, settPath); err != nil {
		_ = os.Remove(settTmp)
		return fmt.Errorf("replacing settings: %w", err)
	}

	return nil
}

// expandHome replaces a leading "~/" with the user's home directory.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// claudeSettingsPath returns the path to ~/.claude/settings.json.
func claudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}
