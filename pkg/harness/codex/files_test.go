package codex

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestHTTPSRuntimeArchiveStagesCABundle(t *testing.T) {
	bundle, server := testPublicCABundle(t)
	filename := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(filename, bundle, 0o444); err != nil {
		t.Fatal(err)
	}
	// The explicit trusted file must not be replaced by process-wide SSL inputs.
	t.Setenv("SSL_CERT_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("SSL_CERT_DIR", filepath.Join(t.TempDir(), "missing"))
	manager, fake, request, _ := testManager(t, Options{CABundlePath: filename})
	request.Model.BaseURL = "https://api.example/v1"
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	entries := runtimeArchiveEntries(t, fake.archive)
	entry, ok := entries["etc/ssl/certs/ca-certificates.crt"]
	if !ok {
		t.Fatal("HTTPS runtime is missing its CA trust bundle")
	}
	if entry.header.Typeflag != tar.TypeReg || entry.header.Mode != 0o644 || entry.header.Uid != 0 || entry.header.Gid != 0 || !bytes.Equal(entry.content, bundle) {
		t.Fatalf("invalid CA trust bundle staging: %#v", entry.header)
	}
	for _, name := range []string{"etc", "etc/ssl", "etc/ssl/certs"} {
		entry, ok := entries[name]
		if !ok || entry.header.Typeflag != tar.TypeDir || entry.header.Mode != 0o755 || entry.header.Uid != 0 || entry.header.Gid != 0 {
			t.Fatalf("invalid CA directory %q: %#v", name, entry.header)
		}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(entry.content) {
		t.Fatal("staged CA trust bundle has no certificates")
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: "api.example"}); err != nil {
		t.Fatalf("staged CA does not verify its HTTPS server: %v", err)
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: "other.example"}); err == nil {
		t.Fatal("certificate hostname verification was lost")
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: x509.NewCertPool(), DNSName: "api.example"}); err == nil {
		t.Fatal("server certificate passed without its trusted CA")
	}
}

func TestHTTPRuntimeArchiveDoesNotRequireCABundle(t *testing.T) {
	manager, fake, request, _ := testManager(t, Options{CABundlePath: filepath.Join(t.TempDir(), "missing-ca.pem")})
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	for name := range runtimeArchiveEntries(t, fake.archive) {
		if name == "etc" || name == "etc/ssl" || name == "etc/ssl/certs" || name == "etc/ssl/certs/ca-certificates.crt" {
			t.Fatalf("HTTP staged a CA dependency at %q", name)
		}
	}
}

func TestHTTPSRejectsInvalidCABundleBeforeContainerCreation(t *testing.T) {
	bundle, _ := testPublicCABundle(t)
	for _, kind := range []string{"missing", "empty", "not-pem", "invalid-certificate", "private-key", "oversized", "symlink", "fifo", "directory", "writable"} {
		t.Run(kind, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "ca.pem")
			content := bundle
			switch kind {
			case "empty":
				content = nil
			case "not-pem":
				content = []byte("not a certificate")
			case "invalid-certificate":
				content = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid DER")})
			case "private-key":
				content = append(bytes.Clone(bundle), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("test-only-private-material")})...)
			}
			switch kind {
			case "missing":
			case "fifo":
				if err := syscall.Mkfifo(filename, 0o644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(filename, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filename + ".target"
				if err := os.WriteFile(target, bundle, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filename); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(filename, content, 0o644); err != nil {
					t.Fatal(err)
				}
				if kind == "oversized" {
					if err := os.Truncate(filename, maxCABundleBytes+1); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "writable" {
					if err := os.Chmod(filename, 0o666); err != nil {
						t.Fatal(err)
					}
				}
			}
			manager, fake, request, _ := testManager(t, Options{CABundlePath: filename})
			request.Model.BaseURL = "https://api.example/v1"
			if err := manager.Start(context.Background(), request); err == nil {
				t.Fatal("accepted an invalid HTTPS CA source")
			}
			if len(fake.calls) != 0 || len(fake.archive) != 0 {
				t.Fatal("invalid CA source reached the harness container")
			}
		})
	}
}

func testPublicCABundle(t *testing.T) ([]byte, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ARIES test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), DNSNames: []string{"api.example"},
		NotBefore: root.NotBefore, NotAfter: root.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), server
}

type runtimeArchiveEntry struct {
	header  tar.Header
	content []byte
}

func runtimeArchiveEntries(t *testing.T, archive []byte) map[string]runtimeArchiveEntry {
	t.Helper()
	entries := map[string]runtimeArchiveEntry{}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = runtimeArchiveEntry{header: *header, content: content}
	}
	return entries
}

func TestStaticExecutableRejectsScriptsDynamicELFAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "static")
	writeStaticELF(t, valid)
	if _, err := readStaticExecutable(valid); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, make([]byte, 56)...)
	binary.LittleEndian.PutUint64(content[32:], 64)
	binary.LittleEndian.PutUint16(content[54:], 56)
	binary.LittleEndian.PutUint16(content[56:], 1)
	binary.LittleEndian.PutUint32(content[64:], 3) // PT_INTERP
	dynamic := filepath.Join(dir, "dynamic")
	if err := os.WriteFile(dynamic, content, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dynamic, script, link} {
		if _, err := readStaticExecutable(name); err == nil {
			t.Errorf("accepted invalid executable %s", name)
		}
	}
}

func TestPrivateSourceRejectsFIFOWithoutBlocking(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(filename, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSource(filename, 0o600, maxOutputBytes); err == nil {
		t.Fatal("accepted FIFO as private source")
	}
}
