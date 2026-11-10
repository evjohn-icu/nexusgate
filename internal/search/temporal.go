package search

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// TimeInterval and Interval are asset-relative half-open millisecond ranges.
type TimeInterval = domain.TimeInterval

const (
	RelationSimultaneous = "simultaneous"
	RelationBefore       = "before"
	RelationAfter        = "after"
	RelationDuring       = "during"
	RelationStartsWithin = "starts_within"
	RelationEndsWithin   = "ends_within"

	EvidenceStatusConfirmed EvidenceStatus = "confirmed"
	EvidenceStatusPossible  EvidenceStatus = "possible"

	// DefaultTemporalMaxGapMS is the explicit 5-second gap used when a
	// recognized before/after template does not specify a bound.
	DefaultTemporalMaxGapMS int64 = 5000
)

// EvidenceStatus is the temporal assertion strength, distinct from retrieval
// score and from the legacy shot-level EvidenceState.
type EvidenceStatus string

// Evidence sources used by multimodal and temporal observations. The existing
// search package already defines SourceTranscript and SourceVisual.
const (
	SourceLexical    EvidenceSource = "lexical"
	SourceSoundEvent EvidenceSource = "sound_event"
	SourceSpeaker    EvidenceSource = "speaker"
)

// ObservedAt is one record's evidence at a specific interval. A record may be
// called canonical timed evidence only when the upstream canonical record has
// precise timing; model scores and shot-wide labels should be marked possible.
type ObservedAt struct {
	RecordID                  string         `json:"record_id"`
	Modality                  string         `json:"modality"`
	Interval                  TimeInterval   `json:"interval"`
	CanonicalTimedObservation bool           `json:"canonical_timed_observation"`
	EvidenceStatus            EvidenceStatus `json:"evidence_status,omitempty"`
	EvidenceSource            EvidenceSource `json:"evidence_source,omitempty"`
}

// PredicateResult retains the computed relation and its supporting intervals
// together with the strongest status and source attribution available.
type PredicateResult struct {
	PredicateID      string           `json:"predicate_id,omitempty"`
	Relation         string           `json:"relation"`
	Matched          bool             `json:"matched"`
	SupportIntervals []Interval       `json:"support_intervals"`
	EvidenceStatus   EvidenceStatus   `json:"evidence_status"`
	EvidenceSource   EvidenceSource   `json:"evidence_source"`
	EvidenceSources  []EvidenceSource `json:"evidence_sources,omitempty"`
}

func validTemporalRelation(relation string) bool {
	switch relation {
	case RelationSimultaneous, RelationBefore, RelationAfter, RelationDuring, RelationStartsWithin, RelationEndsWithin:
		return true
	default:
		return false
	}
}

// EvaluatePredicate applies the exact half-open temporal relation. Touching
// intervals are not simultaneous; before/after use the requested gap only for
// their distance check and never expand overlap.
func EvaluatePredicate(relation string, left, right TimeInterval, maxGapMS int64) (bool, error) {
	if !validTemporalRelation(relation) {
		return false, fmt.Errorf("search: unknown temporal relation %q", relation)
	}
	if maxGapMS < 0 {
		return false, errors.New("search: temporal max gap must not be negative")
	}
	if (relation == RelationBefore || relation == RelationAfter) && maxGapMS == 0 {
		return false, fmt.Errorf("search: temporal relation %q requires max gap > 0", relation)
	}
	if err := validateInterval(left); err != nil {
		return false, fmt.Errorf("search: invalid left interval: %w", err)
	}
	if err := validateInterval(right); err != nil {
		return false, fmt.Errorf("search: invalid right interval: %w", err)
	}

	switch relation {
	case RelationSimultaneous:
		return left.StartMS < right.EndMS && left.EndMS > right.StartMS, nil
	case RelationBefore:
		return left.EndMS <= right.StartMS && right.StartMS-left.EndMS <= maxGapMS, nil
	case RelationAfter:
		return right.EndMS <= left.StartMS && left.StartMS-right.EndMS <= maxGapMS, nil
	case RelationDuring:
		return right.StartMS <= left.StartMS && left.EndMS <= right.EndMS, nil
	case RelationStartsWithin:
		return right.StartMS <= left.StartMS && left.StartMS < right.EndMS, nil
	case RelationEndsWithin:
		return right.StartMS < left.EndMS && left.EndMS <= right.EndMS, nil
	default:
		panic("unreachable temporal relation")
	}
}

func validateInterval(interval TimeInterval) error {
	if interval.StartMS < 0 {
		return errors.New("start is negative")
	}
	if interval.EndMS <= interval.StartMS {
		return errors.New("interval is empty or reversed")
	}
	return nil
}

// Intersect returns the non-empty intersection of two half-open intervals.
func Intersect(a, b TimeInterval) (Interval, bool) {
	return a.Intersect(b)
}

