package capture

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type fieldCandidate struct {
	field    string
	family   FieldFamily
	value    string
	evidence []Evidence
}

type fieldDefinition struct {
	field  string
	family FieldFamily
	read   func(Capture) (string, bool)
	write  func(*Capture, string)
}

// Merge deterministically combines capture observations. Precedence depends
// on the field family: original/embedded evidence is strongest for device
// identity, while explicit sidecar markers are allowed to override embedded
// capture markers. Every distinct losing value remains in Conflicts.
func Merge(observations ...CaptureObservation) MergedCapture {
	definitions := mergeFieldDefinitions()
	merged := MergedCapture{Provenance: Provenance{Fields: make(map[string]FieldProvenance)}}
	byField := make(map[string][]fieldCandidate, len(definitions))

	for _, observation := range observations {
		for _, definition := range definitions {
			value, ok := definition.read(observation)
			if !ok {
				continue
			}
			byField[definition.field] = append(byField[definition.field], fieldCandidate{
				field:    definition.field,
				family:   definition.family,
				value:    value,
				evidence: evidenceFor(observation, definition.field),
			})
		}
	}

	fields := make([]string, 0, len(byField))
	for field := range byField {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	allEvidence := make([]Evidence, 0)
	for _, field := range fields {
		candidates := collapseCandidates(byField[field])
		sort.Slice(candidates, func(i, j int) bool {
			return candidateBetter(candidates[i], candidates[j])
		})
		winner := candidates[0]
		for _, definition := range definitions {
			if definition.field == field {
				definition.write(&merged, winner.value)
				break
			}
		}

		observed := make([]Evidence, 0)
		for _, candidate := range candidates {
			observed = append(observed, candidate.evidence...)
			allEvidence = append(allEvidence, candidate.evidence...)
		}
		observed = uniqueEvidence(observed)
		selected := uniqueEvidence(winner.evidence)
		alternates := subtractEvidence(observed, selected)
		merged.Provenance.Fields[field] = FieldProvenance{
			Selected:   selected,
			Alternates: alternates,
			Observed:   observed,
		}
		if len(candidates) > 1 {
			conflict := Conflict{
				Field:    field,
				Family:   candidates[0].family,
				Selected: winner.value,
				Values:   make([]ConflictValue, 0, len(candidates)),
			}
			for _, candidate := range candidates {
				conflict.Values = append(conflict.Values, ConflictValue{
					Value:    candidate.value,
					Evidence: uniqueEvidence(candidate.evidence),
				})
			}
			sort.Slice(conflict.Values, func(i, j int) bool { return conflict.Values[i].Value < conflict.Values[j].Value })
			merged.Conflicts = append(merged.Conflicts, conflict)
		}
	}
	sort.Slice(merged.Conflicts, func(i, j int) bool { return merged.Conflicts[i].Field < merged.Conflicts[j].Field })
	merged.Evidence = uniqueEvidence(allEvidence)
	if len(merged.Evidence) == 0 {
		merged.Evidence = nil
	}
	if len(merged.Conflicts) == 0 {
		merged.Conflicts = nil
	}
	return merged
}

func mergeFieldDefinitions() []fieldDefinition {
	return []fieldDefinition{
		{FieldIdentityDeviceID, FieldFamilyIdentity, func(c Capture) (string, bool) { return present(c.Identity.DeviceID) }, func(c *Capture, v string) { c.Identity.DeviceID = v }},
		{FieldIdentityDeviceSerial, FieldFamilyIdentity, func(c Capture) (string, bool) { return present(c.Identity.DeviceSerial) }, func(c *Capture, v string) { c.Identity.DeviceSerial = v }},
		{FieldIdentityManufacturer, FieldFamilyIdentity, func(c Capture) (string, bool) { return present(c.Identity.Manufacturer) }, func(c *Capture, v string) { c.Identity.Manufacturer = v }},
		{FieldIdentityModel, FieldFamilyIdentity, func(c Capture) (string, bool) { return present(c.Identity.Model) }, func(c *Capture, v string) { c.Identity.Model = v }},
		{FieldIdentityLensModel, FieldFamilyIdentity, func(c Capture) (string, bool) { return present(c.Identity.LensModel) }, func(c *Capture, v string) { c.Identity.LensModel = v }},
		{FieldContextCapturedAt, FieldFamilyContext, func(c Capture) (string, bool) {
			if c.Context.CapturedAt.IsZero() {
				return "", false
			}
			return c.Context.CapturedAt.UTC().Format(time.RFC3339Nano), true
		}, func(c *Capture, v string) { c.Context.CapturedAt, _ = time.Parse(time.RFC3339Nano, v) }},
		{FieldContextDuration, FieldFamilyContext, func(c Capture) (string, bool) {
			if c.Context.Duration == 0 {
				return "", false
			}
			return strconv.FormatInt(int64(c.Context.Duration), 10), true
		}, func(c *Capture, v string) {
			n, _ := strconv.ParseInt(v, 10, 64)
			c.Context.Duration = time.Duration(n)
		}},
		{FieldContextFilename, FieldFamilyContext, func(c Capture) (string, bool) { return present(c.Context.Filename) }, func(c *Capture, v string) { c.Context.Filename = v }},
		{FieldContextSourceID, FieldFamilyContext, func(c Capture) (string, bool) { return present(c.Context.SourceID) }, func(c *Capture, v string) { c.Context.SourceID = v }},
		{FieldContextSessionMarker, FieldFamilyMarker, func(c Capture) (string, bool) { return present(c.Context.SessionMarker) }, func(c *Capture, v string) { c.Context.SessionMarker = v }},
		{FieldContextReelMarker, FieldFamilyMarker, func(c Capture) (string, bool) { return present(c.Context.ReelMarker) }, func(c *Capture, v string) { c.Context.ReelMarker = v }},
		{FieldContextFlightMarker, FieldFamilyMarker, func(c Capture) (string, bool) { return present(c.Context.FlightMarker) }, func(c *Capture, v string) { c.Context.FlightMarker = v }},
		{FieldSource, FieldFamilySource, func(c Capture) (string, bool) {
			if c.Source == "" {
				return "", false
			}
			return string(c.Source), true
		}, func(c *Capture, v string) { c.Source = SourceKind(v) }},
	}
}

func present(value string) (string, bool) {
	value = strings.TrimSpace(value)
	return value, value != ""
}

func evidenceFor(observation Capture, field string) []Evidence {
	if evidence := observation.FieldEvidence[field]; len(evidence) > 0 {
		return sortedEvidence(evidence)
	}
	if len(observation.Evidence) > 0 {
		return sortedEvidence(observation.Evidence)
	}
	priority := priorityForSource(observation.Source)
	source := string(observation.Source)
	if source == "" {
		source = string(SourceUnknown)
	}
	return []Evidence{{Source: source, Priority: priority}}
}

func priorityForSource(source SourceKind) SourcePriority {
	switch source {
	case SourceOriginal:
		return PriorityOriginal
	case SourceEmbedded:
		return PriorityEmbedded
	case SourceSidecar:
		return PrioritySidecar
	case SourceProxy:
		return PriorityProxy
	case SourceDerived:
		return PriorityDerived
	case SourceManual:
		return PriorityManual
	default:
		return PriorityUnknown
	}
}

func collapseCandidates(candidates []fieldCandidate) []fieldCandidate {
	byValue := make(map[string]int, len(candidates))
	collapsed := make([]fieldCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		index, ok := byValue[candidate.value]
		if !ok {
			byValue[candidate.value] = len(collapsed)
			candidate.evidence = uniqueEvidence(candidate.evidence)
			collapsed = append(collapsed, candidate)
			continue
		}
		collapsed[index].evidence = uniqueEvidence(append(collapsed[index].evidence, candidate.evidence...))
	}
	return collapsed
}

func candidateBetter(left, right fieldCandidate) bool {
	leftRank := bestEvidenceRank(left.family, left.evidence)
	rightRank := bestEvidenceRank(right.family, right.evidence)
	if leftRank != rightRank {
		return leftRank > rightRank
	}
	leftKey := candidateKey(left)
	rightKey := candidateKey(right)
	return leftKey < rightKey
}

func bestEvidenceRank(family FieldFamily, evidence []Evidence) int {
	best := -1
	for _, item := range evidence {
		rank := familyPrecedence(family, item.Priority)
		if rank > best {
			best = rank
		}
	}
	return best
}

// familyPrecedence is intentionally explicit. Identity should be anchored in
// the original/embedded media; context follows embedded capture metadata;
// explicit sidecar markers are strongest for marker fields.
func familyPrecedence(family FieldFamily, priority SourcePriority) int {
	orders := map[FieldFamily][]SourcePriority{
		FieldFamilyIdentity: {PriorityManual, PriorityOriginal, PriorityEmbedded, PrioritySidecar, PriorityFilename, PriorityProxy, PriorityDerived, PriorityUnknown},
		FieldFamilyContext:  {PriorityManual, PriorityEmbedded, PriorityOriginal, PrioritySidecar, PriorityFilename, PriorityProxy, PriorityDerived, PriorityUnknown},
		FieldFamilyMarker:   {PriorityManual, PrioritySidecar, PriorityEmbedded, PriorityOriginal, PriorityFilename, PriorityProxy, PriorityDerived, PriorityUnknown},
		FieldFamilySource:   {PriorityOriginal, PriorityEmbedded, PrioritySidecar, PriorityManual, PriorityProxy, PriorityDerived, PriorityFilename, PriorityUnknown},
	}
	order, ok := orders[family]
	if !ok {
		order = orders[FieldFamilyContext]
	}
	for rank, candidate := range order {
		if candidate == priority {
			return len(order) - rank
		}
	}
	return int(priority)
}

func candidateKey(candidate fieldCandidate) string {
	evidence := sortedEvidence(candidate.evidence)
	key := candidate.value
	for _, item := range evidence {
		key += "\x00" + evidenceKey(item)
	}
	return key
}

func sortedEvidence(evidence []Evidence) []Evidence {
	copyOf := append([]Evidence(nil), evidence...)
	sort.Slice(copyOf, func(i, j int) bool { return evidenceKey(copyOf[i]) < evidenceKey(copyOf[j]) })
	return copyOf
}

func uniqueEvidence(evidence []Evidence) []Evidence {
	if len(evidence) == 0 {
		return nil
	}
	sorted := sortedEvidence(evidence)
	unique := make([]Evidence, 0, len(sorted))
	for _, item := range sorted {
		if len(unique) > 0 && evidenceKey(unique[len(unique)-1]) == evidenceKey(item) {
			continue
		}
		unique = append(unique, item)
	}
	return unique
}

func subtractEvidence(all, selected []Evidence) []Evidence {
	selectedKeys := make(map[string]struct{}, len(selected))
	for _, item := range selected {
		selectedKeys[evidenceKey(item)] = struct{}{}
	}
	result := make([]Evidence, 0, len(all))
	for _, item := range all {
		if _, ok := selectedKeys[evidenceKey(item)]; !ok {
			result = append(result, item)
		}
	}
	return result
}

func evidenceKey(item Evidence) string {
	return fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s", item.Priority, item.Source, item.Locator, item.Note, item.ObservedAt.UTC().Format(time.RFC3339Nano))
}
