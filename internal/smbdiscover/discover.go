// Package smbdiscover finds SMB servers on the local network so an operator
// can pick a share to add as a library root without having to know its
// hostname or IP in advance.
//
// It uses three layers, each weaker-but-wider than the last:
//
//  1. mDNS (zeroconf) — browse _smb._tcp.local, catching NAS and Windows
//     machines that advertise SMB (the "network neighbourhood" view).
//  2. TCP port scan of the local network — a host that does not advertise
//     mDNS but still answers on 445 (a NAS with mDNS disabled, say).
//  3. SMB guest share enumeration (go-smb2) — for each host that answers on
//     445, try an anonymous/guest session and list share names. A host that
//     refuses the guest session is reported with NeedsAuth=true and no share
//     list, rather than being dropped.
//
// The package deliberately never touches credentials: it only ever attempts
// an anonymous guest session. Shares that need real credentials are flagged
// and left to the existing mount wizard (internal/mount), which writes
// credentials to a 0600 file the operator controls.
package smbdiscover

import (
	"context"
	"net"
	"sort"
	"strings"
	"time"
)

// Host is one discovered SMB server.
type Host struct {
	// Name is the hostname when known (mDNS), otherwise the IP.
	Name string `json:"name"`
	// IP is the resolved address, always set.
	IP string `json:"ip"`
	// Shares lists share names when the guest session could enumerate them.
	// Empty with NeedsAuth=true means the host answered on 445 but refused
	// the anonymous session.
	Shares []string `json:"shares,omitempty"`
	// NeedsAuth reports that the host exists but the anonymous guest session
	// could not enumerate its shares; a credential is required to read them.
	NeedsAuth bool `json:"needs_auth,omitempty"`
	// Source says which layer found it: "mdns", "portscan" or "mdns+portscan".
	Source string `json:"source"`
}

// Options configures a discovery run.
type Options struct {
	// ScanTimeout bounds the whole run. Zero uses DefaultTimeout.
	ScanTimeout time.Duration
	// PortDialTimeout is the per-host TCP connect timeout for the port scan.
	// Zero uses DefaultPortDialTimeout.
	PortDialTimeout time.Duration
	// ShareTimeout bounds the SMB guest enumeration per host. Zero uses
	// DefaultShareTimeout.
	ShareTimeout time.Duration
	// DisablePortScan skips the TCP 445 sweep (mDNS only). Used when the
	// operator does not want an active scan, or in tests.
	DisablePortScan bool
	// DisableShareEnum skips the SMB guest enumeration (hosts + port scan
	// only). Used when go-smb2 is unavailable or in tests.
	DisableShareEnum bool
}

const (
	// DefaultTimeout bounds a whole discovery run.
	DefaultTimeout = 15 * time.Second
	// DefaultPortDialTimeout is the per-host TCP connect timeout for the
	// port-scan layer.
	DefaultPortDialTimeout = 1 * time.Second
	// DefaultShareTimeout bounds the SMB guest enumeration for one host.
	DefaultShareTimeout = 3 * time.Second
	// smbPort is the SMB-over-TCP port.
	smbPort = 445
)

// DefaultOptions returns Options with the standard timeouts.
func DefaultOptions() Options {
	return Options{
		ScanTimeout:     DefaultTimeout,
		PortDialTimeout: DefaultPortDialTimeout,
		ShareTimeout:    DefaultShareTimeout,
	}
}

// Discover runs the three-layer discovery and returns a deduplicated, sorted
// list of SMB hosts.
func Discover(ctx context.Context, opts Options) ([]Host, error) {
	return discoverWith(ctx, opts, browseMDNS, scanPort, enumerateShares)
}

// discoverWith is Discover with the three network layers injected, so the
// merge/dedupe/sort logic is testable without touching a real network.
func discoverWith(
	ctx context.Context,
	opts Options,
	browse func(context.Context) []Host,
	scan func(context.Context, time.Duration) []net.IP,
	enumerate func(context.Context, time.Duration, map[string]*Host, []string),
) ([]Host, error) {
	if opts.ScanTimeout <= 0 {
		opts.ScanTimeout = DefaultTimeout
	}
	if opts.PortDialTimeout <= 0 {
		opts.PortDialTimeout = DefaultPortDialTimeout
	}
	if opts.ShareTimeout <= 0 {
		opts.ShareTimeout = DefaultShareTimeout
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.ScanTimeout)
		defer cancel()
	}

	// Layer 1: mDNS.
	mdnsHosts := browse(ctx)

	// Layer 2: port scan of the local network.
	var scanned []net.IP
	if !opts.DisablePortScan {
		scanned = scan(ctx, opts.PortDialTimeout)
	}

	// Merge mDNS results with port-scan hits by IP. A host found by both is
	// one host; mDNS names win over raw IPs.
	byIP := map[string]*Host{}
	var order []string // stable first-seen order
	addHost := func(h Host) {
		h.Name = strings.TrimSuffix(h.Name, ".")
		key := h.IP
		if existing, ok := byIP[key]; ok {
			// Merge: keep the better name, union sources.
			if existing.Name == "" || existing.Name == existing.IP {
				existing.Name = h.Name
			}
			if !strings.Contains(existing.Source, h.Source) {
				existing.Source = mergeSources(existing.Source, h.Source)
			}
			if len(h.Shares) > 0 && len(existing.Shares) == 0 {
				existing.Shares = h.Shares
				existing.NeedsAuth = false
			}
			return
		}
		h.Source = normalizeSource(h.Source)
		byIP[key] = &h
		order = append(order, key)
	}
	for _, h := range mdnsHosts {
		addHost(h)
	}
	for _, ip := range scanned {
		addHost(Host{Name: ip.String(), IP: ip.String(), Source: "portscan"})
	}

	// Layer 3: guest share enumeration for every host that answered 445
	// (mDNS hosts also imply 445 is open, so enumerate them too).
	if !opts.DisableShareEnum {
		enumerate(ctx, opts.ShareTimeout, byIP, order)
	}

	out := make([]Host, 0, len(order))
	for _, key := range order {
		h := *byIP[key]
		if h.Name == "" {
			h.Name = h.IP
		}
		sort.Strings(h.Shares)
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].IP < out[j].IP
	})
	return out, nil
}

func normalizeSource(source string) string {
	parts := map[string]bool{}
	for _, p := range strings.Split(source, "+") {
		if p != "" {
			parts[p] = true
		}
	}
	var names []string
	for _, p := range []string{"mdns", "portscan"} {
		if parts[p] {
			names = append(names, p)
		}
	}
	if len(names) == 0 {
		return source
	}
	return strings.Join(names, "+")
}

func mergeSources(a, b string) string {
	return normalizeSource(a + "+" + b)
}
