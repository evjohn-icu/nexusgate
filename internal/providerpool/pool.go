// Package providerpool selects enabled provider members for a capability while
// tracking local concurrency and transient provider health. It deliberately
// contains no transport, credentials, or persistence concerns.
package providerpool

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseCooldown = time.Second
	defaultMaxCooldown  = 30 * time.Second
)

// Capability identifies an independent provider operation channel.
type Capability string

// Member is one ordered provider route member. A member may serve more than
// one capability; its health and inflight state is tracked independently for
// each capability.
type Member struct {
	Name         string
	Capability   Capability
	Capabilities []Capability
	Enabled      bool
	// Weight biases how often the cursor starts on this member. Zero or negative
	// means one share.
	Weight int
	// MaxInflight caps concurrent leases for this member. It exists so one
	// provider can be configured with several keys that are rate limited
	// independently. Zero or negative means unlimited.
	MaxInflight int
}

// Provider is an alias for callers that prefer provider terminology.
type Provider = Member

// Options controls the clock and bounded cooldown policy. A zero Options uses
// a one-second initial cooldown capped at thirty seconds.
type Options struct {
	Now          func() time.Time
	BaseCooldown time.Duration
	MaxCooldown  time.Duration
}

// ErrNoAvailable means that no enabled member can currently accept work for a
// capability. It includes an empty/unknown capability route and a route whose
// members are all disabled, cooled, or already probing.
var ErrNoAvailable = errors.New("providerpool: no available member")

type memberState struct {
	member Member
	health map[Capability]*healthState
}

type healthState struct {
	inflight       int
	retryableFails int
	cooldownUntil  time.Time
	halfOpen       bool
}

// Pool owns ordered capability channels. The input order is retained as the
// route order for every capability, while each capability gets its own cursor
// and per-member health state.
type Pool struct {
	mu      sync.Mutex
	members []memberState
	routes  map[Capability][]int
	// schedule is routes expanded by member weight. Selection walks this so a
	// heavier member is reached from more cursor positions, while routes stays
	// deduplicated for callers that just want to see the route.
	schedule map[Capability][]int
	cursors  map[Capability]int
	options  Options
}

// Channel is a capability-bound view of a Pool. It prevents callers that
// already know their capability from accidentally selecting another route.
type Channel struct {
	pool       *Pool
	capability Capability
}

// CapabilityChannel is an alias for Channel.
type CapabilityChannel = Channel

// NewPool builds a pool from the ordered provider members.
func NewPool(members []Member, options ...Options) (*Pool, error) {
	if len(options) > 1 {
		return nil, fmt.Errorf("providerpool: at most one Options value is allowed")
	}
	configured := Options{}
	if len(options) == 1 {
		configured = options[0]
	}
	if configured.Now == nil {
		configured.Now = time.Now
	}
	if configured.BaseCooldown <= 0 {
		configured.BaseCooldown = defaultBaseCooldown
	}
	if configured.MaxCooldown <= 0 {
		configured.MaxCooldown = defaultMaxCooldown
	}
	if configured.MaxCooldown < configured.BaseCooldown {
		configured.MaxCooldown = configured.BaseCooldown
	}

	p := &Pool{
		members:  make([]memberState, len(members)),
		routes:   make(map[Capability][]int),
		schedule: make(map[Capability][]int),
		cursors:  make(map[Capability]int),
		options:  configured,
	}
	seen := make(map[Capability]map[string]struct{})
	for i, member := range members {
		member.Name = strings.TrimSpace(member.Name)
		if member.Name == "" {
			return nil, fmt.Errorf("providerpool: member %d has an empty name", i)
		}
		member.Capability = normalizeCapability(member.Capability)
		member.Capabilities = normalizeCapabilities(member.Capability, member.Capabilities)
		if len(member.Capabilities) == 0 {
			return nil, fmt.Errorf("providerpool: member %q has no capabilities", member.Name)
		}

		weight := member.Weight
		if weight < 1 {
			weight = 1
		}

		p.members[i] = memberState{
			member: member,
			health: make(map[Capability]*healthState, len(member.Capabilities)),
		}
		for _, capability := range member.Capabilities {
			if seen[capability] == nil {
				seen[capability] = make(map[string]struct{})
			}
			if _, duplicate := seen[capability][member.Name]; duplicate {
				return nil, fmt.Errorf("providerpool: member %q appears twice in capability %q route", member.Name, capability)
			}
			seen[capability][member.Name] = struct{}{}
			p.routes[capability] = append(p.routes[capability], i)
			for share := 0; share < weight; share++ {
				p.schedule[capability] = append(p.schedule[capability], i)
			}
			p.members[i].health[capability] = &healthState{}
		}
	}
	return p, nil
}

