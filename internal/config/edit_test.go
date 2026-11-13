package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEditProjectClaudeArgs(t *testing.T) {
	channels := []string{"--dangerously-load-development-channels", "server:xwb"}
	tests := []struct {
		name string
		in   string
		repo string
		args []string
		want string
	}{
		{
			name: "empty file gets the table",
			in:   "",
			repo: "/repo/a",
			args: channels,
			want: "[projects.\"/repo/a\"]\nclaude_args = [\"--dangerously-load-development-channels\", \"server:xwb\"]\n",
		},
		{
			name: "new table is appended after a blank line",
			in:   "# top\n[theme]\nprimary = \"#fff\" # keep\n",
			repo: "/repo/a",
			args: []string{"--verbose"},
			want: "# top\n[theme]\nprimary = \"#fff\" # keep\n\n[projects.\"/repo/a\"]\nclaude_args = [\"--verbose\"]\n",
		},
		{
			name: "file without trailing newline",
			in:   "[theme]\nprimary = \"#fff\"",
			repo: "/repo/a",
			args: []string{"--verbose"},
			want: "[theme]\nprimary = \"#fff\"\n\n[projects.\"/repo/a\"]\nclaude_args = [\"--verbose\"]\n",
		},
		{
			name: "key is added after the last key of an existing table",
			in:   "[projects.\"/repo/a\"]\nworkspace_symlinks = [\".env\"]\n\n# next\n[projects.\"/repo/b\"]\nworkspace_symlinks = [\"x\"]\n",
			repo: "/repo/a",
			args: []string{"--verbose"},
			want: "[projects.\"/repo/a\"]\nworkspace_symlinks = [\".env\"]\nclaude_args = [\"--verbose\"]\n\n# next\n[projects.\"/repo/b\"]\nworkspace_symlinks = [\"x\"]\n",
		},
		{
			name: "key is added to the last table of a file without trailing newline",
			in:   "[projects.\"/repo/a\"]\nworkspace_symlinks = [\".env\"]",
			repo: "/repo/a",
			args: []string{"--verbose"},
			want: "[projects.\"/repo/a\"]\nworkspace_symlinks = [\".env\"]\nclaude_args = [\"--verbose\"]\n",
		},
		{
			name: "key is added to a table with no keys",
			in:   "[projects.\"/repo/a\"]\n# empty\n[theme]\nprimary = \"#fff\"\n",
			repo: "/repo/a",
			args: []string{"--verbose"},
			want: "[projects.\"/repo/a\"]\nclaude_args = [\"--verbose\"]\n# empty\n[theme]\nprimary = \"#fff\"\n",
		},
		{
			name: "existing value is replaced in place",
			in:   "[projects.\"/repo/a\"]\nclaude_args = [\"--old\"]\nworkspace_symlinks = [\".env\"]\n",
			repo: "/repo/a",
			args: []string{"--new", "v"},
			want: "[projects.\"/repo/a\"]\nclaude_args = [\"--new\", \"v\"]\nworkspace_symlinks = [\".env\"]\n",
		},
		{
			name: "multi-line value is replaced whole",
			in:   "[projects.\"/repo/a\"]\nclaude_args = [\n  \"--old\", # why\n  \"v\",\n]\nworkspace_symlinks = [\".env\"]\n",
			repo: "/repo/a",
			args: []string{"--new"},
			want: "[projects.\"/repo/a\"]\nclaude_args = [\"--new\"]\nworkspace_symlinks = [\".env\"]\n",
		},
		{
			name: "only the named project changes",
			in:   "[projects.\"/repo/a\"]\nclaude_args = [\"--a\"]\n[projects.\"/repo/b\"]\nclaude_args = [\"--b\"]\n",
			repo: "/repo/b",
			args: []string{"--new"},
			want: "[projects.\"/repo/a\"]\nclaude_args = [\"--a\"]\n[projects.\"/repo/b\"]\nclaude_args = [\"--new\"]\n",
		},
		{
			name: "literal-quoted table name is found",
			in:   "[projects.'/repo/a']\nclaude_args = [\"--old\"]\n",
			repo: "/repo/a",
			args: []string{"--new"},
			want: "[projects.'/repo/a']\nclaude_args = [\"--new\"]\n",
		},
		{
			name: "empty args remove the key",
			in:   "[projects.\"/repo/a\"]\nworkspace_symlinks = [\".env\"]\nclaude_args = [\"--old\"]\n",
			repo: "/repo/a",
			want: "[projects.\"/repo/a\"]\nworkspace_symlinks = [\".env\"]\n",
		},
		{
			name: "removing from a project without the table changes nothing",
			in:   "[theme]\nprimary = \"#fff\"\n",
			repo: "/repo/a",
			want: "[theme]\nprimary = \"#fff\"\n",
		},
		{
			name: "argument with quote and backslash is escaped",
			in:   "",
			repo: "/repo/a",
			args: []string{`--x="a\b"`},
			want: "[projects.\"/repo/a\"]\nclaude_args = [\"--x=\\\"a\\\\b\\\"\"]\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := editProjectClaudeArgs([]byte(tt.in), tt.repo, tt.args)
			if err != nil {
				t.Fatalf("editProjectClaudeArgs: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestEditProjectClaudeArgs_RejectsFormsItCannotEdit(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "invalid TOML", in: "[projects\n"},
		{name: "project as inline table", in: "[projects]\n\"/repo/a\" = { claude_args = [\"--old\"] }\n"},
		{name: "project as dotted key", in: "[projects]\n\"/repo/a\".claude_args = [\"--old\"]\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := editProjectClaudeArgs([]byte(tt.in), "/repo/a", []string{"--new"}); err == nil {
				t.Errorf("want error, got:\n%s", got)
			}
		})
	}
}

func TestSetProjectClaudeArgs_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	args := []string{"--dangerously-load-development-channels", "server:xwb"}

	if err := SetProjectClaudeArgs(path, "/repo/a", args); err != nil {
		t.Fatalf("set: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := cfg.ClaudeArgs("/repo/a"); !slices.Equal(got, args) {
		t.Errorf("ClaudeArgs(/repo/a) = %v, want %v", got, args)
	}
	if got := cfg.ClaudeArgs("/repo/b"); got != nil {
		t.Errorf("ClaudeArgs(/repo/b) = %v, want nil", got)
	}

	if err := SetProjectClaudeArgs(path, "/repo/a", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[projects.\"/repo/a\"]\n"; string(data) != want {
		t.Errorf("after clear:\n%s\nwant:\n%s", data, want)
	}
}
