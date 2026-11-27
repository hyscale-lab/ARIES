package app

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"unsafe"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/sys/unix"
)

// Docker containers are children of the Engine, not of the aries process, so
// they escape the CPU set of the scheduler allocation or other cgroup that
// launched aries unless execution.cpuset_cpus pins them. The checks here run
// before a run starts, where a bad cpuset would otherwise fail every task at
// ContainerCreate or quietly place containers on CPUs another job owns.

// resolveCPUSet turns execution.cpuset_cpus into the canonical list every
// container of the run receives. "inherit" selects the CPUs the aries process
// may run on; an explicit list must stay within them. An empty value resolves
// to no cpuset without consulting the host.
func resolveCPUSet(value string, processCPUs func() ([]int, error)) (string, []int, error) {
	if value == "" {
		return "", nil, nil
	}
	allowed, err := processCPUs()
	if err != nil {
		return "", nil, fmt.Errorf("read the CPUs aries may run on for execution.cpuset_cpus: %w", err)
	}
	if value == config.InheritCPUSet {
		return formatCPUSet(allowed), allowed, nil
	}
	cpus, err := config.ParseCPUSet(value)
	if err != nil {
		return "", nil, fmt.Errorf("execution.cpuset_cpus %q: %w", value, err)
	}
	permitted := make(map[int]bool, len(allowed))
	for _, cpu := range allowed {
		permitted[cpu] = true
	}
	for _, cpu := range cpus {
		if !permitted[cpu] {
			return "", nil, fmt.Errorf("execution.cpuset_cpus %q names CPU %d, outside the CPUs aries may run on (%s)", value, cpu, formatCPUSet(allowed))
		}
	}
	return formatCPUSet(cpus), cpus, nil
}

// maxAffinityCPUs matches the 16-bit CPU indices config.ParseCPUSet accepts.
const maxAffinityCPUs = 1 << 16

// processCPUs returns the CPUs the scheduler lets the aries process run on,
// which under a batch scheduler are the allocation's CPUs. unix.CPUSet holds
// only 1024 CPUs, and the kernel refuses (EINVAL) a mask shorter than the
// host's CPU count, so the mask doubles until the kernel accepts it.
func processCPUs() ([]int, error) {
	for words := 1024 / bits.UintSize; ; words *= 2 {
		mask := make([]uint, words)
		_, _, errno := unix.RawSyscall(unix.SYS_SCHED_GETAFFINITY, 0, uintptr(len(mask))*unsafe.Sizeof(mask[0]), uintptr(unsafe.Pointer(&mask[0])))
		if errno == unix.EINVAL && words*bits.UintSize < maxAffinityCPUs {
			continue
		}
		if errno != 0 {
			return nil, errno
		}
		return maskCPUs(mask), nil
	}
}

// maskCPUs lists the CPUs set in a kernel affinity mask, in ascending order.
func maskCPUs(mask []uint) []int {
	var cpus []int
	for word, value := range mask {
		for value != 0 {
			bit := bits.TrailingZeros(value)
			cpus = append(cpus, word*bits.UintSize+bit)
			value &^= 1 << bit
		}
	}
	return cpus
}

// checkCPUSetFit rejects a task whose sandbox and harness CPU quotas together
// exceed the cpuset they share. Docker accepts such a quota, but the two
// containers then compete for fewer CPUs than the task declares. An unset or
// zero (unlimited) quota counts as zero.
func checkCPUSetFit(tasks []core.Task, overrides config.RuntimeOverrides, cpuset string, count int) error {
	if cpuset == "" {
		return nil
	}
	harness := 0.0
	if overrides.HarnessResources.CPU != nil {
		harness = *overrides.HarnessResources.CPU
	}
	for _, task := range tasks {
		sandbox := task.Environment.CPU
		if overrides.AgentSandboxResources.CPU != nil {
			sandbox = *overrides.AgentSandboxResources.CPU
		}
		if sandbox+harness > float64(count) {
			return fmt.Errorf("task %q needs %g CPUs (sandbox %g, harness %g), more than the %d in execution.cpuset_cpus %s", task.ID, sandbox+harness, sandbox, harness, count, cpuset)
		}
	}
	return nil
}

// formatCPUSet writes ascending CPUs as a Linux cpuset list, joining runs
// into ranges.
func formatCPUSet(cpus []int) string {
	var parts []string
	for start := 0; start < len(cpus); {
		end := start
		for end+1 < len(cpus) && cpus[end+1] == cpus[end]+1 {
			end++
		}
		part := strconv.Itoa(cpus[start])
		if end > start {
			part += "-" + strconv.Itoa(cpus[end])
		}
		parts = append(parts, part)
		start = end + 1
	}
	return strings.Join(parts, ",")
}
