package smbdiscover

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/hirochachacha/go-smb2"
)

// browseMDNS returns hosts advertising _smb._tcp.local over multicast DNS.
// A network without multicast (a common VM/container situation) yields nothing;
// that is a "no SMB servers announce themselves" fact, not an error.
func browseMDNS(ctx context.Context) []Host {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil
	}
	entries := make(chan *zeroconf.ServiceEntry)
	err = resolver.Browse(ctx, "_smb._tcp", "local.", entries)
	if err != nil {
		return nil
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
	return collected
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

// scanPort returns the local-network IPs that answered on port 445. The local
// network is derived from the host's own unicast interfaces, so a machine
// with no LAN route scans nothing.
func scanPort(ctx context.Context, dialTimeout time.Duration) []net.IP {
	nets := localNetworks()
	if len(nets) == 0 {
		return nil
	}
	var candidates []net.IP
	seen := map[string]bool{}
	for _, n := range nets {
		// A subnet wider than the cap would mean scanning tens of thousands
		// to millions of addresses; refuse rather than turn a library-root
		// wizard click into a LAN-wide flood. /16 (65534 hosts) is the widest
		// we will sweep.
		if n.HostCount > maxScanHosts {
			continue
		}
		addrs := n.Hosts()
		for _, cand := range addrs {
			key := cand.String()
			if !seen[key] {
				seen[key] = true
				candidates = append(candidates, cand)
			}
		}
	}

	var mu sync.Mutex
	var hits []net.IP
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	dialer := net.Dialer{Timeout: dialTimeout}
	for _, cand := range candidates {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(ip net.IP) {
			defer wg.Done()
			defer func() { <-sem }()
			// DialContext (not DialTimeout) so a cancelled discovery context
			// aborts in-flight probes instead of waiting out each dial timeout.
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), smbPortStr))
			if err != nil {
				return
			}
			conn.Close()
			mu.Lock()
			hits = append(hits, ip)
			mu.Unlock()
		}(cand)
	}
	wg.Wait()
	return hits
}

// maxScanHosts caps how many addresses one discovery may sweep. A /16 is
// 65534 usable hosts; anything wider is left to manual entry rather than
// flooding the LAN. 64 concurrent dials at 1s each cover 65534 hosts in about
// 17 minutes worst case, which is why the cap exists.
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

// enumerateShares fills Shares/NeedsAuth on each host in byIP using anonymous
// guest SMB sessions, bounded per host by shareTimeout.
func enumerateShares(ctx context.Context, shareTimeout time.Duration, byIP map[string]*Host, order []string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentEnumeration)
	for _, key := range order {
		host := byIP[key]
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(h *Host) {
			defer wg.Done()
			defer func() { <-sem }()
			shares, err := guestShareNames(ctx, h.IP, shareTimeout)
			if err != nil {
				// Only a definitive guest-session refusal is "needs
				// credentials". A timeout (slow or wedged host) or a
				// cancelled overall scan says nothing about credentials, so
				// leave NeedsAuth false rather than promising a password
				// will help against a blackholed host.
				if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
					h.NeedsAuth = true
				}
				return
			}
			h.Shares = shares
			h.NeedsAuth = false
		}(host)
	}
	wg.Wait()
}

// maxConcurrentEnumeration bounds how many SMB guest sessions run at once.
const maxConcurrentEnumeration = 8

// smbPortStr is the SMB-over-TCP port. It is a var (not a const) so tests can
// point guestShareNames at a fake server on an ephemeral port without needing
// root to bind 445.
var smbPortStr = "445"

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
	var out []string
	for _, n := range names {
		if n == "" {
			continue
		}
		out = append(out, strings.TrimSpace(n))
	}
	return out, nil
}
