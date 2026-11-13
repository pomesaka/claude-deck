package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// claudeArgsKey is the key of ProjectConfig.ClaudeArgs in the config file.
const claudeArgsKey = "claude_args"

var claudeArgsLineRE = regexp.MustCompile(`^\s*` + claudeArgsKey + `\s*=`)

// SetProjectClaudeArgs rewrites claude_args of [projects."<repoPath>"] in the config
// file at path, creating the file and the table when missing. Empty args remove the key.
func SetProjectClaudeArgs(path, repoPath string, args []string) error {
	// WHY: 設定ファイルが dotfiles へのシンボリックリンクでも、リンクを実ファイルで置き換えない。
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	mode := os.FileMode(0o644)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("creating config dir: %w", err)
		}
	default:
		return fmt.Errorf("reading config: %w", err)
	}

	out, err := editProjectClaudeArgs(data, repoPath, args)
	if err != nil {
		return err
	}
	if string(out) == string(data) {
		return nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.toml.*")
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// editProjectClaudeArgs returns data with claude_args of [projects."<repoPath>"] set to
// args (removed when args is empty). Every other line is kept as it is.
//
// WHY 行単位で書き換える: BurntSushi/toml v1.6.0 で decode して encode し直すと、
// コメントと項目の並びが失われる。
// WHY 書き換えた後に読み直して比べる: 行単位の編集は、[projects] を inline table や
// dotted key で書いた設定を正しく扱えない。その形のときに壊れた設定を書き出さず、
// エラーにして手での編集に任せる。
func editProjectClaudeArgs(data []byte, repoPath string, args []string) ([]byte, error) {
	var before map[string]any
	if err := toml.Unmarshal(data, &before); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	newLine := ""
	if len(args) > 0 {
		encoded, err := toml.Marshal(map[string][]string{claudeArgsKey: args})
		if err != nil {
			return nil, fmt.Errorf("encoding %s: %w", claudeArgsKey, err)
		}
		newLine = string(encoded)
	}

	header, end := findProjectTable(lines, repoPath)
	switch {
	case header < 0 && newLine == "":
		// nothing to remove
	case header < 0:
		if len(lines) > 0 {
			lines[len(lines)-1] = withNewline(lines[len(lines)-1])
			lines = append(lines, "\n")
		}
		lines = append(lines, "[projects."+strconv.Quote(repoPath)+"]\n", newLine)
	default:
		start, stop := findClaudeArgs(lines, header+1, end)
		if start < 0 {
			// Insert after the table's last key, before the blank lines and comments
			// that lead to the next table.
			start = header + 1
			for i := end - 1; i > header; i-- {
				if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
					start = i + 1
					break
				}
			}
			stop = start
			lines[start-1] = withNewline(lines[start-1])
		}
		var replaced []string
		replaced = append(replaced, lines[:start]...)
		if newLine != "" {
			replaced = append(replaced, newLine)
		}
		lines = append(replaced, lines[stop:]...)
	}
	out := []byte(strings.Join(lines, ""))

	var after map[string]any
	if err := toml.Unmarshal(out, &after); err != nil {
		return nil, fmt.Errorf("%s を書き換えられない形の設定です。config.toml を手で編集してください: %w", claudeArgsKey, err)
	}
	if !reflect.DeepEqual(withClaudeArgs(before, repoPath, args), after) {
		return nil, fmt.Errorf("%s を書き換えられない形の設定です。config.toml を手で編集してください", claudeArgsKey)
	}
	return out, nil
}

// findProjectTable returns the line index of the [projects."<repoPath>"] header and
// the index one past the table's last line. header is -1 when there is no such table.
func findProjectTable(lines []string, repoPath string) (header, end int) {
	header = -1
	for i, line := range lines {
		table, ok := parseTableHeader(line)
		if !ok {
			continue
		}
		if header >= 0 {
			return header, i
		}
		if projects, ok := table["projects"].(map[string]any); ok {
			if _, ok := projects[repoPath]; ok && len(table) == 1 && len(projects) == 1 {
				header = i
			}
		}
	}
	return header, len(lines)
}

// parseTableHeader decodes a line as a document of its own when it looks like a
// table header, so quoting and escapes in the table name are read by the TOML parser.
func parseTableHeader(line string) (map[string]any, bool) {
	if !strings.HasPrefix(strings.TrimSpace(line), "[") {
		return nil, false
	}
	var table map[string]any
	if err := toml.Unmarshal([]byte(line), &table); err != nil {
		return nil, false
	}
	return table, true
}

// findClaudeArgs returns the lines [start, stop) of the claude_args key within
// lines[from:to]. start is -1 when the key is absent.
func findClaudeArgs(lines []string, from, to int) (start, stop int) {
	for i := from; i < to; i++ {
		if !claudeArgsLineRE.MatchString(lines[i]) {
			continue
		}
		// An array may span lines: the value ends at the first line where the text
		// from the key parses as a document.
		for j := i + 1; j <= to; j++ {
			var v map[string]any
			if toml.Unmarshal([]byte(strings.Join(lines[i:j], "")), &v) == nil {
				return i, j
			}
		}
		return i, i + 1
	}
	return -1, -1
}

func withNewline(line string) string {
	if strings.HasSuffix(line, "\n") {
		return line
	}
	return line + "\n"
}

// withClaudeArgs returns the decoded config doc with claude_args of the project set
// to args: what the file must decode to after the edit.
func withClaudeArgs(doc map[string]any, repoPath string, args []string) map[string]any {
	if doc == nil {
		doc = map[string]any{}
	}
	projects, _ := doc["projects"].(map[string]any)
	project, _ := projects[repoPath].(map[string]any)
	if len(args) == 0 {
		delete(project, claudeArgsKey)
		return doc
	}
	if projects == nil {
		projects = map[string]any{}
		doc["projects"] = projects
	}
	if project == nil {
		project = map[string]any{}
		projects[repoPath] = project
	}
	value := make([]any, len(args))
	for i, a := range args {
		value[i] = a
	}
	project[claudeArgsKey] = value
	return doc
}
