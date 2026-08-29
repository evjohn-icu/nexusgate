package worker

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/hubtls"
	"github.com/evjohn-icu/timingdex/internal/remote"
)

// TestClientPinsTheBootstrapCertificateFingerprint ties the Worker runtime's
// TLS pinning to the identity the generated install script embeds: both derive
// from the Hub's own self-signed certificate (the fingerprint via
// hubtls.FingerprintCertificate, the script's curl pin via the same leaf's
// SPKI). The HTTP client must accept exactly that fingerprint and reject any
// other certificate, so the post-enrollment connection trusts the same key the
// bootstrap download was pinned to.
func TestClientPinsTheBootstrapCertificateFingerprint(t *testing.T) {
	certPath, keyPath, fingerprint, err := hubtls.EnsureSelfSigned(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"worker":{"id":"worker-1"},"token":"worker-token"}`))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	defer server.Close()

	registration := remote.WorkerRegistration{Name: "pinned", Platform: "linux-amd64"}
	if _, err := NewClient(server.URL, fingerprint).Enroll(context.Background(), "pairing", registration); err != nil {
		t.Fatalf("client rejected the fingerprint its own bootstrap script would embed: %v", err)
	}
	// A server presenting a different leaf must be refused by the pin, exactly
	// as a rogue Hub impersonating the address would be.
	if _, err := NewClient(server.URL, strings.Repeat("a", 64)).Enroll(context.Background(), "pairing", registration); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("mismatching fingerprint error = %v, want a fingerprint mismatch", err)
	}
}
