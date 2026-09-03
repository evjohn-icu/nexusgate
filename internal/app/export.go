package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/nleexport"
)

// ErrPlanNotApproved is returned when an export is requested for a plan that a
// human has not approved. Export is the point where a selection stops being a
// proposal and becomes an edit someone will cut with, so it is gated on the
// same approval that locks the plan. Without this an agent could draft a plan
// and hand the operator a finished EDL, which reverses the review order the
// whole repurpose flow exists to enforce.
var ErrPlanNotApproved = errors.New("repurpose plan is not approved")

// ErrPlanNotExportable is returned when an approved plan cannot be rendered as
// a timeline — no selected shots, or a selected shot whose asset has never been
// probed. These are conditions of the library, not of the request, so they are
// reported rather than silently dropped: an EDL that quietly omits a section is
// worse than one that refuses to be written.
var ErrPlanNotExportable = errors.New("repurpose plan cannot be exported")

// ErrPlanNotFound is app's name for domain.ErrPlanNotFound — an alias, not a
// second value, because the repository now wraps that same sentinel when a
// revision write finds the plan gone, and two names for one condition is the
// trap 27ac022 closed. See internal/domain/errors.go for why the identity
// lives there and what it means; this file is one of its two callers, the
// export path below being the other.
var ErrPlanNotFound = domain.ErrPlanNotFound

// ErrModelRunNotRunning is the repository's strict CAS transition sentinel.
var ErrModelRunNotRunning = domain.ErrModelRunNotRunning

// ExportPlanEDL renders an approved plan as a CMX3600 edit decision list.
func (s *Service) ExportPlanEDL(ctx context.Context, planID string) (string, error) {
	timeline, _, err := s.planTimeline(ctx, planID)
	if err != nil {
		return "", err
	}
	return nleexport.EDL(timeline)
}

// ExportPlanFCPXML renders an approved plan as an FCPXML 1.9 document.
//
// Note that FCPXML, unlike an EDL, embeds the absolute path of every original
// file. That is what makes it importable without a relink step, and it is also
// why this output is administrator-only and outside the agent allowlist: it is
// the one export that discloses the library's filesystem layout.
func (s *Service) ExportPlanFCPXML(ctx context.Context, planID string) (string, error) {
	timeline, sources, err := s.planTimeline(ctx, planID)
	if err != nil {
		return "", err
	}
	return nleexport.FCPXML(timeline, sources)
}

// planTimeline turns an approved plan's selected shots into the ordered
// timeline both exporters consume. Sections keep their plan order, which is the
// order a human approved them in; nothing here re-ranks or re-selects.
func (s *Service) planTimeline(ctx context.Context, planID string) (nleexport.Timeline, map[string]nleexport.Source, error) {
	plan, err := s.repo.GetRepurposePlan(ctx, planID)
	if err != nil {
		return nleexport.Timeline{}, nil, err
	}
	if plan == nil {
		return nleexport.Timeline{}, nil, fmt.Errorf("%w: no plan matches id %s", ErrPlanNotFound, planID)
	}
	if plan.Status != "approved" {
		return nleexport.Timeline{}, nil, fmt.Errorf("%w: %s is %s", ErrPlanNotApproved, planID, plan.Status)
	}

	timeline := nleexport.Timeline{Title: plan.Title}
	sources := make(map[string]nleexport.Source)
	// Reel names are assigned across the whole plan, not per section, so one
	// original reused in three sections stays one source. reelFor is what keeps
	// two different originals from colliding onto one name before nleexport
	// ever sees them -- its own collision handling works on distinct input
	// strings, so a collision introduced here would be invisible to it.
	reels := make(map[string]string)
	rates := make(map[string]float64)
	claimed := make(map[string]struct{})

	for _, section := range plan.Sections {
		if section.SelectedShotID == "" {
			continue
		}
		candidate, found := selectedCandidate(section)
		if !found {
			return nleexport.Timeline{}, nil, fmt.Errorf("%w: section %q selects shot %s, which is not among its candidates", ErrPlanNotExportable, section.Role, section.SelectedShotID)
		}

		reel, ok := reels[candidate.AssetID]
		if !ok {
			location, locErr := s.repo.GetPrimaryLocation(ctx, candidate.AssetID)
			if locErr != nil {
				return nleexport.Timeline{}, nil, fmt.Errorf("%w: asset %s for section %q: %v", ErrPlanNotExportable, candidate.AssetID, section.Role, locErr)
			}
			metadata, metaErr := s.repo.GetMediaMetadata(ctx, candidate.AssetID)
			if metaErr != nil {
				return nleexport.Timeline{}, nil, fmt.Errorf("%w: asset %s for section %q: %v", ErrPlanNotExportable, candidate.AssetID, section.Role, metaErr)
			}
			if metadata == nil || metadata.FPS <= 0 {
				return nleexport.Timeline{}, nil, fmt.Errorf("%w: asset %s (%s) has no frame rate on record; run the pipeline over it before exporting", ErrPlanNotExportable, candidate.AssetID, filepath.Base(location.AbsolutePath))
			}
			reel = reelFor(*metadata, location.AbsolutePath, claimed)
			claimed[reel] = struct{}{}
			reels[candidate.AssetID] = reel
			rates[reel] = metadata.FPS
			sources[reel] = nleexport.Source{Path: location.AbsolutePath, Name: filepath.Base(location.AbsolutePath), Duration: metadata.DurationMS}
		}
		timeline.Clips = append(timeline.Clips, nleexport.Clip{Reel: reel, SourceInMS: candidate.StartMS, SourceOutMS: candidate.EndMS, FPS: rates[reel]})
	}

	if len(timeline.Clips) == 0 {
		return nleexport.Timeline{}, nil, fmt.Errorf("%w: %s has no selected shots", ErrPlanNotExportable, planID)
	}
	return timeline, sources, nil
}

// selectedCandidate finds the candidate a human settled on. The selection is
// stored as a shot id rather than an index precisely so that reordering
// candidates cannot silently repoint an approved plan at different footage, so
// a selection with no matching candidate is a corrupt plan, not a default.
func selectedCandidate(section domain.PlanSection) (domain.PlanCandidate, bool) {
	for _, candidate := range section.Candidates {
		if candidate.ShotID == section.SelectedShotID {
			return candidate, true
		}
	}
	return domain.PlanCandidate{}, false
}

// reelFor picks the name an original is known by on the timeline. Camera reel
// metadata is preferred over the filename because that is the identifier the
// footage was shot under and the one an assistant editor will be matching
// against; the filename is the fallback for anything the camera did not stamp.
//
// The claimed set exists because two originals can legitimately carry the same
// reel id -- two cards labelled A001 from different cameras is ordinary -- and
// a shared name would make both exporters treat them as one file.
func reelFor(metadata domain.MediaMetadata, absolutePath string, claimed map[string]struct{}) string {
	name := strings.TrimSpace(metadata.Reel)
	if name == "" {
		name = strings.TrimSpace(metadata.Clip)
	}
	if name == "" {
		base := filepath.Base(absolutePath)
		name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if name == "" {
		name = "source"
	}
	candidate := name
	for n := 2; ; n++ {
		if _, taken := claimed[candidate]; !taken {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", name, n)
	}
}
