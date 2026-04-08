package tui

// layoutMode controls pane arrangement.
type layoutMode int

const (
	layoutListLeft   layoutMode = iota // default: list left, detail right (35/65)
	layoutListRight                    // reversed: detail left, list right
	layoutListHidden                   // list hidden: detail full width
)

// pane identifies which pane has keyboard focus.
type pane int

const (
	paneList   pane = iota
	paneDetail
)

// Layout owns all pane visibility and focus state.
//
// Invariant: when mode == layoutListHidden, focus is always paneDetail.
// This invariant is enforced by CycleMode and FocusList — callers cannot
// violate it through the public API.
type Layout struct {
	mode  layoutMode
	focus pane
}

// CycleMode advances to the next layout mode: ListLeft → ListRight → ListHidden → ListLeft.
// Enforces the invariant: ListHidden forces focus to the detail pane.
func (l *Layout) CycleMode() {
	l.mode = (l.mode + 1) % 3
	if l.mode == layoutListHidden {
		l.focus = paneDetail
	}
}

// FocusList moves focus to the list pane. No-op when the list is hidden.
func (l *Layout) FocusList() {
	if l.mode != layoutListHidden {
		l.focus = paneList
	}
}

// FocusDetail moves focus to the detail pane.
func (l *Layout) FocusDetail() {
	l.focus = paneDetail
}

// ToggleFocus alternates focus between list and detail panes.
// No-op when the list is hidden (detail is always focused in that mode).
func (l *Layout) ToggleFocus() {
	if l.mode == layoutListHidden {
		return
	}
	if l.focus == paneDetail {
		l.focus = paneList
	} else {
		l.focus = paneDetail
	}
}

// IsDetailFocused reports whether the detail pane has keyboard focus.
func (l Layout) IsDetailFocused() bool { return l.focus == paneDetail }

// IsListVisible reports whether the list pane is currently shown.
func (l Layout) IsListVisible() bool { return l.mode != layoutListHidden }

// IsListRight reports whether the list pane is positioned on the right side.
func (l Layout) IsListRight() bool { return l.mode == layoutListRight }

// Indicator returns a short label for non-default layout modes, used in the footer.
// Returns empty string for the default layout (list left).
func (l Layout) Indicator() string {
	switch l.mode {
	case layoutListRight:
		return " [⇄]"
	case layoutListHidden:
		return " [⊡]"
	}
	return ""
}
