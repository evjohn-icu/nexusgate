package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// timelineRepository is the optional read capability behind the timeline
// route. Asserted rather than embedded so repositories without multimodal
// tables keep serving the rest of the API.
type timelineRepository interface {
	ListAudioEvents(ctx context.Context, assetID string, startMS, endMS int64) ([]domain.AudioEventRow, error)
	ListAudioWindows(ctx context.Context, assetID string, startMS, endMS int64) ([]domain.AudioWindowRow, error)
	ListSpeakerTurns(ctx context.Context, assetID string, startMS, endMS int64) ([]domain.SpeakerTurnRow, error)
	ListVisualRefs(ctx context.Context, assetID string) ([]domain.VisualRef, error)
	ListModalityCoverage(ctx context.Context, assetID string) ([]domain.ModalityCoverageRow, error)
}

// AssetTimeline assembles the synchronized tracks for one asset. Absent tracks
// are reported as empty arrays plus processing coverage, never as a fabricated
// "nothing happened here": no annotation source may confirm absence.
func (s *Service) AssetTimeline(ctx context.Context, assetID string, startMS, endMS int64, limit int) (domain.AssetTimeline, map[string]bool, error) {
	detail, err := s.repo.GetAssetDetail(ctx, assetID)
	if err != nil {
		return domain.AssetTimeline{}, nil, err
	}
	if detail == nil {
		return domain.AssetTimeline{}, nil, fmt.Errorf("asset %s not found", assetID)
	}
	timeline := domain.AssetTimeline{AssetID: assetID}
	if detail.Metadata != nil {
		timeline.DurationMS = detail.Metadata.DurationMS
	}
	if startMS < 0 {
		startMS = 0
	}
	if endMS <= startMS {
		endMS = timeline.DurationMS
	}
	if timeline.DurationMS > 0 && endMS > timeline.DurationMS {
		endMS = timeline.DurationMS
	}

	shots, err := s.repo.ListAssetShots(ctx, assetID)
	if err != nil {
		return domain.AssetTimeline{}, nil, err
	}
	timeline.Shots = make([]domain.AssetShot, 0, len(shots))
	for _, shot := range shots {
		if overlaps(shot.StartMS, shot.EndMS, startMS, endMS) {
			timeline.Shots = append(timeline.Shots, shot)
		}
	}
	timeline.VisualRefs = []domain.VisualRef{}
	timeline.Segments = []domain.TranscriptSegment{}
	timeline.Words = []domain.AlignmentWord{}
	timeline.Turns = []domain.SpeakerTurnRow{}
	timeline.Events = []domain.AudioEventRow{}
	timeline.AudioWindows = []domain.AudioWindowRow{}
	timeline.Coverage = []domain.ModalityCoverageRow{}

	if detail.Transcript != nil {
		for _, segment := range detail.Transcript.Segments {
			if overlaps(segment.StartMS, segment.EndMS, startMS, endMS) {
				timeline.Segments = append(timeline.Segments, segment)
			}
		}
	}
	words, err := s.repo.GetAlignmentWords(ctx, assetID)
	if err == nil {
		for _, word := range words {
			if overlaps(word.StartMS, word.EndMS, startMS, endMS) {
				timeline.Words = append(timeline.Words, word)
			}
		}
	}

	repo, ok := s.repo.(timelineRepository)
	if !ok {
		return timeline, map[string]bool{}, nil
	}
	if events, err := repo.ListAudioEvents(ctx, assetID, startMS, endMS); err == nil {
		timeline.Events = events
	}
	if windows, err := repo.ListAudioWindows(ctx, assetID, startMS, endMS); err == nil {
		timeline.AudioWindows = windows
	}
	if turns, err := repo.ListSpeakerTurns(ctx, assetID, startMS, endMS); err == nil {
		timeline.Turns = turns
	}
	if refs, err := repo.ListVisualRefs(ctx, assetID); err == nil {
		for _, ref := range refs {
			if overlaps(ref.StartMS, ref.EndMS, startMS, endMS) {
				timeline.VisualRefs = append(timeline.VisualRefs, ref)
			}
		}
	}
	if coverage, err := repo.ListModalityCoverage(ctx, assetID); err == nil {
		timeline.Coverage = coverage
	}

	truncated := make(map[string]bool)
	if limit > 0 {
		timeline.Shots, truncated["shots"] = capShots(timeline.Shots, limit)
		timeline.Events, truncated["sound_events"] = capEvents(timeline.Events, limit)
		timeline.Turns, truncated["speaker_turns"] = capTurns(timeline.Turns, limit)
		timeline.Segments, truncated["transcript_segments"] = capSegments(timeline.Segments, limit)
		timeline.Words, truncated["transcript_words"] = capWords(timeline.Words, limit)
		timeline.VisualRefs, truncated["visual_refs"] = capRefs(timeline.VisualRefs, limit)
	}
	return timeline, truncated, nil
}

func overlaps(startMS, endMS, viewStart, viewEnd int64) bool {
	return startMS < viewEnd && endMS > viewStart
}

func capShots(in []domain.AssetShot, limit int) ([]domain.AssetShot, bool) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].StartMS != in[j].StartMS {
			return in[i].StartMS < in[j].StartMS
		}
		if in[i].EndMS != in[j].EndMS {
			return in[i].EndMS < in[j].EndMS
		}
		return in[i].ID < in[j].ID
	})
	if len(in) <= limit {
		return in, false
	}
	return in[:limit], true
}

func capEvents(in []domain.AudioEventRow, limit int) ([]domain.AudioEventRow, bool) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].StartMS != in[j].StartMS {
			return in[i].StartMS < in[j].StartMS
		}
		if in[i].EndMS != in[j].EndMS {
			return in[i].EndMS < in[j].EndMS
		}
		return in[i].ID < in[j].ID
	})
	if len(in) <= limit {
		return in, false
	}
	return in[:limit], true
}

func capTurns(in []domain.SpeakerTurnRow, limit int) ([]domain.SpeakerTurnRow, bool) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].StartMS != in[j].StartMS {
			return in[i].StartMS < in[j].StartMS
		}
		if in[i].EndMS != in[j].EndMS {
			return in[i].EndMS < in[j].EndMS
		}
		return in[i].ID < in[j].ID
	})
	if len(in) <= limit {
		return in, false
	}
	return in[:limit], true
}

func capSegments(in []domain.TranscriptSegment, limit int) ([]domain.TranscriptSegment, bool) {
	if len(in) <= limit {
		return in, false
	}
	return in[:limit], true
}

func capWords(in []domain.AlignmentWord, limit int) ([]domain.AlignmentWord, bool) {
	if len(in) <= limit {
		return in, false
	}
	return in[:limit], true
}

func capRefs(in []domain.VisualRef, limit int) ([]domain.VisualRef, bool) {
	if len(in) <= limit {
		return in, false
	}
	return in[:limit], true
}
