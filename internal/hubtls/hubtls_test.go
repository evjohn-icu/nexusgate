package hubtls

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureSelfSignedCreatesAndReusesECDSACertificate(t *testing.T) {
	dataDir := t.TempDir()

	certPath, keyPath, fingerprint, err := EnsureSelfSigned(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint == "" {
		t.Fatal("EnsureSelfSigned returned an empty certificate fingerprint")
	}
	if len(fingerprint) != sha256.Size*2 {
		t.Fatalf("fingerprint length=%d, want %d hexadecimal characters", len(fingerprint), sha256.Size*2)
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		t.Fatalf("fingerprint is not hexadecimal: %v", err)
	}

	certBytesBefore, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBytesBefore, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("generated certificate/key pair is not usable: %v", err)
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if certificate.PublicKeyAlgorithm != x509.ECDSA {
		t.Fatalf("public key algorithm=%v, want ECDSA", certificate.PublicKeyAlgorithm)
	}
	if err := certificate.VerifyHostname("localhost"); err != nil {
		t.Fatalf("certificate does not cover localhost: %v", err)
	}
	if certificate.CheckSignatureFrom(certificate) != nil {
		t.Fatal("certificate is not self-signed")
	}
	certificateFingerprint := sha256.Sum256(certificate.Raw)
	if fingerprint != hex.EncodeToString(certificateFingerprint[:]) {
		t.Fatalf("fingerprint does not match certificate DER")
	}

	certPathAgain, keyPathAgain, fingerprintAgain, err := EnsureSelfSigned(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if certPathAgain != certPath || keyPathAgain != keyPath {
		t.Fatalf("paths changed on reuse: first=(%q, %q), second=(%q, %q)", certPath, keyPath, certPathAgain, keyPathAgain)
	}
	if fingerprintAgain != fingerprint {
		t.Fatalf("fingerprint changed on reuse: first=%q, second=%q", fingerprint, fingerprintAgain)
	}
	certBytesAfter, err := os.ReadFile(certPathAgain)
	if err != nil {
		t.Fatal(err)
	}
	keyBytesAfter, err := os.ReadFile(keyPathAgain)
	if err != nil {
		t.Fatal(err)
	}
	if string(certBytesAfter) != string(certBytesBefore) {
		t.Fatal("certificate was regenerated instead of reused")
	}
	if string(keyBytesAfter) != string(keyBytesBefore) {
		t.Fatal("private key was regenerated instead of reused")
	}
	if filepath.Dir(certPath) != dataDir || filepath.Dir(keyPath) != dataDir {
		t.Fatalf("assets are outside data directory: cert=%q key=%q", certPath, keyPath)
	}
}

// TestCertificateSPKIPinDerivesCurlCompatiblePin pins the SPKI pin shape a
// generated bootstrap script must embed: `sha256//<base64>` over the leaf's
// SubjectPublicKeyInfo, so curl --pinnedpubkey accepts it against the same
// self-signed certificate the fingerprint was computed from.
func TestCertificateSPKIPinDerivesCurlCompatiblePin(t *testing.T) {
	certPath, _, fingerprint, err := EnsureSelfSigned(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := LoadCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	pin := CertificateSPKIPin(certificate)
	if !strings.HasPrefix(pin, "sha256//") {
		t.Fatalf("SPKI pin %q does not carry the curl sha256// prefix", pin)
	}
	encoded := strings.TrimPrefix(pin, "sha256//")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("SPKI pin is not valid base64: %v", err)
	}
	if len(decoded) != sha256.Size {
		t.Fatalf("SPKI pin digest length=%d, want %d", len(decoded), sha256.Size)
	}
	manual := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	if !strings.EqualFold(pin, "sha256//"+base64.StdEncoding.EncodeToString(manual[:])) {
		t.Fatalf("SPKI pin %q does not match manual SubjectPublicKeyInfo hash", pin)
	}
	fingerprintSum := sha256.Sum256(certificate.Raw)
	if fingerprint != hex.EncodeToString(fingerprintSum[:]) {
		t.Fatalf("LoadCertificate fingerprint does not match the certificate DER")
	}
}

// TestCertificateSPKIPinDiffersAcrossKeys proves the pin is key-bound: two
// freshly generated certificates never share an SPKI pin, so a stale pin
// cannot authenticate a different Hub identity.
func TestCertificateSPKIPinDiffersAcrossKeys(t *testing.T) {
	pinA := CertificateSPKIPin(loadTestCertificate(t, t.TempDir()))
	pinB := CertificateSPKIPin(loadTestCertificate(t, t.TempDir()))
	if pinA == pinB {
		t.Fatalf("two independent Hubs produced the same SPKI pin %q", pinA)
	}
}

func loadTestCertificate(t *testing.T, dataDir string) *x509.Certificate {
	t.Helper()
	certPath, _, _, err := EnsureSelfSigned(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := LoadCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
