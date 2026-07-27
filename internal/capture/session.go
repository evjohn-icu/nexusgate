package capture

import (
	"sort"
	"strings"
	"time"
)

// MaxAutomaticCaptureGap is the largest gap allowed for an implicit
// same-device shoot-session join.
const MaxAutomaticCaptureGap = 30 * time.Minute

// ShootSession is a deterministic group of source captures. Captures are
// sorted by capture time, with stable lexical tie-breakers.
type ShootSession struct {
	ID       string
	Identity CaptureIdentity
	Start    time.Time
	End      time.Time
	Markers  []string
	Captures []Capture
}

// AggregateSessions groups source captures into shoot sessions. A pair can
// join automatically only when a stable device ID or serial matches and the
// capture-time gap is at most MaxAutomaticCaptureGap. A shared explicit
// session, reel, or flight marker can join captures without that automatic
// rule. Proxy and sidecar observations are always excluded.
func AggregateSessions(captures []Capture) []ShootSession {
	eligible := make([]Capture, 0, len(captures))
	for _, capture := range captures {
		if isSessionExcluded(capture.Source) {
			continue
		}
		eligible = append(eligible, capture)
	}
	if len(eligible) == 0 {
		return nil
	}

	orderCaptures(eligible)
	sets := newDisjointSet(len(eligible))
	joinByExplicitMarkers(eligible, sets)
	joinByDeviceAndGap(eligible, sets)

	groups := make(map[int][]Capture, len(eligible))
	for index, capture := range eligible {
		root := sets.find(index)
		groups[root] = append(groups[root], capture)
	}

	sessions := make([]ShootSession, 0, len(groups))
	for _, group := range groups {
		orderCaptures(group)
		merged := Merge(group...)
		session := ShootSession{
			Identity: merged.Identity,
			Captures: append([]Capture(nil), group...),
			Markers:  sessionMarkers(group),
		}
		if len(group) > 0 {
			session.Start = group[0].Context.CapturedAt
			session.End = group[0].Context.CapturedAt
			for _, capture := range group[1:] {
				if capture.Context.CapturedAt.Before(session.Start) || session.Start.IsZero() {
					session.Start = capture.Context.CapturedAt
				}
				if captureEnd(capture).After(captureEnd(group[0])) {
					session.End = captureEnd(capture)
				}
			}
			if end := captureEnd(group[0]); end.After(session.End) {
				session.End = end
			}
		}
		session.ID = sessionID(group)
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Start.IsZero() != sessions[j].Start.IsZero() {
			return !sessions[i].Start.IsZero()
		}
		if !sessions[i].Start.Equal(sessions[j].Start) {
			return sessions[i].Start.Before(sessions[j].Start)
		}
		return sessions[i].ID < sessions[j].ID
	})
	return sessions
}

// AggregateShootSessions is a variadic convenience wrapper for callers that
// already have individual observations.
func AggregateShootSessions(captures ...Capture) []ShootSession {
	return AggregateSessions(captures)
}

func isSessionExcluded(source SourceKind) bool {
	return source == SourceProxy || source == SourceSidecar || source == SourceDerived
}

func orderCaptures(captures []Capture) {
	sort.SliceStable(captures, func(i, j int) bool {
		left, right := captures[i], captures[j]
		if left.Context.CapturedAt.IsZero() != right.Context.CapturedAt.IsZero() {
			return !left.Context.CapturedAt.IsZero()
		}
		if !left.Context.CapturedAt.Equal(right.Context.CapturedAt) {
			return left.Context.CapturedAt.Before(right.Context.CapturedAt)
		}
		return captureOrderKey(left) < captureOrderKey(right)
	})
}

func captureOrderKey(capture Capture) string {
	return strings.Join([]string{
		capture.ID,
		strings.ToLower(strings.TrimSpace(capture.Identity.DeviceSerial)),
		strings.ToLower(strings.TrimSpace(capture.Identity.DeviceID)),
		string(capture.Source),
		strings.ToLower(strings.TrimSpace(capture.Context.SourceID)),
		strings.ToLower(strings.TrimSpace(capture.Context.Filename)),
	}, "\x00")
}

func joinByExplicitMarkers(captures []Capture, sets *disjointSet) {
	markers := make(map[string]int)
	for index, capture := range captures {
		for _, marker := range captureMarkerKeys(capture) {
			if previous, ok := markers[marker]; ok {
				sets.union(index, previous)
			} else {
				markers[marker] = index
			}
		}
	}
}

