// Package claudecode provides utilities for interacting with Claude Code's
// configuration files.
package claudecode

import (
	json "encoding/json/v2"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pomesaka/sandbox/claude-deck/internal/debuglog"
)

// configPath returns the path to ~/.claude.json.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// EnsureDataDirTrusted ensures that the data directory is recognized as a
// trusted workspace by Claude Code.
//
// Claude Code は cwd から親を辿って .git を探し、見つかったディレクトリを git root として
// ~/.claude.json の projects[gitRoot].hasTrustDialogAccepted で trust 判定する。
// dataDir に空の .git ディレクトリを置き、そのパスを trusted として登録することで、
// 配下の全ワークスペースで trust プロンプトをスキップする。
func EnsureDataDirTrusted(dataDir string) error {
	// dataDir/.git を作成（既にあればスキップ）
	gitDir := filepath.Join(dataDir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		return fmt.Errorf("creating .git dir: %w", err)
	}

	// ~/.claude.json に trust 登録
	cfgPath, err := configPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config: %w", err)
	}

	var config map[string]jsontext.Value
	if len(data) > 0 {
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("parsing config: %w", err)
		}
	}
	if config == nil {
		config = make(map[string]jsontext.Value)
	}

	var projects map[string]jsontext.Value
	if raw, ok := config["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			projects = make(map[string]jsontext.Value)
		}
	}
	if projects == nil {
		projects = make(map[string]jsontext.Value)
	}

	// 既に trust 済みかチェック
	if raw, ok := projects[dataDir]; ok {
		var entry map[string]jsontext.Value
		if err := json.Unmarshal(raw, &entry); err == nil {
			if val, ok := entry["hasTrustDialogAccepted"]; ok && string(val) == "true" {
				debuglog.Printf("[claudecode] dataDir already trusted: %s", dataDir)
				return nil
			}
		}
	}

	// エントリ作成（既存があればマージ）
	var entry map[string]any
	if raw, ok := projects[dataDir]; ok {
		_ = json.Unmarshal(raw, &entry)
	}
	if entry == nil {
		entry = map[string]any{}
	}
	entry["hasTrustDialogAccepted"] = true

	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling project entry: %w", err)
	}
	projects[dataDir] = jsontext.Value(entryJSON)

	projectsJSON, err := json.Marshal(projects)
	if err != nil {
		return fmt.Errorf("marshaling projects: %w", err)
	}
	config["projects"] = jsontext.Value(projectsJSON)

	out, err := json.Marshal(config, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	debuglog.Printf("[claudecode] dataDir trusted: %s", dataDir)
	return nil
}
