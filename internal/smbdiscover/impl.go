package smbdiscover

import (
	"context"
	"errors"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/hirochachacha/go-smb2"
)

// browseMDNS returns hosts advertising _smb._tcp.local over multicast DNS.
// A network without multicast (a common VM/container situation) yields nothing;
// that is a "no SMB servers announce themselves" fact, not an error.
func browseMDNS(ctx context.Context) ([]Host, bool) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, false
	}
	entries := make(chan *zeroconf.ServiceEntry)
	err = resolver.Browse(ctx, "_smb._tcp", "local.", entries)
	if err != nil {
		return nil, false
	}

	// Browse sends entries on the channel until ctx is cancelled and closes
	// the channel itself (zeroconf's mainloop does this on ctx.Done), so we
	// must NOT close entries here — a second close panics.
	var mu sync.Mutex
	var collected []Host
	var wg sync.WaitGroup
	wg.Add(1)
	go func(results <-chan *zeroconf.ServiceEntry) {
		defer wg.Done()
		for entry := range results {
			if entry == nil {
				continue
			}
			h := Host{
				Name:   entry.HostName,
				IP:     firstIP(entry.AddrIPv4),
				Source: "mdns",
			}
			if h.IP == "" {
				h.IP = firstIP(entry.AddrIPv6)
			}
			if h.IP == "" || h.Name == "" {
				continue
			}
			h.Name = strings.TrimSuffix(h.Name, ".")
			mu.Lock()
			collected = append(collected, h)
			mu.Unlock()
		}
	}(entries)

	<-ctx.Done()
	wg.Wait()
	return collected, true
}

func firstIP(addrs []net.IP) string {
	if len(addrs) == 0 {
		return ""
	}
	return addrs[0].String()
}

// localNet is one unicast IPv4 network this host belongs to, with the prefix
// length the interface reports (the mask DHCP actually handed out, not a
// hardcoded /24).
type localNet struct {
	// IP is the interface's own address (used only for diagnostics/ordering).
	IP net.IP
	// Base is the network address.
	Base net.IP
	// PrefixLen is the CIDR prefix length (8..30).
	PrefixLen int
	// HostCount is the number of usable host addresses (2^(32-prefix) - 2),
	// excluding the network and broadcast addresses.
	HostCount uint32
}

// scanPort returns the local-network IPs that answered on port 445, together
// with the networks selected, the networks rejected by the safety cap, and
// whether the port budget stopped dispatch before every candidate was dialled.
// The local network is derived from the host's own unicast interfaces, so a
// machine with no LAN route scans nothing.
func scanPort(ctx context.Context, dialTimeout time.Duration) ([]net.IP, []string, []string, bool) {
	nets := localNetworks()
	if len(nets) == 0 {
		return nil, nil, nil, false
	}
	var candidates []net.IP
	seen := map[string]bool{}
	seenScanned := map[string]bool{}
	var scannedNetworks []string
	var skippedNetworks []string
	for _, n := range nets {
		cidr := n.CIDR()
		// Keep the /16 hard cap as a flood-safety boundary. The normal eight
		// second port budget can only attempt roughly 64*8/1s ≈ 512
		// one-second dials, so larger allowed ranges are reported as
		// truncated rather than pretending that the whole range was swept.
		if n.HostCount > maxScanHosts {
			if !containsString(skippedNetworks, cidr) {
				skippedNetworks = append(skippedNetworks, cidr)
			}
			continue
		}
		if !seenScanned[cidr] {
			seenScanned[cidr] = true
			scannedNetworks = append(scannedNetworks, cidr)
		}
		for _, cand := range n.Hosts() {
			key := cand.String()
			if !seen[key] {
				seen[key] = true
				candidates = append(candidates, cand)
			}
		}
	}
	sort.Strings(scannedNetworks)
	sort.Strings(skippedNetworks)

	var mu sync.Mutex
	var hits []net.IP
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	dialer := net.Dialer{Timeout: dialTimeout}
	launched := 0
	truncated := false
	for launched < len(candidates) {
		select {
		case <-ctx.Done():
			truncated = true
			launched = len(candidates)
		case sem <- struct{}{}:
			if ctx.Err() != nil {
				<-sem
				truncated = true
				launched = len(candidates)
				continue
			}
			ip := candidates[launched]
			launched++
			wg.Add(1)
			go func(ip net.IP) {
				defer wg.Done()
				defer func() { <-sem }()
				// DialContext (not DialTimeout) so a cancelled discovery
				// context aborts in-flight probes instead of waiting out each
				// dial timeout.
				conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), smbPortStr))
				if err != nil {
					return
				}
				conn.Close()
				mu.Lock()
				hits = append(hits, ip)
				mu.Unlock()
			}(ip)
		}
	}
	wg.Wait()
	return hits, scannedNetworks, skippedNetworks, truncated
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// maxScanHosts is a hard /16 safety cap. The port layer has an eight-second
// budget, so at 64 concurrent one-second dials it can normally reach only
// about 512 addresses; the cap remains a separate flood guard and Truncated
// makes that partial sweep explicit.
const maxScanHosts uint32 = 65534