// New is a short constructor alias for NewPool.
func New(members []Member, options ...Options) (*Pool, error) {
	return NewPool(members, options...)
}

// Route returns the ordered members that serve capability. The returned
// members and their capability slices are copies.
func (p *Pool) Route(capability Capability) []Member {
	if p == nil {
		return nil
	}
	capability = normalizeCapability(capability)
	p.mu.Lock()
	defer p.mu.Unlock()
	indices := p.routes[capability]
	result := make([]Member, 0, len(indices))
	for _, index := range indices {
		member := p.members[index].member
		member.Capabilities = append([]Capability(nil), member.Capabilities...)
		result = append(result, member)
	}
	return result
}

// Members is an alias for Route.
func (p *Pool) Members(capability Capability) []Member {
	return p.Route(capability)
}

// Channel returns a capability-bound selector for an existing route.
func (p *Pool) Channel(capability Capability) (*Channel, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrNoAvailable)
	}
	capability = normalizeCapability(capability)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.routes[capability]) == 0 {
		return nil, fmt.Errorf("%w: capability %q", ErrNoAvailable, capability)
	}
	return &Channel{pool: p, capability: capability}, nil
}

// SetEnabled changes whether a member can be selected. Health state is kept so
// disabling and re-enabling a member does not erase an active cooldown.
func (p *Pool) SetEnabled(name string, enabled bool) bool {
	if p == nil {
		return false
	}
	name = strings.TrimSpace(name)
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	for i := range p.members {
		if p.members[i].member.Name == name {
			p.members[i].member.Enabled = enabled
			changed = true
		}
	}
	return changed
}

// EnabledState reports whether the pool would currently select a member, and
// whether it knows the name at all. It exists so a status view can show an
// operator a member the pool retired at runtime: no configuration row records
// that, so without this the UI keeps calling a dead key "enabled".
func (p *Pool) EnabledState(name string) (enabled bool, known bool) {
	if p == nil {
		return false, false
	}
	name = strings.TrimSpace(name)
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.members {
		if p.members[i].member.Name == name {
			return p.members[i].member.Enabled, true
		}
	}
	return false, false
}

// MemberHealth is the observable, secret-free health state of one member for
// one capability. A zero CooldownUntil means the member is not cooling.
type MemberHealth struct {
	CooldownUntil time.Time
	HalfOpen      bool
	Inflight      int
	// RetryableFails counts consecutive Retryable failures since the member
	// last recovered (a successful call, a non-retryable failure, or a
	// completed half-open probe all reset it — see Pool.complete). It is what
	// lets a caller tell one unlucky call from a cooldown that has already
	// survived a second, freshly-issued attempt: the second failure can only
	// happen once the first cooldown has actually elapsed and Select tried
	// the member again, so a count of two or more is evidence gathered over
	// real wall-clock time, not a guess from a single sample.
	RetryableFails int
}

