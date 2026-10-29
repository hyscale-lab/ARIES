package main

import (
	"context"
	"testing"
	"time"
)

func TestNativeReferenceCapturesEachChildAndExcludesWarmups(t *testing.T) {
	result, err := measure(t.Context(), `printf native`, 3, 2)
	if err != nil || result.Output != "native" || len(result.SamplesMS) != 3 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, elapsed := range result.SamplesMS {
		if elapsed <= 0 {
			t.Fatalf("invalid child wall duration %v", elapsed)
		}
	}
}
func TestNativeReferenceRejectsFailedOrChangingChildren(t *testing.T) {
	for _, body := range []string{`exit 17`, `printf 'warning' >&2`, `echo $$`} {
		if _, err := measure(t.Context(), body, 2, 0); err == nil {
			t.Fatalf("accepted unreliable reference %q", body)
		}
	}
}
func TestNativeReferenceCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := measure(ctx, `sleep 60`, 1, 0); err == nil {
		t.Fatal("canceled native child succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("canceled reference failed to stop child")
	}
}
