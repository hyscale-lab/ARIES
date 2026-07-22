package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	managedLabelValue = "true"
	taskContainerKind = "task-container"
	harnessKind       = "openclaw-harness"
	maxContainerName  = 255
	maxMemoryBytes    = uint64(1 << 60)
	maxOnlineCPUs     = int64(4096)
	maxCPUPercent     = float64(1_000_000)
)

var errContainerGone = errors.New("container disappeared during sampling")

type statsValidationError struct {
	err error
}

func (failure *statsValidationError) Error() string {
	return failure.err.Error()
}

func (failure *statsValidationError) Unwrap() error {
	return failure.err
}

type engineClient struct {
	http             *http.Client
	maxResponseBytes int64
}

type listedContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

type sampledContainer struct {
	taskID   string
	kind     string
	id       string
	name     string
	cpu      float64
	memory   uint64
	memLimit uint64
}

type statsDocument struct {
	CPUStats    *cpuStats    `json:"cpu_stats"`
	PreCPUStats *cpuStats    `json:"precpu_stats"`
	MemoryStats *memoryStats `json:"memory_stats"`
}

type cpuStats struct {
	CPUUsage *cpuUsage `json:"cpu_usage"`
	System   *uint64   `json:"system_cpu_usage"`
	Online   *int64    `json:"online_cpus"`
}

type cpuUsage struct {
	Total  *uint64  `json:"total_usage"`
	PerCPU []uint64 `json:"percpu_usage"`
}

type memoryStats struct {
	Usage *uint64 `json:"usage"`
	Limit *uint64 `json:"limit"`
}

func newEngineClient(socket string, maxResponseBytes int64) *engineClient {
	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	return &engineClient{
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("Docker Engine redirects are not allowed")
			},
		},
		maxResponseBytes: maxResponseBytes,
	}
}

func (client *engineClient) closeIdleConnections() {
	client.http.CloseIdleConnections()
}

func (client *engineClient) discover(ctx context.Context, runID string, tasks map[string]struct{}) ([]listedContainer, error) {
	filterJSON, err := json.Marshal(map[string][]string{
		"label": {"aries.managed=" + managedLabelValue, "aries.run=" + runID},
	})
	if err != nil {
		return nil, fmt.Errorf("encode Docker container filters: %w", err)
	}
	query := url.Values{}
	query.Set("all", "false")
	query.Set("filters", string(filterJSON))
	var listed []listedContainer
	if _, err := client.getJSON(ctx, "/containers/json", query, &listed); err != nil {
		return nil, fmt.Errorf("list monitored Docker containers: %w", err)
	}

	allowed := make([]listedContainer, 0, len(listed))
	seen := make(map[string]struct{}, len(listed))
	for index, container := range listed {
		if container.Labels == nil || container.Labels["aries.managed"] != managedLabelValue || container.Labels["aries.run"] != runID {
			return nil, fmt.Errorf("Docker container list record %d has wrong ARIES ownership labels", index)
		}
		kind := container.Labels["aries.kind"]
		taskID := container.Labels["aries.task"]
		_, taskAllowed := tasks[taskID]
		kindAllowed := kind == taskContainerKind || kind == harnessKind
		if !taskAllowed || !kindAllowed || container.State != "running" {
			continue
		}
		if err := validateContainerID(container.ID); err != nil {
			return nil, fmt.Errorf("Docker container list record %d: %w", index, err)
		}
		if len(container.Names) != 1 || !strings.HasPrefix(container.Names[0], "/") {
			return nil, fmt.Errorf("Docker container %s has an invalid name list", container.ID)
		}
		name := strings.TrimPrefix(container.Names[0], "/")
		if name == "" || len(name) > maxContainerName || strings.ContainsAny(name, "\x00\r\n") {
			return nil, fmt.Errorf("Docker container %s has an invalid name", container.ID)
		}
		if _, duplicate := seen[container.ID]; duplicate {
			return nil, fmt.Errorf("Docker container list repeats ID %s", container.ID)
		}
		seen[container.ID] = struct{}{}
		container.Names = []string{name}
		allowed = append(allowed, container)
	}
	sort.Slice(allowed, func(i, j int) bool {
		left, right := allowed[i].Labels, allowed[j].Labels
		if left["aries.task"] != right["aries.task"] {
			return left["aries.task"] < right["aries.task"]
		}
		if left["aries.kind"] != right["aries.kind"] {
			return left["aries.kind"] < right["aries.kind"]
		}
		return allowed[i].ID < allowed[j].ID
	})
	return allowed, nil
}

