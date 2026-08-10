package providerchannels

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evjohn-icu/timingdex/internal/providerpool"
)

var (
	// ErrNoRoute means that a capability has no enabled/available provider
	// route.
	ErrNoRoute = errors.New("providerchannels: no available route")
	// ErrInvalidOperation means the caller did not provide a supported
	// operation function.
	ErrInvalidOperation = errors.New("providerchannels: invalid operation")
	// ErrRouteExhausted means every enabled member on a capability's route is
	// failing retryably — not one call that went wrong, but no key left to try.
	// It is a sentinel so callers can match it with errors.Is instead of
	// reading error text; app.Pipeline already classifies several failure modes
	// by substring and this must not join them.
	ErrRouteExhausted = errors.New("providerchannels: every provider key on this route is failing")
)

// attemptsPerChannel is how many members of one channel Execute will try
// before moving to the next ordered provider route. Three is the operator-
// facing promise ("every key gets three tries"): a channel is the unit an
// operator configures their keys into, so the count is per channel rather
// than per route.
const attemptsPerChannel = 3

// Invocation is the non-secret context passed to a caller operation. A
// caller can use SecretRef to resolve a Hub-side secret; no secret value is
// present here and this package never sends one to a worker.
type Invocation struct {
	Capability     Capability `json:"capability"`
	ChannelID      string     `json:"channel_id"`
	ChannelLabel   string     `json:"channel_label,omitempty"`
	Provider       string     `json:"provider,omitempty"`
	ProviderName   string     `json:"provider_name"`
	Protocol       string     `json:"protocol,omitempty"`
	Endpoint       string     `json:"endpoint,omitempty"`
	Path           string     `json:"path,omitempty"`
	Model          string     `json:"model,omitempty"`
	AuthHeader     string     `json:"auth_header,omitempty"`
	AuthScheme     string     `json:"auth_scheme,omitempty"`
	TimeoutSeconds int        `json:"timeout_seconds,omitempty"`
	MemberID       string     `json:"member_id"`
	MemberLabel    string     `json:"member_label,omitempty"`
	SecretRef      string     `json:"secret_ref,omitempty"`
}

// Operation is the preferred operation shape.
type Operation func(context.Context, Invocation) error

// MemberOperation is a convenience for callers that want the complete public
// member metadata rather than a separate invocation value.
type MemberOperation func(context.Context, Member) error

// IDSecretOperation is a convenience for callers that need only the selected
// member ID and Hub-side secret reference.
type IDSecretOperation func(context.Context, string, string) error

// Options is an alias for providerpool.Options so callers can inject a clock
// and cooldown policy into all channel-local pools.
type Options = providerpool.Options

type channelRuntime struct {
	channel  Channel
	pool     *providerpool.Pool
	members  map[string]Member
	memberID map[string]string
}

type memberStats struct {
	attempts         uint64
	successes        uint64
	failures         uint64
	lastSuccessAt    time.Time
	lastFailureAt    time.Time
	lastFailureRetry bool
	// latencyEWMA is the exponentially weighted moving average of successful
	// invocation durations, alpha 0.2 (latencyEWMA = 0.2*latency +
	// 0.8*latencyEWMA), kept in nanoseconds as a time.Duration. It is a
	// smoothed signal for the providers page, not a precise measurement.
	latencyEWMA time.Duration
	// retryable429 and serverError5xx count the two retryable failures an
	// operator can actually act on: a rate limit, and a provider server
	// fault. Other retryable failures (408, deadline, network drop) count in
	// failures but in neither histogram.
	retryable429   uint64
	serverError5xx uint64
	// lastOutcomeDeferrable records whether the *most recent* outcome was a
	// failure that leaves the route worth trying later — a retryable one, or a
	// key that retired itself. lastFailureRetry cannot answer that: it stays
	// true forever once set, even after the member starts succeeding again.
	// Route exhaustion is a statement about the route right now, so it needs
	// the current outcome, not the last bad one.
	lastOutcomeDeferrable bool
}

