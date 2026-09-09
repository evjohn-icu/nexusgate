package video

import (
	"context"
	"errors"
	"testing"
)

// limiterProvider is a minimal VideoUnderstandingProvider that also declares
// an inline ceiling, so InlineVideoBudget's limiter branch can be driven
// directly without pulling in a real adapter.
type limiterProvider struct {
	stubProvider
	limit int64
}

func (p *limiterProvider) MaxInlineVideoBytes() int64 { return p.limit }

// uploaderProvider declares it prepares video out of band, so it must never
// be limited by request size.
type uploaderProvider struct {
	stubProvider
}

func (p *uploaderProvider) RequiresVideoPreparation() bool { return true }

func (p *uploaderProvider) PrepareVideo(context.Context, PrepareVideoRequest) (PreparedVideo, error) {
	return PreparedVideo{}, nil
}

func TestInlineVideoBudgetUsesTheDeclaredLimitWhenPositive(t *testing.T) {
	provider := &limiterProvider{stubProvider: stubProvider{name: "declared"}, limit: 5 << 20}
	budget, inline, err := InlineVideoBudget(provider, 24<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !inline || budget != 5<<20 {
		t.Fatalf("budget=%d inline=%v, want the declared 5MiB", budget, inline)
	}
}

func TestInlineVideoBudgetFallsBackWhenTheProviderDeclaresNothing(t *testing.T) {
	provider := &limiterProvider{stubProvider: stubProvider{name: "silent"}, limit: 0}
	budget, inline, err := InlineVideoBudget(provider, 24<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !inline || budget != 24<<20 {
		t.Fatalf("budget=%d inline=%v, want the fallback for an undeclared (zero) limit", budget, inline)
	}
}

// A provider that uploads out of band is never limited by request size, and
// must not be asked for a budget at all — RequiresVideoPreparation wins over
// any MaxInlineVideoBytes the type might also implement.
func TestInlineVideoBudgetSkipsUploadingProviders(t *testing.T) {
	provider := &uploaderProvider{stubProvider: stubProvider{name: "uploader"}}
	budget, inline, err := InlineVideoBudget(provider, 24<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inline || budget != 0 {
		t.Fatalf("budget=%d inline=%v, an uploading provider must report inline=false", budget, inline)
	}
}

// NoInlineRoom is a real, computed answer — "there is no room" — not the
// same thing as "unknown". InlineVideoBudget has to keep that distinction:
// folding it into the fallback would make an operator's explicit, tight
// limit produce a *larger* effective budget than declaring nothing.
func TestInlineVideoBudgetReportsErrNoInlineRoomDistinctlyFromUnknown(t *testing.T) {
	provider := &limiterProvider{stubProvider: stubProvider{name: "starved"}, limit: NoInlineRoom}
	budget, inline, err := InlineVideoBudget(provider, 24<<20)
	if !errors.Is(err, ErrNoInlineRoom) {
		t.Fatalf("err=%v, want ErrNoInlineRoom", err)
	}
	if budget != 0 || !inline {
		t.Fatalf("budget=%d inline=%v on the NoInlineRoom path, expected a zero, non-usable budget with inline=true", budget, inline)
	}
}
