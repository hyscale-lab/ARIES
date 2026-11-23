// This file is built into the dedicated diagnostic guest only.
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const deletionEvent = "events/filemap/mm_filemap_delete_from_page_cache"

type deletionEvidence struct {
	Filter     string            `json:"filter"`
	Format     string            `json:"format"`
	Clock      string            `json:"trace_clock"`
	Stats      map[string]string `json:"per_cpu_stats"`
	Trace      []byte            `json:"trace_gzip"`
	Begin      int64             `json:"guest_begin_ns"`
	End        int64             `json:"guest_end_ns"`
	CaptureEnd int64             `json:"guest_capture_end_ns"`
	Error      string            `json:"error,omitempty"`
}

type wheelTracer struct {
	dir      string
	evidence deletionEvidence
}

func (t *wheelTracer) set(name, value string) error {
	return os.WriteFile(filepath.Join(t.dir, name), []byte(value+"\n"), 0600)
}
func (t *wheelTracer) get(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(t.dir, name))
	return string(data), err
}
func (t *wheelTracer) close() {
	_ = t.set("tracing_on", "0")
	_ = t.set(deletionEvent+"/enable", "0")
	_ = os.Remove(t.dir)
}
func openWheelTracer(ids []wheelAdvice) (*wheelTracer, error) {
	root := "/sys/kernel/tracing"
	if _, err := os.Stat(filepath.Join(root, deletionEvent, "format")); err != nil {
		if err := unix.Mount("tracefs", root, "tracefs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return nil, fmt.Errorf("mount guest tracefs: %w", err)
		}
	}
	dir, err := os.MkdirTemp(filepath.Join(root, "instances"), "zbwheel-")
	if err != nil {
		return nil, err
	}
	t := &wheelTracer{dir: dir}
	ok := false
	defer func() {
		if !ok {
			t.close()
		}
	}()
	if len(ids) == 0 {
		return nil, fmt.Errorf("empty bounded wheel filter")
	}
	device := ids[0].Device
	predicates := make([]string, 0, len(ids))
	for _, r := range ids {
		if r.Device != device {
			return nil, fmt.Errorf("controlled inodes span filesystems")
		}
		predicates = append(predicates, fmt.Sprintf("i_ino == %d", r.Inode))
	}
	kdev := uint64(unix.Major(device))<<20 | uint64(unix.Minor(device))
	filter := fmt.Sprintf("s_dev == %d && (%s)", kdev, strings.Join(predicates, " || "))
	for _, s := range [][2]string{{"tracing_on", "0"}, {"current_tracer", "nop"}, {"trace_clock", "mono"}, {"buffer_size_kb", "8192"}, {deletionEvent + "/filter", filter}, {deletionEvent + "/enable", "1"}} {
		if err = t.set(s[0], s[1]); err != nil {
			return nil, fmt.Errorf("%s: %w", s[0], err)
		}
	}
	if _, err = os.Stat(filepath.Join(t.dir, "options/nsecs")); err == nil {
		if err = t.set("options/nsecs", "1"); err != nil {
			return nil, err
		}
	}
	if t.evidence.Filter, err = t.get(deletionEvent + "/filter"); err != nil {
		return nil, err
	}
	if t.evidence.Format, err = t.get(deletionEvent + "/format"); err != nil {
		return nil, err
	}
	for _, f := range []string{" pfn;", " i_ino;", " index;", " s_dev;", " order;"} {
		if !strings.Contains(t.evidence.Format, f) {
			return nil, fmt.Errorf("missing event field %q", f)
		}
	}
	if t.evidence.Clock, err = t.get("trace_clock"); err != nil || !strings.Contains(t.evidence.Clock, "[mono]") {
		return nil, fmt.Errorf("guest mono trace clock unavailable: %v", err)
	}
	if t.evidence.Begin, err = monotonic(); err != nil {
		return nil, err
	}
	if err = t.set("tracing_on", "1"); err != nil {
		return nil, err
	}
	ok = true
	return t, nil
}
func (t *wheelTracer) stop() error {
	if err := t.set("tracing_on", "0"); err != nil {
		return err
	}
	var err error
	t.evidence.End, err = monotonic()
	return err
}
func (t *wheelTracer) export() error {
	paths, err := filepath.Glob(filepath.Join(t.dir, "per_cpu/cpu*/stats"))
	if err != nil || len(paths) != 1 {
		return fmt.Errorf("expected one guest CPU trace buffer, found %d: %v", len(paths), err)
	}
	t.evidence.Stats = map[string]string{}
	for _, p := range paths {
		d, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		t.evidence.Stats[filepath.Base(filepath.Dir(p))] = string(d)
	}
	f, err := os.Open(filepath.Join(t.dir, "trace"))
	if err != nil {
		return err
	}
	defer f.Close()
	var encoded bytes.Buffer
	z, err := gzip.NewWriterLevel(&encoded, gzip.BestSpeed)
	if err != nil {
		return err
	}
	const limit = 32 << 20
	n, err := io.Copy(z, io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if err = z.Close(); err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("guest trace exceeds %d bytes", limit)
	}
	t.evidence.Trace = encoded.Bytes()
	t.evidence.CaptureEnd, err = monotonic()
	return err
}
