package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

type deriveModalityOutboxRepository interface {
	ListDeriveModalityOutbox(context.Context, string, int) ([]domain.Job, error)
	ClearDeriveModalityOutbox(context.Context, string) error
}

func (s *Service) drainDeriveModalityOutbox(ctx context.Context) error {
	repo, ok := s.repo.(deriveModalityOutboxRepository)
	if !ok || s.inferenceRepo == nil {
		return nil
	}
	const batchSize = 64
	var firstError error
	after := ""
	for {
		jobs, err := repo.ListDeriveModalityOutbox(ctx, after, batchSize)
		if err != nil {
			return errors.Join(firstError, err)
		}
		for _, job := range jobs {
			after = job.ID
			if err := s.replayDerivedModalities(ctx, repo, job); err != nil && firstError == nil {
				firstError = fmt.Errorf("derive job %s: %w", job.ID, err)
			}
		}
		if len(jobs) < batchSize {
			return firstError
		}
	}
}

func (s *Service) replayDerivedModalities(ctx context.Context, repo deriveModalityOutboxRepository, job domain.Job) error {
	inputHash := job.InputHash
	if strings.HasPrefix(inputHash, "cache-rederive:") {
		location, err := s.repo.GetPrimaryLocation(ctx, job.AssetID)
		if err != nil {
			return err
		}
		inputHash = canonicalDeriveInputHash(inputHash, location.QuickFingerprint, location.FileSize, location.ProbeModifiedNS)
	}
	if err := s.enqueuePlannedModalities(ctx, job.AssetID, inputHash); err != nil {
		return err
	}
	return repo.ClearDeriveModalityOutbox(ctx, job.ID)
}
