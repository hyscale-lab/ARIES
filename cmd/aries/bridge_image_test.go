package main

import (
	"strings"
	"testing"
)

func TestKubernetesBridgeImageAppendsTheBridgeCommit(t *testing.T) {
	previous := bridgeRevision
	t.Cleanup(func() { bridgeRevision = previous })

	bridgeRevision = "0123456789ab"
	if image, err := kubernetesBridgeImage("aries-bridge:v1"); err != nil || image != "aries-bridge:v1-0123456789ab" {
		t.Fatalf("image = %q, %v", image, err)
	}
	bridgeRevision = "0123456789ab-dirty"
	if image, err := kubernetesBridgeImage("aries-bridge:v1"); err != nil || image != "aries-bridge:v1-0123456789ab-dirty" {
		t.Fatalf("dirty image = %q, %v", image, err)
	}
	bridgeRevision = "main"
	if _, err := kubernetesBridgeImage("aries-bridge:v1"); err == nil {
		t.Fatal("a revision that is not a commit was accepted")
	}
	bridgeRevision = ""
	if _, err := kubernetesBridgeImage("aries-bridge:v1"); err == nil || !strings.Contains(err.Error(), "make build") {
		t.Fatalf("unstamped = %v", err)
	}
}
