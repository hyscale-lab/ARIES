package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
)

const (
	liveValidationName       = "live-validation.json"
	deepSeekBaseURL          = "https://api.deepseek.com"
	deepSeekModelsURL        = deepSeekBaseURL + "/models"
	deepSeekRequestTimeout   = 10 * time.Second
	deepSeekRetryDelay       = 2 * time.Second
	deepSeekMaxAttempts      = 2
	deepSeekMaxResponseBytes = 64 << 10
)

type liveValidationStatus string
type liveValidationCategory string

const (
	liveValidationNotRequested liveValidationStatus = "not_requested"
	liveValidationSucceeded    liveValidationStatus = "succeeded"
	liveValidationFailed       liveValidationStatus = "failed"

	liveValidationSkipped              liveValidationCategory = "not_requested"
	liveValidationConfirmed            liveValidationCategory = "model_confirmed"
	liveValidationConfigurationInvalid liveValidationCategory = "configuration_invalid"
	liveValidationCredentialMissing    liveValidationCategory = "credential_missing"
	liveValidationCredentialInvalid    liveValidationCategory = "credential_invalid"
	liveValidationCanceled             liveValidationCategory = "canceled"
	liveValidationTransport            liveValidationCategory = "transport_error"
	liveValidationServer               liveValidationCategory = "provider_unavailable"
	liveValidationUnauthorized         liveValidationCategory = "unauthorized"
	liveValidationForbidden            liveValidationCategory = "forbidden"
	liveValidationRateLimited          liveValidationCategory = "rate_limited"
	liveValidationRedirect             liveValidationCategory = "redirect_rejected"
	liveValidationHTTP                 liveValidationCategory = "unexpected_http_status"
	liveValidationResponseTooLarge     liveValidationCategory = "response_too_large"
	liveValidationResponseRead         liveValidationCategory = "response_read_error"
	liveValidationMalformed            liveValidationCategory = "malformed_response"
	liveValidationModelMissing         liveValidationCategory = "model_missing"
)

type liveValidation struct {
	SchemaVersion int                    `json:"schema_version"`
	Status        liveValidationStatus   `json:"status"`
	Category      liveValidationCategory `json:"category"`
	Provider      string                 `json:"provider"`
	BaseURL       string                 `json:"base_url"`
	Model         string                 `json:"model"`
	Attempts      int                    `json:"attempts"`
}

type liveValidationError struct {
	category liveValidationCategory
	attempts int
}

