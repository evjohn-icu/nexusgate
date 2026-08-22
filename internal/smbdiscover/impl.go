package smbdiscover

import (
	"context"
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

// scanPort returns the local-network IPs that answered on port 445.
// The local network is derived from the host's own unicast interfaces, so a
// machine with no LAN route scans nothing.
func scanPort(ctx context.Context, dialTimeout time.Duration) []net.IP {
	addrs := localNetworkAddresses()
	if len(addrs) == 0 {
		return nil
	}
	// Enumerate candidate IPs across the /24 of each local address.
	var candidates []net.IP
	seen := map[string]bool{}
	for _, a := range addrs {
		ip := a
		if ip.To4() == nil {
			continue
		}
		ip4 := ip.To4()
		base := int(ip4[0])<<24 | int(ip4[1])<<16 | int(ip4[2])<<8
		for i := 1; i <= 254; i++ {
			cand := net.IPv4(byte(base>>24), byte(base>>16&0xff), byte(base>>8&0xff), byte(i))
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
	for _, cand := range candidates {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(ip net.IP) {
			defer wg.Done()
			defer func() { <-sem }()
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "445"), dialTimeout)
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

// localNetworkAddresses returns the machine's unicast interface addresses, the
// basis for the port-scan target list.
func localNetworkAddresses() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLoopback() {
				out = append(out, ip)
			}
		}
	}
	return out
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
				// The host answered 445 but refused the guest session: it
				// needs credentials. Never report an error upward; the host
				// is still a useful discovery result.
				h.NeedsAuth = true
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

// guestShareNames opens an anonymous/guest SMB session to host and lists the
// share names. The guest session is the one credential-free probe SMB allows;
// anything that needs a real user/password returns an error here.
func guestShareNames(ctx context.Context, host string, timeout time.Duration) ([]string, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(host, "445"))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	session, err := (&smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     "Guest",
			Password: "",
			Domain:   "WORKGROUP",
		},
	}).Dial(conn)
	if err != nil {
		return nil, err
	}
	defer session.Logoff()

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
