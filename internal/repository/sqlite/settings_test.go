package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

func TestSettingsPipelineThrottle(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	t.Run("fresh database returns default and no error", func(t *testing.T) {
		throttle, err := repo.GetPipelineThrottle(ctx)
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		def := domain.DefaultPipelineThrottle()
		if throttle.ReadRate != def.ReadRate || throttle.CooldownSeconds != def.CooldownSeconds || throttle.OffPeakStart != def.OffPeakStart || throttle.OffPeakEnd != def.OffPeakEnd || throttle.DeferAboveBytes != def.DeferAboveBytes || throttle.ImmediateMaxBytes != def.ImmediateMaxBytes {
			t.Fatalf("got %+v, want default %+v", throttle, def)
		}
	})

	t.Run("save then get round-trips every field", func(t *testing.T) {
		want := domain.PipelineThrottle{
			ReadRate:          1.5,
			CooldownSeconds:   30,
			OffPeakEnabled:    true,
			OffPeakStart:      "02:00",
			OffPeakEnd:        "06:00",
			DeferAboveBytes:   1000000000,
			ImmediateMaxBytes: 50000000,
		}
		if err := repo.SavePipelineThrottle(ctx, want); err != nil {
			t.Fatalf("SavePipelineThrottle: %v", err)
		}
		got, err := repo.GetPipelineThrottle(ctx)
		if err != nil {
			t.Fatalf("GetPipelineThrottle: %v", err)
		}
		if got.ReadRate != want.ReadRate || got.CooldownSeconds != want.CooldownSeconds || got.OffPeakEnabled != want.OffPeakEnabled || got.OffPeakStart != want.OffPeakStart || got.OffPeakEnd != want.OffPeakEnd || got.DeferAboveBytes != want.DeferAboveBytes || got.ImmediateMaxBytes != want.ImmediateMaxBytes {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("saving twice updates existing row", func(t *testing.T) {
		first := domain.PipelineThrottle{ReadRate: 1.0}
		if err := repo.SavePipelineThrottle(ctx, first); err != nil {
			t.Fatal(err)
		}
		second := domain.PipelineThrottle{ReadRate: 2.0}
		if err := repo.SavePipelineThrottle(ctx, second); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key='pipeline_throttle'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("expected 1 row, got %d", count)
		}
		got, err := repo.GetPipelineThrottle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.ReadRate != 2.0 {
			t.Fatalf("got ReadRate=%f, want 2.0", got.ReadRate)
		}
	})

	t.Run("Validate rejection propagates and leaves prior value untouched", func(t *testing.T) {
		want := domain.PipelineThrottle{ReadRate: 3.0}
		if err := repo.SavePipelineThrottle(ctx, want); err != nil {
			t.Fatal(err)
		}
		bad := domain.PipelineThrottle{ReadRate: -1}
		err := repo.SavePipelineThrottle(ctx, bad)
		if err == nil {
			t.Fatal("expected validation error for negative ReadRate")
		}
		got, err := repo.GetPipelineThrottle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.ReadRate != 3.0 {
			t.Fatalf("expected ReadRate=3.0 preserved after failed save, got %f", got.ReadRate)
		}
	})

	t.Run("corrupt stored value returns default and error", func(t *testing.T) {
		if _, err := repo.db.ExecContext(ctx, `INSERT OR REPLACE INTO settings(key,value,updated_at) VALUES('pipeline_throttle','not-json','2026-01-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		throttle, err := repo.GetPipelineThrottle(ctx)
		if err == nil {
			t.Fatal("expected error for corrupt JSON, got nil")
		}
		def := domain.DefaultPipelineThrottle()
		if throttle.ReadRate != def.ReadRate || throttle.CooldownSeconds != def.CooldownSeconds || throttle.OffPeakStart != def.OffPeakStart || throttle.OffPeakEnd != def.OffPeakEnd {
			t.Fatalf("got %+v, want default %+v", throttle, def)
		}
	})
}
