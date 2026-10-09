// Package deckmod embeds the Claude Code plugin (a Mods hooks module that
// reports session status to claude-deck, and a skill that teaches the session
// the claude-deck CLI), and installs it where Claude Code can load it with
// --plugin-dir.
package deckmod

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:.claude-plugin hooks/hooks.json hooks/register.ts skills
var files embed.FS

// Install writes the plugin into dir, leaving files that already match.
// Returns dir, ready to pass as --plugin-dir.
func Install(dir string) (string, error) {
	err := fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, path)
		if got, err := os.ReadFile(dst); err == nil && bytes.Equal(got, want) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		// 一時ファイルに書いてから rename する。起動中の Claude Code が書きかけを読まないように。
		// 一時ファイル名は呼び出しごとに変える。TUI と CLI が同時に展開しても互いの書きかけを rename しない。
		tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(want); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Chmod(tmp.Name(), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), dst)
	})
	if err != nil {
		return "", fmt.Errorf("installing claude-deck plugin: %w", err)
	}
	return dir, nil
}
