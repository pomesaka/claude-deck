package session

// TreeOrigin is how a runtime session (one conversation context) came to be.
type TreeOrigin int

const (
	// OriginStart is the first context of a deck session that is not a fork.
	OriginStart TreeOrigin = iota
	// OriginReset follows the previous context of the same deck session.
	// SessionChain は /clear と、ID が変わった compact を区別して持たないので、ここでも区別しない。
	OriginReset
	// OriginFork is the first context of a deck session forked from another context.
	OriginFork
)

// TreeNode is one runtime session in the session tree.
type TreeNode struct {
	// RuntimeID is empty for a deck session whose runtime has not reported an ID yet.
	RuntimeID RuntimeSessionID
	Origin    TreeOrigin
	// Session is the deck session the context belongs to.
	Session Snapshot
	// IsCurrent marks the newest context of its deck session: the one a resume continues.
	IsCurrent bool
	// MissingParent is the context a fork came from when no session in the list
	// has it any more (its deck session was pruned). The node is then a root.
	MissingParent RuntimeSessionID
	// Children are the next context of the same deck session, if any, followed
	// by the forks made from this context.
	Children []*TreeNode
}

// TreeRepo holds the session trees of one repository.
type TreeRepo struct {
	RepoPath string
	RepoName string
	Roots    []*TreeNode
}

// Chain returns the runtime session IDs of the session, oldest first. The last
// one is the current ID.
func (s Snapshot) Chain() []RuntimeSessionID {
	if s.RuntimeSessionID == "" {
		return nil
	}
	chain := make([]RuntimeSessionID, 0, len(s.PriorRuntimeIDs)+1)
	chain = append(chain, s.PriorRuntimeIDs...)
	return append(chain, s.RuntimeSessionID)
}

// BuildTree arranges the runtime sessions of snaps into trees, grouped by
// repository. A context's parent is the previous context of its deck session,
// or for the first context of a fork, the context it was forked from.
// Repositories and siblings keep the order of snaps.
func BuildTree(snaps []Snapshot) []TreeRepo {
	byID := make(map[RuntimeSessionID]*TreeNode)
	heads := make([]*TreeNode, len(snaps))
	for i, snap := range snaps {
		chain := snap.Chain()
		if len(chain) == 0 {
			chain = []RuntimeSessionID{""}
		}
		var prev *TreeNode
		for j, id := range chain {
			node := &TreeNode{RuntimeID: id, Origin: OriginReset, Session: snap, IsCurrent: j == len(chain)-1}
			if prev == nil {
				node.Origin = OriginStart
				heads[i] = node
			} else {
				prev.Children = append(prev.Children, node)
			}
			if _, taken := byID[id]; id != "" && !taken {
				byID[id] = node
			}
			prev = node
		}
	}

	var repos []TreeRepo
	repoIndex := make(map[string]int)
	for i, snap := range snaps {
		head := heads[i]
		if snap.ForkedFrom != "" {
			head.Origin = OriginFork
			// 自分の SessionChain を親にすると木が閉じる。そういう行は根として出す。
			if parent, ok := byID[snap.ForkedFrom]; ok && parent.Session.ID != snap.ID {
				parent.Children = append(parent.Children, head)
				continue
			}
			head.MissingParent = snap.ForkedFrom
		}
		idx, ok := repoIndex[snap.RepoPath]
		if !ok {
			idx = len(repos)
			repoIndex[snap.RepoPath] = idx
			repos = append(repos, TreeRepo{RepoPath: snap.RepoPath, RepoName: snap.RepoName})
		}
		repos[idx].Roots = append(repos[idx].Roots, head)
	}
	return repos
}
