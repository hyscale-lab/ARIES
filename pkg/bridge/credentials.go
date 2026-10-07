package bridge

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"time"
)

// ControlCredentials are private staged files, never transport metadata.
type ControlCredentials struct{ CA, ServerCert, ServerKey, ClientCert, ClientKey []byte }

func newControlCredentials() (ControlCredentials, error) {
	var b ControlCredentials
	now := time.Now()
	serial := func() *big.Int { n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120)); return n }
	caPub, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return b, err
	}
	ca := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "aries bridge control"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, caPub, caKey)
	if err != nil {
		return b, err
	}
	b.CA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	issue := func(server bool) ([]byte, []byte, error) {
		pub, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, nil, e
		}
		cert := &x509.Certificate{SerialNumber: serial(), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature}
		if server {
			cert.DNSNames = []string{"aries-bridge"}
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		} else {
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		encoded, e := x509.CreateCertificate(rand.Reader, cert, ca, pub, caKey)
		if e != nil {
			return nil, nil, e
		}
		k, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			return nil, nil, e
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k}), nil
	}
	b.ServerCert, b.ServerKey, err = issue(true)
	if err != nil {
		return b, err
	}
	b.ClientCert, b.ClientKey, err = issue(false)
	return b, err
}
func controlTLS(ca, cert, key []byte, server bool) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid control CA")
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, errors.New("invalid control identity")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: pool, ServerName: "aries-bridge"}
	if server {
		config.ClientCAs = pool
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return config, nil
}
