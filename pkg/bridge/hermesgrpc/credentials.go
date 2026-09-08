package hermesgrpc

// Per-task credential material. Two self-signed Ed25519 certificates are
// generated per session, one per side, and each side both trusts the other's
// certificate and pins its exact bytes. There is no certificate authority:
// exactly one peer is ever authorized, which mirrors the SSH bridge comparing
// one marshalled public key rather than validating a chain.
//
// The material exists only for the life of one task. The private key is
// removed at revocation; the certificate is retained as evidence of what the
// harness was told to trust.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// randomSessionID returns the value the harness must echo in the
// aries-session-id header on every call.
func randomSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate Hermes gRPC session identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// generateSessionCertificates returns the server's TLS certificate, the
// client's parsed certificate for pinning, and the client's certificate and
// key in PEM form for staging into the harness container.
func generateSessionCertificates(gateway string) (tls.Certificate, *x509.Certificate, []byte, []byte, error) {
	address := net.ParseIP(gateway)
	if address == nil {
		return tls.Certificate{}, nil, nil, nil, fmt.Errorf("task network gateway %q is not an IP address", gateway)
	}

	serverPEM, serverKeyPEM, err := selfSignedCertificate("aries-bridge", []net.IP{address})
	if err != nil {
		return tls.Certificate{}, nil, nil, nil, fmt.Errorf("generate Hermes gRPC server certificate: %w", err)
	}
	clientPEM, clientKeyPEM, err := selfSignedCertificate(lockedUsername, nil)
	if err != nil {
		return tls.Certificate{}, nil, nil, nil, fmt.Errorf("generate Hermes gRPC client certificate: %w", err)
	}

	serverCertificate, err := tls.X509KeyPair(serverPEM, serverKeyPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, nil, fmt.Errorf("load Hermes gRPC server keypair: %w", err)
	}
	clientCertificate, err := parseCertificate(clientPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, nil, err
	}
	return serverCertificate, clientCertificate, clientPEM, clientKeyPEM, nil
}

// selfSignedCertificate issues one Ed25519 certificate that is its own issuer.
// IsCA is set so each side can place the peer's certificate directly in a
// trust pool; the pin in VerifyPeerCertificate is what actually restricts the
// peer to one identity.
func selfSignedCertificate(commonName string, addresses []net.IP) ([]byte, []byte, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(certificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           addresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certificatePEM, keyPEM, nil
}

func parseCertificate(certificatePEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("decode Hermes gRPC certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse Hermes gRPC certificate: %w", err)
	}
	return certificate, nil
}

// pinnedPeer accepts exactly one peer certificate, compared by raw bytes.
func pinnedPeer(expected *x509.Certificate) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCertificates [][]byte, _ [][]*x509.Certificate) error {
		for _, raw := range rawCertificates {
			if len(raw) == len(expected.Raw) && subtleEqual(raw, expected.Raw) {
				return nil
			}
		}
		return errors.New("peer certificate rejected")
	}
}

func subtleEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func listenerHost(listener net.Listener) string {
	host, _, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return ""
	}
	return host
}

func listenerPort(listener net.Listener) string {
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return ""
	}
	return port
}
