package ratelimits

import (
	"testing"
	"time"
)

// The JSON is what deckmod/hooks/register.ts passes: JSON.stringify of
// session.measure's rateLimits.
func TestParseMeasured(t *testing.T) {
	tests := []struct {
		name string
		json string
		want Status
	}{
		{
			name: "both windows",
			json: `[{"kind":"five_hour","percentUsed":23.5,"resetsAt":"2026-10-09T12:30:00.000Z"},{"kind":"seven_day","percentUsed":7,"resetsAt":"2026-10-11T14:00:00Z"}]`,
			want: Status{
				FiveHour:          Window{UsedPct: 23.5, ResetsAt: time.Date(2026, 10, 9, 12, 30, 0, 0, time.UTC)},
				FiveHourAvailable: true,
				SevenDay:          Window{UsedPct: 7, ResetsAt: time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)},
				SevenDayAvailable: true,
			},
		},
		{
			name: "one window without a reset time, an unknown kind",
			json: `[{"kind":"spend_limit","percentUsed":120},{"kind":"five_hour","percentUsed":3}]`,
			want: Status{FiveHour: Window{UsedPct: 3}, FiveHourAvailable: true},
		},
		{
			name: "no windows",
			json: `[]`,
			want: Status{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMeasured([]byte(tt.json))
			if err != nil {
				t.Fatalf("ParseMeasured: %v", err)
			}
			if got.FiveHourAvailable != tt.want.FiveHourAvailable || got.FiveHour.UsedPct != tt.want.FiveHour.UsedPct || !got.FiveHour.ResetsAt.Equal(tt.want.FiveHour.ResetsAt) {
				t.Errorf("FiveHour = %#v (available %v), want %#v (available %v)", got.FiveHour, got.FiveHourAvailable, tt.want.FiveHour, tt.want.FiveHourAvailable)
			}
			if got.SevenDayAvailable != tt.want.SevenDayAvailable || got.SevenDay.UsedPct != tt.want.SevenDay.UsedPct || !got.SevenDay.ResetsAt.Equal(tt.want.SevenDay.ResetsAt) {
				t.Errorf("SevenDay = %#v (available %v), want %#v (available %v)", got.SevenDay, got.SevenDayAvailable, tt.want.SevenDay, tt.want.SevenDayAvailable)
			}
		})
	}
}

func TestParseMeasured_RejectsInvalid(t *testing.T) {
	if _, err := ParseMeasured([]byte(`{"five_hour":1}`)); err == nil {
		t.Error("ParseMeasured of an object: want error, got nil")
	}
}

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

// Watch reports what the file holds when it starts, and then every Save: the
// TUI shows nothing until the first report, and a session reports only when its
// limits change.
func TestWatch(t *testing.T) {
	status := func(pct float64) Status {
		return Status{FiveHour: Window{UsedPct: pct, ResetsAt: time.Unix(1_800_000_000, 0)}, FiveHourAvailable: true}
	}
	tests := []struct {
		name string
		// before is saved before Watch starts; nil leaves the file missing.
		before *Status
		// saves are saved one by one after Watch started, each awaited.
		saves []Status
		// want are the FiveHour.UsedPct values reported, in order.
		want []float64
	}{
		{
			name:   "the file exists when the watch starts",
			before: new(status(10)),
			saves:  []Status{status(20), status(30), status(40)},
			want:   []float64{10, 20, 30, 40},
		},
		{
			name:  "the file appears after the watch starts",
			saves: []Status{status(20), status(30), status(40)},
			want:  []float64{20, 30, 40},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if tt.before != nil {
				if err := Save(dataDir, *tt.before); err != nil {
					t.Fatalf("Save: %v", err)
				}
			}
			reports := make(chan float64, 16)
			if err := Watch(t.Context(), dataDir, func(s Status) { reports <- s.FiveHour.UsedPct }); err != nil {
				t.Fatalf("Watch: %v", err)
			}
			// next waits for the report of want, skipping repeats of the one before.
			last := -1.0
			next := func(want float64) {
				t.Helper()
				for {
					select {
					case got := <-reports:
						if got == last {
							continue
						}
						if got != want {
							t.Fatalf("reported %v, want %v", got, want)
						}
						last = got
						return
					case <-time.After(3 * time.Second):
						t.Fatalf("no report of %v", want)
					}
				}
			}
			want := tt.want
			if tt.before != nil {
				next(want[0])
				want = want[1:]
			}
			for i, s := range tt.saves {
				if err := Save(dataDir, s); err != nil {
					t.Fatalf("Save: %v", err)
				}
				next(want[i])
			}
		})
	}
}
