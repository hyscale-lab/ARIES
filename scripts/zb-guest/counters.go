package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Counter reads run outside the measured cgroup. The container init PID proves
// the observed cgroup is the workload's, including after snapshot restore.
func counters(pid int, out *json.Encoder) error {
	if pid <= 1 {
		return fmt.Errorf("expected workload init PID > 1")
	}
	proc := fmt.Sprintf("/proc/%d", pid)
	cgroup, err := os.ReadFile(filepath.Join(proc, "cgroup"))
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(strings.TrimSpace(string(cgroup)), "0::")
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || name == "/" || strings.Contains(name, "\n") {
		return fmt.Errorf("expected dedicated cgroup v2 workload")
	}
	cgdir := filepath.Join("/sys/fs/cgroup", name)
	const period = time.Second
	for next := time.Now(); ; {
		time.Sleep(time.Until(next))
		begin, err := monotonic()
		if err != nil {
			return err
		}
		current, err := os.ReadFile(filepath.Join(proc, "cgroup"))
		if err != nil || string(current) != string(cgroup) {
			return fmt.Errorf("workload init cgroup changed or disappeared")
		}
		data := make(map[string]string)
		unavailable := make(map[string]string)
		for _, path := range []string{"/proc/meminfo", "/proc/buddyinfo", "/proc/vmstat", "memory.current", "memory.stat", "memory.events", "memory.pressure", "io.pressure", "io.stat", "cpu.stat"} {
			source := path
			if !filepath.IsAbs(source) {
				source = filepath.Join(cgdir, path)
			}
			v, err := os.ReadFile(source)
			if err != nil {
				unavailable[path] = err.Error()
			} else {
				data[path] = string(v)
			}
		}
		end, err := monotonic()
		if err != nil {
			return err
		}
		missed := time.Since(next) / period
		if err := out.Encode(map[string]any{"guest_begin_ns": begin, "guest_end_ns": end, "guest_cgroup": name, "workload_pid": pid, "source": data, "unavailable": unavailable, "missed_ticks": missed}); err != nil {
			return err
		}
		next = next.Add((missed + 1) * period)
	}
}
