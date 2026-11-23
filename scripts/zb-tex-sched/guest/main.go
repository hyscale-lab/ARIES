// tex-sched records the original compilation from a separate tracefs instance.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

const traceFS = "/sys/kernel/tracing"
const maxTrace = 32 << 20

var traceEvents = []string{
	"sched/sched_switch", "sched/sched_waking", "sched/sched_wakeup",
	"sched/sched_process_fork", "sched/sched_process_exec", "sched/sched_process_exit",
	"filemap/mm_filemap_add_to_page_cache", "block/block_bio_queue", "block/block_bio_complete",
}

type session struct {
	path    string
	formats map[string]string
	before  map[string]string
}

func (s *session) read(path string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.path, path))
	return string(data), err
}

func (s *session) write(path, value string) error {
	err := os.WriteFile(filepath.Join(s.path, path), []byte(value), 0600)
	if err != nil {
		return fmt.Errorf("tracefs %s=%q: %w", path, value, err)
	}
	return nil
}

func (s *session) stats() (map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(s.path, "per_cpu/cpu*/stats"))
	if err != nil || len(files) != 2 {
		return nil, fmt.Errorf("expected two trace CPU buffers: %v %v", files, err)
	}
	stats := make(map[string]string, len(files))
	for _, path := range files {
		value, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		stats[filepath.Base(filepath.Dir(path))] = string(value)
	}
	return stats, nil
}

func (s *session) arm() (err error) {
	if s.path != "" {
		return fmt.Errorf("trace already armed")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(traceFS, &fs); err != nil || fs.Type != unix.TRACEFS_MAGIC {
		if err := unix.Mount("tracefs", traceFS, "tracefs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return err
		}
	}
	path := filepath.Join(traceFS, "instances/zb-tex-compile")
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	s.path = path
	defer func() {
		if err != nil {
			_ = s.write("tracing_on", "0")
		}
	}()
	for _, setting := range [][2]string{{"tracing_on", "0"}, {"trace_clock", "mono"},
		{"buffer_size_kb", "8192"}, {"options/overwrite", "0"}} {
		if err := s.write(setting[0], setting[1]); err != nil {
			return err
		}
	}
	s.formats = make(map[string]string, len(traceEvents))
	for _, event := range traceEvents {
		base := "events/" + event
		format, err := s.read(base + "/format")
		if err != nil {
			return err
		}
		s.formats[event] = format
		if err := s.write(base+"/enable", "1"); err != nil {
			return err
		}
	}
	if err := s.write("events/sched/sched_switch/trigger", "stacktrace if prev_state & 2"); err != nil {
		return err
	}
	s.before, err = s.stats()
	if err != nil {
		return err
	}
	return s.write("tracing_on", "1")
}

func (s *session) mark(token string) (map[string]any, error) {
	if s.path == "" || len(token) != 2 || !strings.Contains("ab", token[:1]) ||
		!strings.Contains("01234", token[1:]) {
		return nil, fmt.Errorf("invalid or unarmed marker")
	}
	var start, end unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &start); err != nil {
		return nil, err
	}
	if err := s.write("trace_marker", "ZB_TEX_SYNC_"+token); err != nil {
		return nil, err
	}
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &end); err != nil {
		return nil, err
	}
	return map[string]any{"token": token, "guest_before_ns": start.Nano(), "guest_after_ns": end.Nano()}, nil
}

func (s *session) stop() (map[string]any, error) {
	if s.path == "" {
		return nil, fmt.Errorf("no active trace")
	}
	if err := s.write("tracing_on", "0"); err != nil {
		return nil, err
	}
	after, err := s.stats()
	if err != nil {
		return nil, err
	}
	clock, err := s.read("trace_clock")
	if err != nil {
		return nil, err
	}
	trigger, err := s.read("events/sched/sched_switch/trigger")
	if err != nil {
		return nil, err
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	mountinfo, err := os.ReadFile("/proc/1/mountinfo")
	if err != nil {
		return nil, err
	}
	trace, err := os.Open(filepath.Join(s.path, "trace"))
	if err != nil {
		return nil, err
	}
	defer trace.Close()
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(writer, io.LimitReader(trace, maxTrace+1))
	if err != nil || n > maxTrace {
		return nil, fmt.Errorf("trace exceeds read limit or read failed: %d %v", n, err)
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return map[string]any{
		"guest_clock": strings.TrimSpace(clock), "guest_boot_id": strings.TrimSpace(string(bootID)),
		"buffer_before": s.before, "buffer_after": after, "event_formats": s.formats,
		"switch_stack_trigger": strings.TrimSpace(trigger), "guest_mountinfo": string(mountinfo),
		"trace_text_bytes": n, "trace_gzip": compressed.Bytes(),
	}, nil
}

func (s *session) handle(conn net.Conn) {
	defer conn.Close()
	peer, ok := conn.RemoteAddr().(*vsock.Addr)
	if !ok || peer.ContextID != vsock.Host || conn.SetDeadline(time.Now().Add(20*time.Second)) != nil {
		return
	}
	request, err := bufio.NewReaderSize(io.LimitReader(conn, 32), 32).ReadString('\n')
	if err != nil {
		return
	}
	reply := map[string]any{}
	switch {
	case request == "arm\n":
		err = s.arm()
	case request == "stop\n":
		reply, err = s.stop()
	case strings.HasPrefix(request, "sync "):
		reply, err = s.mark(strings.TrimSuffix(strings.TrimPrefix(request, "sync "), "\n"))
	default:
		err = fmt.Errorf("invalid fixed observer request")
	}
	if err != nil {
		reply = map[string]any{"error": err.Error()}
	}
	_ = json.NewEncoder(conn).Encode(reply)
}

func run() error {
	listener, err := vsock.Listen(4107, nil)
	if err != nil {
		return err
	}
	defer listener.Close()
	s := new(session)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		s.handle(conn)
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