// HealthState reports the health of one member for one capability. The bool
// reports whether the pool knows the member name at all; a known member that
// does not serve capability yields a zero state. It exists so a status view
// can show an operator why a key configuration still enables is not being
// selected, without the view reaching into pool internals.
func (p *Pool) HealthState(name string, capability Capability) (MemberHealth, bool) {
	if p == nil {
		return MemberHealth{}, false
	}
	name = strings.TrimSpace(name)
	capability = normalizeCapability(capability)
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.members {
		if p.members[i].member.Name == name {
			health := p.members[i].health[capability]
			if health == nil {
				return MemberHealth{}, true
			}
			return MemberHealth{CooldownUntil: health.cooldownUntil, HalfOpen: health.halfOpen, Inflight: health.inflight, RetryableFails: health.retryableFails}, true
		}
	}
	return MemberHealth{}, false
}

// Selectable reports whether the pool would select the member for capability
// right now: enabled, not cooling, and not a half-open probe already in
// flight. It is Select's eligibility rule exposed as a yes/no, so a status
// view can distinguish "the operator enabled this key" from "the route would
// actually accept a call on it" — the two diverge exactly while a member
// cools, probes, or has been retired for a spent key.
func (p *Pool) Selectable(name string, capability Capability) bool {
	if p == nil {
		return false
	}
	name = strings.TrimSpace(name)
	capability = normalizeCapability(capability)
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.members {
		if p.members[i].member.Name == name {
			return p.members[i].member.Enabled && eligible(p.members[i].member, p.members[i].health[capability], p.options.Now())
		}
	}
	return false
}

// Select acquires one member for capability and increments its inflight count.
// The caller must complete the returned lease with Done or Release.
func (p *Pool) Select(capability Capability) (*Lease, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrNoAvailable)
	}
	capability = normalizeCapability(capability)
	p.mu.Lock()
	defer p.mu.Unlock()

	route := p.schedule[capability]
	if len(route) == 0 {
		return nil, fmt.Errorf("%w: capability %q", ErrNoAvailable, capability)
	}
	now := p.options.Now()
	start := p.cursors[capability] % len(route)
	selectedRoutePosition := -1
	selectedMemberIndex := -1
	leastInflight := int(^uint(0) >> 1)
	for offset := 0; offset < len(route); offset++ {
		routePosition := (start + offset) % len(route)
		memberIndex := route[routePosition]
		member := &p.members[memberIndex]
		health := member.health[capability]
		if !member.member.Enabled || !eligible(member.member, health, now) {
			continue
		}
		if health.inflight < leastInflight {
			leastInflight = health.inflight
			selectedRoutePosition = routePosition
			selectedMemberIndex = memberIndex
		}
	}
	if selectedMemberIndex < 0 {
		return nil, fmt.Errorf("%w: capability %q", ErrNoAvailable, capability)
	}

	health := p.members[selectedMemberIndex].health[capability]
	wasProbe := !health.cooldownUntil.IsZero() && !now.Before(health.cooldownUntil)
	if wasProbe {
		health.halfOpen = true
	}
	health.inflight++
	p.cursors[capability] = (selectedRoutePosition + 1) % len(route)
	return &Lease{
		pool:       p,
		member:     selectedMemberIndex,
		capability: capability,
		probe:      wasProbe,
	}, nil
}

// Acquire is an alias for Select.
func (p *Pool) Acquire(capability Capability) (*Lease, error) {
	return p.Select(capability)
}

// Select acquires a member from the channel.
func (c *Channel) Select() (*Lease, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: nil channel", ErrNoAvailable)
	}
	return c.pool.Select(c.capability)
}

// Acquire is an alias for Channel.Select.
func (c *Channel) Acquire() (*Lease, error) {
	return c.Select()
}

// Route returns the channel's ordered route.
func (c *Channel) Route() []Member {
	if c == nil {
		return nil
	}
	return c.pool.Route(c.capability)
}

