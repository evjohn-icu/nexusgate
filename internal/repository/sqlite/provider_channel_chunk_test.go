package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

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

		const n = 1200
		for i := 0; i < n; i++ {
			label := fmt.Sprintf("chunk-channel-%04d", i)
			_, err := repo.UpsertProviderChannel(ctx, domain.ProviderChannel{
				Capability:   "video_analysis",
				Label:        label,
				ProviderName: "gemini",
				Protocol:     "gemini_generate_content",
				Endpoint:     "https://example.invalid/v1",
				Model:        "gemini-flash",
				Enabled:      true,
				RouteOrder:   i,
				Members: []domain.ProviderChannelMember{
					{Label: "key-a", SecretRef: fmt.Sprintf("provider/chunk-a-%04d", i), Enabled: true, Weight: 1, MaxInflight: 1},
					{Label: "key-b", SecretRef: fmt.Sprintf("provider/chunk-b-%04d", i), Enabled: true, Weight: 1, MaxInflight: 1},
				},
			})
			if err != nil {
				t.Fatalf("upsert channel %d: %v", i, err)
			}
		}

		channels, err := repo.ListProviderChannels(ctx, "video_analysis")
		if err != nil {
			t.Fatalf("ListProviderChannels: %v", err)
		}
		if len(channels) != n {
			t.Fatalf("expected %d channels, got %d", n, len(channels))
		}

		// Channels must be ordered by route_order (ascending). Since route_order
		// matches insertion order (0..1199), the label suffix must ascend. If
		// chunking dropped or reordered chunks, this would catch it.
		for i, ch := range channels {
			expectedLabel := fmt.Sprintf("chunk-channel-%04d", i)
			if ch.Label != expectedLabel {
				t.Fatalf("channel %d: label %q, want %q (chunking may have reordered)", i, ch.Label, expectedLabel)
			}
			if ch.RouteOrder != i {
				t.Fatalf("channel %d: route_order %d, want %d", i, ch.RouteOrder, i)
			}
			if len(ch.Members) != 2 {
				t.Fatalf("channel %s: expected 2 members, got %d", ch.Label, len(ch.Members))
			}
			// Members must be ordered by label within each channel.
			if ch.Members[0].Label != "key-a" {
				t.Fatalf("channel %s: first member label mismatch: %q", ch.Label, ch.Members[0].Label)
			}
			if ch.Members[1].Label != "key-b" {
				t.Fatalf("channel %s: second member label mismatch: %q", ch.Label, ch.Members[1].Label)
			}
		}
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
		const n = 501
		for i := 0; i < n; i++ {
			label := fmt.Sprintf("boundary-%04d", i)
			_, err := repo.UpsertProviderChannel(ctx, domain.ProviderChannel{
				Capability:   "video_analysis",
				Label:        label,
				ProviderName: "gemini",
				Protocol:     "gemini_generate_content",
				Endpoint:     "https://example.invalid/v1",
				Model:        "gemini-flash",
				Enabled:      true,
				RouteOrder:   i,
				Members: []domain.ProviderChannelMember{
					{Label: "only", SecretRef: fmt.Sprintf("provider/boundary-%04d", i), Enabled: true, Weight: 1, MaxInflight: 1},
				},
			})
			if err != nil {
				t.Fatalf("upsert channel %d: %v", i, err)
			}
		}

		channels, err := repo.ListProviderChannels(ctx, "video_analysis")
		if err != nil {
			t.Fatalf("ListProviderChannels boundary: %v", err)
		}
		if len(channels) != n {
			t.Fatalf("expected %d channels, got %d (chunk boundary may have truncated)", n, len(channels))
		}

		// Every channel must have its single member.
		for i, ch := range channels {
			if len(ch.Members) != 1 {
				t.Fatalf("channel %d %q: expected 1 member, got %d", i, ch.Label, len(ch.Members))
			}
		}
	})
}