// Executor selects a capability-bound provider channel and executes at most
// attemptsPerChannel members on that channel before moving to the next ordered
// provider route. Each channel owns its providerpool, so a retry cannot
// silently jump across a provider boundary or capability.
type Executor struct {
	mu       sync.RWMutex
	now      func() time.Time
	routes   map[Capability][]int
	channels []channelRuntime
	stats    map[string]memberStats
}

// NewExecutor validates channel metadata and creates one provider pool per
// channel. The optional pool options are applied to every channel pool.
func NewExecutor(channels []Channel, options ...providerpool.Options) (*Executor, error) {
	if len(options) > 1 {
		return nil, fmt.Errorf("providerchannels: at most one providerpool.Options value is allowed")
	}
	poolOptions := providerpool.Options{}
	if len(options) == 1 {
		poolOptions = options[0]
	}
	now := poolOptions.Now
	if now == nil {
		now = time.Now
	}

	e := &Executor{
		now:    now,
		routes: make(map[Capability][]int),
		stats:  make(map[string]memberStats),
	}
	seenChannels := make(map[string]struct{}, len(channels))
	for channelIndex, input := range channels {
		channel, members, poolMembers, err := normalizeChannel(input, channelIndex)
		if err != nil {
			return nil, err
		}
		if _, exists := seenChannels[channel.ID]; exists {
			return nil, fmt.Errorf("providerchannels: duplicate channel ID %q", channel.ID)
		}
		seenChannels[channel.ID] = struct{}{}

		pool, err := providerpool.NewPool(poolMembers, poolOptions)
		if err != nil {
			return nil, fmt.Errorf("providerchannels: channel %q: %w", channel.ID, err)
		}
		runtime := channelRuntime{channel: channel, pool: pool, members: members, memberID: make(map[string]string, len(members))}
		for memberID := range members {
			runtime.memberID[memberID] = memberID
			e.stats[statsKey(channel.ID, memberID)] = memberStats{}
		}
		e.channels = append(e.channels, runtime)
	}

	order := make([]int, len(e.channels))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		left, right := e.channels[order[i]].channel, e.channels[order[j]].channel
		if left.RouteOrder != right.RouteOrder {
			return left.RouteOrder < right.RouteOrder
		}
		return order[i] < order[j]
	})
	for _, channelIndex := range order {
		for _, capability := range e.channels[channelIndex].channel.capabilities() {
			e.routes[capability] = append(e.routes[capability], channelIndex)
		}
	}
	return e, nil
}

func normalizeChannel(input Channel, channelIndex int) (Channel, map[string]Member, []providerpool.Member, error) {
	channel := input
	channel.ID = strings.TrimSpace(channel.ID)
	channel.Provider = channel.provider()
	channel.ProviderName = channel.Provider
	channel.Label = strings.TrimSpace(channel.Label)
	if channel.ID == "" {
		if channel.Provider == "" {
			return Channel{}, nil, nil, fmt.Errorf("providerchannels: channel %d needs an ID or provider name", channelIndex)
		}
		channel.ID = "channel/" + channel.Provider + "/" + fmt.Sprint(channelIndex)
	}
	if channel.Provider == "" {
		return Channel{}, nil, nil, fmt.Errorf("providerchannels: channel %q has no provider name", channel.ID)
	}
	channel.Capabilities = normalizeCapabilities(channel.Capability, channel.Capabilities)
	if len(channel.Capabilities) == 0 {
		return Channel{}, nil, nil, fmt.Errorf("providerchannels: channel %q has no capabilities", channel.ID)
	}
	channel.Capability = channel.Capabilities[0]

	members := make(map[string]Member, len(channel.Members))
	normalizedMembers := make([]Member, 0, len(channel.Members))
	poolMembers := make([]providerpool.Member, 0, len(channel.Members))
	for memberIndex, inputMember := range channel.Members {
		member := inputMember
		member.ID = strings.TrimSpace(member.ID)
		if member.ID == "" {
			member.ID = channel.ID + "/member/" + fmt.Sprint(memberIndex)
		}
		if _, exists := members[member.ID]; exists {
			return Channel{}, nil, nil, fmt.Errorf("providerchannels: channel %q has duplicate member ID %q", channel.ID, member.ID)
		}
		member.ChannelID = channel.ID
		member.Provider = channel.Provider
		member.ProviderName = channel.Provider
		member.Label = strings.TrimSpace(member.Label)
		member.SecretRef = strings.TrimSpace(member.SecretRef)
		member.Capabilities = intersectCapabilities(channel.Capabilities, member.Capabilities)
		members[member.ID] = member
		normalizedMembers = append(normalizedMembers, member)
		if len(member.Capabilities) == 0 {
			continue
		}
		poolCapabilities := make([]providerpool.Capability, 0, len(member.Capabilities))
		for _, capability := range member.Capabilities {
			poolCapabilities = append(poolCapabilities, providerpool.Capability(capability))
		}
		poolMembers = append(poolMembers, providerpool.Member{Name: member.ID, Capabilities: poolCapabilities, Enabled: member.Enabled, Weight: member.Weight, MaxInflight: member.MaxInflight})
	}
	channel.Members = normalizedMembers
	return channel, members, poolMembers, nil
}