func captureMarkerKeys(capture Capture) []string {
	markers := make([]string, 0, 3)
	for prefix, value := range map[string]string{
		"flight":  capture.Context.FlightMarker,
		"reel":    capture.Context.ReelMarker,
		"session": capture.Context.SessionMarker,
	} {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			markers = append(markers, prefix+":"+value)
		}
	}
	sort.Strings(markers)
	return markers
}

func joinByDeviceAndGap(captures []Capture, sets *disjointSet) {
	byKey := make(map[string][]int)
	for index, capture := range captures {
		for _, key := range stableDeviceKeys(capture.Identity) {
			byKey[key] = append(byKey[key], index)
		}
	}
	for _, indexes := range byKey {
		sort.Slice(indexes, func(i, j int) bool {
			left, right := captures[indexes[i]], captures[indexes[j]]
			if left.Context.CapturedAt.IsZero() != right.Context.CapturedAt.IsZero() {
				return !left.Context.CapturedAt.IsZero()
			}
			if !left.Context.CapturedAt.Equal(right.Context.CapturedAt) {
				return left.Context.CapturedAt.Before(right.Context.CapturedAt)
			}
			return captureOrderKey(left) < captureOrderKey(right)
		})
		for index := 1; index < len(indexes); index++ {
			previous, current := captures[indexes[index-1]], captures[indexes[index]]
			if withinAutomaticGap(previous.Context.CapturedAt, current.Context.CapturedAt) {
				sets.union(indexes[index-1], indexes[index])
			}
		}
	}
}

func stableDeviceKeys(identity CaptureIdentity) []string {
	keys := make([]string, 0, 2)
	if value := strings.ToLower(strings.TrimSpace(identity.DeviceSerial)); value != "" {
		keys = append(keys, "serial:"+value)
	}
	if value := strings.ToLower(strings.TrimSpace(identity.DeviceID)); value != "" {
		keys = append(keys, "id:"+value)
	}
	return keys
}

func withinAutomaticGap(left, right time.Time) bool {
	if left.IsZero() || right.IsZero() {
		return false
	}
	if right.Before(left) {
		left, right = right, left
	}
	return right.Sub(left) <= MaxAutomaticCaptureGap
}

func sessionMarkers(captures []Capture) []string {
	set := make(map[string]struct{})
	for _, capture := range captures {
		for _, marker := range []string{
			capture.Context.SessionMarker,
			capture.Context.ReelMarker,
			capture.Context.FlightMarker,
		} {
			if marker = strings.TrimSpace(marker); marker != "" {
				set[marker] = struct{}{}
			}
		}
	}
	markers := make([]string, 0, len(set))
	for marker := range set {
		markers = append(markers, marker)
	}
	sort.Strings(markers)
	return markers
}

func captureEnd(capture Capture) time.Time {
	if capture.Context.CapturedAt.IsZero() {
		return time.Time{}
	}
	if capture.Context.Duration > 0 {
		return capture.Context.CapturedAt.Add(capture.Context.Duration)
	}
	return capture.Context.CapturedAt
}

func sessionID(captures []Capture) string {
	if len(captures) == 0 {
		return ""
	}
	return captureOrderKey(captures[0])
}

type disjointSet struct {
	parent []int
	rank   []uint8
}

func newDisjointSet(size int) *disjointSet {
	parent := make([]int, size)
	for index := range parent {
		parent[index] = index
	}
	return &disjointSet{parent: parent, rank: make([]uint8, size)}
}

func (sets *disjointSet) find(value int) int {
	if sets.parent[value] == value {
		return value
	}
	sets.parent[value] = sets.find(sets.parent[value])
	return sets.parent[value]
}

func (sets *disjointSet) union(left, right int) {
	leftRoot, rightRoot := sets.find(left), sets.find(right)
	if leftRoot == rightRoot {
		return
	}
	if sets.rank[leftRoot] < sets.rank[rightRoot] {
		leftRoot, rightRoot = rightRoot, leftRoot
	}
	sets.parent[rightRoot] = leftRoot
	if sets.rank[leftRoot] == sets.rank[rightRoot] {
		sets.rank[leftRoot]++
	}
}
