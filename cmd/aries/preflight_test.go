package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
)

type preflightReply struct {
	status int
	body   string
	err    error
}

type preflightDoer struct {
	t              *testing.T
	replies        []preflightReply
	requests       int
	authorizations []string
}

type failingResponseBody struct {
	err    error
	closed bool
}

func (body *failingResponseBody) Read([]byte) (int, error) {
	return 0, body.err
}

func (body *failingResponseBody) Close() error {
	body.closed = true
	return nil
}

type failingResponseDoer struct {
	body     *failingResponseBody
	requests int
}

type sglangPreflightDoer struct {
	t             *testing.T
	body          string
	status        int
	requests      int
	authorization string
	wantURL       string
}

type sequenceSGLangDoer struct {
	t       *testing.T
	replies []preflightReply
}

func (doer *sequenceSGLangDoer) Do(request *http.Request) (*http.Response, error) {
	doer.t.Helper()
	if request.Method != http.MethodGet || request.URL.String() != "http://fake.invalid/v1/models" || len(doer.replies) == 0 {
		doer.t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
	}
	reply := doer.replies[0]
	doer.replies = doer.replies[1:]
	if reply.err != nil {
		return nil, reply.err
	}
	return &http.Response{StatusCode: reply.status, Body: io.NopCloser(strings.NewReader(reply.body)), Request: request}, nil
}

func (doer *sglangPreflightDoer) Do(request *http.Request) (*http.Response, error) {
	doer.t.Helper()
	doer.requests++
	doer.authorization = request.Header.Get("Authorization")
	if request.Method != http.MethodGet || request.URL.String() != doer.wantURL {
		doer.t.Fatalf("request = %s %s", request.Method, request.URL)
	}
	status := doer.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(doer.body)), Request: request}, nil
}

func (doer *failingResponseDoer) Do(request *http.Request) (*http.Response, error) {
	doer.requests++
	return &http.Response{StatusCode: http.StatusOK, Body: doer.body, Request: request}, nil
}

func (doer *preflightDoer) Do(request *http.Request) (*http.Response, error) {
	doer.t.Helper()
	doer.requests++
	doer.authorizations = append(doer.authorizations, request.Header.Get("Authorization"))
	if request.Method != http.MethodGet || request.URL.String() != deepSeekModelsURL || request.Body != nil {
		doer.t.Fatalf("unexpected request: %s %s body=%v", request.Method, request.URL, request.Body)
	}
	if len(doer.replies) == 0 {
		doer.t.Fatal("unexpected preflight request")
	}
	reply := doer.replies[0]
	doer.replies = doer.replies[1:]
	if reply.err != nil {
		return nil, reply.err
	}
	return &http.Response{
		StatusCode: reply.status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(reply.body)),
		Request:    request,
	}, nil
}

func officialDeepSeekModel() config.ModelConfig {
	return config.ModelConfig{Provider: "deepseek", BaseURL: deepSeekBaseURL, Model: "deepseek-v4-flash", APIKeyEnv: deepSeekAPIKey}
}

