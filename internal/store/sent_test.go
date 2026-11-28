package store

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStore_Sent(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	createOldDatabase(t, path) // a database from before the table existed
	st := openTestStore(t, path)
	t0 := time.Unix(1_800_000_000, 0)
	rows := []Sent{
		{SessionID: "a", SentAt: t0, Source: "new", TextSHA256: "h1"},
		{SessionID: "a", ClaudeSessionID: "c1", SentAt: t0.Add(time.Hour), Source: "send", TextSHA256: "h2"},
		{SessionID: "b", ClaudeSessionID: "c2", SentAt: t0.Add(2 * time.Hour), Source: "broadcast", TextSHA256: "h3"},
	}
	for _, r := range rows {
		if err := st.InsertSent(r); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		since time.Time
		want  []Sent
	}{
		{"every row", time.Time{}, rows},
		{"since a row's own time", t0.Add(time.Hour), rows[1:]},
		{"after the last", t0.Add(3 * time.Hour), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := st.ListSent(tt.since)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ListSent() = (%+v, %v), want %+v", got, err, tt.want)
			}
		})
	}
}
