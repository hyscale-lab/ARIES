package sglang

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func helper(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "python helper")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSGLangProcessPreservesArgvAndStopsProcessGroup(t *testing.T) {
	args := filepath.Join(t.TempDir(), "args")
	executable := helper(t, `printf '%s\n' "$@" > "$ARGS_OUT"; sleep 60 & wait`)
	t.Setenv("ARGS_OUT", args)
	configPath := filepath.Join(t.TempDir(), "native config.yaml")
	runtime, err := New(Options{Executable: executable, ConfigPath: configPath, OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(args); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not record argv")
		}
		time.Sleep(time.Millisecond)
	}
	content, _ := os.ReadFile(args)
	want := "-m\nsglang.launch_server\n--config\n" + configPath + "\n"
	if string(content) != want {
		t.Fatalf("argv=%q want=%q", content, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if runtime.Err() != nil {
		t.Fatalf("intentional stop err=%v", runtime.Err())
	}
}

func TestIntentionalStopIsNotUnexpectedExit(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "sleep 60"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	<-runtime.Done()
	if runtime.Err() != nil {
		t.Fatalf("Err=%v", runtime.Err())
	}
}

func TestStopRetryNeverSignalsAfterDone(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "sleep 60"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	signals := 0
	originalSignal := runtime.signalGroup
	runtime.signalGroup = func(pid int, signal syscall.Signal) error { signals++; return originalSignal(pid, signal) }
	runtime.probeGroup = func(int) error { return os.ErrPermission }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Stop(ctx); err == nil {
		t.Fatal("expected unconfirmed absence")
	}
	afterDoneSignals := signals
	runtime.probeGroup = func(int) error { return syscall.ESRCH }
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop=%v", err)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("stable Stop=%v", err)
	}
	if signals != afterDoneSignals {
		t.Fatalf("retry signaled after Done: before=%d after=%d", afterDoneSignals, signals)
	}
}

func TestCanceledStopEscalatesAndConfirmsAbsence(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "trap '' TERM; sleep 60"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUnexpectedExitPublishesErrorBeforeDone(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "exit 0"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-runtime.Done()
	if runtime.Err() == nil {
		t.Fatal("expected unexpected exit")
	}
	if err := runtime.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpectedly") {
		t.Fatalf("Stop=%v", err)
	}
}

func TestSGLangHealthRouteBoundsAndClassification(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		redirect bool
		retry    bool
	}{{"ready", 200, false, false}, {"starting", 503, false, true}, {"rate", 429, false, true}, {"terminal", 404, false, false}, {"redirect", 302, true, false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seenPath := ""
			runtime := &Runtime{options: Options{BaseURL: "http://example.invalid/health"}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				seenPath = r.URL.Path
				if r.Header.Get("Authorization") != "" {
					t.Error("authorization sent")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			})}}
			err := runtime.Health(context.Background())
			if tc.status == 200 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var health *healthError
				if !errors.As(err, &health) || health.Retryable() != tc.retry {
					t.Fatalf("err=%v", err)
				}
			}
			if seenPath != "/health" {
				t.Fatalf("path=%q", seenPath)
			}
		})
	}
}

func TestHealthRejectsReadErrorsAndDoesNotExposeBody(t *testing.T) {
	runtime := &Runtime{options: Options{BaseURL: "http://example.invalid/health"}, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("body-canary"))}, nil
	})}}
	err := runtime.Health(context.Background())
	if err == nil || strings.Contains(err.Error(), "body-canary") {
		t.Fatalf("err=%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
