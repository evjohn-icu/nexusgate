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
func (s *Service) AssetTimeline(ctx context.Context, assetID string, startMS, endMS int64, limit int, cursors map[string]string) (domain.AssetTimeline, map[string]string, map[string]bool, error) {
	detail, err := s.repo.GetAssetDetail(ctx, assetID)
	if err != nil {
		return domain.AssetTimeline{}, nil, nil, err
	}
	if detail == nil {
		return domain.AssetTimeline{}, nil, nil, fmt.Errorf("asset %s not found", assetID)
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
		return domain.AssetTimeline{}, nil, nil, err
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
		return timeline, map[string]string{}, map[string]bool{}, nil
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
	next := make(map[string]string)
	if limit > 0 {
		timeline.Shots, next["shots"], truncated["shots"] = paginateTimeline(timeline.Shots, limit, cursors["shots"],
			func(a, b domain.AssetShot) bool { return shotLess(a, b) },
			func(row domain.AssetShot) string { return timelineCursor(row.StartMS, row.EndMS, row.ID) })
		if !truncated["shots"] {
			delete(next, "shots")
		}
		timeline.Events, next["sound_events"], truncated["sound_events"] = paginateTimeline(timeline.Events, limit, cursors["sound_events"],
			func(a, b domain.AudioEventRow) bool { return eventLess(a, b) },
			func(row domain.AudioEventRow) string { return timelineCursor(row.StartMS, row.EndMS, row.ID) })
		if !truncated["sound_events"] {
			delete(next, "sound_events")
		}
		timeline.Turns, next["speaker_turns"], truncated["speaker_turns"] = paginateTimeline(timeline.Turns, limit, cursors["speaker_turns"],
			func(a, b domain.SpeakerTurnRow) bool { return turnLess(a, b) },
			func(row domain.SpeakerTurnRow) string { return timelineCursor(row.StartMS, row.EndMS, row.ID) })
		if !truncated["speaker_turns"] {
			delete(next, "speaker_turns")
		}
		timeline.Segments, next["transcript_segments"], truncated["transcript_segments"] = paginateTimeline(timeline.Segments, limit, cursors["transcript_segments"],
			func(a, b domain.TranscriptSegment) bool { return segmentLess(a, b) },
			func(row domain.TranscriptSegment) string { return timelineCursor(row.StartMS, row.EndMS, row.Text) })
		if !truncated["transcript_segments"] {
			delete(next, "transcript_segments")
		}
		timeline.Words, next["transcript_words"], truncated["transcript_words"] = paginateTimeline(timeline.Words, limit, cursors["transcript_words"],
			func(a, b domain.AlignmentWord) bool { return wordLess(a, b) },
			func(row domain.AlignmentWord) string { return timelineCursor(row.StartMS, row.EndMS, row.Text) })
		if !truncated["transcript_words"] {
			delete(next, "transcript_words")
		}
		timeline.VisualRefs, next["visual_refs"], truncated["visual_refs"] = paginateTimeline(timeline.VisualRefs, limit, cursors["visual_refs"],
			func(a, b domain.VisualRef) bool { return refLess(a, b) },
			func(row domain.VisualRef) string {
				return timelineCursor(row.StartMS, row.EndMS, fmt.Sprintf("%s:%d", row.ShotID, row.SampleIndex))
			})
		if !truncated["visual_refs"] {
			delete(next, "visual_refs")
		}
		timeline.AudioWindows, next["audio_windows"], truncated["audio_windows"] = paginateTimeline(timeline.AudioWindows, limit, cursors["audio_windows"],
			func(a, b domain.AudioWindowRow) bool { return audioWindowLess(a, b) },
			func(row domain.AudioWindowRow) string {
				return timelineCursor(row.StartMS, row.EndMS, row.GenerationID)
			})
		if !truncated["audio_windows"] {
			delete(next, "audio_windows")
		}
	}
	timeline.NextCursors = next
	return timeline, next, truncated, nil
}

// paginateTimeline sorts one track by its interval and stable tiebreak, skips
// rows at or before the supplied cursor, and returns the next cursor only when
// the page was truncated.
func paginateTimeline[T any](rows []T, limit int, cursor string, less func(a, b T) bool, key func(T) string) ([]T, string, bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		if less(rows[i], rows[j]) {
			return true
		}
		if less(rows[j], rows[i]) {
			return false
		}
		return key(rows[i]) < key(rows[j])
	})
	if cursor != "" {
		index := sort.Search(len(rows), func(i int) bool { return key(rows[i]) > cursor })
		rows = rows[index:]
	}
	if len(rows) <= limit {
		return rows, "", false
	}
	return rows[:limit], key(rows[limit-1]), true
}

func timelineCursor(startMS, endMS int64, tiebreak string) string {
	return fmt.Sprintf("%013d|%013d|%s", startMS, endMS, tiebreak)
}

func shotLess(a, b domain.AssetShot) bool {
	if a.StartMS != b.StartMS {
		return a.StartMS < b.StartMS
	}
	return a.EndMS < b.EndMS
}

func eventLess(a, b domain.AudioEventRow) bool {
	if a.StartMS != b.StartMS {
		return a.StartMS < b.StartMS
	}
	return a.EndMS < b.EndMS
}

func turnLess(a, b domain.SpeakerTurnRow) bool {
	if a.StartMS != b.StartMS {
		return a.StartMS < b.StartMS
	}
	return a.EndMS < b.EndMS
}

func segmentLess(a, b domain.TranscriptSegment) bool {
	if a.StartMS != b.StartMS {
		return a.StartMS < b.StartMS
	}
	return a.EndMS < b.EndMS
}

func wordLess(a, b domain.AlignmentWord) bool {
	if a.StartMS != b.StartMS {
		return a.StartMS < b.StartMS
	}
	return a.EndMS < b.EndMS
}

func refLess(a, b domain.VisualRef) bool {
	if a.StartMS != b.StartMS {
		return a.StartMS < b.StartMS
	}
	return a.EndMS < b.EndMS
}

func audioWindowLess(a, b domain.AudioWindowRow) bool {
	if a.StartMS != b.StartMS {
		return a.StartMS < b.StartMS
	}
	return a.EndMS < b.EndMS
}

func overlaps(startMS, endMS, viewStart, viewEnd int64) bool {
	return startMS < viewEnd && endMS > viewStart
}

// GetArtifactForAsset returns one derived artifact row. The waveform route
// needs the manifest path; containment is checked by the artifact's own cache
// location and by the reader's root check.
func (s *Service) GetArtifactForAsset(ctx context.Context, assetID, typ string) (*domain.DerivedArtifact, error) {
	return s.repo.GetArtifact(ctx, assetID, typ)
}
