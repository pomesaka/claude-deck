package tui

import (
	"fmt"
	"io"

	"charm.land/bubbles/v2/list"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const ellipsis = "…"

// repoDelegate is a custom delegate for the repo selector list.
// DefaultDelegate は Filtering 状態で選択ハイライトを出さないため、
// Filtering 中でも SelectedTitle スタイルを適用するカスタム Render を持つ。
type repoDelegate struct {
	list.DefaultDelegate
}

func newRepoDelegate() repoDelegate {
	d := list.NewDefaultDelegate()
	d.ShowDescription = false
	d.Styles.FilterMatch = lipgloss.NewStyle().Background(lipgloss.Color("#4A3A00")).Bold(true)
	return repoDelegate{DefaultDelegate: d}
}

// Render prints an item. Filtering 状態でも選択アイテムをハイライト表示する。
func (d repoDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	i, ok := item.(list.DefaultItem)
	if !ok {
		return
	}

	title := i.Title()
	s := &d.Styles

	if m.Width() <= 0 {
		return
	}

	textwidth := m.Width() - s.NormalTitle.GetPaddingLeft() - s.NormalTitle.GetPaddingRight()
	title = ansi.Truncate(title, textwidth, ellipsis)

	isSelected := index == m.Index()
	isFiltered := m.FilterState() == list.Filtering || m.FilterState() == list.FilterApplied

	if isSelected {
		if isFiltered {
			matchedRunes := m.MatchesForItem(index)
			unmatched := s.SelectedTitle.Inline(true)
			matched := unmatched.Inherit(s.FilterMatch)
			title = lipgloss.StyleRunes(title, matchedRunes, matched, unmatched)
		}
		title = s.SelectedTitle.Render(title)
	} else {
		if isFiltered {
			matchedRunes := m.MatchesForItem(index)
			unmatched := s.NormalTitle.Inline(true)
			matched := unmatched.Inherit(s.FilterMatch)
			title = lipgloss.StyleRunes(title, matchedRunes, matched, unmatched)
		}
		title = s.NormalTitle.Render(title)
	}

	fmt.Fprintf(w, "%s", title) //nolint: errcheck
}

