package openclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/harness/internal/privatefiles"
)

// StepObservationOptions stages the specific native research plugin. It does
// not change tool parameters, model requests, or the native tool set.
type StepObservationOptions struct{ PluginDir, ObserverURL string }

func (o StepObservationOptions) validate(mode string) error {
	if mode != ModeAgent {
		return errors.New("OpenClaw Step observation requires agent mode")
	}
	if !filepath.IsAbs(o.PluginDir) || filepath.Clean(o.PluginDir) != o.PluginDir || o.PluginDir == "/" || strings.ContainsRune(o.PluginDir, 0) {
		return errors.New("Step plugin directory must be a clean absolute path")
	}
	u, e := url.Parse(o.ObserverURL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("Step observer URL must be HTTP(S) without credentials, query or fragment")
	}
	return nil
}
func renderStepObservation(configuration []byte) ([]byte, error) {
	var cfg map[string]any
	if err := json.Unmarshal(configuration, &cfg); err != nil {
		return nil, err
	}
	plugins, _ := cfg["plugins"].(map[string]any)
	if plugins == nil {
		plugins = map[string]any{}
		cfg["plugins"] = plugins
	}
	entries, _ := plugins["entries"].(map[string]any)
	if entries == nil {
		entries = map[string]any{}
		plugins["entries"] = entries
	}
	// Non-bundled native terminal hooks require explicit conversation-hook permission.
	// The research plugin records identities/timing only, never message content.
	entries["aries-step-pages"] = map[string]any{
		"enabled": true,
		"hooks":   map[string]any{"allowConversationAccess": true},
	}
	plugins["load"] = map[string]any{"paths": []string{"/run/aries/step-pages"}}
	return json.MarshalIndent(cfg, "", "  ")
}
func stepObservationEnvironment(o *StepObservationOptions, occurrence *harness.Occurrence) []string {
	// JSON tuple is unambiguous even when IDs contain delimiters. The harness
	// currently admits one user request per occurrence, so its user turn is 1.
	identity, _ := json.Marshal([]string{occurrence.RunID, occurrence.TaskID, occurrence.AttemptID})
	return []string{"ARIES_STEP_OBSERVER_URL=" + o.ObserverURL, "ARIES_STEP_SESSION_ID=" + string(identity), "ARIES_STEP_TURN_ID=1", "ARIES_STEP_LOG_PATH=" + stateContainerPath + "/agents/main/sessions/aries-step-pages.jsonl"}
}
func stageStepObservation(files map[string]harness.ArchiveFile, dir string) error {
	for _, name := range []string{"index.mjs", "end-request.mjs", "openclaw.plugin.json", "package.json"} {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("read Step plugin %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return fmt.Errorf("Step plugin %s is not a bounded regular file", name)
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if len(content) > 1<<20 {
			return fmt.Errorf("Step plugin %s exceeds limit", name)
		}
		files["run/aries/step-pages/"+name] = harness.ArchiveFile{Content: content, Mode: 0o400}
	}
	return nil
}

// A cancellation abandons only the current uncollected epoch. It never waits
// for a full metadata scan/export, and never certifies a complete measurement.
const stepObservationCleanupTimeout = 30 * time.Second

func (manager *Manager) releaseStepObservation(ctx context.Context, active *session) (resultErr error) {
	begin := time.Now()
	receipt := map[string]any{"schema": "aries.openclaw.observer-release.v1",
		"reason": "host_confirmed_harness_stopped", "started_at": begin.UTC(),
		"budget_ms": stepObservationCleanupTimeout.Milliseconds(), "harness_stopped": true}
	defer func() {
		receipt["finished_at"] = time.Now().UTC()
		receipt["duration_ns"] = time.Since(begin).Nanoseconds()
		receipt["released"] = resultErr == nil
		if resultErr != nil {
			receipt["error"] = "observer release not confirmed; verifier must remain blocked"
		}
		content, err := json.MarshalIndent(receipt, "", "  ")
		if err == nil {
			path := filepath.Join(active.ArtifactDir, fmt.Sprintf("observer-release-%d.json", begin.UnixNano()))
			err = privatefiles.WriteArtifact(path, append(content, '\n'))
			if err == nil && active.Artifacts != nil {
				active.Artifacts.Add(path)
			}
		}
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("persist observer release evidence: %w", err))
		}
	}()
	identity, _ := json.Marshal([]string{active.RunID, active.TaskID, active.AttemptID})
	payload, _ := json.Marshal(map[string]any{"session_id": string(identity), "turn_id": "1",
		"reason": "host_confirmed_harness_stopped", "terminal_complete": false})
	endpoint := strings.TrimRight(manager.options.StepObservation.ObserverURL, "/") + "/session/abort"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return errors.New("construct observer release request")
	}
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("observer release transport failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16385))
	if err != nil || len(body) > 16384 {
		return errors.New("observer release response invalid")
	}
	var ack struct {
		OK               bool   `json:"ok"`
		Released         bool   `json:"released"`
		Paused           *bool  `json:"paused"`
		MeasurementValid bool   `json:"measurement_valid"`
		TerminalComplete bool   `json:"terminal_complete"`
		SessionID        string `json:"session_id"`
		TurnID           string `json:"turn_id"`
	}
	if json.Unmarshal(body, &ack) != nil || response.StatusCode != http.StatusOK ||
		!ack.OK || !ack.Released || ack.Paused == nil || *ack.Paused ||
		ack.SessionID != string(identity) || ack.TurnID != "1" {
		return errors.New("observer released acknowledgement not confirmed")
	}
	receipt["observer_ack"] = json.RawMessage(body)
	receipt["measurement_valid"] = ack.MeasurementValid
	receipt["terminal_complete"] = ack.TerminalComplete
	return nil
}
