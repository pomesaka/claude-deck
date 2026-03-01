package session

import (
	"testing"
)

func TestIsFullWidthSeparator(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"120 box-drawing dashes", "────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────", true},
		{"40 ASCII dashes", "----------------------------------------", true},
		{"20 dashes (minimum)", "────────────────────", true},
		{"19 dashes (too short)", "───────────────────", false},
		{"mixed content", "───── hello ─────", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFullWidthSeparator(tt.line); got != tt.want {
				t.Errorf("isFullWidthSeparator(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestFilterBottomChrome(t *testing.T) {
	sep := "────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────"

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "nil input",
			in:   nil,
			want: nil,
		},
		{
			name: "no separator - tool running",
			in: []string{
				"╭──────────────────────╮",
				"│ Running: go test ... │",
				"╰──────────────────────╯",
			},
			want: []string{
				"╭──────────────────────╮",
				"│ Running: go test ... │",
				"╰──────────────────────╯",
			},
		},
		{
			name: "actual Claude Code bottom chrome",
			in: []string{
				"⏺ Running tool: Bash",
				"  ⠹ go test ./...",
				sep,
				"❯\u00a0",
				sep,
				"  Opus 4.6 │ $0.382",
				" Session Activity Inquiry",
			},
			want: []string{
				"⏺ Running tool: Bash",
				"  ⠹ go test ./...",
			},
		},
		{
			name: "output with trailing blank then chrome",
			in: []string{
				"  残課題: ...",
				"",
				sep,
				"❯\u00a0",
				sep,
				"  Opus 4.6 │ $0.382",
			},
			want: []string{
				"  残課題: ...",
				"",
			},
		},
		{
			name: "many output lines with chrome at bottom",
			in: func() []string {
				lines := make([]string, 35)
				for i := range lines {
					lines[i] = "output line"
				}
				lines = append(lines, sep, "❯ ", sep, "status", "tabs")
				return lines
			}(),
			want: func() []string {
				lines := make([]string, 35)
				for i := range lines {
					lines[i] = "output line"
				}
				return lines
			}(),
		},
		{
			name: "no chrome at all",
			in: []string{
				"line 1",
				"line 2",
				"line 3",
			},
			want: []string{
				"line 1",
				"line 2",
				"line 3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterBottomChrome(tt.in)

			if tt.want == nil {
				if got != nil {
					t.Errorf("want nil, got %v", got)
				}
				return
			}

			if len(got) != len(tt.want) {
				t.Fatalf("length mismatch: want %d, got %d\nwant: %v\ngot:  %v", len(tt.want), len(got), tt.want, got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("line %d: want %q, got %q", i, tt.want[i], got[i])
				}
			}
		})
	}
}