func TestValidateLiveModelConfirmsExactModelAndClearsCredentialBuffer(t *testing.T) {
	secret := []byte("synthetic-preflight-key")
	returned := append([]byte(nil), secret...)
	doer := &preflightDoer{t: t, replies: []preflightReply{{
		status: http.StatusOK,
		body:   `{"object":"list","data":[{"id":"other"},{"id":"deepseek-v4-flash"}]}`,
	}}}
	validation, err := validateLiveModel(context.Background(), officialDeepSeekModel(), func(name string) ([]byte, bool) {
		if name != deepSeekAPIKey {
			t.Fatalf("lookup name = %q", name)
		}
		return returned, true
	}, doer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != liveValidationSucceeded || validation.Category != liveValidationConfirmed || validation.Attempts != 1 || doer.requests != 1 {
		t.Fatalf("validation = %+v, requests=%d", validation, doer.requests)
	}
	if len(doer.authorizations) != 1 || doer.authorizations[0] != "Bearer "+string(secret) {
		t.Fatalf("authorization = %q", doer.authorizations)
	}
	if !bytes.Equal(returned, make([]byte, len(returned))) {
		t.Fatal("preflight did not clear the lookup buffer")
	}
	encoded, marshalErr := json.Marshal(validation)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if bytes.Contains(encoded, secret) || strings.Contains(errString(err), string(secret)) {
		t.Fatal("preflight result exposed the credential")
	}
}

func TestValidateLiveModelRetriesOnlyTransport500And503(t *testing.T) {
	transportErr := errors.New("synthetic transport failure")
	tests := []struct {
		name  string
		first preflightReply
	}{
		{name: "transport", first: preflightReply{err: transportErr}},
		{name: "500", first: preflightReply{status: http.StatusInternalServerError, body: "internal"}},
		{name: "503", first: preflightReply{status: http.StatusServiceUnavailable, body: "unavailable"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doer := &preflightDoer{t: t, replies: []preflightReply{
				test.first,
				{status: http.StatusOK, body: `{"data":[{"id":"deepseek-v4-flash"}]}`},
			}}
			var sleeps []time.Duration
			validation, err := validateLiveModel(context.Background(), officialDeepSeekModel(), syntheticLookup, doer, func(_ context.Context, duration time.Duration) error {
				sleeps = append(sleeps, duration)
				return nil
			})
			if err != nil || validation.Status != liveValidationSucceeded || validation.Attempts != 2 || doer.requests != 2 {
				t.Fatalf("validation=%+v requests=%d err=%v", validation, doer.requests, err)
			}
			if len(sleeps) != 1 || sleeps[0] != 2*time.Second {
				t.Fatalf("sleeps = %v", sleeps)
			}
		})
	}
}

func TestValidateLiveModelFailureCategoriesDoNotRetry(t *testing.T) {
	tests := []struct {
		name     string
		reply    preflightReply
		category liveValidationCategory
	}{
		{name: "unauthorized", reply: preflightReply{status: http.StatusUnauthorized}, category: liveValidationUnauthorized},
		{name: "forbidden", reply: preflightReply{status: http.StatusForbidden}, category: liveValidationForbidden},
		{name: "rate limited", reply: preflightReply{status: http.StatusTooManyRequests}, category: liveValidationRateLimited},
		{name: "redirect", reply: preflightReply{status: http.StatusFound}, category: liveValidationRedirect},
		{name: "other status", reply: preflightReply{status: http.StatusTeapot}, category: liveValidationHTTP},
		{name: "malformed", reply: preflightReply{status: http.StatusOK, body: "not-json"}, category: liveValidationMalformed},
		{name: "model missing", reply: preflightReply{status: http.StatusOK, body: `{"data":[{"id":"other"}]}`}, category: liveValidationModelMissing},
		{name: "oversized", reply: preflightReply{status: http.StatusOK, body: strings.Repeat("x", deepSeekMaxResponseBytes+1)}, category: liveValidationResponseTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doer := &preflightDoer{t: t, replies: []preflightReply{test.reply}}
			slept := false
			validation, err := validateLiveModel(context.Background(), officialDeepSeekModel(), syntheticLookup, doer, func(context.Context, time.Duration) error {
				slept = true
				return nil
			})
			if err == nil || validation.Status != liveValidationFailed || validation.Category != test.category || validation.Attempts != 1 {
				t.Fatalf("validation=%+v err=%v", validation, err)
			}
			if doer.requests != 1 || slept {
				t.Fatalf("requests=%d slept=%v", doer.requests, slept)
			}
		})
	}
}

