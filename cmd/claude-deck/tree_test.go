package main

import (
	"testing"

	"github.com/pomesaka/claude-deck/internal/session"
)

func TestRenderTree(t *testing.T) {
	// snap builds a deck session whose chain is ids (oldest first).
	snap := func(id, name, repo string, status session.Status, forkedFrom string, ids ...string) session.Snapshot {
		s := session.Snapshot{
			ID: session.DeckSessionID(id), Name: name, RepoPath: "/repos/" + repo, RepoName: repo,
			Status: status, ForkedFrom: session.RuntimeSessionID(forkedFrom),
		}
		for i, rid := range ids {
			if i == len(ids)-1 {
				s.RuntimeSessionID = session.RuntimeSessionID(rid)
			} else {
				s.PriorRuntimeIDs = append(s.PriorRuntimeIDs, session.RuntimeSessionID(rid))
			}
		}
		return s
	}

	tests := []struct {
		name  string
		snaps []session.Snapshot
		want  string
	}{
		{
			name:  "no sessions",
			snaps: nil,
			want:  "",
		},
		{
			name: "one context",
			snaps: []session.Snapshot{
				snap("a", "anna-8cc7", "deck", session.StatusIdle, "", "11111111-aaaa"),
			},
			want: "deck\n" +
				"└─ 11111111  anna-8cc7  現在 idle\n",
		},
		{
			name: "cleared twice",
			snaps: []session.Snapshot{
				snap("a", "anna-8cc7", "deck", session.StatusRunning, "", "11111111-aaaa", "22222222-aaaa", "33333333-aaaa"),
			},
			want: "deck\n" +
				"└─ 11111111  anna-8cc7\n" +
				"   └─ 22222222  /clear\n" +
				"      └─ 33333333  /clear  現在 running\n",
		},
		{
			name: "fork from a context that was cleared afterwards",
			snaps: []session.Snapshot{
				snap("a", "anna-8cc7", "deck", session.StatusRunning, "", "11111111-aaaa", "22222222-aaaa", "33333333-aaaa"),
				snap("b", "maika-1a96", "deck", session.StatusCompleted, "22222222-aaaa", "44444444-bbbb", "55555555-bbbb"),
				snap("c", "risa-e68d", "deck", session.StatusIdle, "22222222-aaaa", "66666666-cccc"),
			},
			want: "deck\n" +
				"└─ 11111111  anna-8cc7\n" +
				"   └─ 22222222  /clear\n" +
				"      ├─ 33333333  /clear  現在 running\n" +
				"      ├─ 44444444  fork  maika-1a96\n" +
				"      │  └─ 55555555  /clear  現在 completed\n" +
				"      └─ 66666666  fork  risa-e68d  現在 idle\n",
		},
		{
			name: "fork of a fork, listed before its parent",
			snaps: []session.Snapshot{
				snap("c", "risa-e68d", "deck", session.StatusIdle, "44444444-bbbb", "66666666-cccc"),
				snap("b", "maika-1a96", "deck", session.StatusCompleted, "11111111-aaaa", "44444444-bbbb"),
				snap("a", "anna-8cc7", "deck", session.StatusCompleted, "", "11111111-aaaa"),
			},
			want: "deck\n" +
				"└─ 11111111  anna-8cc7  現在 completed\n" +
				"   └─ 44444444  fork  maika-1a96  現在 completed\n" +
				"      └─ 66666666  fork  risa-e68d  現在 idle\n",
		},
		{
			name: "fork whose source is gone",
			snaps: []session.Snapshot{
				snap("b", "maika-1a96", "deck", session.StatusIdle, "99999999-zzzz", "44444444-bbbb"),
			},
			want: "deck\n" +
				"└─ 44444444  fork（分岐元 99999999 は一覧に無い）  maika-1a96  現在 idle\n",
		},
		{
			name: "session whose ID has not arrived, and a second repository",
			snaps: []session.Snapshot{
				snap("a", "anna-8cc7", "deck", session.StatusIdle, ""),
				snap("m", "hana-08c0", "meguru", session.StatusIdle, "", "77777777-mmmm"),
				snap("d", "iori-b4bd", "deck", session.StatusCompleted, "", "88888888-dddd"),
			},
			want: "deck\n" +
				"├─ (ID なし)  anna-8cc7  現在 idle\n" +
				"└─ 88888888  iori-b4bd  現在 completed\n" +
				"meguru\n" +
				"└─ 77777777  hana-08c0  現在 idle\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderTree(session.BuildTree(tt.snaps)); got != tt.want {
				t.Errorf("renderTree() =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestInfoFromSnapshot_Lineage(t *testing.T) {
	info := infoFromSnapshot(session.Snapshot{
		ID:               "b",
		RuntimeSessionID: "55555555-bbbb",
		PriorRuntimeIDs:  []session.RuntimeSessionID{"44444444-bbbb"},
		ForkedFrom:       "22222222-aaaa",
	})
	if len(info.SessionChain) != 2 || info.SessionChain[0] != "44444444-bbbb" || info.SessionChain[1] != "55555555-bbbb" {
		t.Errorf("SessionChain = %v, want [44444444-bbbb 55555555-bbbb]", info.SessionChain)
	}
	if info.ForkedFrom != "22222222-aaaa" {
		t.Errorf("ForkedFrom = %q, want 22222222-aaaa", info.ForkedFrom)
	}
}
