package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	testScript = "/data/statusline.sh"

	settingsMiddle = `{
  "model": "opus",
  "statusLine": {
    "type": "command",
    "command": "/data/statusline.sh"
  },
  "zeta": [1, 2],
  "alpha": true
}
`
	settingsLast = `{
  "model": "opus",
  "statusLine": { "type": "command", "command": "/data/statusline.sh" }
}
`
	settingsOnly  = `{"statusLine":{"type":"command","command":"/data/statusline.sh"}}`
	settingsOther = `{
  "statusLine": { "type": "command", "command": "/home/me/own.sh" }
}
`
	settingsNone = `{
  "model": "opus"
}
`
)

func TestUnhookStatusLine(t *testing.T) {
	tests := []struct {
		name        string
		settings    string
		prevCmd     string
		want        string
		wantChanged bool
	}{
		{
			name:     "chained command is put back, order and layout kept",
			settings: settingsMiddle,
			prevCmd:  "/home/me/fancy.sh",
			want: `{
  "model": "opus",
  "statusLine": {
    "type": "command",
    "command": "/home/me/fancy.sh"
  },
  "zeta": [1, 2],
  "alpha": true
}
`,
			wantChanged: true,
		},
		{
			name:     "no chained command: a member in the middle is removed",
			settings: settingsMiddle,
			want: `{
  "model": "opus",
  "zeta": [1, 2],
  "alpha": true
}
`,
			wantChanged: true,
		},
		{
			name:     "no chained command: the last member is removed",
			settings: settingsLast,
			want: `{
  "model": "opus"
}
`,
			wantChanged: true,
		},
		{
			name:        "no chained command: the only member is removed",
			settings:    settingsOnly,
			want:        `{}`,
			wantChanged: true,
		},
		{
			name:     "another status line is left alone",
			settings: settingsOther,
			prevCmd:  "/home/me/fancy.sh",
			want: `{
  "statusLine": { "type": "command", "command": "/home/me/own.sh" }
}
`,
		},
		{
			name:     "no status line",
			settings: settingsNone,
			want: `{
  "model": "opus"
}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := unhookStatusLine([]byte(tt.settings), testScript, tt.prevCmd)
			if err != nil {
				t.Fatalf("unhookStatusLine: %v", err)
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if string(got) != tt.want {
				t.Errorf("settings =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// The script is what earlier versions of claude-deck wrote; the settings file
// is reached through a symlink, as with a dotfiles repository.
func TestRestoreStatusLine(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dataDir, "statusline.sh")
	script := `#!/bin/sh
# claude-deck statusline wrapper — managed by claude-deck, do not edit
input=$(cat)
printf '%s' "$input" | jq -c '{rate_limits: .rate_limits}' \
  > '/data/rate-limits.json.tmp' 2>/dev/null \
  && mv '/data/rate-limits.json.tmp' '/data/rate-limits.json' 2>/dev/null
printf '%s' "$input" | '/home/me/fancy.sh'
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	realSettings := filepath.Join(dir, "dotfiles-settings.json")
	settings := `{"statusLine":{"type":"command","command":"` + scriptPath + `"},"model":"opus"}`
	if err := os.WriteFile(realSettings, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	settPath := filepath.Join(dir, "settings.json")
	if err := os.Symlink(realSettings, settPath); err != nil {
		t.Fatal(err)
	}

	if err := restoreStatusLine(settPath, dataDir); err != nil {
		t.Fatalf("restoreStatusLine: %v", err)
	}

	got, err := os.ReadFile(realSettings)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"statusLine":{"type":"command","command":"/home/me/fancy.sh"},"model":"opus"}`; string(got) != want {
		t.Errorf("settings = %s, want %s", got, want)
	}
	if info, err := os.Lstat(settPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("settings.json is no longer a symlink (err=%v)", err)
	}
	if _, err := os.Stat(scriptPath); !os.IsNotExist(err) {
		t.Errorf("script still exists (err=%v)", err)
	}

	// With the script gone there is nothing left to undo.
	if err := restoreStatusLine(settPath, dataDir); err != nil {
		t.Errorf("second restoreStatusLine: %v", err)
	}
}