func TestValidateLiveModelResponseReadFailureIsClosedSanitizedAndNotRetried(t *testing.T) {
	credential := "synthetic-preflight-key"
	bodyCanary := "provider-body-must-not-leak"
	body := &failingResponseBody{err: errors.New(bodyCanary)}
	doer := &failingResponseDoer{body: body}
	slept := false
	validation, err := validateLiveModel(context.Background(), officialDeepSeekModel(), func(string) ([]byte, bool) {
		return []byte(credential), true
	}, doer, func(context.Context, time.Duration) error {
		slept = true
		return nil
	})
	if err == nil || validation.Status != liveValidationFailed || validation.Category != liveValidationResponseRead || validation.Attempts != 1 {
		t.Fatalf("validation=%+v err=%v", validation, err)
	}
	if doer.requests != 1 || slept || !body.closed {
		t.Fatalf("requests=%d slept=%v body closed=%v", doer.requests, slept, body.closed)
	}
	encoded, marshalErr := json.Marshal(validation)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, secret := range []string{credential, bodyCanary} {
		if strings.Contains(err.Error(), secret) || bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("preflight result exposed %q", secret)
		}
	}
}

func TestValidateLiveModelBoundsAttemptsAndSanitizesTerminalFailures(t *testing.T) {
	secret := "synthetic-do-not-leak"
	for _, test := range []struct {
		name     string
		replies  []preflightReply
		category liveValidationCategory
	}{
		{name: "transport", replies: []preflightReply{{err: errors.New(secret)}, {err: errors.New(secret)}}, category: liveValidationTransport},
		{name: "server", replies: []preflightReply{{status: 503, body: secret}, {status: 503, body: secret}}, category: liveValidationServer},
	} {
		t.Run(test.name, func(t *testing.T) {
			doer := &preflightDoer{t: t, replies: test.replies}
			validation, err := validateLiveModel(context.Background(), officialDeepSeekModel(), syntheticLookup, doer, func(context.Context, time.Duration) error { return nil })
			if err == nil || validation.Category != test.category || validation.Attempts != 2 || doer.requests != 2 {
				t.Fatalf("validation=%+v requests=%d err=%v", validation, doer.requests, err)
			}
			encoded, _ := json.Marshal(validation)
			if strings.Contains(err.Error(), secret) || bytes.Contains(encoded, []byte(secret)) {
				t.Fatal("sanitized terminal result exposed provider content")
			}
		})
	}
}

func TestValidateLiveModelConfigurationCredentialAndCancellation(t *testing.T) {
	t.Run("unknown provider", func(t *testing.T) {
		called := false
		model := officialDeepSeekModel()
		model.Provider = ""
		validation, err := validateLiveModel(context.Background(), model, func(string) ([]byte, bool) {
			called = true
			return nil, false
		}, nil, nil)
		if err == nil || called || validation.Status != liveValidationFailed || validation.Category != liveValidationConfigurationInvalid || validation.Attempts != 0 {
			t.Fatalf("validation=%+v called=%v err=%v", validation, called, err)
		}
	})
	t.Run("sglang", func(t *testing.T) {
		model := config.ModelConfig{Provider: "sglang", BaseURL: "http://fake.invalid/v1", Model: "local", APIKeyEnv: "SGLANG_API_KEY"}
		doer := &sglangPreflightDoer{t: t, wantURL: "http://fake.invalid/v1/models", body: `{"data":[{"id":"local"}]}`}
		validation, err := validateLiveModel(context.Background(), model, func(string) ([]byte, bool) { return []byte("dummy"), true }, doer, nil)
		if err != nil || validation.Provider != "sglang" || validation.Status != liveValidationSucceeded || validation.Attempts != 1 || doer.authorization != "Bearer dummy" {
			t.Fatalf("validation=%+v auth=%q error=%v", validation, doer.authorization, err)
		}
		encoded, _ := json.Marshal(validation)
		if bytes.Contains(encoded, []byte("dummy")) {
			t.Fatal("validation exposed key")
		}
	})
	t.Run("wrong environment", func(t *testing.T) {
		model := officialDeepSeekModel()
		model.APIKeyEnv = "OTHER_KEY"
		validation, err := validateLiveModel(context.Background(), model, syntheticLookup, nil, nil)
		assertPreflightFailure(t, validation, err, liveValidationConfigurationInvalid, 0)
	})
	t.Run("missing credential", func(t *testing.T) {
		validation, err := validateLiveModel(context.Background(), officialDeepSeekModel(), func(string) ([]byte, bool) { return nil, false }, nil, nil)
		assertPreflightFailure(t, validation, err, liveValidationCredentialMissing, 0)
	})
	t.Run("invalid credential", func(t *testing.T) {
		key := []byte("bad\nkey")
		validation, err := validateLiveModel(context.Background(), officialDeepSeekModel(), func(string) ([]byte, bool) { return key, true }, nil, nil)
		assertPreflightFailure(t, validation, err, liveValidationCredentialInvalid, 0)
		if !bytes.Equal(key, make([]byte, len(key))) {
			t.Fatal("invalid credential buffer was not cleared")
		}
	})
	t.Run("canceled during retry delay", func(t *testing.T) {
		doer := &preflightDoer{t: t, replies: []preflightReply{{err: errors.New("temporary")}}}
		validation, err := validateLiveModel(context.Background(), officialDeepSeekModel(), syntheticLookup, doer, func(context.Context, time.Duration) error {
			return context.Canceled
		})
		assertPreflightFailure(t, validation, err, liveValidationCanceled, 1)
	})
}

