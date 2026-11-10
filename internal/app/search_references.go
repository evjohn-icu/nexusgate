package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
)

const (
	// MaxSearchReferenceBytes is the upload ceiling. 8 MiB decodes to well
	// under 20 MP for any ordinary camera image; the pixel bound catches the
	// decompression bomb that fits in 8 MiB of PNG.
	MaxSearchReferenceBytes = 8 << 20
	// MaxSearchReferencePixels bounds decoded area.
	MaxSearchReferencePixels = 20_000_000
)

// ErrSearchReferenceNotFound reports an unknown reference id.
var ErrSearchReferenceNotFound = errors.New("search reference not found")

// searchReferenceRepository is asserted on the concrete repository so a
// minimal test service still compiles without query-image support.
type searchReferenceRepository interface {
	CreateSearchReference(ctx context.Context, ref domain.SearchReference) error
	GetSearchReference(ctx context.Context, id string) (domain.SearchReference, error)
	DeleteSearchReference(ctx context.Context, id string) (bool, error)
	ExpiredSearchReferences(ctx context.Context, now time.Time) ([]domain.SearchReference, error)
	CancelInferenceTasksForReference(ctx context.Context, referenceID string) error
}

func (s *Service) searchReferences() (searchReferenceRepository, error) {
	repo, ok := s.repo.(searchReferenceRepository)
	if !ok {
		return nil, fmt.Errorf("query images are not supported by this repository")
	}
	return repo, nil
}

// CreateSearchReference decodes an uploaded image, rejects anything that is not
// a JPEG/PNG within the byte and pixel bounds, re-encodes it to strip metadata,
// and publishes it under a generated name. The caller supplies bytes, never a
// path or URL.
func (s *Service) CreateSearchReference(ctx context.Context, raw io.Reader) (domain.SearchReference, error) {
	repo, err := s.searchReferences()
	if err != nil {
		return domain.SearchReference{}, err
	}
	limited := io.LimitReader(raw, MaxSearchReferenceBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return domain.SearchReference{}, fmt.Errorf("read query image: %w", err)
	}
	if len(data) == 0 {
		return domain.SearchReference{}, fmt.Errorf("query image is empty")
	}
	if len(data) > MaxSearchReferenceBytes {
		return domain.SearchReference{}, fmt.Errorf("query image exceeds %d bytes", MaxSearchReferenceBytes)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return domain.SearchReference{}, fmt.Errorf("query image is not a decodable JPEG or PNG: %w", err)
	}
	if format != "jpeg" && format != "png" {
		return domain.SearchReference{}, fmt.Errorf("query image format %q is not supported", format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxSearchReferencePixels {
		return domain.SearchReference{}, fmt.Errorf("query image exceeds %d pixels", MaxSearchReferencePixels)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return domain.SearchReference{}, fmt.Errorf("decode query image: %w", err)
	}

	id := idgen.New()
	relative := filepath.Join("search-references", id+".jpg")
	absolute := filepath.Join(s.cfg.CacheDir, relative)
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return domain.SearchReference{}, err
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, decoded, &jpeg.Options{Quality: 92}); err != nil {
		return domain.SearchReference{}, fmt.Errorf("re-encode query image: %w", err)
	}
	tmp := absolute + ".tmp"
	if err := os.WriteFile(tmp, encoded.Bytes(), 0o600); err != nil {
		return domain.SearchReference{}, err
	}
	if err := os.Rename(tmp, absolute); err != nil {
		os.Remove(tmp)
		return domain.SearchReference{}, err
	}
	sum := sha256.Sum256(encoded.Bytes())
	now := time.Now().UTC()
	ref := domain.SearchReference{
		ID:                id,
		ContentHash:       "sha256:" + hex.EncodeToString(sum[:]),
		MediaType:         "image/jpeg",
		RelativeCachePath: relative,
		Width:             cfg.Width,
		Height:            cfg.Height,
		CreatedAt:         now,
		ExpiresAt:         now.Add(domain.SearchReferenceTTL),
	}
	if err := repo.CreateSearchReference(ctx, ref); err != nil {
		os.Remove(absolute)
		return domain.SearchReference{}, err
	}
	return ref, nil
}

// GetSearchReference returns the row and the absolute path of its bytes,
// verifying that the stored path stays inside the managed cache.
func (s *Service) GetSearchReference(ctx context.Context, id string) (domain.SearchReference, string, error) {
	repo, err := s.searchReferences()
	if err != nil {
		return domain.SearchReference{}, "", err
	}
	ref, err := repo.GetSearchReference(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrSearchReferenceExpired) {
			return domain.SearchReference{}, "", err
		}
		return domain.SearchReference{}, "", ErrSearchReferenceNotFound
	}
	absolute, err := s.resolveReferencePath(ref.RelativeCachePath)
	if err != nil {
		return domain.SearchReference{}, "", err
	}
	return ref, absolute, nil
}

func (s *Service) resolveReferencePath(relative string) (string, error) {
	root := filepath.Join(s.cfg.CacheDir, "search-references")
	absolute := filepath.Join(s.cfg.CacheDir, relative)
	rel, err := filepath.Rel(root, absolute)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("search reference path escapes the managed cache")
	}
	return absolute, nil
}

// DeleteSearchReference removes the row, cancels dependent query tasks and
// deletes the managed bytes. It reports not-found for an unknown id.
func (s *Service) DeleteSearchReference(ctx context.Context, id string) error {
	repo, err := s.searchReferences()
	if err != nil {
		return err
	}
	if err := repo.CancelInferenceTasksForReference(ctx, id); err != nil {
		return err
	}
	ref, err := repo.GetSearchReference(ctx, id)
	if err != nil && !errors.Is(err, domain.ErrSearchReferenceExpired) {
		return ErrSearchReferenceNotFound
	}
	removed, err := repo.DeleteSearchReference(ctx, id)
	if err != nil {
		return err
	}
	if !removed {
		return ErrSearchReferenceNotFound
	}
	if ref.RelativeCachePath != "" {
		if absolute, err := s.resolveReferencePath(ref.RelativeCachePath); err == nil {
			os.Remove(absolute)
		}
	}
	return nil
}

// PurgeExpiredSearchReferences removes rows and files whose lifetime ended. It
// returns the number of references removed; a file that is already gone is not
// an error, because the row is the authority.
func (s *Service) PurgeExpiredSearchReferences(ctx context.Context) (int, error) {
	repo, err := s.searchReferences()
	if err != nil {
		return 0, err
	}
	expired, err := repo.ExpiredSearchReferences(ctx, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, ref := range expired {
		if err := repo.CancelInferenceTasksForReference(ctx, ref.ID); err != nil {
			return removed, err
		}
		ok, err := repo.DeleteSearchReference(ctx, ref.ID)
		if err != nil {
			return removed, err
		}
		if !ok {
			continue
		}
		removed++
		if absolute, err := s.resolveReferencePath(ref.RelativeCachePath); err == nil {
			os.Remove(absolute)
		}
	}
	return removed, nil
}
