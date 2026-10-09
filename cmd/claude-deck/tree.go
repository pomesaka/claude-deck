package main

import (
	"strings"

	"github.com/pomesaka/claude-deck/internal/session"
)

// shortIDLen is how many characters of a runtime session ID the tree prints.
const shortIDLen = 8

func shortID(id session.RuntimeSessionID) string {
	if len(id) > shortIDLen {
		return string(id[:shortIDLen])
	}
	return string(id)
}

// renderTree prints the session trees as text, one repository after another:
//
//	repo
//	└─ 2054caae  anna-8cc7
//	   └─ 7c1d90e2  /clear
//	      ├─ 9f3e01bc  /clear  現在 running
//	      └─ 5b1e77aa  fork  maika-1a96  現在 completed
func renderTree(repos []session.TreeRepo) string {
	var b strings.Builder
	for _, repo := range repos {
		b.WriteString(repo.RepoName)
		b.WriteByte('\n')
		renderTreeNodes(&b, repo.Roots, "")
	}
	return b.String()
}

func renderTreeNodes(b *strings.Builder, nodes []*session.TreeNode, indent string) {
	for i, node := range nodes {
		branch, childIndent := "├─ ", indent+"│  "
		if i == len(nodes)-1 {
			branch, childIndent = "└─ ", indent+"   "
		}
		b.WriteString(indent)
		b.WriteString(branch)
		b.WriteString(strings.Join(treeNodeFields(node), "  "))
		b.WriteByte('\n')
		renderTreeNodes(b, node.Children, childIndent)
	}
}

// treeNodeFields are the columns of one line: the context's ID, how it came to
// be, the deck session's name where a new deck session starts, and the status
// on the context a resume would continue.
func treeNodeFields(node *session.TreeNode) []string {
	id := shortID(node.RuntimeID)
	if id == "" {
		id = "(ID なし)"
	}
	fields := []string{id}
	switch node.Origin {
	case session.OriginStart:
		fields = append(fields, node.Session.Name)
	case session.OriginReset:
		fields = append(fields, "/clear")
	case session.OriginFork:
		if node.MissingParent != "" {
			fields = append(fields, "fork（分岐元 "+shortID(node.MissingParent)+" は一覧に無い）", node.Session.Name)
		} else {
			fields = append(fields, "fork", node.Session.Name)
		}
	}
	if node.IsCurrent {
		fields = append(fields, "現在 "+node.Session.Status.ID())
	}
	return fields
}