func intersectCapabilities(channelCapabilities, memberCapabilities []Capability) []Capability {
	if len(memberCapabilities) == 0 {
		return append([]Capability(nil), channelCapabilities...)
	}
	wanted := make(map[Capability]struct{}, len(memberCapabilities))
	for _, capability := range normalizeCapabilities("", memberCapabilities) {
		wanted[capability] = struct{}{}
	}
	result := make([]Capability, 0, len(channelCapabilities))
	for _, capability := range channelCapabilities {
		if _, ok := wanted[capability]; ok {
			result = append(result, capability)
		}
	}
	return result
}

// Route returns a copy of the ordered provider channels for a capability.
func (e *Executor) Route(capability Capability) []Channel {
	if e == nil {
		return nil
	}
	capability = normalizeCapability(capability)
	e.mu.RLock()
	defer e.mu.RUnlock()
	indices := e.routes[capability]
	result := make([]Channel, 0, len(indices))
	for _, index := range indices {
		result = append(result, cloneChannel(e.channels[index].channel))
	}
	return result
}

// Routes is an alias for Route.
func (e *Executor) Routes(capability Capability) []Channel { return e.Route(capability) }

// Execute invokes operation against the selected route. Supported operation
// shapes are Operation, MemberOperation, IDSecretOperation, and their
// equivalent unnamed function types.
func (e *Executor) Execute(ctx context.Context, capability Capability, operation any) error {
	if e == nil {
		return fmt.Errorf("%w: nil executor", ErrNoRoute)
	}
	invoke, err := makeInvoker(operation)
	if err != nil {
		return err
	}
	capability = normalizeCapability(capability)
	e.mu.RLock()
	route := append([]int(nil), e.routes[capability]...)
	e.mu.RUnlock()
	if len(route) == 0 {
		return fmt.Errorf("%w: %w: capability %q", ErrNoRoute, providerpool.ErrNoAvailable, capability)
	}

	var lastNonTerminal error
	for _, channelIndex := range route {
		e.mu.RLock()
		runtime := &e.channels[channelIndex]
		channel := runtime.channel
		e.mu.RUnlock()
		if !channel.Enabled {
			continue
		}

		// Retiring a spent key does not spend one of the channel's three tries.
		// The budget exists for a call that might go differently next time, and
		// a retired member is never selected again — charging the budget for it
		// would let two dead keys hide a live third. The loop still terminates:
		// every retirement permanently shrinks the route, so Select runs out of
		// members. The retirements bound is a belt on that, not the usual exit.
		attempts, retirements := 0, 0
		for attempts < attemptsPerChannel && retirements <= len(channel.Members) {
			if err := ctx.Err(); err != nil {
				return err
			}
			lease, err := runtime.pool.Select(providerpool.Capability(capability))
			if err != nil {
				if errors.Is(err, providerpool.ErrNoAvailable) {
					break
				}
				return err
			}
			memberID := lease.Name()
			e.mu.RLock()
			member, ok := runtime.members[runtime.memberID[memberID]]
			e.mu.RUnlock()
			if !ok {
				lease.Done(ErrNoRoute)
				return fmt.Errorf("providerchannels: selected unknown member %q", memberID)
			}
			invocation := Invocation{Capability: capability, ChannelID: channel.ID, ChannelLabel: channel.Label, Provider: channel.Provider, ProviderName: channel.ProviderName, Protocol: channel.Protocol, Endpoint: channel.Endpoint, Path: channel.Path, Model: channel.Model, AuthHeader: channel.AuthHeader, AuthScheme: channel.AuthScheme, TimeoutSeconds: channel.TimeoutSeconds, MemberID: member.ID, MemberLabel: member.Label, SecretRef: member.SecretRef}
			started := e.now()
			callErr := invoke(ctx, invocation, member)
			lease.Done(callErr)
			e.record(invocation, callErr, e.now().Sub(started))
			if callErr == nil {
				return nil
			}
			switch providerpool.ClassifyFailure(callErr) {
			case providerpool.NonRetryable:
				return callErr
			case providerpool.MemberSpent:
				// lease.Done has already retired the member, so the next Select
				// moves past it on its own. The error is still kept: if it turns
				// out nothing on the route survives, it is what tells the
				// operator which key died and why.
				retirements++
			default:
				attempts++
			}
			lastNonTerminal = callErr
		}
	}
	// Reaching here means no member succeeded and none failed in a way the
	// request itself caused (both return early). What is left is the difference
	// the caller actually has to act on: one unlucky call, or a route with
	// nothing left to try — every key cooled, retired, or both.
	if e.routeFailingEverywhere(capability, route) {
		if lastNonTerminal != nil {
			return fmt.Errorf("%w: %w", ErrRouteExhausted, lastNonTerminal)
		}
		return fmt.Errorf("%w: %w: capability %q", ErrRouteExhausted, providerpool.ErrNoAvailable, capability)
	}
	if lastNonTerminal != nil {
		return lastNonTerminal
	}
	return fmt.Errorf("%w: %w: capability %q", ErrNoRoute, providerpool.ErrNoAvailable, capability)
}

