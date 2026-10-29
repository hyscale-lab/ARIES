package apptainer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	// clockTicksPerSecond is Linux's USER_HZ, the unit of process times in
	// /proc: 100 on every architecture Apptainer supports.
	clockTicksPerSecond = 100
	// exitedCPUFile, in a runtime's directory, holds one line per finished
	// command: the CPU ticks its wrapper read from the kernel as it ended.
	exitedCPUFile = "exited-cpu"
	// exitedCPULostFile marks a runtime whose finished command's CPU could not
	// be recorded, so its samples fail rather than undercount.
	exitedCPULostFile = "exited-cpu.lost"
	// processRecordSuffix marks the record a process runtime keeps beside its
	// workdir while it runs, so the run's resource source can find it.
	processRecordSuffix  = ".process"
	instanceStarterTitle = "Apptainer instance:"
)

// ResourceOptions scope one resource source to a run, as for Docker.
type ResourceOptions struct {
	// StateDir is the transport's state directory.
	StateDir string
	// Scope is "task", for the instances of the given tasks, or "run", for the
	// run's bridge.
	Scope   string
	RunID   string
	TaskIDs []string
	// ProcRoot is /proc unless a test substitutes a tree of its own.
	ProcRoot string
}

// ResourceSource measures runtimes from /proc. On a host with cgroups v1 an
// unprivileged user has no cgroup per instance, so each instance is measured
// by its processes. Its CPU is the time of every live process in its PID
// namespace, including what they have reaped, and of its starter's storage
// helpers, plus the time of each finished command, which the command's
// wrapper reads from the kernel as it ends: a command shorter than the
// sampling interval is still counted, and an orphan's time reaches the
// instance's init when the init reaps it. Memory is the processes'
// proportional set size, so a shared page counts once; unlike a cgroup's
// usage it excludes the page cache. As with Docker, apptainer's own launcher
// and archive transfers are not charged to the instance. The bridge, a plain
// process, is measured by its own time and memory; the commands it runs count
// for the sandbox that runs them.
type ResourceSource struct {
	options ResourceOptions
	tasks   map[string]struct{}

	mu   sync.Mutex
	last map[string]uint64
}

func NewResourceSource(options ResourceOptions) (*ResourceSource, error) {
	if options.RunID == "" {
		return nil, errors.New("Apptainer resource source requires a run ID")
	}
	if options.StateDir == "" || !filepath.IsAbs(options.StateDir) {
		return nil, errors.New("Apptainer resource source requires an absolute state directory")
	}
	if options.Scope == "" {
		options.Scope = "task"
	}
	switch options.Scope {
	case "task":
		if len(options.TaskIDs) == 0 {
			return nil, errors.New("Apptainer resource source requires at least one task ID")
		}
	case "run":
		if len(options.TaskIDs) != 0 {
			return nil, errors.New("run resources cannot select task IDs")
		}
	default:
		return nil, errors.New("invalid Apptainer resource scope")
	}
	if options.ProcRoot == "" {
		options.ProcRoot = "/proc"
	}
	tasks := make(map[string]struct{}, len(options.TaskIDs))
	for _, taskID := range options.TaskIDs {
		if taskID == "" {
			return nil, errors.New("Apptainer resource task ID is empty")
		}
		if _, repeated := tasks[taskID]; repeated {
			return nil, fmt.Errorf("Apptainer resource task ID %q is repeated", taskID)
		}
		tasks[taskID] = struct{}{}
	}
	return &ResourceSource{options: options, tasks: tasks, last: map[string]uint64{}}, nil
}

func (source *ResourceSource) Sample(context.Context) ([]core.ResourceReading, error) {
	table, err := readProcesses(source.options.ProcRoot)
	if err != nil {
		return nil, err
	}
	limit, err := memoryTotal(source.options.ProcRoot)
	if err != nil {
		return nil, err
	}
	var readings []core.ResourceReading
	if source.options.Scope == "run" {
		readings, err = source.bridges(table)
	} else {
		readings, err = source.instances(table)
	}
	if err != nil {
		return nil, err
	}
	observed := time.Now().UTC()
	source.mu.Lock()
	defer source.mu.Unlock()
	for index := range readings {
		reading := &readings[index]
		reading.Scope, reading.RunID, reading.ObservedAt, reading.MemoryLimitBytes = source.options.Scope, source.options.RunID, observed, limit
		// A command's time leaves the live processes before its wrapper's report
		// is recorded; holding the highest total keeps the counter monotonic.
		if previous := source.last[reading.RuntimeID]; reading.CPUUsageNanoseconds < previous {
			reading.CPUUsageNanoseconds = previous
		}
		source.last[reading.RuntimeID] = reading.CPUUsageNanoseconds
	}
	return readings, nil
}

func (*ResourceSource) Close() error { return nil }

