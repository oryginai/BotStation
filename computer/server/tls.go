package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// loadOrCreateCert uses TLS_CERT/TLS_KEY when given. Otherwise it makes a
// self-signed certificate for TLS_HOSTS once and keeps it in TLS_DIR, so the
// fingerprint stays the same across restarts of the same container.
func loadOrCreateCert(cfg config) (tls.Certificate, error) {
	certFile, keyFile := cfg.tlsCert, cfg.tlsKey
	if certFile == "" {
		certFile = filepath.Join(cfg.tlsDir, "cert.pem")
		keyFile = filepath.Join(cfg.tlsDir, "key.pem")
		if _, err := os.Stat(certFile); os.IsNotExist(err) {
			if err := createSelfSigned(certFile, keyFile, cfg.tlsHosts); err != nil {
				return tls.Certificate{}, err
			}
		}
		log.Printf("using a self-signed certificate (set TLS_CERT/TLS_KEY for a real one)")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return cert, err
	}
	sum := sha256.Sum256(cert.Certificate[0])
	log.Printf("TLS certificate SHA-256 fingerprint: %X", sum)
	return cert, nil
}

func createSelfSigned(certFile, keyFile string, hosts []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "desktopd"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h == "" {
			continue
		} else if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	log.Printf("created a self-signed certificate for %s", strings.Join(hosts, ", "))
	return nil
}
