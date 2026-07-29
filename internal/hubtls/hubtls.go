// Package hubtls manages the Hub's local self-signed HTTPS identity.
package hubtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	certificateFileName = "hub-cert.pem"
	keyFileName         = "hub-key.pem"
)

// EnsureSelfSigned returns the paths and SHA-256 fingerprint of the Hub's
// self-signed ECDSA certificate. Existing, valid matching files are reused.
// New files are written through same-directory temporary files and renamed
// into place so readers never observe a partially written file.
func EnsureSelfSigned(dataDir string) (certificatePath, keyPath, fingerprint string, err error) {
	if dataDir == "" {
		return "", "", "", fmt.Errorf("Hub TLS data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", "", "", fmt.Errorf("create Hub TLS data directory: %w", err)
	}

	certificatePath = filepath.Join(dataDir, certificateFileName)
	keyPath = filepath.Join(dataDir, keyFileName)
	if fingerprint, ok := validExistingPair(certificatePath, keyPath); ok {
		return certificatePath, keyPath, fingerprint, nil
	}

	certificatePEM, keyPEM, fingerprint, err := generateCertificate()
	if err != nil {
		return "", "", "", fmt.Errorf("generate Hub TLS certificate: %w", err)
	}
	if err := writeAtomically(certificatePath, certificatePEM, 0o644); err != nil {
		return "", "", "", fmt.Errorf("write Hub TLS certificate: %w", err)
	}
	if err := writeAtomically(keyPath, keyPEM, 0o600); err != nil {
		return "", "", "", fmt.Errorf("write Hub TLS key: %w", err)
	}
	if _, ok := validExistingPair(certificatePath, keyPath); !ok {
		return "", "", "", fmt.Errorf("generated Hub TLS certificate and key do not form a valid pair")
	}
	return certificatePath, keyPath, fingerprint, nil
}

// FingerprintCertificate reports the SHA-256 fingerprint a Worker pins, read
// from a certificate file on disk. It exists so a running Hub can tell an
// operator the value to paste into `worker enroll` without the fingerprint
// having to be threaded through startup and held in memory — the certificate
// file is already the authoritative copy.
func FingerprintCertificate(certificatePath string) (string, error) {
	raw, err := os.ReadFile(certificatePath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s does not contain a PEM certificate", certificatePath)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(sum[:]), nil
}

func validExistingPair(certificatePath, keyPath string) (string, bool) {
	pair, err := tls.LoadX509KeyPair(certificatePath, keyPath)
	if err != nil || len(pair.Certificate) == 0 {
		return "", false
	}
	if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
		return "", false
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || certificate.PublicKeyAlgorithm != x509.ECDSA {
		return "", false
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return "", false
	}
	if err := certificate.CheckSignatureFrom(certificate); err != nil {
		return "", false
	}
	if err := certificate.VerifyHostname("localhost"); err != nil {
		return "", false
	}
	sum := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(sum[:]), true
}

func generateCertificate() (certificatePEM, keyPEM []byte, fingerprint string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", err
	}
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, "", err
	}
	if serialNumber.Sign() == 0 {
		serialNumber.SetInt64(1)
	}

	now := time.Now().UTC()
	dnsNames, ipAddresses := localCertificateNames()
	template := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: "Timingdex Hub"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, "", err
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, "", err
	}

	sum := sha256.Sum256(certificateDER)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}),
		hex.EncodeToString(sum[:]), nil
}

func localCertificateNames() ([]string, []net.IP) {
	dnsNames := []string{"localhost"}
	if hostname, err := os.Hostname(); err == nil && hostname != "" && hostname != "localhost" {
		dnsNames = append(dnsNames, hostname)
	}

	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	seen := map[string]struct{}{"127.0.0.1": {}, "::1": {}}
	interfaces, err := net.Interfaces()
	if err != nil {
		return dnsNames, ipAddresses
	}
	for _, networkInterface := range interfaces {
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip := addressIP(address)
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			key := ip.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			ipAddresses = append(ipAddresses, ip)
		}
	}
	return dnsNames, ipAddresses
}

func addressIP(address net.Addr) net.IP {
	switch address := address.(type) {
	case *net.IPNet:
		return address.IP
	case *net.IPAddr:
		return address.IP
	default:
		host, _, err := net.SplitHostPort(address.String())
		if err != nil {
			return net.ParseIP(address.String())
		}
		return net.ParseIP(host)
	}
}

func writeAtomically(path string, data []byte, mode os.FileMode) (err error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}