func eligible(member Member, health *healthState, now time.Time) bool {
	if health == nil || health.inflight > 0 && health.halfOpen {
		return false
	}
	// A saturated member is skipped rather than queued: Select never blocks, so
	// the caller either falls back to the next member or gets ErrNoAvailable.
	if member.MaxInflight > 0 && health.inflight >= member.MaxInflight {
		return false
	}
	if health.cooldownUntil.IsZero() {
		return true
	}
	if health.halfOpen {
		return health.inflight == 0
	}
	return !now.Before(health.cooldownUntil) && health.inflight == 0
}

func (p *Pool) complete(lease *Lease, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	member := &p.members[lease.member]
	health := member.health[lease.capability]
	if health.inflight > 0 {
		health.inflight--
	}

	switch ClassifyFailure(err) {
	case Retryable:
		health.retryableFails++
		delay := p.cooldownFor(health.retryableFails)
		health.cooldownUntil = p.options.Now().Add(delay)
		health.halfOpen = false
		return
	case MemberSpent:
		// A cooldown is a bet that the same key works later; a spent key never
		// does, so it is retired instead — the same state SetEnabled produces,
		// including keeping health untouched. The retirement covers every
		// capability the member serves because it is the credential that died,
		// not one route through it.
		//
		// It lives in memory only. A revoked key and an hour-long IP block look
		// identical from here, and this project lets humans, not guesses, make
		// durable decisions: editing the channel or restarting the Hub rebuilds
		// the pool and gives the member one more call to prove itself. Until
		// then the route reports itself exhausted rather than quietly failing
		// jobs.
		member.member.Enabled = false
		return
	}

	// A concurrent request must not erase a cooldown established by another
	// retryable failure. A completed half-open probe, however, owns the state
	// transition and can restore the member immediately on success or a
	// permanent failure.
	if lease.probe || health.cooldownUntil.IsZero() {
		health.retryableFails = 0
		health.cooldownUntil = time.Time{}
		health.halfOpen = false
	}
}

func (p *Pool) cooldownFor(failures int) time.Duration {
	delay := p.options.BaseCooldown
	for i := 1; i < failures && delay < p.options.MaxCooldown; i++ {
		if delay > p.options.MaxCooldown/2 {
			return p.options.MaxCooldown
		}
		delay *= 2
	}
	if delay > p.options.MaxCooldown {
		return p.options.MaxCooldown
	}
	return delay
}

func normalizeCapability(capability Capability) Capability {
	return Capability(strings.TrimSpace(string(capability)))
}

func normalizeCapabilities(singular Capability, capabilities []Capability) []Capability {
	result := make([]Capability, 0, len(capabilities)+1)
	seen := make(map[Capability]struct{}, len(capabilities)+1)
	appendCapability := func(capability Capability) {
		capability = normalizeCapability(capability)
		if capability == "" {
			return
		}
		if _, exists := seen[capability]; exists {
			return
		}
		seen[capability] = struct{}{}
		result = append(result, capability)
	}
	appendCapability(singular)
	for _, capability := range capabilities {
		appendCapability(capability)
	}
	return result
}

// Lease is one inflight selection. It is safe to complete a lease more than
// once; only the first completion changes pool state.
type Lease struct {
	once       sync.Once
	pool       *Pool
	member     int
	capability Capability
	probe      bool
}

// Member returns the selected provider member.
func (l *Lease) Member() Member {
	if l == nil || l.pool == nil {
		return Member{}
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	member := l.pool.members[l.member].member
	member.Capabilities = append([]Capability(nil), member.Capabilities...)
	return member
}

// Name returns the selected member name.
func (l *Lease) Name() string {
	return l.Member().Name
}

// Capability returns the capability channel used by this lease.
func (l *Lease) Capability() Capability {
	if l == nil {
		return ""
	}
	return l.capability
}

// Done reports the operation result and releases the inflight slot.
func (l *Lease) Done(err error) {
	if l == nil || l.pool == nil {
		return
	}
	l.once.Do(func() { l.pool.complete(l, err) })
}

// Release is an alias for Done.
func (l *Lease) Release(err error) {
	l.Done(err)
}
