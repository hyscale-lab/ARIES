package main

import (
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func TestNextjsSetupBindsReviewedTrials(t *testing.T) {
	setup, ok := sourceSetup(&harbor.Recording{Task: nextjsTask, Trial: nextjsTrial})
	if !ok || setup != nextjsSetup {
		t.Fatal("reviewed Next.js source did not receive its fixture checks")
	}
	setup, ok = sourceSetup(&harbor.Recording{Task: nextjsTask, Trial: nextjsWave2Trial})
	if !ok || !strings.Contains(setup.initialCheck, "20260831T080311Z") ||
		!strings.Contains(setup.initialCheck, "/usr/local/bin/python3") {
		t.Fatal("wave two Next.js source did not receive the dated Python fixture")
	}
	if _, ok := sourceSetup(&harbor.Recording{Task: nextjsTask, Trial: "117ff592-3774-4ad2-bcc4-7d0e6292c096"}); ok {
		t.Fatal("another Next.js recording received a ready-state profile")
	}
}
