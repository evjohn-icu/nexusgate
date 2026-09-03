package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// TestProviderChannelMembersRejectSharedSecretRef pins what real SQLite does
// when a channel's second member repeats an earlier member's secret_ref:
// UNIQUE(secret_ref) (migration 0013) rejects the insert, so a channel with
// two members on one ref is a state the database cannot store. The app-level
// test that used to seed exactly that shape ran against an in-memory fake and
// guarded a state the schema forbids; this test documents the boundary the app
// cannot cross and logs the raw error so the failure mode is visible in
// `go test -v`.
func TestProviderChannelMembersRejectSharedSecretRef(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "shared-ref.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	sharedRef := "provider/shared"
	_, err = repo.UpsertProviderChannel(ctx, domain.ProviderChannel{
		Capability: "video_analysis", Label: "Shared ref channel", ProviderName: "gemini",
		Protocol: "gemini_generate_content", Endpoint: "https://example.invalid/v1", Model: "gemini-flash", Enabled: true,
		Members: []domain.ProviderChannelMember{
			{ID: "m1", Label: "key-a", SecretRef: sharedRef, Enabled: true, Weight: 1, MaxInflight: 1},
			{ID: "m2", Label: "key-b", SecretRef: sharedRef, Enabled: true, Weight: 1, MaxInflight: 1},
		},
	})
	if err == nil {
		t.Fatalf("expected two members sharing %q to be rejected by UNIQUE(secret_ref)", sharedRef)
	}
	t.Logf("real SQLite error: %v", err)
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("error is not a UNIQUE constraint failure: %v", err)
	}

	// The whole save is one transaction, so a rejected member insert must
	// leave no partial member row behind.
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_channel_members`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("shared-ref rejection must roll back the whole channel, got %d member rows", count)
	}
}
