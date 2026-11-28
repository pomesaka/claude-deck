package session

import (
	"reflect"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
)

func TestRecordSent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0) // in the local zone, as the store reads it back
	tests := []struct {
		name   string
		snap   Snapshot
		source SentSource
		text   string
		want   []store.Sent
	}{
		{"a line to a session", Snapshot{ID: "abc", RuntimeSessionID: "c1"}, SentBySend, "続けて",
			[]store.Sent{{SessionID: "abc", ClaudeSessionID: "c1", SentAt: now, Source: "send", TextSHA256: TextSHA256("続けて")}}},
		{"a first instruction before the session has an ID", Snapshot{ID: "abc"}, SentByNew, "調べて",
			[]store.Sent{{SessionID: "abc", SentAt: now, Source: "new", TextSHA256: TextSHA256("調べて")}}},
		{"a slash command is not recorded", Snapshot{ID: "abc", RuntimeSessionID: "c1"}, SentByBroadcast, "/reload-plugins", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			recorded, err := RecordSent(st, tt.snap, tt.source, tt.text, now)
			if err != nil || recorded != (tt.want != nil) {
				t.Fatalf("RecordSent() = (%v, %v), want recorded %v", recorded, err, tt.want != nil)
			}
			got, err := st.ListSent(time.Time{})
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ListSent() = (%+v, %v), want %+v", got, err, tt.want)
			}
		})
	}
}

// The first instruction gets the Claude Code session ID once SessionStart reports it.
func TestRecordSessionStart_FillsSentID(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Insert(store.Record{ID: "abc", Status: "idle", Isolation: store.IsolationDirect}); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSent(st, Snapshot{ID: "abc"}, SentByNew, "調べて", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSent(st, Snapshot{ID: "other"}, SentByNew, "ほかの", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := RecordSessionStart(st, "abc", "c1", SourceStartup); err != nil {
		t.Fatal(err)
	}
	// A later ID (after /clear) does not move rows that already have one.
	if err := RecordSessionStart(st, "abc", "c2", SourceClear); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.ListSent(time.Time{})
	got := map[string]string{}
	for _, r := range rows {
		got[r.SessionID] = r.ClaudeSessionID
	}
	if got["abc"] != "c1" || got["other"] != "" {
		t.Errorf("claude session IDs = %v, want abc → c1 and other untouched", got)
	}
}
