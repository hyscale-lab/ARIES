// Keep a controlled ordinary-file workload in the sleeping task's guest filesystem.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

const mib = 1 << 20

type fixture struct {
	dir      string
	file     *os.File
	size     int64
	expected string
}

func prepare(size int64) (*fixture, error) {
	pids, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, pid := range pids {
		cmd, _ := os.ReadFile(filepath.Join(pid, "cmdline"))
		if string(cmd) == "/bin/sleep\x00infinity\x00" {
			dir := filepath.Join(pid, "root", "app")
			if info, err := os.Stat(filepath.Join(dir, "personal-site")); err == nil && info.IsDir() {
				dirs = append(dirs, dir)
			}
		}
	}
	if len(dirs) != 1 {
		return nil, errors.New("expected one owned idle task filesystem")
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(dirs[0], &stat); err != nil {
		return nil, err
	}
	if uint64(stat.Bsize)*stat.Bavail < uint64(size+128*mib) || stat.Type == unix.TMPFS_MAGIC {
		return nil, errors.New("insufficient non-tmpfs capacity in the task filesystem")
	}
	return &fixture{dir: dirs[0], size: size}, nil
}

func now() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic(err)
	}
	return ts.Nano()
}

func (x *fixture) fill() (map[string]any, error) {
	if x.file != nil {
		return nil, errors.New("file already filled")
	}
	f, err := os.CreateTemp(x.dir, ".zb-pressure-*")
	if err != nil {
		return nil, err
	}
	x.file = f
	block := make([]byte, mib)
	seed := uint64(1251903)
	for i := 0; i < len(block); i += 8 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		binary.LittleEndian.PutUint64(block[i:], seed)
	}
	h := sha256.New()
	begin := now()
	for offset := int64(0); offset < x.size; offset += mib {
		binary.LittleEndian.PutUint64(block, uint64(offset))
		if n, err := f.Write(block); n != len(block) || err != nil {
			return nil, errors.Join(io.ErrShortWrite, err)
		}
		_, _ = h.Write(block)
	}
	writesEnd := now()
	if err := f.Sync(); err != nil {
		return nil, err
	}
	syncEnd := now()
	x.expected = hex.EncodeToString(h.Sum(nil))
	result, err := x.read()
	if err != nil {
		return nil, err
	}
	result["guest_write_ns"] = writesEnd - begin
	result["guest_fsync_ns"] = syncEnd - writesEnd
	return result, nil
}

func (x *fixture) read() (map[string]any, error) {
	if x.file == nil || x.expected == "" {
		return nil, errors.New("file was not filled")
	}
	if _, err := x.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	h := sha256.New()
	begin := now()
	n, err := io.Copy(h, x.file)
	end := now()
	if err != nil {
		return nil, err
	}
	result := hex.EncodeToString(h.Sum(nil))
	if n != x.size || result != x.expected {
		return nil, errors.New("file reread length or checksum differs")
	}
	return map[string]any{"guest_read_ns": end - begin, "bytes": n, "sha256": result}, nil
}

func (x *fixture) fileCache() (uint64, error) {
	if x.file == nil {
		return 0, errors.New("file was not filled")
	}
	pageSize := os.Getpagesize()
	mapping, err := unix.Mmap(int(x.file.Fd()), 0, int(x.size), unix.PROT_NONE, unix.MAP_SHARED)
	if err != nil {
		return 0, err
	}
	defer unix.Munmap(mapping)
	pages := make([]byte, len(mapping)/pageSize)
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&mapping[0])),
		uintptr(len(mapping)), uintptr(unsafe.Pointer(&pages[0])))
	if errno != 0 {
		return 0, errno
	}
	var total uint64
	for _, status := range pages {
		total += uint64(status&1) * uint64(pageSize)
	}
	return total, nil
}

