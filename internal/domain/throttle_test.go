package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPipelineThrottleUnmarshalNewCostGuidesTakePrecedence(t *testing.T) {
	var throttle PipelineThrottle
	if err := json.Unmarshal([]byte(`{"daily_cost_guide":0,"monthly_cost_guide":12.5,"daily_budget":50,"monthly_budget":75}`), &throttle); err != nil {
		t.Fatal(err)
	}
	if throttle.DailyCostGuide != 0 || throttle.MonthlyCostGuide != 12.5 {
		t.Fatalf("new cost-guide fields must win, including explicit zero: %+v", throttle)
	}
}

func TestPipelineThrottle_OffPeakOpenAt(t *testing.T) {
	// All tests use a single reference date. The clock-only comparison makes day/month/year irrelevant.
	ref := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		throttle  PipelineThrottle
		hour, min int
		want      bool
	}{
		// Normal window 01:00-07:00
		{name: "normal open at start (01:00)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"}, hour: 1, min: 0, want: true},
		{name: "normal open mid-window (03:00)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"}, hour: 3, min: 0, want: true},
		{name: "normal closed at end (07:00 excl)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"}, hour: 7, min: 0, want: false},
		{name: "normal closed before start (00:59)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"}, hour: 0, min: 59, want: false},
		{name: "normal closed midday (12:00)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"}, hour: 12, min: 0, want: false},

		// Wrap window 22:00-06:00 (past midnight)
		{name: "wrap open after start (23:30)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "22:00", OffPeakEnd: "06:00"}, hour: 23, min: 30, want: true},
		{name: "wrap open past midnight (00:30)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "22:00", OffPeakEnd: "06:00"}, hour: 0, min: 30, want: true},
		{name: "wrap open at start (22:00)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "22:00", OffPeakEnd: "06:00"}, hour: 22, min: 0, want: true},
		{name: "wrap closed at end (06:00 excl)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "22:00", OffPeakEnd: "06:00"}, hour: 6, min: 0, want: false},
		{name: "wrap closed midday (12:00)", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "22:00", OffPeakEnd: "06:00"}, hour: 12, min: 0, want: false},

		// Disabled always returns true regardless of clock strings
		{name: "disabled returns true", throttle: PipelineThrottle{OffPeakEnabled: false, OffPeakStart: "01:00", OffPeakEnd: "07:00"}, hour: 12, min: 0, want: true},

		// Malformed clocks with OffPeakEnabled return true (fail-open)
		{name: "malformed start fail-open", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "not-a-clock", OffPeakEnd: "07:00"}, hour: 12, min: 0, want: true},
		{name: "malformed end fail-open", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "not-a-clock"}, hour: 12, min: 0, want: true},
		{name: "malformed both fail-open", throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "bad", OffPeakEnd: "bad"}, hour: 12, min: 0, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(ref.Year(), ref.Month(), ref.Day(), tt.hour, tt.min, 0, 0, ref.Location())
			got := tt.throttle.OffPeakOpenAt(now)
			if got != tt.want {
				t.Errorf("OffPeakOpenAt(%s) = %v, want %v", now.Format("15:04"), got, tt.want)
			}
		})
	}
}

func TestPipelineThrottle_MaxAssetBytesAt(t *testing.T) {
	ref := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)  // midday — off-peak window is closed
	night := time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC) // 03:00 — inside 01:00-07:00 window

	tests := []struct {
		name     string
		throttle PipelineThrottle
		now      time.Time
		want     int64
	}{
		{
			name:     "DeferAboveBytes=0 returns 0",
			throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00", DeferAboveBytes: 0},
			now:      ref,
			want:     0,
		},
		{
			name:     "window open returns 0 regardless of DeferAboveBytes",
			throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00", DeferAboveBytes: 500_000_000},
			now:      night,
			want:     0,
		},
		{
			name:     "deferred when window closed and configured",
			throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00", DeferAboveBytes: 500_000_000},
			now:      ref,
			want:     500_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.throttle.MaxAssetBytesAt(tt.now)
			if got != tt.want {
				t.Errorf("MaxAssetBytesAt(%s) = %d, want %d", tt.now.Format("15:04"), got, tt.want)
			}
		})
	}
}

