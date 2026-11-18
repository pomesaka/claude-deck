package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// repoHarness drives Model.Update the way the bubbletea loop would: it runs
// the returned Cmds and feeds their messages back. Cmds that do not return
// quickly (ticks such as clearStatusCmd, cursor blink) are dropped, so time
// never advances. The discovery is a stub that only counts launches; tests
// deliver repoListMsg themselves to control when results arrive.
type repoHarness struct {
	t        *testing.T
	m        Model
	launches int
}

type stubDiscoveryLaunched struct{}

func newRepoHarness(t *testing.T) *repoHarness {
	t.Helper()
	h := &repoHarness{t: t}
	h.m = Model{
		repoList:        newRepoList(),
		repoDiscovering: true,
	}
	h.m.discoverRepos = func() tea.Msg {
		h.launches++
		return stubDiscoveryLaunched{}
	}
	h.drain(h.m.Init())
	return h
}

func (h *repoHarness) send(msg tea.Msg) {
	h.t.Helper()
	next, cmd := h.m.Update(msg)
	m, ok := next.(Model)
	if !ok {
		h.t.Fatalf("Update returned %T, want Model", next)
	}
	h.m = m
	h.drain(cmd)
}

func (h *repoHarness) drain(cmd tea.Cmd) {
	h.t.Helper()
	if cmd == nil {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(20 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case nil, stubDiscoveryLaunched:
	case tea.BatchMsg:
		for _, c := range msg {
			h.drain(c)
		}
	default:
		h.send(msg)
	}
}

func (h *repoHarness) open()                 { h.key(tea.KeyPressMsg{Code: 'n', Text: "n"}) }
func (h *repoHarness) esc()                  { h.key(tea.KeyPressMsg{Code: tea.KeyEscape}) }
func (h *repoHarness) down()                 { h.key(tea.KeyPressMsg{Code: tea.KeyDown}) }
func (h *repoHarness) key(k tea.KeyPressMsg) { h.send(k) }

func (h *repoHarness) typeText(s string) {
	for _, r := range s {
		h.key(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func (h *repoHarness) deliver(dirs ...string) {
	repos := make([]repoItem, len(dirs))
	for i, d := range dirs {
		repos[i] = repoItem{repoPath: d, projectDir: d}
	}
	h.send(repoListMsg{repos: repos})
}

func (h *repoHarness) fail() { h.send(repoListMsg{err: errors.New("fd failed")}) }

func (h *repoHarness) shownDirs() []string {
	var dirs []string
	for _, it := range h.m.repoList.Items() {
		if ri, ok := it.(repoItem); ok {
			dirs = append(dirs, ri.projectDir)
		}
	}
	return dirs
}

func (h *repoHarness) selectedDir() string {
	if ri, ok := h.m.repoList.SelectedItem().(repoItem); ok {
		return ri.projectDir
	}
	return ""
}

func TestRepoSelect(t *testing.T) {
	type want struct {
		mode     viewMode
		shown    []string // nil = 確かめない
		filter   *string
		selected string // "" = 確かめない
		launches int    // 0 = 確かめない
		status   string // 部分一致。"" = 確かめない
	}
	str := func(s string) *string { return &s }

	tests := []struct {
		name  string
		steps func(h *repoHarness)
		want  want
	}{
		{
			name:  "起動時に探索を 1 回始める",
			steps: func(h *repoHarness) {},
			want:  want{mode: viewDashboard, launches: 1},
		},
		{
			name: "結果が届く前に開いても、空の一覧で文字を打てる",
			steps: func(h *repoHarness) {
				h.open()
				h.typeText("a")
			},
			want: want{mode: viewSelectRepo, shown: []string{}, filter: str("a"), launches: 1},
		},
		{
			name: "前回の結果をすぐ、昇順で、空のフィルタで出す",
			steps: func(h *repoHarness) {
				h.deliver("/r/c", "/r/a", "/r/b")
				h.open()
			},
			want: want{mode: viewSelectRepo, shown: []string{"/r/a", "/r/b", "/r/c"}, filter: str(""), launches: 2},
		},
		{
			name: "開き直すと前回打った文字は消える",
			steps: func(h *repoHarness) {
				h.deliver("/r/a", "/r/b")
				h.open()
				h.typeText("ab")
				h.esc()
				h.open()
			},
			want: want{mode: viewSelectRepo, filter: str("")},
		},
		{
			name: "起動時の探索中に開いても探索を重ねない",
			steps: func(h *repoHarness) {
				h.open()
				h.esc()
				h.open()
			},
			want: want{mode: viewSelectRepo, launches: 1},
		},
		{
			name: "失敗の後は次の n で探索し直す",
			steps: func(h *repoHarness) {
				h.fail()
				h.open()
			},
			want: want{mode: viewSelectRepo, launches: 2},
		},
		{
			name: "選択画面で n を打つとフィルタの文字になる",
			steps: func(h *repoHarness) {
				h.open()
				h.typeText("n")
			},
			want: want{mode: viewSelectRepo, filter: str("n"), launches: 1},
		},
		{
			name: "探索中に打った文字は結果が届いても消えない",
			steps: func(h *repoHarness) {
				h.deliver("/r/ab", "/r/cd")
				h.open()
				h.typeText("ab")
				h.deliver("/r/ab", "/r/abc", "/r/cd")
			},
			want: want{mode: viewSelectRepo, shown: []string{"/r/ab", "/r/abc", "/r/cd"}, filter: str("ab")},
		},
		{
			name: "選んでいた候補は位置が変わっても選んだまま",
			steps: func(h *repoHarness) {
				h.deliver("/r/a", "/r/b", "/r/c")
				h.open()
				h.down()
				h.deliver("/r/0", "/r/a", "/r/b", "/r/c")
			},
			want: want{mode: viewSelectRepo, selected: "/r/b"},
		},
		{
			name: "選んでいた候補が消えたら先頭を選ぶ",
			steps: func(h *repoHarness) {
				h.deliver("/r/a", "/r/b")
				h.open()
				h.down()
				h.deliver("/r/a")
			},
			want: want{mode: viewSelectRepo, selected: "/r/a"},
		},
		{
			name: "開いているときに失敗し、候補があれば画面に留まる",
			steps: func(h *repoHarness) {
				h.deliver("/r/a", "/r/b")
				h.open()
				h.fail()
			},
			want: want{mode: viewSelectRepo, shown: []string{"/r/a", "/r/b"}, status: "リポジトリ検索エラー"},
		},
		{
			name: "開いているときに失敗し、候補が無ければダッシュボードに戻る",
			steps: func(h *repoHarness) {
				h.open()
				h.fail()
			},
			want: want{mode: viewDashboard, status: "リポジトリ検索エラー"},
		},
		{
			name: "閉じた後に届いた結果は、画面を変えず次に開いたときに出る",
			steps: func(h *repoHarness) {
				h.open()
				h.esc()
				h.deliver("/r/a", "/r/b")
			},
			want: want{mode: viewDashboard},
		},
		{
			name: "閉じた後に届いた結果を、次に開くと出す",
			steps: func(h *repoHarness) {
				h.open()
				h.esc()
				h.deliver("/r/a", "/r/b")
				h.open()
			},
			want: want{mode: viewSelectRepo, shown: []string{"/r/a", "/r/b"}, filter: str("")},
		},
		{
			name: "閉じた後の失敗は画面を変えず、前回の結果を残す",
			steps: func(h *repoHarness) {
				h.deliver("/r/a", "/r/b")
				h.open()
				h.esc()
				h.fail()
				h.open()
			},
			want: want{mode: viewSelectRepo, shown: []string{"/r/a", "/r/b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRepoHarness(t)
			tt.steps(h)

			if h.m.mode != tt.want.mode {
				t.Errorf("mode = %v, want %v", h.m.mode, tt.want.mode)
			}
			if tt.want.shown != nil {
				if got := h.shownDirs(); !slices.Equal(got, tt.want.shown) && (len(got) != 0 || len(tt.want.shown) != 0) {
					t.Errorf("shown = %v, want %v", got, tt.want.shown)
				}
			}
			if tt.want.filter != nil {
				if got := h.m.repoList.FilterValue(); got != *tt.want.filter {
					t.Errorf("filter = %q, want %q", got, *tt.want.filter)
				}
			}
			if tt.want.selected != "" {
				if got := h.selectedDir(); got != tt.want.selected {
					t.Errorf("selected = %q, want %q", got, tt.want.selected)
				}
			}
			if tt.want.launches != 0 && h.launches != tt.want.launches {
				t.Errorf("launches = %d, want %d", h.launches, tt.want.launches)
			}
			if tt.want.status != "" && !strings.Contains(h.m.statusMsg, tt.want.status) {
				t.Errorf("statusMsg = %q, want it to contain %q", h.m.statusMsg, tt.want.status)
			}
		})
	}
}
