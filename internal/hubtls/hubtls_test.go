package hubtls

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
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