func TestOfficialDeepSeekSelectionIsExact(t *testing.T) {
	for _, model := range []config.ModelConfig{
		{Provider: "deepseek", BaseURL: deepSeekBaseURL, Model: "deepseek-v4-flash"},
		{Provider: "deepseek", BaseURL: deepSeekBaseURL, Model: "deepseek-v4-pro"},
	} {
		if !isOfficialDeepSeek(model) {
			t.Fatalf("official model not selected: %+v", model)
		}
	}
	for _, model := range []config.ModelConfig{
		{Provider: "deepseek", BaseURL: deepSeekBaseURL + "/", Model: "deepseek-v4-flash"},
		{Provider: "deepseek", BaseURL: deepSeekBaseURL, Model: "deepseek-v4"},
		{Provider: "deepseek", BaseURL: "http://api.deepseek.com", Model: "deepseek-v4-pro"},
	} {
		if isOfficialDeepSeek(model) {
			t.Fatalf("near match selected: %+v", model)
		}
	}
}

func TestSGLangPreflightFailsClosedWithSanitizedCategories(t *testing.T) {
	model := config.ModelConfig{Provider: "sglang", BaseURL: "http://fake.invalid/v1", Model: "local", APIKeyEnv: "SGLANG_API_KEY"}
	for _, test := range []struct {
		name     string
		status   int
		body     string
		category liveValidationCategory
	}{
		{"missing", 200, `{"data":[{"id":"other"}]}`, liveValidationModelMissing},
		{"malformed", 200, "body-canary", liveValidationMalformed},
		{"unauthorized", 401, "body-canary", liveValidationUnauthorized},
		{"redirect", 302, "body-canary", liveValidationRedirect},
		{"oversized", 200, strings.Repeat("x", (1<<20)+1), liveValidationResponseTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			doer := &sglangPreflightDoer{t: t, wantURL: "http://fake.invalid/v1/models", status: test.status, body: test.body}
			validation, err := validateLiveModel(context.Background(), model, func(string) ([]byte, bool) { return []byte("key-canary"), true }, doer, nil)
			if err == nil || validation.Category != test.category || validation.Provider != "sglang" || validation.Attempts != 1 {
				t.Fatalf("validation=%+v error=%v", validation, err)
			}
			encoded, _ := json.Marshal(validation)
			for _, canary := range []string{"key-canary", "body-canary"} {
				if strings.Contains(err.Error(), canary) || bytes.Contains(encoded, []byte(canary)) {
					t.Fatalf("exposed %q", canary)
				}
			}
		})
	}
}