// routeFailingEverywhere reports whether every enabled member that can serve
// capability last ended in a failure that leaves the route worth trying later:
// a retryable one, or a key that retired itself.
//
// Retired members are counted rather than skipped. They are what exhaustion is
// made of on a channel of pooled plan keys, and skipping them would leave a
// wholly retired route looking like a route with no members at all — reported
// as a configuration mistake instead of the outage it is.
//
// It deliberately looks at recorded outcomes rather than only at what this
// call did. A spent monthly quota answers 429 on every key at once, and
// providerpool cools each key as it fails, so the *next* Execute can find the
// whole route unavailable without issuing a single request — pool.Select
// returns ErrNoAvailable and no attempt is made. If that shape reached the
// caller as an ordinary retryable error, the second job of a run would spend
// its whole attempt budget on a wall the first job already found, inside the
// few seconds the backoff allows, and fail permanently.
//
// A member that has never been called (attempts == 0) makes this false: a
// route that was never tried is not an exhausted one, and a misconfigured or
// entirely disabled route must keep reporting itself as ErrNoRoute.
func (e *Executor) routeFailingEverywhere(capability Capability, route []int) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	considered := 0
	for _, channelIndex := range route {
		runtime := &e.channels[channelIndex]
		if !runtime.channel.Enabled {
			continue
		}
		for _, member := range runtime.channel.Members {
			if !member.Enabled || !servesCapability(member.Capabilities, capability) {
				continue
			}
			considered++
			stats := e.stats[statsKey(runtime.channel.ID, member.ID)]
			if stats.attempts == 0 || !stats.lastOutcomeDeferrable {
				return false
			}
		}
	}
	return considered > 0
}