func TestPipelineThrottle_ReadRateFor(t *testing.T) {
	tests := []struct {
		name      string
		throttle  PipelineThrottle
		sizeBytes int64
		want      float64
	}{
		{
			name:      "ReadRate=0 returns 0",
			throttle:  PipelineThrottle{ReadRate: 0},
			sizeBytes: 1_000_000_000,
			want:      0,
		},
		{
			name:      "large file returns ReadRate",
			throttle:  PipelineThrottle{ReadRate: 2.0, ImmediateMaxBytes: 100_000_000},
			sizeBytes: 1_000_000_000,
			want:      2.0,
		},
		{
			name:      "small file at or below ImmediateMaxBytes returns 0",
			throttle:  PipelineThrottle{ReadRate: 2.0, ImmediateMaxBytes: 100_000_000},
			sizeBytes: 100_000_000,
			want:      0,
		},
		{
			name:      "one byte above ImmediateMaxBytes returns ReadRate",
			throttle:  PipelineThrottle{ReadRate: 2.0, ImmediateMaxBytes: 100_000_000},
			sizeBytes: 100_000_001,
			want:      2.0,
		},
		{
			name:      "sizeBytes=0 (unknown) returns ReadRate, no exemption",
			throttle:  PipelineThrottle{ReadRate: 2.0, ImmediateMaxBytes: 100_000_000},
			sizeBytes: 0,
			want:      2.0,
		},
		{
			name:      "sizeBytes negative returns ReadRate, no exemption",
			throttle:  PipelineThrottle{ReadRate: 2.0, ImmediateMaxBytes: 100_000_000},
			sizeBytes: -1,
			want:      2.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.throttle.ReadRateFor(tt.sizeBytes)
			if got != tt.want {
				t.Errorf("ReadRateFor(%d) = %.2f, want %.2f", tt.sizeBytes, got, tt.want)
			}
		})
	}
}

func TestPipelineThrottle_CooldownAt(t *testing.T) {
	midday := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) // outside window -> cooldown applies
	night := time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC)   // inside 01:00-07:00 window -> skip

	tests := []struct {
		name     string
		throttle PipelineThrottle
		now      time.Time
		want     time.Duration
	}{
		{
			name:     "CooldownSeconds=0 returns 0",
			throttle: PipelineThrottle{CooldownSeconds: 0},
			now:      midday,
			want:     0,
		},
		{
			name:     "outside window returns full duration",
			throttle: PipelineThrottle{CooldownSeconds: 10, OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"},
			now:      midday,
			want:     10 * time.Second,
		},
		{
			name:     "inside enabled window returns 0",
			throttle: PipelineThrottle{CooldownSeconds: 10, OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"},
			now:      night,
			want:     0,
		},
		{
			name:     "OffPeakEnabled=false returns cooldown even when window would be open",
			throttle: PipelineThrottle{CooldownSeconds: 10, OffPeakEnabled: false, OffPeakStart: "01:00", OffPeakEnd: "07:00"},
			now:      night,
			want:     10 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.throttle.CooldownAt(tt.now)
			if got != tt.want {
				t.Errorf("CooldownAt(%s) = %v, want %v", tt.now.Format("15:04"), got, tt.want)
			}
		})
	}
}

