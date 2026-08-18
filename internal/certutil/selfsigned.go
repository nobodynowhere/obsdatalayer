package certutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SelfSignedRequest describes the certificate files and identities to generate.
type SelfSignedRequest struct {
	CertFile  string
	KeyFile   string
	Hosts     []string
	ValidFor  time.Duration
	Overwrite bool
}

// GenerateSelfSigned writes a self-signed server certificate and ECDSA private
// key. Hosts are written as subject alternative names.
func GenerateSelfSigned(req SelfSignedRequest) error {
	if req.CertFile == "" {
		return fmt.Errorf("certificate file path is required")
	}
	if req.KeyFile == "" {
		return fmt.Errorf("private key file path is required")
	}
	if len(req.Hosts) == 0 {
		return fmt.Errorf("at least one DNS name or IP address is required")
	}
	if req.ValidFor <= 0 {
		return fmt.Errorf("certificate validity must be positive")
	}
	if !req.Overwrite {
		if exists(req.CertFile) {
			return fmt.Errorf("certificate file %s already exists", req.CertFile)
		}
		if exists(req.KeyFile) {
			return fmt.Errorf("private key file %s already exists", req.KeyFile)
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate private key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial number: %w", err)
	}

	notBefore := time.Now().Add(-time.Minute)
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: req.Hosts[0],
		},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(req.ValidFor),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, host := range req.Hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	if len(template.DNSNames) == 0 && len(template.IPAddresses) == 0 {
		return fmt.Errorf("at least one non-empty DNS name or IP address is required")
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}

	if err := writePEM(req.CertFile, 0o644, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return err
	}
	if err := writePEM(req.KeyFile, 0o600, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return err
	}
	return nil
}

func writePEM(path string, perm os.FileMode, block *pem.Block) error {
	path = os.ExpandEnv(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parent directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, block); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(os.ExpandEnv(path))
	return err == nil
}
