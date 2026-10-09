// Package claudecode provides utilities for interacting with Claude Code's
// configuration files.
package claudecode

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pomesaka/claude-deck/internal/debuglog"
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
// dataDir に空の .git ディレクトリを置いて trusted として登録すると、.git を持たない
// ワークスペース（colocated でないリポジトリのもの）は dataDir が git ルートになり、
// trust プロンプトが出ない。.git を持つワークスペースには届かないので、そちらは
// 作成時に EnsureTrusted で個別に登録する。
func EnsureDataDirTrusted(dataDir string) error {
	gitDir := filepath.Join(dataDir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		return fmt.Errorf("creating .git dir: %w", err)
	}
	return EnsureTrusted(dataDir)
}

// EnsureTrusted registers dir as a trusted workspace in ~/.claude.json, so
// Claude Code started in it (or below it, up to its git root) does not show the
// trust dialog. Other keys of the entry and of the file are kept.
//
// WHY ディレクトリごとに登録する: Claude Code は cwd から git ルートまでの各ディレクトリについて
// projects[dir].hasTrustDialogAccepted を見て、git ルートより上は見ない
// （Claude Code 2.1.295 の本体で確認）。colocated リポジトリのワークスペースは .git の symlink を
// 持つので自分が git ルートになり、親の dataDir の登録は効かない。
// WHY NOT .git の symlink をやめる: ワークスペースで git / gh が使えなくなる。
func EnsureTrusted(dir string) error {
	changed, err := updateProjects(func(projects map[string]jsontext.Value) (bool, error) {
		var entry map[string]jsontext.Value
		if raw, ok := projects[dir]; ok {
			_ = json.Unmarshal(raw, &entry)
		}
		if string(entry["hasTrustDialogAccepted"]) == "true" {
			return false, nil
		}
		if entry == nil {
			entry = make(map[string]jsontext.Value)
		}
		entry["hasTrustDialogAccepted"] = jsontext.Value("true")
		entryJSON, err := json.Marshal(entry)
		if err != nil {
			return false, fmt.Errorf("marshaling project entry: %w", err)
		}
		projects[dir] = jsontext.Value(entryJSON)
		return true, nil
	})
	if err != nil {
		return err
	}
	debuglog.Printf("[claudecode] trust %s: changed=%v", dir, changed)
	return nil
}

// ForgetProjects removes what Claude Code recorded in ~/.claude.json about the
// directories match accepts, and returns how many entries that is. With dryRun
// it only counts. Claude Code adds an entry for every directory a session ran
// in and never removes one, so the entries of deleted workspaces would pile up.
func ForgetProjects(match func(dir string) bool, dryRun bool) (int, error) {
	n := 0
	_, err := updateProjects(func(projects map[string]jsontext.Value) (bool, error) {
		for key := range projects {
			if match(key) {
				n++
				if !dryRun {
					delete(projects, key)
				}
			}
		}
		return n > 0 && !dryRun, nil
	})
	if err != nil {
		return 0, err
	}
	debuglog.Printf("[claudecode] forget projects: n=%d dryRun=%v", n, dryRun)
	return n, nil
}

// updateProjects applies fn to the projects map of ~/.claude.json and writes
// the file back when fn reports a change. The rest of the file is kept.
//
// 動いている Claude Code もこのファイルを書き換える。読んでから書くまでの間に書かれた更新は
// 失われるので、fn が変更なしと答えたときは書かない。
func updateProjects(fn func(projects map[string]jsontext.Value) (bool, error)) (bool, error) {
	cfgPath, err := configPath()
	if err != nil {
		return false, fmt.Errorf("resolving config path: %w", err)
	}

	mode := os.FileMode(0o600)
	if info, err := os.Stat(cfgPath); err == nil {
		mode = info.Mode().Perm()
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("reading config: %w", err)
	}

	var config map[string]jsontext.Value
	if len(data) > 0 {
		if err := json.Unmarshal(data, &config); err != nil {
			return false, fmt.Errorf("parsing config: %w", err)
		}
	}
	if config == nil {
		config = make(map[string]jsontext.Value)
	}

	var projects map[string]jsontext.Value
	if raw, ok := config["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return false, fmt.Errorf("parsing projects: %w", err)
		}
	}
	if projects == nil {
		projects = make(map[string]jsontext.Value)
	}

	changed, err := fn(projects)
	if err != nil || !changed {
		return false, err
	}

	projectsJSON, err := json.Marshal(projects)
	if err != nil {
		return false, fmt.Errorf("marshaling projects: %w", err)
	}
	config["projects"] = jsontext.Value(projectsJSON)

	out, err := json.Marshal(config, jsontext.WithIndent("  "))
	if err != nil {
		return false, fmt.Errorf("marshaling config: %w", err)
	}

	// 一時ファイルに書いてから rename する。動いている Claude Code が書きかけの
	// ~/.claude.json を読まないようにする。
	tmp, err := os.CreateTemp(filepath.Dir(cfgPath), filepath.Base(cfgPath)+".*.tmp")
	if err != nil {
		return false, fmt.Errorf("writing config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return false, fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("writing config: %w", err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return false, fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmp.Name(), cfgPath); err != nil {
		return false, fmt.Errorf("writing config: %w", err)
	}
	return true, nil
}
