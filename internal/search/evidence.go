package search

import (
	"strings"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/textindex"
)

// Evidence is the honest bridge between retrieval and assertion. A semantic
// similarity of 0.92 does not mean the shot contains the thing — only
// shot-level observation fields can claim that. This file computes, per
// constraint, how much support a candidate actually has, and attributes it to
// the source fields so a consumer can see WHY a shot matched.
//
// The critical distinction is missing evidence vs negative evidence: a shot
// whose text never mentions "person" has UNKNOWN person evidence, not "no
// person". Absence is never asserted from silence. The one place absence IS
// asserted is the shot's own words: "empty beach no people" explicitly
// negates person — that is contradicted evidence for a positive person query
// and supporting (unknown-as-observed) evidence for a no-person query.

// evaluateConstraints computes evidence for one candidate against the query's
// constraints. transcriptSpans are the aligned words overlapping the shot;
// pass nil to skip the transcript source. Constraints are evaluated in
// Must, Should, MustNot order.
func evaluateConstraints(q SearchQuery, c Candidate, transcriptSpans []domain.AlignmentWord) []Evidence {
	out := make([]Evidence, 0, len(q.Must)+len(q.Should)+len(q.MustNot))
	seen := map[string]bool{}
	var transcriptText string
	for _, w := range transcriptSpans {
		transcriptText += " " + w.Text
	}
	appendEvidence := func(constraint Constraint, negated bool) {
		if seen[constraint.Value] {
			return
		}
		seen[constraint.Value] = true
		out = append(out, evaluateConstraint(constraint, negated, c, transcriptText))
	}
	for _, constraint := range q.Must {
		appendEvidence(constraint, false)
	}
	for _, constraint := range q.Should {
		appendEvidence(constraint, false)
	}
	for _, constraint := range q.MustNot {
		appendEvidence(constraint, true)
	}
	return out
}

// evaluateConstraint scores one constraint against one candidate.
//
// Positive constraints: structured fields (objects/actions/tags/mood) confirm
// (the model directly observed it); description-only and transcript-only are
// "possible" (mention is not visual proof); an explicit absence phrase aimed
// at the canonical ("no people", "没有人") is "contradicted".
//
// Negated (MustNot) constraints read the other way: structured evidence means
// the forbidden thing WAS observed (the gate excludes the shot); description
// mention without negation is weak observation (also excluded); an explicit
// absence phrase is the absence being stated, so it supports keeping the shot
// — the API reports unknown, never "确认无人".
func evaluateConstraint(constraint Constraint, negated bool, c Candidate, transcriptText string) Evidence {
	e := Evidence{Constraint: constraint.Type, Value: constraint.Value, Negated: negated}
	value := constraint.Value

	fieldHits := map[EvidenceSource]bool{}
	structured := []struct {
		source EvidenceSource
		values []string
	}{
		{SourceObjects, c.Objects},
		{SourceActions, c.Actions},
		{SourceTags, c.Tags},
		{SourceMood, c.Mood},
	}
	for _, field := range structured {
		for _, v := range field.values {
			if matchesCanonical(value, v) {
				fieldHits[field.source] = true
			}
		}
	}
	if len(fieldHits) > 0 {
		if matchesCanonical(value, c.Description) {
			fieldHits[SourceDescription] = true
		}
		e.State = EvidenceConfirmed
		for source := range fieldHits {
			e.Sources = append(e.Sources, source)
		}
		return e
	}

	descriptionHas := matchesCanonical(value, c.Description)
	descriptionNegates := canonicalNegatedIn(c.Description, value)

	if negated {
		// For a MustNot constraint, absence text supports the negative.
		if descriptionHas && !descriptionNegates {
			e.State = EvidencePossible
			e.Sources = []EvidenceSource{SourceDescription}
			return e
		}
		e.State = EvidenceUnknown
		return e
	}

	if descriptionHas {
		if descriptionNegates {
			e.State = EvidenceContradicted
			return e
		}
		e.State = EvidencePossible
		e.Sources = []EvidenceSource{SourceDescription}
		return e
	}

	if transcriptText != "" && transcriptMentions(constraint, transcriptText) {
		e.State = EvidencePossible
		e.Sources = []EvidenceSource{SourceTranscript}
		return e
	}
	e.State = EvidenceUnknown
	return e
}

