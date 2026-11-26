package main

import (
	"testing"

	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func TestNextjsSetupBindsOneTrial(t *testing.T) {
	setup, ok := sourceSetup(&harbor.Recording{Task: nextjsTask, Trial: nextjsTrial})
	if !ok || setup != nextjsSetup {
		t.Fatal("reviewed Next.js source did not receive its fixture checks")
	}
	if _, ok := sourceSetup(&harbor.Recording{Task: nextjsTask, Trial: "09288c37-6d48-4073-a05a-1d5770fecef0"}); ok {
		t.Fatal("another Next.js recording received a ready-state profile")
	}
}
