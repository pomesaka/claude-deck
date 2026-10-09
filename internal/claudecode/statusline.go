package claudecode

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// statusLineChain matches the line of the wrapper script that hands the input
// on to the status line command the user had configured before the wrapper.
var statusLineChain = regexp.MustCompile(`(?m)^printf '%s' "\$input" \| '(.+)'$`)

// RestoreStatusLine undoes what claude-deck used to do to the user's status
// line: it had put a wrapper script (dataDir/statusline.sh) under "statusLine"
// in ~/.claude/settings.json to read the rate limits. The deck-status plugin
// reports them now (ADR-013), so the setting goes back to the command the
// wrapper chained to, or away when there was none, and the script is removed.
//
// Does nothing once the script is gone.
//
// FIXME: 古い版を使っていた全部のマシンでこの版を一度起動し終えたら、このファイルと
// main.go の呼び出しを削除する（ADR-013 の「悪い点」も直す）。
func RestoreStatusLine(dataDir string) error {
	settPath, err := claudeSettingsPath()
	if err != nil {
		return fmt.Errorf("resolving settings path: %w", err)
	}
	return restoreStatusLine(settPath, dataDir)
}

func restoreStatusLine(settPath, dataDir string) error {
	scriptPath := filepath.Clean(filepath.Join(dataDir, "statusline.sh"))
	script, err := os.ReadFile(scriptPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading statusline script: %w", err)
	}
	var prevCmd string
	if m := statusLineChain.FindSubmatch(script); m != nil {
		prevCmd = string(m[1])
	}

	settings, err := os.ReadFile(settPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading settings: %w", err)
	}
	if err == nil {
		restored, changed, err := unhookStatusLine(settings, scriptPath, prevCmd)
		if err != nil {
			return fmt.Errorf("parsing settings: %w", err)
		}
		if changed {
			if err := replaceFile(settPath, restored); err != nil {
				return fmt.Errorf("writing settings: %w", err)
			}
		}
	}

	if err := os.Remove(scriptPath); err != nil {
		return fmt.Errorf("removing statusline script: %w", err)
	}
	return nil
}

// unhookStatusLine returns settings with "statusLine" pointing at prevCmd
// instead of scriptPath, or without "statusLine" when prevCmd is empty. Settings
// whose status line is not scriptPath come back unchanged.
//
// WHY バイト列の差し替え: settings.json は利用者が手で書き、dotfiles のリポジトリで管理していることもある。
// map に読んで書き戻すと、キーの順序と整形が変わる。
func unhookStatusLine(settings []byte, scriptPath, prevCmd string) ([]byte, bool, error) {
	top, err := objectMembers(settings)
	if err != nil {
		return nil, false, err
	}
	i := memberIndex(top, "statusLine")
	if i < 0 {
		return settings, false, nil
	}
	statusLine := settings[top[i].valStart:top[i].valEnd]
	inner, err := objectMembers(statusLine)
	if err != nil {
		return settings, false, nil // not the object form: not ours
	}
	j := memberIndex(inner, "command")
	if j < 0 {
		return settings, false, nil
	}
	var command string
	if err := json.Unmarshal(statusLine[inner[j].valStart:inner[j].valEnd], &command); err != nil {
		return settings, false, nil
	}
	if filepath.Clean(expandHome(command)) != scriptPath {
		return settings, false, nil
	}

	if prevCmd == "" {
		from, to := top[i].keyStart, top[i].valEnd
		switch {
		case i+1 < len(top):
			to = top[i+1].keyStart
		case i > 0:
			from = top[i-1].valEnd
		}
		return splice(settings, from, to, nil), true, nil
	}
	quoted, err := json.Marshal(prevCmd)
	if err != nil {
		return nil, false, err
	}
	base := top[i].valStart
	return splice(settings, base+inner[j].valStart, base+inner[j].valEnd, quoted), true, nil
}

// member is where one member of a JSON object sits in its text.
type member struct {
	name                       string
	keyStart, valStart, valEnd int
}

// objectMembers returns the members of the JSON object in data, in order.
func objectMembers(data []byte) ([]member, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	if tok.Kind() != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	var members []member
	for dec.PeekKind() != '}' {
		key, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		var m member
		if err := json.Unmarshal(key, &m.name); err != nil {
			return nil, err
		}
		m.keyStart = int(dec.InputOffset()) - len(key)
		val, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		m.valEnd = int(dec.InputOffset())
		m.valStart = m.valEnd - len(val)
		members = append(members, m)
	}
	return members, nil
}

func memberIndex(members []member, name string) int {
	for i, m := range members {
		if m.name == name {
			return i
		}
	}
	return -1
}

func splice(data []byte, from, to int, with []byte) []byte {
	out := make([]byte, 0, len(data)-(to-from)+len(with))
	out = append(out, data[:from]...)
	out = append(out, with...)
	return append(out, data[to:]...)
}

// replaceFile writes data over the file at path through a temporary file.
func replaceFile(path string, data []byte) error {
	// WHY EvalSymlinks: settings.json は dotfiles のリポジトリへの symlink のことがある。
	// リンクのパスに rename すると、リンクが通常のファイルに置き換わる。
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
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