func servesCapability(capabilities []Capability, capability Capability) bool {
	for _, candidate := range capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

// memberCapabilityForStatus picks the capability whose health one member's
// status entry should reflect. Health is tracked per capability, but MemberStatus
// has room for one cooldown per member. A filtered snapshot examines the
// requested capability — and a member that does not serve it cannot make the
// route available for it, so it is reported as not selectable; an unfiltered
// snapshot falls back to the first capability the member serves, which is the
// only one a single-capability channel ever has.
func memberCapabilityForStatus(member Member, filter Capability) Capability {
	if filter != "" {
		if !servesCapability(member.Capabilities, filter) {
			return ""
		}
		return filter
	}
	for _, capability := range member.Capabilities {
		return capability
	}
	return ""
}

// Run is an alias for Execute.
func (e *Executor) Run(ctx context.Context, capability Capability, operation any) error {
	return e.Execute(ctx, capability, operation)
}

func makeInvoker(operation any) (func(context.Context, Invocation, Member) error, error) {
	switch op := operation.(type) {
	case Operation:
		return func(ctx context.Context, invocation Invocation, _ Member) error { return op(ctx, invocation) }, nil
	case func(context.Context, Invocation) error:
		return func(ctx context.Context, invocation Invocation, _ Member) error { return op(ctx, invocation) }, nil
	case MemberOperation:
		return func(ctx context.Context, _ Invocation, member Member) error { return op(ctx, member) }, nil
	case func(context.Context, Member) error:
		return func(ctx context.Context, _ Invocation, member Member) error { return op(ctx, member) }, nil
	case IDSecretOperation:
		return func(ctx context.Context, invocation Invocation, _ Member) error {
			return op(ctx, invocation.MemberID, invocation.SecretRef)
		}, nil
	case func(context.Context, string, string) error:
		return func(ctx context.Context, invocation Invocation, _ Member) error {
			return op(ctx, invocation.MemberID, invocation.SecretRef)
		}, nil
	default:
		return nil, fmt.Errorf("%w: want func(context.Context, Invocation), func(context.Context, Member), or func(context.Context, string, string)", ErrInvalidOperation)
	}
}

// record folds one invocation outcome into the member's counters. latency is
// the duration of the invoke call itself, measured through the injected clock
// so a fake clock can drive deterministic latency in tests.
func (e *Executor) record(invocation Invocation, err error, latency time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := statsKey(invocation.ChannelID, invocation.MemberID)
	stats := e.stats[key]
	stats.attempts++
	if err == nil {
		stats.successes++
		stats.lastSuccessAt = e.now().UTC()
		stats.latencyEWMA = time.Duration(0.2*float64(latency) + 0.8*float64(stats.latencyEWMA))
		stats.lastOutcomeDeferrable = false
	} else {
		stats.failures++
		stats.lastFailureAt = e.now().UTC()
		class := providerpool.ClassifyFailure(err)
		// 429 and 5xx are the retryable failures with a distinct operator
		// story — throttled, and the provider's server is down. Only status
		// code distinguishes them; everything else about the classification
		// (the pool cools the member either way) already happened above.
		if class == providerpool.Retryable {
			switch code := statusCodeOf(err); {
			case code == 429:
				stats.retryable429++
			case code >= 500 && code <= 599:
				stats.serverError5xx++
			}
		}
		// A retired key is a failure the operator has to fix, not a transient
		// one, so it must not be reported as retryable in the status view. It
		// still leaves the route deferrable: a route with no key left is worth
		// asking about later, not worth failing a job over now.
		stats.lastFailureRetry = class == providerpool.Retryable
		stats.lastOutcomeDeferrable = class == providerpool.Retryable || class == providerpool.MemberSpent
	}
	e.stats[key] = stats
}

// statusCodeOf extracts the HTTP status an error chain carries, or 0 when
// none does. Both providerpool.HTTPError (StatusCode) and *common.StatusError
// (HTTPStatusCode) are status-bearing; the dual probe mirrors
// providerpool.ClassifyFailure's own classifier without importing a transport
// package.
func statusCodeOf(err error) int {
	var withStatus interface{ StatusCode() int }
	if errors.As(err, &withStatus) {
		return withStatus.StatusCode()
	}
	var withHTTPStatus interface{ HTTPStatusCode() int }
	if errors.As(err, &withHTTPStatus) {
		return withHTTPStatus.HTTPStatusCode()
	}
	return 0
}

// StatusSnapshot contains operational metadata and counters only. It has no
// secret references or secret values, making it safe for status responses.
type StatusSnapshot struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Channels    []ChannelStatus `json:"channels"`
}

