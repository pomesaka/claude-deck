package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/pomesaka/claude-deck/internal/session"
)

func TestRenderSessionItem(t *testing.T) {
	started := time.Date(2026, 10, 9, 15, 19, 0, 0, time.Local)
	updated := time.Date(2026, 10, 10, 10, 13, 0, 0, time.Local)
	tests := []struct {
		name  string
		snap  session.Snapshot
		width int
		want  []string
	}{
		{
			name: "no alias",
			snap: session.Snapshot{
				Name: "shoko-677b", RepoPath: "/src/pomesaka/claude-deck", RepoName: "claude-deck",
				Status: session.StatusIdle, BookmarkName: "main", StartedAt: started, LastActivity: updated, Elapsed: 18*time.Hour + 54*time.Minute,
			},
			width: 60,
			want: []string{
				"● /src/pomesaka/claude-deck@main                shoko-677b",
				"  18h54m 10/10 10:13",
			},
		},
		{
			name: "alias on the second line",
			snap: session.Snapshot{
				Name: "shoko-677b", Alias: "review-pr-12", RepoPath: "/src/pomesaka/claude-deck", RepoName: "claude-deck",
				Status: session.StatusRunning, BookmarkName: "feat/alias", StartedAt: started, LastActivity: updated, Elapsed: 18*time.Hour + 54*time.Minute,
			},
			width: 60,
			want: []string{
				"● /src/pomesaka/claude-deck@feat/alias          shoko-677b",
				"  18h54m 10/10 10:13                          review-pr-12",
			},
		},
		{
			name: "long path is cut on the left, sub project kept",
			snap: session.Snapshot{
				Name: "iori-9e7d", RepoPath: "/src/github.com/pomesaka/XmoQ", RepoName: "XmoQ", SubProjectDir: "apps/web",
				Status: session.StatusIdle, BookmarkName: "main", StartedAt: started, LastActivity: updated, Elapsed: 18*time.Hour + 54*time.Minute,
			},
			width: 50,
			want: []string{
				"● …hub.com/pomesaka/XmoQ/apps/web@main iori-9e7d",
				"  18h54m 10/10 10:13",
			},
		},
		{
			name: "error reason on the second line",
			snap: session.Snapshot{
				Name: "kiara-7655", RepoPath: "/src/pomesaka/claude-deck", RepoName: "claude-deck",
				Status: session.StatusError, ErrorMessage: "ディレクトリが見つかりません", StartedAt: started, LastActivity: updated, Elapsed: 18*time.Hour + 54*time.Minute,
			},
			width: 70,
			want: []string{
				"● /src/pomesaka/claude-deck                               kiara-7655",
				"  18h54m 10/10 10:13 ディレクトリが見つかりません",
			},
		},
		{
			name: "wide characters are cut by display width, never wrapped",
			snap: session.Snapshot{
				Name: "emiri-78fb", RepoPath: "/src/pomesaka/claude-deck", RepoName: "claude-deck",
				Status: session.StatusError, ErrorMessage: "ディレクトリが見つかりません", StartedAt: started, LastActivity: updated, Elapsed: 18*time.Hour + 54*time.Minute,
			},
			width: 36,
			want: []string{
				"● …pomesaka/claude-deck emiri-78fb",
				"  18h54m 10/10 10:13 ディレクトリ…",
			},
		},
		{
			name: "narrow: the session name is kept, the path gives way",
			snap: session.Snapshot{
				Name: "anna-4aa4", Alias: "fix-moq-handshake", RepoPath: "/src/github.com/pomesaka/XmoQ", RepoName: "XmoQ", SubProjectDir: "crates/transport",
				Status: session.StatusRunning, BookmarkName: "fix/handshake", StartedAt: started, LastActivity: updated, Elapsed: 18*time.Hour + 54*time.Minute,
			},
			width: 52,
			want: []string{
				"● …a/XmoQ/crates/transport@fix/handshake anna-4aa4",
				"  18h54m 10/10 10:13             fix-moq-handshake",
			},
		},
		{
			name: "long bookmark is kept, the path gives way",
			snap: session.Snapshot{
				Name: "hitomi-fdca", RepoPath: "/src/github.com/Accel-Hack/noah", RepoName: "noah",
				Status: session.StatusIdle, BookmarkName: "feat/adachi-issue-1025-invoice-pdf", StartedAt: started, LastActivity: updated, Elapsed: 18*time.Hour + 54*time.Minute,
			},
			width: 64,
			want: []string{
				"● …el-Hack/noah@feat/adachi-issue-1025-invoice-pdf hitomi-fdca",
				"  18h54m 10/10 10:13",
			},
		},
		{
			name: "the bookmark is cut only when the path is at its minimum",
			snap: session.Snapshot{
				Name: "hitomi-fdca", RepoPath: "/src/github.com/Accel-Hack/noah", RepoName: "noah",
				Status: session.StatusIdle, BookmarkName: "feat/adachi-issue-1025-invoice-pdf", StartedAt: started, LastActivity: updated, Elapsed: 18*time.Hour + 54*time.Minute,
			},
			width: 50,
			want: []string{
				"● …Hack/noah@feat/adachi-issue-1025… hitomi-fdca",
				"  18h54m 10/10 10:13",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, line := range strings.Split(ansi.Strip(renderSessionItem(tt.snap, false, tt.width)), "\n") {
				got = append(got, strings.TrimRight(strings.TrimPrefix(line, " "), " "))
			}
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("rendered:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

func TestFormatUptime(t *testing.T) {
	started := time.Date(2026, 10, 9, 15, 19, 0, 0, time.Local)
	tests := []struct {
		name string
		snap session.Snapshot
		want string
	}{
		{"minutes", session.Snapshot{StartedAt: started, Elapsed: 15 * time.Minute}, "15m"},
		{"hours and minutes", session.Snapshot{StartedAt: started, Elapsed: 18*time.Hour + 54*time.Minute}, "18h54m"},
		{"a day or more", session.Snapshot{StartedAt: started, Elapsed: 50 * time.Hour}, "2d2h"},
		{"just started", session.Snapshot{StartedAt: started}, "0m"},
		{"start unknown", session.Snapshot{Elapsed: time.Hour}, "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatUptime(tt.snap); got != tt.want {
				t.Errorf("formatUptime = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatLastActivity(t *testing.T) {
	at := func(day, hour, min int) time.Time { return time.Date(2026, 10, day, hour, min, 0, 0, time.Local) }
	finished := at(10, 11, 0)
	tests := []struct {
		name string
		snap session.Snapshot
		want string
	}{
		{"last activity", session.Snapshot{StartedAt: at(9, 15, 19), LastActivity: at(10, 10, 13), FinishedAt: &finished}, "10/10 10:13"},
		{"finished without activity", session.Snapshot{StartedAt: at(10, 9, 2), FinishedAt: &finished}, "10/10 11:00"},
		{"only started", session.Snapshot{StartedAt: at(10, 9, 2)}, "10/10 09:02"},
		{"nothing known", session.Snapshot{}, "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatLastActivity(tt.snap); got != tt.want {
				t.Errorf("formatLastActivity = %q, want %q", got, tt.want)
			}
		})
	}
}
