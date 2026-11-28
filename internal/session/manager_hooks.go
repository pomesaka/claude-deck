package session

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
)

// The functions below are called by `claude-deck hook ...`, which the plugin
// runs from inside a Claude Code session. They write the store directly; the
// TUI picks the change up through WatchStore. No Manager is needed, so a hook
// does not touch tmux.

// RecordHookStatus applies an activity reported by a hook, with the wait of a
// WaitingAction. See applyHookStatus.
func RecordHookStatus(st *store.Store, sessionID DeckSessionID, status Activity, w Wait) error {
	_, err := st.Update(string(sessionID), func(r *store.Record) error {
		applyHookStatus(r, status, w)
		markActive(r, time.Now())
		return nil
	})
	return err
}

// RecordSessionStart links the Claude Code session ID reported by SessionStart.
// See applySessionStart.
func RecordSessionStart(st *store.Store, sessionID DeckSessionID, claudeID ClaudeSessionID, source string) error {
	// The first instruction was recorded before the session had a Claude Code session ID.
	// WHY ここで埋める: セッションの行が prune で消えた後も、送った文を会話の記録と ID で照らせるようにする。
	if claudeID != "" {
		if err := st.FillSentClaudeSessionID(string(sessionID), string(claudeID)); err != nil {
			return err
		}
	}
	_, err := st.Update(string(sessionID), func(r *store.Record) error {
		applyContextOnSessionStart(r, source)
		applySessionStart(r, string(claudeID), source)
		markActive(r, time.Now())
		return nil
	})
	return err
}

// RecordContext records the context window reported by session.measure.
// See applyContextMeasured.
func RecordContext(st *store.Store, sessionID DeckSessionID, tokens *int, window int) error {
	_, err := st.Update(string(sessionID), func(r *store.Record) error {
		applyContextMeasured(r, tokens, window)
		markActive(r, time.Now())
		return nil
	})
	return err
}

// maxAliasLen is the longest alias SetAlias accepts.
const maxAliasLen = 40

// aliasPattern is what an alias may be made of: ASCII letters, digits, "-", "_" and ".".
// WHY ASCII だけ: 一覧の桁そろえと切り詰めは 1 文字 1 桁の前提で、全角文字が入ると行が崩れる。
// セッション名（shoko-677b）と同じ見た目にそろえる意味もある。
var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9._-]*$`)

// SetAlias gives the deck session key (an ID, or a name that only one session
// has) the alias, and returns the session. An empty alias removes it.
// Used by `claude-deck alias`; needs no Manager.
// WHY NOT Claude Code の /rename と同期する: /rename で宛先名はすぐ変わる（Claude Code 2.1.296 で確認）が、
// 別のプロセスから相手のセッションを改名するには、tmux でキー入力を送って入力欄の書きかけを壊すか、
// 相手のプラグインが次に動くのを待つしかない。
func SetAlias(st *store.Store, key, alias string) (Snapshot, error) {
	if !aliasPattern.MatchString(alias) {
		return Snapshot{}, fmt.Errorf("alias %q: use only ASCII letters, digits, '-', '_' and '.'", alias)
	}
	if len(alias) > maxAliasLen {
		return Snapshot{}, fmt.Errorf("alias is %d characters long; the limit is %d", len(alias), maxAliasLen)
	}
	var out store.Record
	err := st.Tx(func(tx *store.Tx) error {
		recs, err := tx.List()
		if err != nil {
			return err
		}
		r, err := findRecord(recs, key)
		if err != nil {
			return err
		}
		r.Alias = alias
		out = r
		return tx.Put(r)
	})
	if err != nil {
		return Snapshot{}, err
	}
	return newSessionFromRecord(out).Snapshot(), nil
}

// findRecord looks a row up by deck session ID, falling back to its name.
// A name that several rows have is an error rather than a guess, as in FindSession.
func findRecord(recs []store.Record, key string) (store.Record, error) {
	var matches []store.Record
	for _, r := range recs {
		if r.ID == key {
			return r, nil
		}
		if r.Name == key {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return store.Record{}, fmt.Errorf("session not found: %s", key)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, r := range matches {
			ids[i] = r.ID
		}
		return store.Record{}, fmt.Errorf("session name %q is ambiguous; use one of the IDs: %s", key, strings.Join(ids, ", "))
	}
}

// FindStored looks a deck session up in the store by ID, or by a name that
// only one session has, as FindSession does in memory. Needs no Manager.
func FindStored(st *store.Store, key string) (Snapshot, error) {
	recs, err := st.List()
	if err != nil {
		return Snapshot{}, err
	}
	r, err := findRecord(recs, key)
	if err != nil {
		return Snapshot{}, err
	}
	return newSessionFromRecord(r).Snapshot(), nil
}
