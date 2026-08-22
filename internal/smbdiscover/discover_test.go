package smbdiscover

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"
)

// TestDiscoverMergesMDNSAndPortScan verifies that a host found by both mDNS
// and the port scan becomes one entry, that the mDNS name wins over the raw
// IP, and that sources union correctly.
func TestDiscoverMergesMDNSAndPortScan(t *testing.T) {
	ctx := context.Background()
	got, err := discoverWith(ctx, DefaultOptions(),
		func(context.Context) []Host {
			return []Host{
				{Name: "nas.local.", IP: "192.168.1.50", Source: "mdns"},
				{Name: "other.local.", IP: "192.168.1.60", Source: "mdns"},
			}
		},
		func(context.Context, time.Duration) []net.IP {
			return []net.IP{net.ParseIP("192.168.1.50"), net.ParseIP("192.168.1.99")}
		},
		func(context.Context, time.Duration, map[string]*Host, []string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
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
	got, err := discoverWith(ctx, DefaultOptions(),
		func(context.Context) []Host {
			return []Host{
				{Name: "z.local.", IP: "192.168.1.2", Source: "mdns"},
				{Name: "a.local.", IP: "192.168.1.1", Source: "mdns"},
				{Name: "m.local.", IP: "192.168.1.3", Source: "mdns"},
			}
		},
		func(context.Context, time.Duration) []net.IP { return nil },
		func(context.Context, time.Duration, map[string]*Host, []string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
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
	got, err := discoverWith(ctx, DefaultOptions(),
		func(context.Context) []Host {
			return []Host{
				{Name: "nas.local.", IP: "192.168.1.50", Source: "mdns"},
				{Name: "locked.local.", IP: "192.168.1.51", Source: "mdns"},
			}
		},
		func(context.Context, time.Duration) []net.IP { return nil },
		func(_ context.Context, _ time.Duration, byIP map[string]*Host, order []string) {
			byIP["192.168.1.50"].Shares = []string{"video", "photos"}
			byIP["192.168.1.50"].NeedsAuth = false
			byIP["192.168.1.51"].NeedsAuth = true // enumeration failed
		},
	)
	if err != nil {
		t.Fatal(err)
	}
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
	got, err := discoverWith(ctx, opts,
		func(context.Context) []Host {
			return []Host{{Name: "nas.local.", IP: "192.168.1.50", Source: "mdns"}}
		},
		func(context.Context, time.Duration) []net.IP {
			t.Fatal("port scan must not run when disabled")
			return nil
		},
		func(context.Context, time.Duration, map[string]*Host, []string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "nas.local" {
		t.Fatalf("got %+v, want just nas.local", got)
	}
}

// TestDiscoverNameTrimsTrailingDot verifies mDNS names lose the trailing dot.
func TestDiscoverNameTrimsTrailingDot(t *testing.T) {
	ctx := context.Background()
	got, err := discoverWith(ctx, DefaultOptions(),
		func(context.Context) []Host {
			return []Host{{Name: "nas.local.", IP: "192.168.1.50", Source: "mdns"}}
		},
		func(context.Context, time.Duration) []net.IP { return nil },
		func(context.Context, time.Duration, map[string]*Host, []string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
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