func (client *engineClient) stats(ctx context.Context, container listedContainer) (sampledContainer, error) {
	query := url.Values{}
	query.Set("one-shot", "true")
	query.Set("stream", "false")
	var document statsDocument
	status, err := client.getJSON(ctx, "/containers/"+container.ID+"/stats", query, &document)
	if status == http.StatusNotFound {
		return sampledContainer{}, errContainerGone
	}
	if err != nil {
		return sampledContainer{}, fmt.Errorf("read Docker stats for %s: %w", container.ID, err)
	}
	cpuPercent, memory, limit, err := validateStats(document)
	if err != nil {
		return sampledContainer{}, fmt.Errorf("validate Docker stats for %s: %w", container.ID, &statsValidationError{err: err})
	}
	return sampledContainer{
		taskID:   container.Labels["aries.task"],
		kind:     container.Labels["aries.kind"],
		id:       container.ID,
		name:     container.Names[0],
		cpu:      cpuPercent,
		memory:   memory,
		memLimit: limit,
	}, nil
}

func (client *engineClient) getJSON(ctx context.Context, path string, query url.Values, destination any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path+"?"+query.Encode(), nil)
	if err != nil {
		return 0, fmt.Errorf("construct Docker Engine request: %w", err)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return response.StatusCode, fmt.Errorf("Docker Engine returned HTTP %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, client.maxResponseBytes+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return response.StatusCode, fmt.Errorf("read Docker Engine response: %w", err)
	}
	if int64(len(content)) > client.maxResponseBytes {
		return response.StatusCode, fmt.Errorf("Docker Engine response exceeds %d bytes", client.maxResponseBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	if err := decoder.Decode(destination); err != nil {
		return response.StatusCode, fmt.Errorf("decode Docker Engine response: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return response.StatusCode, errors.New("Docker Engine response contains trailing JSON")
		}
		return response.StatusCode, fmt.Errorf("decode Docker Engine response trailer: %w", err)
	}
	return response.StatusCode, nil
}

func validateStats(document statsDocument) (float64, uint64, uint64, error) {
	if document.CPUStats == nil || document.CPUStats.CPUUsage == nil ||
		document.CPUStats.CPUUsage.Total == nil || document.CPUStats.System == nil {
		return 0, 0, 0, errors.New("required current CPU fields are absent")
	}
	if document.MemoryStats == nil || document.MemoryStats.Usage == nil || document.MemoryStats.Limit == nil {
		return 0, 0, 0, errors.New("required current memory fields are absent")
	}
	total := *document.CPUStats.CPUUsage.Total
	system := *document.CPUStats.System
	online := int64(0)
	if document.CPUStats.Online != nil && *document.CPUStats.Online < 0 {
		return 0, 0, 0, fmt.Errorf("online CPU count %d is outside the bound", *document.CPUStats.Online)
	}
	if document.CPUStats.Online != nil && *document.CPUStats.Online > 0 {
		online = *document.CPUStats.Online
	} else if len(document.CPUStats.CPUUsage.PerCPU) > 0 {
		online = int64(len(document.CPUStats.CPUUsage.PerCPU))
	}
	if online <= 0 || online > maxOnlineCPUs {
		return 0, 0, 0, fmt.Errorf("online CPU count %d is outside the bound", online)
	}
	memory := *document.MemoryStats.Usage
	limit := *document.MemoryStats.Limit
	if memory > maxMemoryBytes || limit > maxMemoryBytes {
		return 0, 0, 0, errors.New("memory measurement exceeds the bound")
	}

	preTotal, preSystem, baselineAvailable := cpuBaseline(document.PreCPUStats)
	if !baselineAvailable {
		return 0, memory, limit, nil
	}
	if total < preTotal || system < preSystem {
		return 0, 0, 0, errors.New("CPU counters decreased")
	}
	cpuDelta := total - preTotal
	systemDelta := system - preSystem
	cpuPercent := float64(0)
	if cpuDelta != 0 {
		if systemDelta == 0 || online == 0 {
			return 0, 0, 0, errors.New("nonzero CPU delta has no system delta or online CPUs")
		}
		cpuPercent = float64(cpuDelta) / float64(systemDelta) * float64(online) * 100
	}
	if math.IsNaN(cpuPercent) || math.IsInf(cpuPercent, 0) || cpuPercent < 0 || cpuPercent > maxCPUPercent {
		return 0, 0, 0, fmt.Errorf("CPU percentage %v is invalid", cpuPercent)
	}
	return cpuPercent, memory, limit, nil
}

func cpuBaseline(previous *cpuStats) (uint64, uint64, bool) {
	if previous == nil || previous.CPUUsage == nil || previous.CPUUsage.Total == nil || previous.System == nil {
		return 0, 0, false
	}
	total := *previous.CPUUsage.Total
	system := *previous.System
	if total == 0 || system == 0 {
		return 0, 0, false
	}
	return total, system, true
}

func validateContainerID(id string) error {
	if len(id) != 64 {
		return fmt.Errorf("container ID length is %d, want 64", len(id))
	}
	for _, character := range id {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("container ID is not lowercase hexadecimal")
		}
	}
	return nil
}

func requestContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= timeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}
