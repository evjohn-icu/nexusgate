package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// listAndAssertChunked is a test helper that creates n channels with 2 members
// each, calls ListProviderChannels, and asserts every member is correctly
// assigned to its parent channel. Member labels embed the channel index so a
// cross-chunk misassignment is caught even when chunk count is identical.
func listAndAssertChunked(t *testing.T, repo *Repository, n int) {
	t.Helper()
	ctx := context.Background()

	for i := 0; i < n; i++ {
		_, err := repo.UpsertProviderChannel(ctx, domain.ProviderChannel{
			Capability:   "video_analysis",
			Label:        fmt.Sprintf("ch-%04d", i),
			ProviderName: "gemini",
			Protocol:     "gemini_generate_content",
			Endpoint:     "https://example.invalid/v1",
			Model:        "gemini-flash",
			Enabled:      true,
			RouteOrder:   i,
			Members: []domain.ProviderChannelMember{
				{
					Label:       fmt.Sprintf("ch-%04d-a", i),
					SecretRef:   fmt.Sprintf("provider/ch-%04d-a", i),
					Enabled:     true,
					Weight:      1,
					MaxInflight: 1,
				},
				{
					Label:       fmt.Sprintf("ch-%04d-b", i),
					SecretRef:   fmt.Sprintf("provider/ch-%04d-b", i),
					Enabled:     true,
					Weight:      1,
					MaxInflight: 1,
				},
			},
		})
		if err != nil {
			t.Fatalf("upsert channel %d: %v", i, err)
		}
	}

	channels, err := repo.ListProviderChannels(ctx, "video_analysis")
	if err != nil {
		t.Fatalf("ListProviderChannels n=%d: %v", n, err)
	}
	if len(channels) != n {
		t.Fatalf("n=%d: expected %d channels, got %d", n, n, len(channels))
	}

	for i, ch := range channels {
		expectedLabel := fmt.Sprintf("ch-%04d", i)
		if ch.Label != expectedLabel {
			t.Fatalf("n=%d channel %d: label %q, want %q", n, i, ch.Label, expectedLabel)
		}
		if ch.RouteOrder != i {
			t.Fatalf("n=%d channel %d: route_order %d, want %d", n, i, ch.RouteOrder, i)
		}
		if len(ch.Members) != 2 {
			t.Fatalf("n=%d channel %q: expected 2 members, got %d", n, ch.Label, len(ch.Members))
		}

		// Prove members belong to THIS channel, not another one.
		for j, m := range ch.Members {
			if m.ChannelID == "" {
				t.Fatalf("n=%d channel %q member %d: ChannelID is empty", n, ch.Label, j)
			}
			if m.ChannelID != ch.ID {
				t.Fatalf("n=%d channel %q member %d %q: ChannelID=%q does not match parent ID=%q (cross-chunk misassignment)", n, ch.Label, j, m.Label, m.ChannelID, ch.ID)
			}
		}

		// Member labels embed channel index — distinct per channel.
		wantA := fmt.Sprintf("ch-%04d-a", i)
		wantB := fmt.Sprintf("ch-%04d-b", i)
		if ch.Members[0].Label != wantA {
			t.Fatalf("n=%d channel %q: first member label mismatch: %q != %q", n, ch.Label, ch.Members[0].Label, wantA)
		}
		if ch.Members[1].Label != wantB {
			t.Fatalf("n=%d channel %q: second member label mismatch: %q != %q", n, ch.Label, ch.Members[1].Label, wantB)
		}
		// SecretRef must also match the channel index.
		if ch.Members[0].SecretRef != fmt.Sprintf("provider/ch-%04d-a", i) {
			t.Fatalf("n=%d channel %q: first member SecretRef mismatch: %q", n, ch.Label, ch.Members[0].SecretRef)
		}
		if ch.Members[1].SecretRef != fmt.Sprintf("provider/ch-%04d-b", i) {
			t.Fatalf("n=%d channel %q: second member SecretRef mismatch: %q", n, ch.Label, ch.Members[1].SecretRef)
		}
	}
}

// TestListProviderChannelsChunksLargeSet creates more channels than the chunk
// size (1200 > 500) and asserts every member is correctly assigned to its
// channel with correct ordering. This proves the batched IN query stays under
// SQLite's variable limit and that chunking does not break grouping or ordering.
func TestListProviderChannelsChunksLargeSet(t *testing.T) {
	ctx := context.Background()

	t.Run("empty", func(t *testing.T) {
		repo, err := Open(filepath.Join(t.TempDir(), "chunk-empty.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		if err := repo.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		channels, err := repo.ListProviderChannels(ctx, "video_analysis")
		if err != nil {
			t.Fatalf("ListProviderChannels empty: %v", err)
		}
		if len(channels) != 0 {
			t.Fatalf("expected 0 channels, got %d", len(channels))
		}
	})

	t.Run("large_set", func(t *testing.T) {
		repo, err := Open(filepath.Join(t.TempDir(), "chunk-large.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		if err := repo.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		listAndAssertChunked(t, repo, 1200)
	})

	t.Run("cross_chunk_boundary", func(t *testing.T) {
		repo, err := Open(filepath.Join(t.TempDir(), "chunk-boundary.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		if err := repo.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		// One more than a single chunk (501 > 500) to cross exactly one boundary.
		listAndAssertChunked(t, repo, 501)
	})
}
