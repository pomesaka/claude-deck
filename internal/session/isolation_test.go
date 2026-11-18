package session

import (
	"math/rand/v2"
	"testing"

	"github.com/pomesaka/claude-deck/internal/store"
)

func TestIsolation(t *testing.T) {
	type want struct {
		workDir                   string
		isWorkspace, hasWorkspace bool
		atRev, parentRev          string
	}
	tests := []struct {
		name string
		iso  Isolation
		want want
	}{
		{"direct", DirectIsolation("/repo/sub"), want{workDir: "/repo/sub"}},
		{"workspace root", WorkspaceIsolation("/data/ws"), want{workDir: "/data/ws", isWorkspace: true, hasWorkspace: true}},
		{"workspace subdir", WorkspaceIsolation("/data/ws/packages/api"), want{workDir: "/data/ws/packages/api", isWorkspace: true, hasWorkspace: true}},
		{"removed", WorkspaceIsolation("/data/ws").Removed("a", "b"), want{isWorkspace: true, atRev: "a", parentRev: "b"}},
		{"removed without revisions", WorkspaceIsolation("/data/ws").Removed("", ""), want{isWorkspace: true}},
		{"a direct session has nothing to remove", DirectIsolation("/repo").Removed("a", "b"), want{workDir: "/repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, parent := tt.iso.Revisions()
			got := want{tt.iso.WorkDir(), tt.iso.IsWorkspace(), tt.iso.HasWorkspace(), at, parent}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Writing an isolation into a row and reading it back gives the same value,
// and a present workspace does not leave older revisions behind.
func TestIsolationRecordRoundTrip(t *testing.T) {
	check := func(t *testing.T, iso Isolation) {
		t.Helper()
		r := store.Record{Name: "ws", LastJJRevision: "at-old", LastJJParentRevision: "parent-old"}
		isolationToRecord(&r, iso)
		if got := isolationFromRecord(r); got != iso {
			t.Errorf("round trip of %+v gave %+v (row %+v)", iso, got, r)
		}
		if iso.HasWorkspace() && (r.LastJJRevision != "" || r.LastJJParentRevision != "") {
			t.Errorf("a present workspace left revisions %q %q", r.LastJJRevision, r.LastJJParentRevision)
		}
		if (r.WorkspaceName != "") != iso.HasWorkspace() {
			t.Errorf("workspace_name = %q for %+v; older binaries read it as whether the workspace exists", r.WorkspaceName, iso)
		}
	}
	for _, iso := range []Isolation{
		DirectIsolation("/repo/sub"),
		WorkspaceIsolation("/data/ws"),
		WorkspaceIsolation("/data/ws/packages/api"),
		WorkspaceIsolation("/data/ws").Removed("a", "b"),
		WorkspaceIsolation("/data/ws").Removed("", ""),
	} {
		check(t, iso)
	}
	rng := rand.New(rand.NewPCG(3, 4))
	pick := func(xs ...string) string { return xs[rng.IntN(len(xs))] }
	for range 2000 {
		dir := pick("/repo", "/data/ws", "/data/ws/sub")
		var iso Isolation
		switch rng.IntN(3) {
		case 0:
			iso = DirectIsolation(dir)
		case 1:
			iso = WorkspaceIsolation(dir)
		default:
			iso = WorkspaceIsolation(dir).Removed(pick("", "a"), pick("", "b"))
		}
		check(t, iso)
	}
}

// A row an older claude-deck closed (isolation set, workspace columns empty)
// reads as a removed workspace with its revisions.
func TestIsolationFromRecord_ClosedByOlderBinary(t *testing.T) {
	r := store.Record{Name: "ws", Isolation: store.IsolationWorkspace, LastJJRevision: "a", LastJJParentRevision: "b"}
	want := WorkspaceIsolation("/x").Removed("a", "b")
	if got := isolationFromRecord(r); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
