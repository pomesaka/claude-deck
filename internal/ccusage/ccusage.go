// Package ccusage fetches Claude Code usage statistics from the ccusage CLI.
package ccusage

import (
	"context"
	json "encoding/json/v2"
	"os/exec"
	"time"
)

// Status holds ccusage data for the status line display.
type Status struct {
	// BlockPercent is the actual usage % of the current 5-hour billing block.
	BlockPercent float64
	// BlockTotalTokens is the total token count used in the current block.
	BlockTotalTokens int
	// BlockCostUSD is the cost of the current block in USD.
	BlockCostUSD float64
	// BlockAvailable is true if active block data was successfully retrieved.
	BlockAvailable bool
	// WeekCostUSD is the cost of the current calendar week in USD.
	WeekCostUSD float64
	// WeekAvailable is true if current-week cost data was successfully retrieved.
	WeekAvailable bool
}

type blocksResponse struct {
	Blocks []blockEntry `json:"blocks"`
}

type blockEntry struct {
	IsActive         bool              `json:"isActive"`
	TotalTokens      int               `json:"totalTokens"`
	CostUSD          float64           `json:"costUSD"`
	TokenLimitStatus *tokenLimitStatus `json:"tokenLimitStatus"`
}

type tokenLimitStatus struct {
	Limit int `json:"limit"`
}

type weeklyResponse struct {
	Weekly []weeklyEntry `json:"weekly"`
}

type weeklyEntry struct {
	Week      string  `json:"week"`
	TotalCost float64 `json:"totalCost"`
}

// Fetch runs ccusage and returns the combined status.
// Errors are silently swallowed; callers get a zero Status on failure.
func Fetch(ctx context.Context) Status {
	var s Status

	blockOut, blockErr := runCmd(ctx, "blocks", "--active", "--json", "--token-limit", "max")
	weekOut, weekErr := runCmd(ctx, "weekly", "--json")

	if blockErr == nil {
		var resp blocksResponse
		if json.Unmarshal(blockOut, &resp) == nil {
			for _, b := range resp.Blocks {
				if b.IsActive && b.TokenLimitStatus != nil && b.TokenLimitStatus.Limit > 0 {
					s.BlockPercent = float64(b.TotalTokens) / float64(b.TokenLimitStatus.Limit) * 100
					s.BlockTotalTokens = b.TotalTokens
					s.BlockCostUSD = b.CostUSD
					s.BlockAvailable = true
					break
				}
			}
		}
	}

	if weekErr == nil {
		var resp weeklyResponse
		if json.Unmarshal(weekOut, &resp) == nil && len(resp.Weekly) > 0 {
			latest := resp.Weekly[len(resp.Weekly)-1]
			// Confirm the entry is the current week (started within the last 7 days).
			weekDate, err := time.ParseInLocation("2006-01-02", latest.Week, time.Local)
			if err == nil && time.Since(weekDate) < 7*24*time.Hour {
				s.WeekCostUSD = latest.TotalCost
				s.WeekAvailable = true
			}
		}
	}

	return s
}

// runCmd executes ccusage with the given arguments.
func runCmd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ccusage", args...)
	return cmd.Output()
}
