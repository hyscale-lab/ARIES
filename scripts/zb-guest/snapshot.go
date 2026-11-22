package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maxPFNs      = 2 << 20 // A single scan reads at most 16 MiB of page metadata.
	maxPFNOffset = (1 << 55) - 1
	maxRuns      = 1 << 18
	dirBudget    = 256
)

const (
	dirty     = uint64(1 << 4)
	lru       = uint64(1 << 5)
	slab      = uint64(1 << 7)
	writeback = uint64(1 << 8)
	buddy     = uint64(1 << 10)
	anon      = uint64(1 << 12)
	swapcache = uint64(1 << 13)
	swapback  = uint64(1 << 14)
	compound  = uint64(1<<15 | 1<<16 | 1<<17 | 1<<22)
	noPage    = uint64(1 << 20)
	offline   = uint64(1 << 23)
	pgtable   = uint64(1 << 26)
	upToDate  = uint64(1 << 3)
)

type sample struct {
	Schema           string            `json:"schema"`
	Clock            string            `json:"clock"`
	BeginNS          int64             `json:"begin_monotonic_ns"`
	EndNS            int64             `json:"end_monotonic_ns"`
	KPageFlagsFromNS int64             `json:"kpageflags_begin_monotonic_ns,omitempty"`
	KPageFlagsToNS   int64             `json:"kpageflags_end_monotonic_ns,omitempty"`
	Sources          map[string]string `json:"sources"`
	Errors           map[string]string `json:"source_errors"`
	PageBytes        int               `json:"page_bytes"`
	PFNs             *pfnScan          `json:"pfn_scan,omitempty"`
}

type pfnRun struct {
	Start uint64 `json:"start_pfn"`
	End   uint64 `json:"end_pfn_exclusive"`
	Flags uint64 `json:"flags_uint64"`
	Class string `json:"class"`
}

type pfnScan struct {
	Start uint64   `json:"requested_start_pfn"`
	Count uint64   `json:"requested_pfn_count"`
	End   uint64   `json:"observed_end_pfn_exclusive"`
	State string   `json:"state"`
	Runs  []pfnRun `json:"ranges"`
}

func snapshot(start, count uint64) sample {
	out := sample{
		Schema: "aries.zb.guest.snapshot.v1", Clock: "guest-CLOCK_MONOTONIC",
		Sources: make(map[string]string), Errors: make(map[string]string), PageBytes: os.Getpagesize(),
	}
	var err error
	out.BeginNS, err = monotonic()
	if err != nil {
		out.Errors["clock_begin"] = err.Error()
	}
	for _, src := range []struct {
		path string
		max  int64
	}{
		{"/proc/meminfo", 64 << 10}, {"/proc/buddyinfo", 64 << 10},
		{"/proc/vmstat", 64 << 10}, {"/proc/swaps", 64 << 10},
		{"/proc/self/mountinfo", 256 << 10}, {"/proc/cmdline", 4096},
		{"/proc/sys/kernel/osrelease", 256},
	} {
		out.read(src.path, src.max)
	}
	for _, root := range []string{
		"/sys/module/damon_reclaim/parameters",
		"/sys/module/damon_lru_sort/parameters",
		"/sys/kernel/mm/damon/admin",
	} {
		out.damon(root)
	}
	if count != 0 {
		out.KPageFlagsFromNS, err = monotonic()
		if err != nil {
			out.Errors["kpageflags_clock_begin"] = err.Error()
		}
		out.PFNs, err = guestFlags(start, count)
		if err != nil {
			out.Errors["/proc/kpageflags"] = err.Error()
		}
		out.KPageFlagsToNS, err = monotonic()
		if err != nil {
			out.Errors["kpageflags_clock_end"] = err.Error()
		}
	}
	out.EndNS, err = monotonic()
	if err != nil {
		out.Errors["clock_end"] = err.Error()
	}
	return out
}

func monotonic() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return ts.Nano(), nil
}

func (s *sample) read(path string, limit int64) {
	f, err := os.Open(path)
	if err != nil {
		s.Errors[path] = err.Error()
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		s.Errors[path] = err.Error()
	} else if int64(len(b)) > limit {
		s.Errors[path] = "sample exceeded byte limit"
	} else {
		s.Sources[path] = string(b)
	}
}

func (s *sample) damon(root string) {
	remaining := dirBudget
	err := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			s.Errors[path] = err.Error()
			return nil
		}
		remaining--
		if remaining < 0 {
			return errors.New("sample exceeded entry limit")
		}
		if de.IsDir() {
			if de.Name() == "tried_regions" {
				s.read(filepath.Join(path, "total_bytes"), 1024)
				return filepath.SkipDir
			}
			if strings.Count(strings.TrimPrefix(path, root), "/") > 11 {
				return filepath.SkipDir
			}
		} else if de.Type().IsRegular() {
			s.read(path, 1024)
		}
		return nil
	})
	if err != nil {
		s.Errors[root] = err.Error()
	}
}

func guestFlags(start, count uint64) (*pfnScan, error) {
	out := &pfnScan{Start: start, Count: count, End: start, State: "unavailable", Runs: []pfnRun{}}
	f, err := os.Open("/proc/kpageflags")
	if err != nil {
		return out, err
	}
	defer f.Close()
	err = scanFlags(f, out, maxRuns)
	return out, err
}

func scanFlags(f io.ReaderAt, out *pfnScan, runLimit int) error {
	var block [32 << 10]byte
	requestedEnd := out.Start + out.Count
	for out.End < requestedEnd {
		pages := min(requestedEnd-out.End, uint64(len(block)/8))
		n, err := f.ReadAt(block[:pages*8], int64(out.End*8))
		if n%8 != 0 {
			out.State = "read_error"
			return errors.New("unaligned /proc/kpageflags reply")
		}
		for i := 0; i < n; i += 8 {
			flags := binary.NativeEndian.Uint64(block[i:])
			if len(out.Runs) > 0 && out.Runs[len(out.Runs)-1].Flags == flags {
				out.Runs[len(out.Runs)-1].End++
			} else {
				if len(out.Runs) == runLimit {
					out.State = "range_limit"
					return nil
				}
				out.Runs = append(out.Runs, pfnRun{out.End, out.End + 1, flags, flagClass(flags)})
			}
			out.End++
		}
		if errors.Is(err, io.EOF) {
			if out.End == requestedEnd {
				break
			}
			out.State = "end_of_kpageflags"
			return nil
		}
		if err != nil || uint64(n) != pages*8 {
			out.State = "read_error"
			return fmt.Errorf("kpageflags read at PFN %d: %v", out.End, err)
		}
	}
	out.State = "complete"
	return nil
}

func flagClass(flags uint64) string {
	// These are mutually exclusive convenience labels. Raw bits retain
	// overlaps for the analysis. No absence of a flag proves an allocation.
	switch {
	case flags&(noPage|offline) != 0:
		return "no-page-or-offline"
	case flags&buddy != 0:
		return "buddy-flagged"
	case flags&slab != 0:
		return "slab-flagged"
	case flags&pgtable != 0:
		return "page-table-flagged"
	case flags&compound != 0:
		return "compound-unresolved"
	case flags&anon != 0:
		return "anonymous-flagged"
	case flags&(swapback|swapcache) != 0:
		return "swap-backed-or-cache-flagged"
	case flags&(lru|upToDate) == lru|upToDate:
		if flags&writeback != 0 {
			return "file-cache-candidate-writeback"
		}
		if flags&dirty != 0 {
			return "file-cache-candidate-dirty"
		}
		return "file-cache-candidate-clean"
	default:
		return "unknown"
	}
}
