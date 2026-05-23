package ratelimits

import (
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	want := Status{
		FiveHour: Window{
			UsedPct:  16,
			ResetsAt: time.Unix(1779547690, 0),
		},
		FiveHourAvailable: true,
		SevenDay: Window{
			UsedPct:  17,
			ResetsAt: time.Unix(1780114955, 0),
		},
		SevenDayAvailable: true,
	}

	if err := Save(dataDir, want); err != nil {
		t.Fatal(err)
	}

	got := Load(dataDir)
	if !got.FiveHourAvailable || got.FiveHour.UsedPct != want.FiveHour.UsedPct || !got.FiveHour.ResetsAt.Equal(want.FiveHour.ResetsAt) {
		t.Fatalf("FiveHour = %#v, want %#v", got.FiveHour, want.FiveHour)
	}
	if !got.SevenDayAvailable || got.SevenDay.UsedPct != want.SevenDay.UsedPct || !got.SevenDay.ResetsAt.Equal(want.SevenDay.ResetsAt) {
		t.Fatalf("SevenDay = %#v, want %#v", got.SevenDay, want.SevenDay)
	}
}