// ChannelStatus is the safe status view of one channel route.
type ChannelStatus struct {
	ID             string         `json:"id"`
	Label          string         `json:"label,omitempty"`
	Provider       string         `json:"provider"`
	ProviderName   string         `json:"provider_name"`
	Protocol       string         `json:"protocol,omitempty"`
	Endpoint       string         `json:"endpoint,omitempty"`
	Path           string         `json:"path,omitempty"`
	Model          string         `json:"model,omitempty"`
	AuthHeader     string         `json:"auth_header,omitempty"`
	AuthScheme     string         `json:"auth_scheme,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	Capabilities   []Capability   `json:"capabilities"`
	Enabled        bool           `json:"enabled"`
	Available      bool           `json:"available"`
	RouteOrder     int            `json:"route_order"`
	Members        []MemberStatus `json:"members"`
}

// MemberStatus is the secret-free status view of one member. Enabled is what
// configuration says; Retired is what the pool has since decided about the key
// behind it, which no configuration row records. CooldownUntil and HalfOpen
// are the pool's current health state for the capability this snapshot was
// taken for: the two tell an operator that an enabled key is alive but parked
// for a bounded wait (or waiting on a probe), which Retired alone cannot
// express.
type MemberStatus struct {
	ID                   string    `json:"id"`
	ChannelID            string    `json:"channel_id"`
	Label                string    `json:"label,omitempty"`
	Provider             string    `json:"provider"`
	ProviderName         string    `json:"provider_name"`
	Enabled              bool      `json:"enabled"`
	Retired              bool      `json:"retired,omitempty"`
	CooldownUntil        time.Time `json:"cooldown_until,omitempty"`
	HalfOpen             bool      `json:"half_open,omitempty"`
	SecretConfigured     bool      `json:"secret_configured"`
	Attempts             uint64    `json:"attempts"`
	Successes            uint64    `json:"successes"`
	Failures             uint64    `json:"failures"`
	LastSuccessAt        time.Time `json:"last_success_at,omitempty"`
	LastFailureAt        time.Time `json:"last_failure_at,omitempty"`
	LastFailureRetryable bool      `json:"last_failure_retryable,omitempty"`
	// LatencyMS is the EWMA of successful invocation durations (alpha 0.2);
	// zero until the first success.
	LatencyMS float64 `json:"latency_ms,omitempty"`
	// Retryable429 and ServerError5xx count the rate-limit and server-fault
	// failures the member has produced.
	Retryable429   uint64 `json:"retryable_429"`
	ServerError5xx uint64 `json:"server_error_5xx"`
}

// Snapshot returns the current safe status for all routes. Passing a
// capability filters the snapshot to channels serving that capability.
func (e *Executor) Snapshot(capabilities ...Capability) StatusSnapshot {
	if e == nil {
		return StatusSnapshot{}
	}
	var filter Capability
	if len(capabilities) > 0 {
		filter = normalizeCapability(capabilities[0])
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	indices := make([]int, 0, len(e.channels))
	if filter != "" {
		indices = append(indices, e.routes[filter]...)
	} else {
		indices = make([]int, len(e.channels))
		for i := range indices {
			indices[i] = i
		}
		sort.SliceStable(indices, func(i, j int) bool {
			left, right := e.channels[indices[i]].channel, e.channels[indices[j]].channel
			if left.RouteOrder != right.RouteOrder {
				return left.RouteOrder < right.RouteOrder
			}
			return indices[i] < indices[j]
		})
	}
	snapshot := StatusSnapshot{GeneratedAt: e.now().UTC(), Channels: make([]ChannelStatus, 0, len(indices))}
	for _, index := range indices {
		runtime := e.channels[index]
		status := ChannelStatus{ID: runtime.channel.ID, Label: runtime.channel.Label, Provider: runtime.channel.Provider, ProviderName: runtime.channel.ProviderName, Protocol: runtime.channel.Protocol, Endpoint: runtime.channel.Endpoint, Path: runtime.channel.Path, Model: runtime.channel.Model, AuthHeader: runtime.channel.AuthHeader, AuthScheme: runtime.channel.AuthScheme, TimeoutSeconds: runtime.channel.TimeoutSeconds, Capabilities: append([]Capability(nil), runtime.channel.capabilities()...), Enabled: runtime.channel.Enabled, RouteOrder: runtime.channel.RouteOrder, Members: make([]MemberStatus, 0, len(runtime.channel.Members))}
		for _, member := range runtime.channel.Members {
			stats := e.stats[statsKey(runtime.channel.ID, member.ID)]
			// Retired: configuration still enables this member, but the pool
			// stopped selecting it because the provider answered 401/402/403 on
			// its key. It is runtime state — editing the channel or restarting
			// the Hub clears it, which is also the only way to give the key
			// another try.
			live, known := runtime.pool.EnabledState(member.ID)
			retired := member.Enabled && known && !live
			examine := memberCapabilityForStatus(member, filter)
			poolCapability := providerpool.Capability(examine)
			health, healthKnown := runtime.pool.HealthState(member.ID, poolCapability)
			memberStatus := MemberStatus{ID: member.ID, ChannelID: runtime.channel.ID, Label: member.Label, Provider: runtime.channel.Provider, ProviderName: runtime.channel.ProviderName, Enabled: member.Enabled, Retired: retired, CooldownUntil: health.CooldownUntil, HalfOpen: health.HalfOpen, SecretConfigured: member.SecretConfigured || strings.TrimSpace(member.SecretRef) != "", Attempts: stats.attempts, Successes: stats.successes, Failures: stats.failures, LastSuccessAt: stats.lastSuccessAt, LastFailureAt: stats.lastFailureAt, LastFailureRetryable: stats.lastFailureRetry, LatencyMS: stats.latencyEWMA.Seconds() * 1000, Retryable429: stats.retryable429, ServerError5xx: stats.serverError5xx}
			// A channel every one of whose keys is cooling, retired, or probing
			// is not available, whatever configuration still says: the pool
			// would refuse every new call, and reporting it as available is how
			// an operator ends up staring at a green route while every job on
			// it parks. Selectable is the pool's own eligibility rule, so this
			// cannot drift from what a real call would find.
			if healthKnown && examine != "" && runtime.pool.Selectable(member.ID, poolCapability) {
				status.Available = status.Available || runtime.channel.Enabled
			}
			status.Members = append(status.Members, memberStatus)
		}
		snapshot.Channels = append(snapshot.Channels, status)
	}
	return snapshot
}

// Status is an alias for Snapshot.
func (e *Executor) Status(capabilities ...Capability) StatusSnapshot {
	return e.Snapshot(capabilities...)
}

func statsKey(channelID, memberID string) string { return channelID + "\x00" + memberID }

func cloneChannel(channel Channel) Channel {
	channel.Capability = normalizeCapability(channel.Capability)
	channel.Capabilities = append([]Capability(nil), channel.capabilities()...)
	channel.Members = append([]Member(nil), channel.Members...)
	for i := range channel.Members {
		channel.Members[i].Capabilities = append([]Capability(nil), channel.Members[i].Capabilities...)
	}
	return channel
}
