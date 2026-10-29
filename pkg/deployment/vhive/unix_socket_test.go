package vhive

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortUnixSocketPathConnectsThroughLongDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("long-directory-", 12))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "firecracker.vsock")
	if len(path) <= 107 {
		t.Fatal("test socket path is not long")
	}
	bindPath, closeBind, err := shortUnixSocketPath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBind()
	listener, err := net.Listen("unix", bindPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dialPath, closeDial, err := shortUnixSocketPath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDial()
	delivered := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_, err = io.WriteString(conn, "guest")
			_ = conn.Close()
		}
		delivered <- err
	}()
	conn, err := net.Dial("unix", dialPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	_ = conn.Close()
	if err != nil || string(data) != "guest" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	closeDial()
	closeDial()
	if _, err := os.Stat(dialPath); !os.IsNotExist(err) {
		t.Fatalf("directory descriptor remained open: %v", err)
	}
}

func TestShortUnixSocketPathLeavesShortPathsIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guest.sock")
	short, closeDir, err := shortUnixSocketPath(path)
	if err != nil || short != path {
		t.Fatalf("short=%q err=%v", short, err)
	}
	closeDir()
}
