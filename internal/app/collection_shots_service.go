package app

import (
	"context"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// ErrCollectionNotFound reports that a collection id names nothing. The
// repository's shot-basket methods enforce the collection's existence only
// through the collection_shots foreign key, which surfaces as a raw SQLite
// constraint failure — one failure class this codebase refuses to classify by
// message text — so the sentinel is minted here, at the service boundary,
// from the structural check GetAssetCollection already performs. The API
// classifier maps it to 404; an operator seeing it has typed or been handed
// an identifier that no longer names a collection.
var ErrCollectionNotFound = domain.ErrCollectionNotFound

// The shot-basket methods are thin wrappers over the repository: the rules
// they enforce (a shot may be pinned once per collection, the reorder list
// must be exactly the collection's pins, positions are display order) live in
// the SQL write transaction. The one thing the wrappers add is the collection
// existence gate, because without it a basket write against a deleted or
// mistyped collection would surface as a raw FK constraint error — the 500
// that reads as "server broken" instead of "no such collection".
func (s *Service) AddShotToCollection(ctx context.Context, collectionID, shotID string) error {
	if err := s.requireCollection(ctx, collectionID); err != nil {
		return err
	}
	return s.repo.AddShotToCollection(ctx, collectionID, shotID)
}

func (s *Service) RemoveShotFromCollection(ctx context.Context, collectionID, shotID string) error {
	if err := s.requireCollection(ctx, collectionID); err != nil {
		return err
	}
	return s.repo.RemoveShotFromCollection(ctx, collectionID, shotID)
}

func (s *Service) ListCollectionShots(ctx context.Context, collectionID string) ([]domain.CollectionShotDetail, error) {
	if err := s.requireCollection(ctx, collectionID); err != nil {
		return nil, err
	}
	return s.repo.ListCollectionShots(ctx, collectionID)
}

func (s *Service) ReorderCollectionShots(ctx context.Context, collectionID string, shotIDs []string) error {
	if err := s.requireCollection(ctx, collectionID); err != nil {
		return err
	}
	return s.repo.ReorderCollectionShots(ctx, collectionID, shotIDs)
}

func (s *Service) requireCollection(ctx context.Context, collectionID string) error {
	collection, err := s.repo.GetAssetCollection(ctx, collectionID)
	if err != nil {
		return err
	}
	if collection == nil {
		return domain.ErrCollectionNotFound
	}
	return nil
}