// localNetworks returns the machine's unicast IPv4 networks (RFC1918 or link
// local), derived from interface addresses so a DHCP-issued prefix other than
// /24 is honoured.
func localNetworks() []localNet {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []localNet
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			ones, bits := ipnet.Mask.Size()
			if bits != 32 {
				continue
			}
			// Only private/link-local ranges are worth sweeping; a public IP
			// on a carrier subnet is not our LAN.
			if !isPrivateIPv4(ip4) {
				continue
			}
			base := ip4.Mask(ipnet.Mask)
			hostCount := (uint32(1) << (32 - uint32(ones))) - 2
			if hostCount < 1 {
				continue // /31 and /32 have no usable hosts
			}
			out = append(out, localNet{
				IP:        append(net.IP(nil), ip4...),
				Base:      append(net.IP(nil), base...),
				PrefixLen: ones,
				HostCount: hostCount,
			})
		}
	}
	return out
}

// CIDR returns the network notation used in discovery diagnostics.
func (n localNet) CIDR() string {
	return (&net.IPNet{IP: n.Base, Mask: net.CIDRMask(n.PrefixLen, 32)}).String()
}

// Hosts enumerates the usable host addresses in the network (excluding the
// network and broadcast addresses).
func (n localNet) Hosts() []net.IP {
	if n.HostCount == 0 {
		return nil
	}
	base4 := n.Base.To4()
	if base4 == nil {
		return nil
	}
	base := uint32(base4[0])<<24 | uint32(base4[1])<<16 | uint32(base4[2])<<8 | uint32(base4[3])
	out := make([]net.IP, 0, n.HostCount)
	for i := uint32(1); i <= n.HostCount; i++ {
		addr := base + i
		out = append(out, net.IPv4(byte(addr>>24), byte(addr>>16&0xff), byte(addr>>8&0xff), byte(addr&0xff)))
	}
	return out
}

// isPrivateIPv4 reports whether addr is in an RFC 1918 private range or the
// link-local 169.254.0.0/16 block.
func isPrivateIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true // 10.0.0.0/8
	case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
		return true // 172.16.0.0/12
	case ip4[0] == 192 && ip4[1] == 168:
		return true // 192.168.0.0/16
	case ip4[0] == 169 && ip4[1] == 254:
		return true // 169.254.0.0/16 link-local
	}
	return false
}

// enumerateShares fills Shares/Probe on each host in byIP using anonymous
// guest SMB sessions, bounded per host by shareTimeout.
func enumerateShares(ctx context.Context, shareTimeout time.Duration, byIP map[string]*Host, order []string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentEnumeration)
scanLoop:
	for i, key := range order {
		host := byIP[key]
		select {
		case <-ctx.Done():
			for _, remaining := range order[i:] {
				byIP[remaining].Probe = "timeout"
				byIP[remaining].NeedsAuth = false
			}
			break scanLoop
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil {
			<-sem
			for _, remaining := range order[i:] {
				byIP[remaining].Probe = "timeout"
				byIP[remaining].NeedsAuth = false
			}
			break scanLoop
		}
		wg.Add(1)
		go func(h *Host) {
			defer wg.Done()
			defer func() { <-sem }()
			shares, err := guestShareNames(ctx, h.IP, shareTimeout)
			if err != nil {
				h.Probe = classifyProbeError(ctx, err)
				h.NeedsAuth = h.Probe == "auth"
				return
			}
			h.Shares = shares
			h.Probe = "ok"
			h.NeedsAuth = false
		}(host)
	}
	wg.Wait()
}