// instances reads the records of the selected tasks' running instances.
func (source *ResourceSource) instances(table processTable) ([]core.ResourceReading, error) {
	entries, err := os.ReadDir(source.options.StateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list Apptainer runtimes: %w", err)
	}
	var readings []core.ResourceReading
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(source.options.StateDir, entry.Name(), recordFile))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read Apptainer runtime record %s: %w", entry.Name(), err)
		}
		var current record
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil, fmt.Errorf("decode Apptainer runtime record %s: %w", entry.Name(), err)
		}
		component, selected := source.selectInstance(current, entry.Name())
		if !selected {
			continue
		}
		init, alive := table.processes[current.InstancePID]
		if current.InstancePID <= 0 || !alive || init.zombie {
			continue
		}
		// An instance in the host's PID namespace, as when an administrator
		// disables PID namespaces, cannot be told from every other process.
		if init.namespace != "" && init.namespace == table.self {
			return nil, fmt.Errorf("Apptainer instance %s has no PID namespace of its own, so its processes cannot be measured", entry.Name())
		}
		members := table.instanceMembers(current.InstancePID, source.options.ProcRoot)
		runtimeDir := filepath.Join(source.options.StateDir, entry.Name())
		if _, err := os.Lstat(filepath.Join(runtimeDir, exitedCPULostFile)); err == nil {
			return nil, fmt.Errorf("Apptainer runtime %s lost a finished command's CPU time", entry.Name())
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("check Apptainer runtime %s: %w", entry.Name(), err)
		}
		ticks, err := exitedTicks(filepath.Join(runtimeDir, exitedCPUFile))
		if err != nil {
			return nil, err
		}
		memory := uint64(0)
		for _, pid := range members {
			ticks += table.processes[pid].ticks
			pss, err := proportionalSet(source.options.ProcRoot, pid)
			if err != nil {
				return nil, err
			}
			memory += pss
		}
		readings = append(readings, core.ResourceReading{
			TaskID: current.Labels["aries.task"], Component: component, RuntimeID: current.Name, RuntimeName: current.Name,
			CPUUsageNanoseconds: ticks * (1e9 / clockTicksPerSecond), MemoryUsageBytes: memory,
		})
	}
	sortReadings(readings)
	return readings, nil
}

func (source *ResourceSource) selectInstance(current record, directory string) (string, bool) {
	if current.Name != directory || current.Labels["aries.managed"] != "true" || current.Labels["aries.run"] != source.options.RunID {
		return "", false
	}
	if _, selected := source.tasks[current.Labels["aries.task"]]; !selected {
		return "", false
	}
	switch component := current.Labels["aries.component"]; component {
	case "sandbox", "evaluation-sandbox", "harness":
		return component, true
	default:
		return "", false
	}
}

// processFile is the record a process runtime keeps while it runs.
type processFile struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	PID    int               `json:"pid"`
	Start  uint64            `json:"start"`
}

// bridges reads the run's process runtimes. Their own time and memory count;
// the commands a bridge runs count for the sandboxes that run them.
func (source *ResourceSource) bridges(table processTable) ([]core.ResourceReading, error) {
	paths, err := filepath.Glob(filepath.Join(source.options.StateDir, "*"+processRecordSuffix))
	if err != nil {
		return nil, err
	}
	var readings []core.ResourceReading
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read process runtime record: %w", err)
		}
		var current processFile
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil, fmt.Errorf("decode process runtime record %s: %w", filepath.Base(path), err)
		}
		labels := current.Labels
		if labels["aries.managed"] != "true" || labels["aries.run"] != source.options.RunID || labels["aries.task"] != "" ||
			labels["aries.component"] != "bridge" || labels["aries.kind"] != "tool-bridge" {
			continue
		}
		process, alive := table.processes[current.PID]
		if !alive || process.zombie || process.start != current.Start {
			continue
		}
		memory, err := proportionalSet(source.options.ProcRoot, current.PID)
		if err != nil {
			return nil, err
		}
		readings = append(readings, core.ResourceReading{
			Component: "bridge", RuntimeID: current.Name, RuntimeName: current.Name,
			CPUUsageNanoseconds: process.own * (1e9 / clockTicksPerSecond), MemoryUsageBytes: memory,
		})
	}
	sortReadings(readings)
	return readings, nil
}

func sortReadings(readings []core.ResourceReading) {
	sort.Slice(readings, func(i, j int) bool {
		if readings[i].TaskID != readings[j].TaskID {
			return readings[i].TaskID < readings[j].TaskID
		}
		if readings[i].Component != readings[j].Component {
			return readings[i].Component < readings[j].Component
		}
		return readings[i].RuntimeID < readings[j].RuntimeID
	})
}