func (err *liveValidationError) Error() string {
	return fmt.Sprintf("DeepSeek preflight failed: %s after %d attempt(s)", err.category, err.attempts)
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type contextSleep func(context.Context, time.Duration) error

func validateLiveModel(
	ctx context.Context,
	model config.ModelConfig,
	lookup func(string) ([]byte, bool),
	client httpDoer,
	sleep contextSleep,
) (liveValidation, error) {
	if !isOfficialDeepSeek(model) {
		return liveValidation{
			SchemaVersion: 1, Status: liveValidationNotRequested, Category: liveValidationSkipped,
			Provider: "none", BaseURL: model.BaseURL, Model: model.Model,
		}, nil
	}
	if model.APIKeyEnv != deepSeekAPIKey {
		return liveValidationFailure(model, liveValidationConfigurationInvalid, 0)
	}
	if lookup == nil {
		return liveValidationFailure(model, liveValidationCredentialMissing, 0)
	}
	key, ok := lookup(model.APIKeyEnv)
	if !ok {
		clear(key)
		return liveValidationFailure(model, liveValidationCredentialMissing, 0)
	}
	defer clear(key)
	if len(key) == 0 || len(key) > maxAPIKeyBytes || bytes.ContainsAny(key, "\x00\r\n") {
		return liveValidationFailure(model, liveValidationCredentialInvalid, 0)
	}
	if client == nil {
		client = newDeepSeekHTTPClient()
	}
	if sleep == nil {
		sleep = sleepWithContext
	}

	for attempt := 1; attempt <= deepSeekMaxAttempts; attempt++ {
		category, retry := deepSeekAttempt(ctx, model.Model, key, client)
		if category == liveValidationConfirmed {
			return liveValidation{
				SchemaVersion: 1, Status: liveValidationSucceeded, Category: category,
				Provider: "deepseek", BaseURL: model.BaseURL, Model: model.Model, Attempts: attempt,
			}, nil
		}
		if !retry || attempt == deepSeekMaxAttempts {
			return liveValidationFailure(model, category, attempt)
		}
		if err := sleep(ctx, deepSeekRetryDelay); err != nil {
			return liveValidationFailure(model, liveValidationCanceled, attempt)
		}
	}
	panic("unreachable DeepSeek preflight attempt count")
}

func newDeepSeekHTTPClient() *http.Client {
	return &http.Client{
		Timeout: deepSeekRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func deepSeekAttempt(ctx context.Context, model string, key []byte, client httpDoer) (liveValidationCategory, bool) {
	requestCtx, cancel := context.WithTimeout(ctx, deepSeekRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, deepSeekModelsURL, nil)
	if err != nil {
		return liveValidationConfigurationInvalid, false
	}
	request.Header.Set("Authorization", "Bearer "+string(key))
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	request.Header.Del("Authorization")
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctx.Err() != nil {
			return liveValidationCanceled, false
		}
		return liveValidationTransport, true
	}
	if response == nil {
		return liveValidationTransport, true
	}
	body, bodyErr := readBoundedResponse(response.Body)
	if response.Body != nil {
		_ = response.Body.Close()
	}
	defer clear(body)

	switch response.StatusCode {
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
		return liveValidationServer, true
	case http.StatusUnauthorized:
		return liveValidationUnauthorized, false
	case http.StatusForbidden:
		return liveValidationForbidden, false
	case http.StatusTooManyRequests:
		return liveValidationRateLimited, false
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return liveValidationRedirect, false
	}
	if response.StatusCode != http.StatusOK {
		return liveValidationHTTP, false
	}
	if bodyErr != nil {
		if errors.Is(bodyErr, errResponseTooLarge) {
			return liveValidationResponseTooLarge, false
		}
		return liveValidationResponseRead, false
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &models); err != nil {
		return liveValidationMalformed, false
	}
	for _, candidate := range models.Data {
		if candidate.ID == model {
			return liveValidationConfirmed, false
		}
	}
	return liveValidationModelMissing, false
}

func readBoundedResponse(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	content, err := io.ReadAll(io.LimitReader(body, deepSeekMaxResponseBytes+1))
	if err != nil {
		clear(content)
		return nil, err
	}
	if len(content) > deepSeekMaxResponseBytes {
		return content, errResponseTooLarge
	}
	return content, nil
}

var errResponseTooLarge = errors.New("response exceeded bound")

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isOfficialDeepSeek(model config.ModelConfig) bool {
	return model.BaseURL == deepSeekBaseURL && (model.Model == "deepseek-v4-flash" || model.Model == "deepseek-v4-pro")
}

func liveValidationFailure(model config.ModelConfig, category liveValidationCategory, attempts int) (liveValidation, error) {
	return failedLiveValidation(model, category, attempts), &liveValidationError{category: category, attempts: attempts}
}

func failedLiveValidation(model config.ModelConfig, category liveValidationCategory, attempts int) liveValidation {
	return liveValidation{
		SchemaVersion: 1, Status: liveValidationFailed, Category: category,
		Provider: "deepseek", BaseURL: model.BaseURL, Model: model.Model, Attempts: attempts,
	}
}

func persistLiveValidation(outputRoot string, validation liveValidation) error {
	content, err := json.MarshalIndent(validation, "", "  ")
	if err != nil {
		return fmt.Errorf("encode live validation: %w", err)
	}
	content = append(content, '\n')
	if err := persistRunResult(filepath.Join(outputRoot, liveValidationName), content); err != nil {
		return fmt.Errorf("persist live validation: %w", err)
	}
	return nil
}