// SupportIntervals returns only the intervals that support a true predicate.
// Overlap-like relations are supported by their intersection; before/after
// claims retain both complete operand intervals.
func SupportIntervals(predicate TemporalPredicate, left, right TimeInterval) []Interval {
	matched, err := EvaluatePredicate(predicate.Relation, left, right, predicate.MaxGapMS)
	if err != nil || !matched {
		return nil
	}
	switch predicate.Relation {
	case RelationBefore, RelationAfter:
		return []Interval{left, right}
	case RelationSimultaneous, RelationDuring, RelationStartsWithin, RelationEndsWithin:
		intersection, ok := Intersect(left, right)
		if !ok {
			return nil
		}
		return []Interval{intersection}
	default:
		return nil
	}
}

// ClassifyPredicateEvidence returns confirmed only when both inputs are
// canonical timed observations and neither operand carries a possible status.
// A shot-wide label or uncalibrated model therefore remains possible even when
// its interval happens to overlap exactly.
func ClassifyPredicateEvidence(left, right ObservedAt) (EvidenceStatus, EvidenceSource, []EvidenceSource) {
	confirmed := canonicalTimed(left) && canonicalTimed(right)
	status := EvidenceStatusPossible
	if confirmed {
		status = EvidenceStatusConfirmed
	}
	leftSource := observationSource(left)
	rightSource := observationSource(right)
	sources := make([]EvidenceSource, 0, 2)
	if leftSource != "" {
		sources = append(sources, leftSource)
	}
	if rightSource != "" && rightSource != leftSource {
		sources = append(sources, rightSource)
	}
	primary := leftSource
	if primary == "" {
		primary = rightSource
	}
	return status, primary, sources
}

func canonicalTimed(observed ObservedAt) bool {
	return observed.CanonicalTimedObservation && observed.EvidenceStatus == EvidenceStatusConfirmed
}

func observationSource(observed ObservedAt) EvidenceSource {
	if observed.EvidenceSource != "" {
		return observed.EvidenceSource
	}
	switch observed.Modality {
	case ModalityLexical:
		return SourceLexical
	case ModalityTranscript:
		return SourceTranscript
	case ModalitySoundEvent:
		return SourceSoundEvent
	case ModalitySpeaker:
		return SourceSpeaker
	case ModalityVisual:
		return SourceVisual
	default:
		return ""
	}
}

// EvaluateObservedPredicate evaluates a predicate and carries its evidence
// classification and supporting interval records into the result.
func EvaluateObservedPredicate(predicate TemporalPredicate, left, right ObservedAt) (PredicateResult, error) {
	matched, err := EvaluatePredicate(predicate.Relation, left.Interval, right.Interval, predicate.MaxGapMS)
	if err != nil {
		return PredicateResult{}, err
	}
	status, source, sources := ClassifyPredicateEvidence(left, right)
	result := PredicateResult{
		PredicateID:     predicate.ID,
		Relation:        predicate.Relation,
		Matched:         matched,
		EvidenceStatus:  status,
		EvidenceSource:  source,
		EvidenceSources: sources,
	}
	if matched {
		result.SupportIntervals = SupportIntervals(predicate, left.Interval, right.Interval)
	}
	return result, nil
}

// CompileTemporalQuery recognizes only the listed exact template shapes.
// Unrecognized grammar returns (nil, false) so callers can retain ordinary
// search rather than inventing a temporal constraint.
func CompileTemporalQuery(query string) (*TemporalQuery, bool) {
	matched := strings.TrimSpace(query)
	if matched == "" {
		return nil, false
	}
	for _, template := range temporalTemplates {
		parts := template.pattern.FindStringSubmatch(matched)
		if len(parts) != 3 {
			continue
		}
		left := strings.TrimSpace(parts[1])
		right := strings.TrimSpace(parts[2])
		if left == "" || right == "" {
			return nil, false
		}
		maxGapMS := int64(0)
		if template.relation == RelationBefore || template.relation == RelationAfter {
			maxGapMS = DefaultTemporalMaxGapMS
		}
		return &TemporalQuery{
			Conditions: []TemporalCondition{
				{ID: "left", Modality: ModalityLexical, Query: left},
				{ID: "right", Modality: ModalityLexical, Query: right},
			},
			Predicates: []TemporalPredicate{{
				ID:       "relation_1",
				Relation: template.relation,
				LeftID:   "left",
				RightID:  "right",
				MaxGapMS: maxGapMS,
			}},
			Interpretation: matched,
		}, true
	}
	return nil, false
}

type temporalTemplate struct {
	pattern  *regexp.Regexp
	relation string
}

var temporalTemplates = []temporalTemplate{
	{regexp.MustCompile(`(?i)^(.+?)\s+and\s+(.+?)\s+(?:happen\s+)?at\s+the\s+same\s+time$`), RelationSimultaneous},
	{regexp.MustCompile(`(?i)^(.+?)\s+while\s+(.+?)$`), RelationSimultaneous},
	{regexp.MustCompile(`^(.+?)\s*和\s*(.+?)\s*同时出现$`), RelationSimultaneous},
	{regexp.MustCompile(`(?i)^(.+?)\s+before\s+(.+?)$`), RelationBefore},
	{regexp.MustCompile(`^(.+?)\s*在\s*(.+?)\s*之前出现$`), RelationBefore},
	{regexp.MustCompile(`(?i)^(.+?)\s+after\s+(.+?)$`), RelationAfter},
	{regexp.MustCompile(`^(.+?)\s*之后出现\s*(.+?)$`), RelationAfter},
}