type processInfo struct {
	parent int
	// own is the process's user and system time; ticks adds what it has
	// reaped from its children.
	own, ticks uint64
	start      uint64
	zombie     bool
	namespace  string
}

type processTable struct {
	processes map[int]processInfo
	children  map[int][]int
	// self is the sampling process's own PID namespace.
	self string
}

// readProcesses reads every process once per sample. A process that exits
// while it is read is skipped.
func readProcesses(root string) (processTable, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return processTable{}, fmt.Errorf("read %s: %w", root, err)
	}
	table := processTable{processes: map[int]processInfo{}, children: map[int][]int{}}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, entry.Name(), "stat"))
		if err != nil {
			continue
		}
		info, err := parseProcessTimes(raw)
		if err != nil {
			return processTable{}, fmt.Errorf("process %d: %w", pid, err)
		}
		// A namespace that cannot be read leaves the process out of every instance.
		info.namespace, _ = os.Readlink(filepath.Join(root, entry.Name(), "ns", "pid"))
		table.processes[pid] = info
		table.children[info.parent] = append(table.children[info.parent], pid)
	}
	table.self, _ = os.Readlink(filepath.Join(root, "self", "ns", "pid"))
	return table, nil
}

// parseProcessTimes reads the parent, the times and the start of a process
// from its /proc stat line.
func parseProcessTimes(raw []byte) (processInfo, error) {
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return processInfo{}, errors.New("unexpected /proc stat format")
	}
	fields := strings.Fields(string(raw[end+1:]))
	// fields[0] is field 3 (the state): the parent is field 4, utime to cstime
	// fields 14 to 17, and the start time field 22.
	if len(fields) < 20 {
		return processInfo{}, errors.New("unexpected /proc stat format")
	}
	values := make([]uint64, 0, 6)
	for _, index := range []int{1, 11, 12, 13, 14, 19} {
		value, err := strconv.ParseUint(fields[index], 10, 64)
		if err != nil {
			return processInfo{}, fmt.Errorf("parse /proc stat field %d: %w", index+3, err)
		}
		values = append(values, value)
	}
	own := values[1] + values[2]
	return processInfo{
		parent: int(values[0]), own: own, ticks: own + values[3] + values[4], start: values[5],
		zombie: fields[0] == "Z" || fields[0] == "X",
	}, nil
}

// instanceMembers returns the processes in the instance's PID namespace and
// the descendants of the starter that runs it, which include its storage
// helpers. Commands started with apptainer exec join the namespace; the
// launcher that started them stays outside it.
func (table processTable) instanceMembers(initPID int, root string) []int {
	members := map[int]struct{}{initPID: {}}
	if namespace := table.processes[initPID].namespace; namespace != "" {
		for pid, info := range table.processes {
			if info.namespace == namespace {
				members[pid] = struct{}{}
			}
		}
	}
	if starter := table.processes[initPID].parent; starter > 1 && isInstanceStarter(root, starter) {
		pending := []int{starter}
		for len(pending) > 0 {
			pid := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			members[pid] = struct{}{}
			pending = append(pending, table.children[pid]...)
		}
	}
	pids := make([]int, 0, len(members))
	for pid := range members {
		if _, live := table.processes[pid]; live {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}

func isInstanceStarter(root string, pid int) bool {
	raw, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline"))
	return err == nil && bytes.HasPrefix(raw, []byte(instanceStarterTitle))
}

// proportionalSet returns a process's proportional set size in bytes, or zero
// for a process that has exited.
func proportionalSet(root string, pid int) (uint64, error) {
	file, err := os.Open(filepath.Join(root, strconv.Itoa(pid), "smaps_rollup"))
	if processGone(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read memory of process %d: %w", pid, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "Pss:" && fields[2] == "kB" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse memory of process %d: %w", pid, err)
			}
			return kib * 1024, nil
		}
	}
	if err := scanner.Err(); err != nil && !processGone(err) {
		return 0, fmt.Errorf("read memory of process %d: %w", pid, err)
	}
	// A process that exits while it is read leaves an empty or partial file.
	return 0, nil
}

// processGone reports an error from a process's /proc entry that means the
// process exited after it was listed: its entry is gone, or the kernel
// answers that no such process remains while it is torn down.
func processGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// exitedTicks sums the finished commands' CPU ticks recorded for a runtime.
func exitedTicks(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read finished commands' CPU: %w", err)
	}
	var total uint64
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		value, err := strconv.ParseUint(line, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("finished commands' CPU has an invalid line %q", line)
		}
		total += value
	}
	return total, nil
}

// memoryTotal is the host's memory: nothing bounds an instance's.
func memoryTotal(root string) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(root, "meminfo"))
	if err != nil {
		return 0, fmt.Errorf("read host memory: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse host memory: %w", err)
			}
			return kib * 1024, nil
		}
	}
	return 0, errors.New("host memory is missing from meminfo")
}