// classifyProbeError uses only context state and structured errors. The
// go-smb2 release in use has no exported authentication-specific error type or
// status constants; its public ResponseError.Code and os.ErrPermission are the
// available structured refusal signals. Unknown response codes remain
// unusable, rather than making a password promise without a reliable basis.
func classifyProbeError(ctx context.Context, err error) string {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var smbContextErr *smb2.ContextError
	if errors.As(err, &smbContextErr) && errors.Is(smbContextErr.Err, context.DeadlineExceeded) {
		return "timeout"
	}
	if isAuthenticationError(err) {
		return "auth"
	}
	return "unusable"
}

func isAuthenticationError(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	var responseErr *smb2.ResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	switch responseErr.Code {
	case 0xc0000022, // STATUS_ACCESS_DENIED
		0xc000006a, // STATUS_WRONG_PASSWORD
		0xc000006d, // STATUS_LOGON_FAILURE
		0xc000006e, // STATUS_ACCOUNT_RESTRICTION
		0xc000006f, // STATUS_INVALID_LOGON_HOURS
		0xc0000070, // STATUS_INVALID_WORKSTATION
		0xc0000071, // STATUS_PASSWORD_EXPIRED
		0xc0000072, // STATUS_ACCOUNT_DISABLED
		0xc000015b, // STATUS_LOGON_TYPE_NOT_GRANTED
		0xc0000193, // STATUS_ACCOUNT_EXPIRED
		0xc0000234: // STATUS_ACCOUNT_LOCKED_OUT
		return true
	default:
		return false
	}
}

// maxConcurrentEnumeration bounds how many SMB guest sessions run at once.
const maxConcurrentEnumeration = 8

// smbPortStr is the SMB-over-TCP port. It is a var (not a const) so tests can
// point guestShareNames at a fake server on an ephemeral port without needing
// root to bind 445.
var smbPortStr = "445"

func filterGuestShareNames(names []string) []string {
	var out []string
	for _, n := range names {
		// Trimmed before the emptiness test, not after: a name of nothing but
		// spaces used to survive the test and reach the caller as "", which the
		// page would then render as a share button with no label.
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if len(n) == 4 &&
			(n[0] == 'i' || n[0] == 'I') &&
			(n[1] == 'p' || n[1] == 'P') &&
			(n[2] == 'c' || n[2] == 'C') && n[3] == '$' {
			// IPC$ is the named-pipe/RPC endpoint, not a filesystem.
			continue
		}
		out = append(out, n)
	}
	return out
}

// guestShareNames opens an anonymous/guest SMB session to host and lists the
// share names. The guest session is the one credential-free probe SMB allows;
// anything that needs a real user/password returns an error here.
func guestShareNames(ctx context.Context, host string, timeout time.Duration) ([]string, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(host, smbPortStr))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// DialContext (not Dial) and Session.WithContext propagate dialCtx into the
	// SMB negotiation AND the share enumeration. Without both, a host that
	// accepts TCP on 445 but then hangs mid-negotiation or mid-listing would
	// block this goroutine for the TCP stack's keepalive timeout (hours on
	// Linux), and enumerateShares' wg.Wait() would make the whole Discover
	// call block with it.
	session, err := (&smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     "Guest",
			Password: "",
			Domain:   "WORKGROUP",
		},
	}).DialContext(dialCtx, conn)
	if err != nil {
		return nil, err
	}
	defer session.Logoff()
	session = session.WithContext(dialCtx)

	names, err := session.ListSharenames()
	if err != nil {
		return nil, err
	}
	return filterGuestShareNames(names), nil
}
