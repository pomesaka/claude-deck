package tui

import (
	"testing"

	"github.com/pomesaka/claude-deck/internal/preview"
)

func TestPreviewModel_AttentionText(t *testing.T) {
	tests := []struct {
		name string
		spec preview.PreviewSpec
		want string
	}{
		{"approval", preview.PreviewSpec{Status: "waiting_approval"}, "Waiting for approval"},
		{"a wait for the user's action shows its reason", preview.PreviewSpec{Status: "waiting_action", StatusReason: "gh にログインしてください"}, "Waiting for your action: gh にログインしてください"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (PreviewModel{spec: tt.spec}).attentionText(); got != tt.want {
				t.Errorf("attentionText() = %q, want %q", got, tt.want)
			}
		})
	}
}