func TestPipelineThrottle_NextOffPeakStart(t *testing.T) {
	tests := []struct {
		name          string
		throttle      PipelineThrottle
		now           time.Time
		wantTime      string // HH:MM format for the returned time
		wantOK        bool
		checkLocation bool
	}{
		{
			name:     "disabled returns false",
			throttle: PipelineThrottle{OffPeakEnabled: false, OffPeakStart: "01:00"},
			now:      time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
			wantTime: "",
			wantOK:   false,
		},
		{
			name:     "now before start returns today",
			throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"},
			now:      time.Date(2026, 1, 15, 0, 30, 0, 0, time.UTC),
			wantTime: "2026-01-15 01:00",
			wantOK:   true,
		},
		{
			name:     "now at start returns tomorrow",
			throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"},
			now:      time.Date(2026, 1, 15, 1, 0, 0, 0, time.UTC),
			wantTime: "2026-01-16 01:00",
			wantOK:   true,
		},
		{
			name:     "now after start returns tomorrow",
			throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"},
			now:      time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
			wantTime: "2026-01-16 01:00",
			wantOK:   true,
		},
		{
			name:          "returned time preserves Location",
			throttle:      PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "07:00"},
			now:           time.Date(2026, 1, 15, 0, 30, 0, 0, time.Local),
			wantTime:      time.Date(2026, 1, 15, 1, 0, 0, 0, time.Local).Format(time.RFC3339),
			wantOK:        true,
			checkLocation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.throttle.NextOffPeakStart(tt.now)
			if ok != tt.wantOK {
				t.Fatalf("NextOffPeakStart(%s) ok = %v, want %v", tt.now.Format(time.RFC3339), ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if tt.checkLocation {
				if got.Location() != tt.now.Location() {
					t.Errorf("NextOffPeakStart(%s) returned time in %s, want %s", tt.now.Location().String(), got.Location().String(), tt.now.Location().String())
				}
				return
			}
			// Parse the expected format
			wantParsed, err := time.Parse("2006-01-02 15:04", tt.wantTime)
			if err != nil {
				t.Fatalf("bad test layout: %v", err)
			}
			if got.Year() != wantParsed.Year() || got.Month() != wantParsed.Month() || got.Day() != wantParsed.Day() || got.Hour() != wantParsed.Hour() || got.Minute() != wantParsed.Minute() {
				t.Errorf("NextOffPeakStart(%s) = %s, want %s", tt.now.Format("2006-01-02 15:04"), got.Format("2006-01-02 15:04"), tt.wantTime)
			}
		})
	}
}

func TestPipelineThrottle_Validate(t *testing.T) {
	tests := []struct {
		name     string
		throttle PipelineThrottle
		wantErr  bool
	}{
		{
			name:     "default is valid",
			throttle: DefaultPipelineThrottle(),
			wantErr:  false,
		},
		{
			name: "fully configured valid values",
			throttle: PipelineThrottle{
				ReadRate:          0.5,
				CooldownSeconds:   10,
				OffPeakEnabled:    true,
				OffPeakStart:      "22:00",
				OffPeakEnd:        "06:00",
				DeferAboveBytes:   500_000_000,
				ImmediateMaxBytes: 100_000_000,
			},
			wantErr: false,
		},
		{
			name:     "ReadRate exactly 0 is valid (unlimited)",
			throttle: PipelineThrottle{ReadRate: 0},
			wantErr:  false,
		},
		{
			name:     "ReadRate exactly 0.25 is valid (floor inclusive)",
			throttle: PipelineThrottle{ReadRate: 0.25},
			wantErr:  false,
		},
		{
			name:     "negative ReadRate rejected",
			throttle: PipelineThrottle{ReadRate: -1},
			wantErr:  true,
		},
		{
			name:     "ReadRate 0.1 below floor rejected",
			throttle: PipelineThrottle{ReadRate: 0.1},
			wantErr:  true,
		},
		{
			name:     "negative CooldownSeconds rejected",
			throttle: PipelineThrottle{CooldownSeconds: -5},
			wantErr:  true,
		},
		{
			name:     "CooldownSeconds above 3600 rejected",
			throttle: PipelineThrottle{CooldownSeconds: 3601},
			wantErr:  true,
		},
		{
			name:     "negative DeferAboveBytes rejected",
			throttle: PipelineThrottle{DeferAboveBytes: -1},
			wantErr:  true,
		},
		{
			name:     "negative ImmediateMaxBytes rejected",
			throttle: PipelineThrottle{ImmediateMaxBytes: -1},
			wantErr:  true,
		},
		{
			name:     "ImmediateMaxBytes > DeferAboveBytes rejected",
			throttle: PipelineThrottle{DeferAboveBytes: 100, ImmediateMaxBytes: 200},
			wantErr:  true,
		},
		{
			name:     "malformed OffPeakStart rejected",
			throttle: PipelineThrottle{OffPeakStart: "abc"},
			wantErr:  true,
		},
		{
			name:     "malformed OffPeakEnd rejected",
			throttle: PipelineThrottle{OffPeakEnd: "def"},
			wantErr:  true,
		},
		{
			name:     "enabled with empty window (start==end) rejected",
			throttle: PipelineThrottle{OffPeakEnabled: true, OffPeakStart: "01:00", OffPeakEnd: "01:00"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.throttle.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
