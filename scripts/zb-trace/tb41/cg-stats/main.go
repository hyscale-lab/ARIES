// Read the stock guest task's cgroup v2 counters through its adopted shim.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/cgroups/v3/cgroup2/stats"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/ttrpc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 3 || filepath.Base(os.Args[2]) != os.Args[2] {
		return fmt.Errorf("usage: cg-stats SHIM_DIR RUNTIME_ID")
	}
	id := os.Args[2]
	address, err := os.ReadFile(filepath.Join(os.Args[1], "aries#"+id, "address"))
	if err != nil {
		return err
	}
	socket := strings.TrimPrefix(strings.TrimSpace(string(address)), "unix://")
	if !filepath.IsAbs(socket) {
		return fmt.Errorf("invalid restored shim socket %q", socket)
	}
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return err
	}
	client := ttrpc.NewClient(conn)
	defer client.Close()
	task := taskapi.NewTTRPCTaskClient(client)
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		tick, err := strconv.Atoi(scanner.Text())
		if err != nil || tick < 0 {
			return fmt.Errorf("invalid sample tick %q", scanner.Text())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
		response, err := task.Stats(ctx, &taskapi.StatsRequest{ID: id})
		cancel()
		if err != nil {
			if writeErr := encoder.Encode(map[string]any{"tick": tick, "unavailable": err.Error()}); writeErr != nil {
				return writeErr
			}
			continue
		}
		var data stats.Metrics
		if err := response.Stats.UnmarshalTo(&data); err != nil {
			return fmt.Errorf("guest task did not return cgroup v2 stats: %w", err)
		}
		if data.Memory == nil || data.Memory.Usage == 0 {
			return fmt.Errorf("missing positive guest memory.current")
		}
		row := map[string]any{
			"tick": tick, "memory_current_bytes": data.Memory.Usage,
			"type_url": response.Stats.TypeUrl, "task_id": id,
			"anon_bytes": data.Memory.Anon, "file_bytes": data.Memory.File,
		}
		{
			row["cpu"] = data.CPU
			row["io"] = data.Io
			row["memory_pressure"] = data.Memory.PSI
			row["shmem_bytes"] = data.Memory.Shmem
			row["file_dirty_bytes"] = data.Memory.FileDirty
			row["file_writeback_bytes"] = data.Memory.FileWriteback
			row["workingset_refault"] = data.Memory.WorkingsetRefault
			row["pgscan"] = data.Memory.Pgscan
			row["pgsteal"] = data.Memory.Pgsteal
			row["pgmajfault"] = data.Memory.Pgmajfault
		}
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	return scanner.Err()
}