func fields(path, delimiter string, names []string) (map[string]uint64, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	accepted := make(map[string]bool, len(names))
	for _, name := range names {
		accepted[name] = true
	}
	output := make(map[string]uint64)
	for _, line := range strings.Split(string(value), "\n") {
		name, rest, found := strings.Cut(line, delimiter)
		if found && accepted[name] {
			number := strings.Fields(rest)
			if len(number) < 1 {
				return nil, fmt.Errorf("empty counter %q", name)
			}
			v, err := strconv.ParseUint(number[0], 10, 64)
			if err != nil {
				return nil, err
			}
			output[name] = v
		}
	}
	return output, nil
}

func stats() (map[string]any, error) {
	begin := now()
	mem, err := fields("/proc/meminfo", ":", []string{
		"MemFree", "MemAvailable", "Cached", "Buffers", "Shmem", "Dirty", "Writeback",
		"Active(file)", "Inactive(file)", "SReclaimable", "AnonPages", "SwapCached",
	})
	if err != nil {
		return nil, err
	}
	vm, err := fields("/proc/vmstat", " ", []string{
		"pgpgin", "pgpgout", "pswpin", "pswpout", "pgmajfault", "pgsteal_kswapd",
		"pgsteal_direct", "pgscan_kswapd", "pgscan_direct", "pgfree",
		"nr_file_pages", "nr_dirty", "nr_writeback",
	})
	if err != nil {
		return nil, err
	}
	damon := map[string]string{}
	limits := []string{"enabled", "sample_interval", "aggr_interval", "min_age", "quota_ms", "quota_sz",
		"quota_reset_interval_ms", "monitor_region_start", "monitor_region_end", "nr_reclaim_tried_regions",
		"nr_reclaimed_regions", "bytes_reclaim_tried_regions", "bytes_reclaimed_regions", "nr_quota_exceeds",
		"wmarks_high", "wmarks_mid", "wmarks_low", "skip_anon"}
	for _, key := range limits {
		value, err := os.ReadFile("/sys/module/damon_reclaim/parameters/" + key)
		if err == nil {
			damon[key] = strings.TrimSpace(string(value))
		}
	}
	if damon["enabled"] == "" {
		return nil, errors.New("guest DAMON enable state unavailable")
	}
	return map[string]any{"guest_begin_ns": begin, "guest_end_ns": now(), "meminfo_kib": mem,
		"vmstat": vm, "damon": damon}, nil
}

func (x *fixture) run(action string) (map[string]any, error) {
	switch action {
	case "stats":
		return stats()
	case "fincore":
		n, err := x.fileCache()
		return map[string]any{"file_cache_resident_bytes": n}, err
	case "fill":
		return x.fill()
	case "reread":
		return x.read()
	case "close":
		if x.file != nil {
			return map[string]any{"closed": true}, errors.Join(x.file.Close(), os.Remove(x.file.Name()))
		}
		return map[string]any{"closed": true}, nil
	}
	return nil, errors.New("unsupported fixed pressure action")
}

func serve() error {
	listener, err := vsock.Listen(4102, nil)
	if err != nil {
		return err
	}
	defer listener.Close()
	var x *fixture
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		func() {
			defer conn.Close()
			if peer, ok := conn.RemoteAddr().(*vsock.Addr); !ok || peer.ContextID != vsock.Host {
				return
			}
			if conn.SetDeadline(time.Now().Add(3*time.Minute)) != nil {
				return
			}
			action, err := bufio.NewReaderSize(conn, 32).ReadSlice('\n')
			if err != nil {
				return
			}
			req := strings.TrimSpace(string(action))
			var result map[string]any
			switch {
			case x == nil && (req == "prepare-small" || req == "prepare-large"):
				size := int64(32 * mib)
				if req == "prepare-large" {
					size = 512 * mib
				}
				x, err = prepare(size)
				result = map[string]any{"file_bytes_requested": size, "file_exists": false}
			case x == nil:
				err = errors.New("pressure fixture has not been prepared")
			default:
				result, err = x.run(req)
				if req == "close" && err == nil {
					x = nil
				}
			}
			if err != nil {
				result = map[string]any{"error": err.Error()}
			}
			_ = json.NewEncoder(conn).Encode(result)
		}()
	}
}

func main() {
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
