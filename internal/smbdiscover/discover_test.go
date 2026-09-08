package smbdiscover

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/hirochachacha/go-smb2"
)

// TestDiscoverMergesMDNSAndPortScan verifies that a host found by both mDNS
// and the port scan becomes one entry, that the mDNS name wins over the raw
// IP, and that sources union correctly.
func TestDiscoverMergesMDNSAndPortScan(t *testing.T) {
	ctx := context.Background()
	result, err := discoverWith(ctx, DefaultOptions(),
		func(context.Context) ([]Host, bool) {
			return []Host{
				{Name: "nas.local.", IP: "192.168.1.50", Source: "mdns"},
				{Name: "other.local.", IP: "192.168.1.60", Source: "mdns"},
			}, true
		},
		func(context.Context, time.Duration) ([]net.IP, []string, []string, bool) {
			return []net.IP{net.ParseIP("192.168.1.50"), net.ParseIP("192.168.1.99")}, nil, nil, false
		},
		func(context.Context, time.Duration, map[string]*Host, []string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Hosts
	if len(got) != 3 {
		t.Fatalf("got %d hosts, want 3: %+v", len(got), got)
	}
	// nas.local was found by both: name kept, source = mdns+portscan.
	var nas *Host
	for i := range got {
		if got[i].IP == "192.168.1.50" {
			nas = &got[i]
		}
	}
	if nas == nil {
		t.Fatal("nas.local (192.168.1.50) missing from results")
	}
	if nas.Name != "nas.local" {
		t.Fatalf("name = %q, want nas.local (mDNS name should win)", nas.Name)
	}
	if nas.Source != "mdns+portscan" {
		t.Fatalf("source = %q, want mdns+portscan", nas.Source)
	}
	// Port-scan-only host has its IP as name.
	var scanOnly *Host
	for i := range got {
		if got[i].IP == "192.168.1.99" {
			scanOnly = &got[i]
		}
	}
	if scanOnly == nil {
		t.Fatal("port-scan-only host missing")
	}
	if scanOnly.Name != "192.168.1.99" {
		t.Fatalf("name = %q, want IP fallback", scanOnly.Name)
	}
	if scanOnly.Source != "portscan" {
		t.Fatalf("source = %q, want portscan", scanOnly.Source)
	}
}

// TestDiscoverSortsResults verifies the output is sorted by name then IP.
func TestDiscoverSortsResults(t *testing.T) {
	ctx := context.Background()
	result, err := discoverWith(ctx, DefaultOptions(),
		func(context.Context) ([]Host, bool) {
			return []Host{
				{Name: "z.local.", IP: "192.168.1.2", Source: "mdns"},
				{Name: "a.local.", IP: "192.168.1.1", Source: "mdns"},
				{Name: "m.local.", IP: "192.168.1.3", Source: "mdns"},
			}, true
		},
		func(context.Context, time.Duration) ([]net.IP, []string, []string, bool) { return nil, nil, nil, false },
		func(context.Context, time.Duration, map[string]*Host, []string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Hosts
	var names []string
	for _, h := range got {
		names = append(names, h.Name)
	}
	want := []string{"a.local", "m.local", "z.local"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v (sorted)", names, want)
	}
}

// TestDiscoverShareEnumeration verifies the enumeration layer fills Shares and
// clears NeedsAuth for hosts it can read, and leaves NeedsAuth=true for hosts
// that fail.
func TestDiscoverShareEnumeration(t *testing.T) {
	ctx := context.Background()
	result, err := discoverWith(ctx, DefaultOptions(),
		func(context.Context) ([]Host, bool) {
			return []Host{
				{Name: "nas.local.", IP: "192.168.1.50", Source: "mdns"},
				{Name: "locked.local.", IP: "192.168.1.51", Source: "mdns"},
			}, true
		},
		func(context.Context, time.Duration) ([]net.IP, []string, []string, bool) { return nil, nil, nil, false },
		func(_ context.Context, _ time.Duration, byIP map[string]*Host, order []string) {
			byIP["192.168.1.50"].Shares = []string{"video", "photos"}
			byIP["192.168.1.50"].Probe = "ok"
			byIP["192.168.1.50"].NeedsAuth = false
			byIP["192.168.1.51"].Probe = "auth"
			byIP["192.168.1.51"].NeedsAuth = true // enumeration failed
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Hosts
	var nas, locked *Host
	for i := range got {
		switch got[i].IP {
		case "192.168.1.50":
			nas = &got[i]
		case "192.168.1.51":
			locked = &got[i]
		}
	}
	if nas == nil || !reflect.DeepEqual(nas.Shares, []string{"photos", "video"}) {
		t.Fatalf("nas shares = %v, want [photos video]", nas.Shares)
	}
	if nas.NeedsAuth {
		t.Fatal("nas should not need auth after successful enumeration")
	}
	if locked == nil || !locked.NeedsAuth {
		t.Fatal("locked host should be marked NeedsAuth")
	}
}

// TestDiscoverPortScanDisabled verifies DisablePortScan drops the scan layer.
func TestDiscoverPortScanDisabled(t *testing.T) {
	opts := DefaultOptions()
	opts.DisablePortScan = true
	ctx := context.Background()
	result, err := discoverWith(ctx, opts,
		func(context.Context) ([]Host, bool) {
			return []Host{{Name: "nas.local.", IP: "192.168.1.50", Source: "mdns"}}, true
		},
		func(context.Context, time.Duration) ([]net.IP, []string, []string, bool) {
			t.Fatal("port scan must not run when disabled")
			return nil, nil, nil, false
		},
		func(context.Context, time.Duration, map[string]*Host, []string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Hosts
	if len(got) != 1 || got[0].Name != "nas.local" {
		t.Fatalf("got %+v, want just nas.local", got)
	}
}

// TestDiscoverNameTrimsTrailingDot verifies mDNS names lose the trailing dot.
func TestDiscoverNameTrimsTrailingDot(t *testing.T) {
	ctx := context.Background()
	result, err := discoverWith(ctx, DefaultOptions(),
		func(context.Context) ([]Host, bool) {
			return []Host{{Name: "nas.local.", IP: "192.168.1.50", Source: "mdns"}}, true
		},
		func(context.Context, time.Duration) ([]net.IP, []string, []string, bool) { return nil, nil, nil, false },
		func(context.Context, time.Duration, map[string]*Host, []string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Hosts
	if got[0].Name != "nas.local" {
		t.Fatalf("name = %q, want nas.local (no trailing dot)", got[0].Name)
	}
}

// TestGuestShareNamesHangsBoundedByTimeout guards the context propagation fix:
// a host that accepts TCP on 445 but never completes SMB negotiation must not
// block guestShareNames for the TCP stack's keepalive timeout. Before the fix
// (Dial instead of DialContext, no WithContext) this test would hang for
// minutes; with the fix it returns within the short timeout.
// TestGuestShareNamesHangsBoundedByTimeout guards the context propagation fix:
// a host that accepts TCP on 445 but never completes SMB negotiation must not
// block guestShareNames for the TCP stack's keepalive timeout. Before the fix
// (Dial instead of DialContext, no WithContext) this test would hang for
// minutes; with the fix it returns within the short timeout.
func TestGuestShareNamesHangsBoundedByTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	oldPort := smbPortStr
	smbPortStr = port
	defer func() { smbPortStr = oldPort }()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept but never respond to SMB negotiation; a hostile or
			// wedged host would keep the connection open.
			go func() {
				defer conn.Close()
				// read to detect close; otherwise stall
				buf := make([]byte, 1024)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()

	ctx := context.Background()
	start := time.Now()
	_, err = guestShareNames(ctx, host, 2*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error from a host that never negotiates SMB")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("guestShareNames took %v, want it bounded by the ~2s timeout", elapsed)
	}
}

func TestGuestShareNamesFiltersIPC(t *testing.T) {
	// " IPC$ " and "   " are the two cases that depend on trimming happening
	// before anything else looks at the name: the first is IPC$ wearing
	// whitespace, and the second becomes the empty string only after the trim,
	// which is why the emptiness test cannot run first. An empty name reaching
	// the caller renders as a share button with no label on it.
	got := filterGuestShareNames([]string{" Video ", "IPC$", "ipc$", "Ipc$", " IPC$ ", "   ", "Media$", "Media$ "})
	want := []string{"Video", "Media$", "Media$"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered shares = %v, want %v", got, want)
	}
}

// TestLocalNetHostsEnumeratesUsableAddresses verifies Hosts() produces the
// full usable range for a /24, excluding network and broadcast addresses.
func TestLocalNetHostsEnumeratesUsableAddresses(t *testing.T) {
	n := localNet{
		Base:      net.IPv4(192, 168, 1, 0),
		PrefixLen: 24,
		HostCount: 254,
	}
	hosts := n.Hosts()
	if len(hosts) != 254 {
		t.Fatalf("len(hosts) = %d, want 254", len(hosts))
	}
	if got := hosts[0].String(); got != "192.168.1.1" {
		t.Fatalf("first host = %s, want 192.168.1.1", got)
	}
	if got := hosts[253].String(); got != "192.168.1.254" {
		t.Fatalf("last host = %s, want 192.168.1.254", got)
	}
	// Network (.0) and broadcast (.255) must not be present.
	for _, h := range hosts {
		if h.String() == "192.168.1.0" || h.String() == "192.168.1.255" {
			t.Fatalf("host %s must be excluded (network/broadcast)", h)
		}
	}
}

// TestLocalNetHostsSupportsNon24Prefix verifies a /20 (like the WSL NAT the
// fixture above saw) enumerates the right first/last usable hosts.
func TestLocalNetHostsSupportsNon24Prefix(t *testing.T) {
	// 172.20.208.0/20 → usable 172.20.208.1 .. 172.20.223.254
	n := localNet{
		Base:      net.IPv4(172, 20, 208, 0),
		PrefixLen: 20,
		HostCount: 4094,
	}
	hosts := n.Hosts()
	if len(hosts) != 4094 {
		t.Fatalf("len(hosts) = %d, want 4094", len(hosts))
	}
	if got := hosts[0].String(); got != "172.20.208.1" {
		t.Fatalf("first host = %s, want 172.20.208.1", got)
	}
	if got := hosts[4093].String(); got != "172.20.223.254" {
		t.Fatalf("last host = %s, want 172.20.223.254", got)
	}
}

// TestIsPrivateIPv4 verifies RFC1918 and link-local classification.
func TestIsPrivateIPv4(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.254", true},
		{"172.32.0.1", false}, // outside 172.16/12
		{"192.168.1.50", true},
		{"169.254.1.1", true},
		{"8.8.8.8", false},    // public
		{"100.64.0.1", false}, // CGNAT, not swept
		{"192.0.0.1", false},
	} {
		if got := isPrivateIPv4(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("isPrivateIPv4(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestDiscoverLayerContextsRemainLiveAfterMDNSBudget ensures each layer gets a
// fresh budget instead of inheriting an already-expired mDNS context.
func TestDiscoverLayerContextsRemainLiveAfterMDNSBudget(t *testing.T) {
	const ip = "192.0.2.10"
	opts := DefaultOptions()
	opts.ScanTimeout = 500 * time.Millisecond
	var scanned bool
	var enumerated bool
	result, err := discoverWith(context.Background(), opts,
		func(ctx context.Context) ([]Host, bool) {
			<-ctx.Done()
			return nil, true
		},
		func(ctx context.Context, _ time.Duration) ([]net.IP, []string, []string, bool) {
			if ctx.Err() != nil {
				t.Fatalf("port scan context = %v, want live context", ctx.Err())
			}
			scanned = true
			return []net.IP{net.ParseIP(ip)}, nil, nil, false
		},
		func(ctx context.Context, _ time.Duration, byIP map[string]*Host, _ []string) {
			if ctx.Err() != nil {
				t.Fatalf("enumeration context = %v, want live context", ctx.Err())
			}
			enumerated = true
			if _, ok := byIP[ip]; !ok {
				t.Fatalf("enumeration did not receive port-scan IP %s", ip)
			}
			byIP[ip].Probe = "ok"
			byIP[ip].Shares = []string{"guest"}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !scanned || !enumerated {
		t.Fatalf("layers ran: scan=%v enumerate=%v", scanned, enumerated)
	}
	if len(result.Hosts) != 1 || result.Hosts[0].IP != ip {
		t.Fatalf("hosts = %+v, want host %s", result.Hosts, ip)
	}
}

// TestDiscoverProbeClassification verifies the four probe outcomes without
// relying on error text from a remote SMB implementation.
func TestDiscoverProbeClassification(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "auth", err: &smb2.ResponseError{Code: 0xc000006d}, want: "auth"},
		{name: "unusable", err: errors.New("mid-negotiation reset"), want: "unusable"},
		{name: "timeout", err: context.DeadlineExceeded, want: "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProbeError(ctx, tc.err); got != tc.want {
				t.Fatalf("classification = %q, want %q", got, tc.want)
			}
		})
	}

	result, err := discoverWith(ctx, DefaultOptions(),
		func(context.Context) ([]Host, bool) {
			return []Host{{Name: "ok.local", IP: "192.0.2.11", Source: "mdns"}}, true
		},
		func(context.Context, time.Duration) ([]net.IP, []string, []string, bool) {
			return nil, nil, nil, false
		},
		func(_ context.Context, _ time.Duration, byIP map[string]*Host, _ []string) {
			byIP["192.0.2.11"].Shares = []string{"guest"}
			byIP["192.0.2.11"].Probe = "ok"
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hosts) != 1 || result.Hosts[0].Probe != "ok" || result.Hosts[0].NeedsAuth {
		t.Fatalf("successful probe = %+v, want ok without auth", result.Hosts)
	}
}

// TestDiscoverReportsScannedAndSkippedNetworks verifies an empty diagnostic
// cannot hide the fact that one network was swept and another was refused.
func TestDiscoverReportsScannedAndSkippedNetworks(t *testing.T) {
	opts := DefaultOptions()
	opts.DisableShareEnum = true
	result, err := discoverWith(context.Background(), opts,
		func(context.Context) ([]Host, bool) { return nil, false },
		func(context.Context, time.Duration) ([]net.IP, []string, []string, bool) {
			return []net.IP{net.ParseIP("192.0.2.12")}, []string{"192.168.1.0/24"}, []string{"172.16.0.0/12"}, true
		},
		func(context.Context, time.Duration, map[string]*Host, []string) {
			t.Fatal("share enumeration must be disabled")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.ScannedNetworks, []string{"192.168.1.0/24"}) {
		t.Fatalf("scanned networks = %v", result.ScannedNetworks)
	}
	if !reflect.DeepEqual(result.SkippedNetworks, []string{"172.16.0.0/12"}) {
		t.Fatalf("skipped networks = %v", result.SkippedNetworks)
	}
	if !result.Truncated {
		t.Fatal("result must report a truncated port sweep")
	}
	if len(result.Hosts) != 1 || result.Hosts[0].IP != "192.0.2.12" {
		t.Fatalf("positive scan control missing: %+v", result.Hosts)
	}
}
