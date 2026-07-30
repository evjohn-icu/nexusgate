package api

import (
	"net"
	"net/http"
	"net/netip"
)

// defaultTrustedReadNetworks is the fallback allowlist for the read routes that
// carry no credential of their own. It is the set of ranges a home library is
// actually reached from: loopback, the RFC1918 LAN ranges, link-local, IPv6
// unique-local, and the CGNAT range that Tailscale and similar overlays assign
// (a Tailnet is the normal way to reach a home Hub from outside, and it should
// keep working without opening the library to the internet).
var defaultTrustedReadNetworks = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

// fromTrustedNetwork reports whether the request's peer address falls inside the
// configured allowlist.
//
// The peer address is taken from RemoteAddr and nothing else. X-Forwarded-For
// and X-Real-IP are attacker-controlled on a directly exposed listener, so
// honouring them would let any internet client claim to be 192.168.1.5 and
// defeat the guard entirely.
//
// The consequence runs the other way too. Behind a reverse proxy, or behind the
// NAT of a published Docker port, every request arrives from the proxy or the
// bridge gateway -- which is itself an RFC1918 address, and therefore trusted --
// so the guard can no longer tell a LAN client from an internet one. Such a
// deployment has to filter at the proxy, or set the allowlist to a range no peer
// can match, which drops every read back to requiring a token. Setting it to
// "0.0.0.0/0,::/0" does the opposite of that: it trusts every caller and
// publishes the library.
func (s *Server) fromTrustedNetwork(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Unix sockets and httptest's synthetic addresses land here. Neither is
		// reachable from the network, so treat them as local rather than
		// locking the operator out of their own machine.
		return host == "" || host == "@" || host == "pipe"
	}
	// An IPv4 peer on a dual-stack listener arrives as ::ffff:a.b.c.d, which
	// matches no IPv4 prefix until it is unmapped.
	addr = addr.Unmap()
	for _, prefix := range s.trustedReadNetworks {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// requireTrustedRead guards the read routes that have no credential of their
// own. The library browse, search, thumbnail and proxy routes are deliberately
// unauthenticated so the browser UI works without a token on the LAN; that is a
// sound trade-off on a home network and a full disclosure of the library the
// moment the listener is port-forwarded.
//
// A credential still wins over topology: an admin or agent token is a stronger
// claim than a source address, so a token holder is admitted from anywhere.
// This is a network boundary, not a replacement for the admin gate on the
// mutating routes.
func (s *Server) requireTrustedRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.fromTrustedNetwork(r) && !s.isHubAdmin(r) && !s.isHubAgent(r) {
			http.Error(w, "Library reads are restricted to trusted networks; present a Hub token or add this network to hub_security.trusted_read_networks", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
