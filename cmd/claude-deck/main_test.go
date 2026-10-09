package main

import (
	"testing"

	"github.com/pomesaka/claude-deck/internal/agentruntime"
	"github.com/pomesaka/claude-deck/internal/config"
)

func TestBuildManagerConfigRuntimeProvider(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		wantProvider agentruntime.Provider
	}{
		{name: "default claude", provider: "claude", wantProvider: agentruntime.ProviderClaude},
		{name: "codex", provider: "codex", wantProvider: agentruntime.ProviderCodex},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Runtime.Provider = tt.provider
			cfg.DataDir = t.TempDir()

			got, err := buildManagerConfig(cfg)
			if err != nil {
				t.Fatalf("buildManagerConfig: %v", err)
			}
			if got.AgentRuntime.Provider() != tt.wantProvider {
				t.Fatalf("AgentRuntime.Provider() = %q, want %q", got.AgentRuntime.Provider(), tt.wantProvider)
			}
			if got.TranscriptReader == nil {
				t.Fatal("TranscriptReader is nil")
			}
		})
	}
}