// transcriptMentions checks whether the overlapping aligned words support the
// constraint. A speech constraint matches when at least one token of the
// claimed phrase is present; any other canonical matches via its canonical
// family (speech is "possible", never "confirmed" — speech is mention, not
// visual observation).
func transcriptMentions(constraint Constraint, transcriptText string) bool {
	if constraint.Type == ConstraintSpeech {
		for _, token := range textindex.Tokens(constraint.Value) {
			if token == "" {
				continue
			}
			if hasCJK(token) {
				if strings.Contains(transcriptText, token) {
					return true
				}
				continue
			}
			if asciiWordSet(transcriptText)[token] {
				return true
			}
		}
		return false
	}
	return matchesCanonical(constraint.Value, transcriptText)
}

// matchesCanonical reports whether a field text carries the canonical term.
// Both sides are canonicalized so the alias bridge holds (objects ["vehicle"]
// and query "car" share the "car" canonical), and the whole-word discipline
// is inherited from vocabulary's pass-through: "carefree" can never confirm
// "car".
func matchesCanonical(canonical, fieldText string) bool {
	if fieldText == "" {
		return false
	}
	lower := strings.ToLower(fieldText)
	for _, m := range Canonicalize(lower) {
		if m.Canonical == canonical {
			return true
		}
	}
	// Fall back to whole-word presence for terms Canonicalize skipped (e.g.
	// CJK runs longer than 3 runes in a sentence).
	if !hasCJK(canonical) {
		return asciiWordSet(lower)[canonical]
	}
	return strings.Contains(lower, canonical)
}

// canonicalNegatedIn reports whether the field text explicitly negates the
// canonical — an absence marker whose window covers the canonical's surface
// word ("no people", "没有人"). Positional, so "empty beach no people" does
// not negate "beach".
func canonicalNegatedIn(text string, canonical string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	position := canonicalSurfacePosition(lower, canonical)
	if position < 0 {
		return false
	}
	for _, span := range NegationSpans(lower) {
		if Negates(span, ConstraintObject, position) {
			return true
		}
	}
	return false
}

// canonicalSurfacePosition finds the earliest surface word position of a
// canonical in the text (byte offset), or -1. It mirrors Canonicalize's
// matching: CJK words substring, ASCII words whole-word.
func canonicalSurfacePosition(text, canonical string) int {
	ascii := asciiWordSet(text)
	best := -1
	for _, family := range vocabulary {
		if family.canonical != canonical {
			continue
		}
		for _, word := range family.words {
			if hasCJK(word) {
				if idx := strings.Index(text, word); idx >= 0 && (best < 0 || idx < best) {
					best = idx
				}
				continue
			}
			if ascii[word] {
				if pos := wordPosition(text, word); pos >= 0 && (best < 0 || pos < best) {
					best = pos
				}
			}
		}
	}
	if best < 0 && !hasCJK(canonical) && len([]rune(canonical)) >= 3 {
		if ascii[canonical] {
			best = wordPosition(text, canonical)
		}
	}
	return best
}

// gateEvidence is the verdict the gate needs from the evidence set.
type gateEvidence struct {
	confirmedMusts  int
	possibleMusts   int
	totalMusts      int
	contradicted    bool
	observedMustNot bool
}

// evidenceForGate extracts the gate-relevant counts from the evidence.
func evidenceForGate(q SearchQuery, evidence []Evidence) gateEvidence {
	g := gateEvidence{}
	for _, e := range evidence {
		if constraintIn(q.Must, e.Value) != nil && !e.Negated {
			g.totalMusts++
			switch e.State {
			case EvidenceConfirmed:
				g.confirmedMusts++
			case EvidencePossible:
				g.possibleMusts++
			case EvidenceContradicted:
				g.contradicted = true
			}
		}
		if e.Negated && constraintIn(q.MustNot, e.Value) != nil {
			switch e.State {
			case EvidenceConfirmed, EvidencePossible:
				// The forbidden thing was observed in this shot.
				g.observedMustNot = true
			}
		}
	}
	return g
}

func constraintIn(constraints []Constraint, value string) *Constraint {
	for i := range constraints {
		if constraints[i].Value == value {
			return &constraints[i]
		}
	}
	return nil
}
