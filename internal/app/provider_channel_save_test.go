package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

func TestProviderKeyReplacementSurvivesFailedChannelUpdate(t *testing.T) {
	ctx := context.Background()
	const oldValue, nextValue = "mock-old-value", "mock-next-value"
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "channels.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, config.Config{
		DataDir: dir, CacheDir: filepath.Join(dir, "cache"),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := service.SaveProviderChannel(ctx, domain.ProviderChannel{
		Capability: "embedding", Label: "Fixture channel", ProviderName: "openai_embeddings",
		Endpoint: "http://127.0.0.1:44080/v1", Model: "fixture", Enabled: true,
		Members: []domain.ProviderChannelMember{{Label: "fixture", Enabled: true, Weight: 1, MaxInflight: 1}},
	}, []string{oldValue})
	if err != nil {
		t.Fatal(err)
	}
	original := saved.Members[0]
	patch := ProviderChannelUpdate{Members: &[]ProviderChannelMemberUpdate{{ID: original.ID, APIKey: nextValue}}}
	if _, err := repo.DB().ExecContext(ctx, `CREATE TRIGGER refuse_provider_update BEFORE UPDATE ON provider_channels BEGIN SELECT RAISE(ABORT, 'fixture rejected update'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateProviderChannel(ctx, saved.ID, patch); err == nil {
		t.Fatal("SQLite trigger should reject the update")
	}
	unchanged, err := service.providerChannelByID(ctx, saved.ID)
	if err != nil || len(unchanged.Members) != 1 || unchanged.Members[0].SecretRef != original.SecretRef {
		t.Fatalf("failed update changed the stored reference: %v", err)
	}
	checkKey := func(ref, want string) {
		t.Helper()
		value, found, err := service.secrets.Get(ref)
		if err != nil || !found || value != want {
			t.Fatalf("secret lookup found=%t err=%v matches=%t", found, err, value == want)
		}
	}
	checkKey(original.SecretRef, oldValue)
	if _, err := repo.DB().ExecContext(ctx, `DROP TRIGGER refuse_provider_update`); err != nil {
		t.Fatal(err)
	}
	replaced, err := service.UpdateProviderChannel(ctx, saved.ID, patch)
	if err != nil || len(replaced.Members) != 1 || replaced.Members[0].ID != original.ID || replaced.Members[0].SecretRef == original.SecretRef {
		t.Fatalf("replacement did not retain member identity with a new reference: %v", err)
	}
	checkKey(replaced.Members[0].SecretRef, nextValue)
	if _, found, err := service.secrets.Get(original.SecretRef); err != nil || found {
		t.Fatalf("previous reference found=%t err=%v after successful commit", found, err)
	}
}
