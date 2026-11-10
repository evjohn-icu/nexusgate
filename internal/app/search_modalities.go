package app

import (
	"context"
	"fmt"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/search"
)

// multimodalReadRepository is the read capability the search adapter needs.
// The concrete sqlite repository implements it; a test repository without
// multimodal tables simply does not, and the adapter is never constructed.
type multimodalReadRepository interface {
	FilteredShotUniverse(ctx context.Context, facets domain.FacetFilter, assetFilter domain.AssetContextFilter) (map[string]bool, error)
	VisualWindowMatches(ctx context.Context, spaceID string, query []float32, universe map[string]bool, limit int) ([]domain.ModalityMatch, error)
	AudioWindowMatches(ctx context.Context, spaceID string, query []float32, universe map[string]bool, limit int) ([]domain.ModalityMatch, error)
	SoundEventMatches(ctx context.Context, labels []string, universe map[string]bool, limit int) ([]domain.ModalityMatch, error)
	SpeakerTurnMatches(ctx context.Context, assetID, generationID, localSpeakerID string, universe map[string]bool, limit int) ([]domain.ModalityMatch, error)
}

// modalityStore adapts repository rows to the search package's read contract.
// The conversion is deliberately mechanical: search owns ranking, the
// repository owns SQL, and neither learns the other's types.
type modalityStore struct {
	repo multimodalReadRepository
}

var _ search.ModalityStore = (*modalityStore)(nil)

// NewModalityStore returns the adapter, or nil when the repository cannot
// answer multimodal queries (test fakes, older schemas).
func NewModalityStore(repo any) search.ModalityStore {
	typed, ok := repo.(multimodalReadRepository)
	if !ok {
		return nil
	}
	return &modalityStore{repo: typed}
}

func (m *modalityStore) FilteredShotUniverse(ctx context.Context, facets domain.FacetFilter, assetFilter domain.AssetContextFilter) (map[string]bool, error) {
	return m.repo.FilteredShotUniverse(ctx, facets, assetFilter)
}

func toWindows(matches []domain.ModalityMatch) []search.ModalityWindow {
	out := make([]search.ModalityWindow, 0, len(matches))
	for _, match := range matches {
		out = append(out, search.ModalityWindow{
			AssetID:        match.AssetID,
			ShotID:         match.ShotID,
			RecordID:       match.RecordID,
			Modality:       string(match.Modality),
			StartMS:        match.StartMS,
			EndMS:          match.EndMS,
			Score:          match.Score,
			Label:          match.Label,
			EvidenceStatus: match.EvidenceStatus,
			EvidenceSource: string(match.Modality),
			ModelID:        match.ModelID,
			ModelVersion:   match.ModelRevision,
			Confidence:     match.Confidence,
		})
	}
	return out
}

func (m *modalityStore) VisualWindows(ctx context.Context, spaceID string, vector []float32, universe map[string]bool, limit int) ([]search.ModalityWindow, error) {
	matches, err := m.repo.VisualWindowMatches(ctx, spaceID, vector, universe, limit)
	if err != nil {
		return nil, err
	}
	return toWindows(matches), nil
}

func (m *modalityStore) AudioWindows(ctx context.Context, spaceID string, vector []float32, universe map[string]bool, limit int) ([]search.ModalityWindow, error) {
	matches, err := m.repo.AudioWindowMatches(ctx, spaceID, vector, universe, limit)
	if err != nil {
		return nil, err
	}
	return toWindows(matches), nil
}

func (m *modalityStore) SoundEvents(ctx context.Context, labels []string, universe map[string]bool, limit int) ([]search.ModalityWindow, error) {
	matches, err := m.repo.SoundEventMatches(ctx, labels, universe, limit)
	if err != nil {
		return nil, err
	}
	return toWindows(matches), nil
}

func (m *modalityStore) SpeakerTurns(ctx context.Context, filter search.SpeakerFilter, universe map[string]bool, limit int) ([]search.ModalityWindow, error) {
	if filter.ProfileID != "" {
		// Profile matching is biometric and admin-only; it is resolved before
		// search reaches this adapter, never as a public channel filter.
		return nil, fmt.Errorf("speaker profile filtering requires an authorised profile search")
	}
	matches, err := m.repo.SpeakerTurnMatches(ctx, filter.AssetID, filter.DiarizationGenerationID, filter.LocalSpeakerID, universe, limit)
	if err != nil {
		return nil, err
	}
	return toWindows(matches), nil
}