func TestManagedSGLangPreflightRetriesUntilReadyOrProcessExit(t *testing.T) {
	cfg := config.Config{
		Model:        config.ModelConfig{Provider: "sglang", BaseURL: "http://fake.invalid/v1", Model: "local", APIKeyEnv: "SGLANG_API_KEY"},
		ModelRuntime: config.ModelRuntimeConfig{Mode: "managed", StartupTimeout: time.Minute},
	}
	lookup := func(string) ([]byte, bool) { return []byte("dummy"), true }
	t.Run("ready", func(t *testing.T) {
		doer := &sequenceSGLangDoer{t: t, replies: []preflightReply{
			{err: errors.New("starting")},
			{status: http.StatusOK, body: `{"data":[{"id":"local"}]}`},
		}}
		validation, err := validateLiveModelForRuntime(
			context.Background(), cfg, &recordingModelRuntime{}, lookup, doer,
			func(context.Context, time.Duration) error { return nil },
		)
		if err != nil || validation.Status != liveValidationSucceeded || validation.Attempts != 2 {
			t.Fatalf("validation = %+v, error = %v", validation, err)
		}
	})
	t.Run("process exit", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		doer := &sequenceSGLangDoer{t: t, replies: []preflightReply{{err: errors.New("starting")}}}
		validation, err := validateLiveModelForRuntime(
			context.Background(), cfg, &recordingModelRuntime{done: done}, lookup, doer,
			func(context.Context, time.Duration) error { return nil },
		)
		if err == nil || validation.Category != liveValidationServer || validation.Attempts != 1 {
			t.Fatalf("validation = %+v, error = %v", validation, err)
		}
	})
}

func TestDeepSeekHTTPClientHasBoundedTimeoutAndRejectsRedirects(t *testing.T) {
	client := newDeepSeekHTTPClient()
	if client.Timeout != 10*time.Second || client.CheckRedirect == nil {
		t.Fatalf("client timeout=%s redirect configured=%t", client.Timeout, client.CheckRedirect != nil)
	}
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy = %v", err)
	}
}

func TestPersistLiveValidationIsPrivateExclusiveAndContainsNoCredential(t *testing.T) {
	for _, validation := range []liveValidation{
		{SchemaVersion: 1, Status: liveValidationSucceeded, Category: liveValidationConfirmed, Provider: "deepseek", BaseURL: deepSeekBaseURL, Model: "deepseek-v4-flash", Attempts: 1},
		{SchemaVersion: 1, Status: liveValidationFailed, Category: liveValidationUnauthorized, Provider: "deepseek", BaseURL: deepSeekBaseURL, Model: "deepseek-v4-flash", Attempts: 1},
	} {
		root := filepath.Join(t.TempDir(), "run")
		if err := createRunOutputRoot(root); err != nil {
			t.Fatal(err)
		}
		if err := persistLiveValidation(root, validation); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, liveValidationName)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %04o", info.Mode().Perm())
		}
		if err := persistLiveValidation(root, validation); !errors.Is(err, os.ErrExist) {
			t.Fatalf("second persist error = %v", err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(content, []byte("synthetic-preflight-key")) {
			t.Fatal("validation artifact contains a credential")
		}
	}
}

func syntheticLookup(name string) ([]byte, bool) {
	if name != deepSeekAPIKey {
		return nil, false
	}
	return []byte("synthetic-preflight-key"), true
}

func assertPreflightFailure(t *testing.T, validation liveValidation, err error, category liveValidationCategory, attempts int) {
	t.Helper()
	if err == nil || validation.Status != liveValidationFailed || validation.Category != category || validation.Attempts != attempts {
		t.Fatalf("validation=%+v err=%v", validation, err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
