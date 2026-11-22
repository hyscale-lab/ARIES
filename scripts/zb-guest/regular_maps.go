package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The guest only exports ranges for present file PTEs in verified ext4 or
// overlay regular files. Reading procfs metadata does not prefault the VMA.
type regularExtent struct {
	PFN      uint64 `json:"pfn"`
	Count    uint64 `json:"count"`
	FilePage uint64 `json:"file_page"`
	Inode    uint64 `json:"inode"`
	Device   uint64 `json:"device"`
	FSType   int64  `json:"fs_type"`
}

type regularMapScan struct {
	Begin     int64           `json:"begin_ns"`
	End       int64           `json:"end_ns"`
	Processes int             `json:"processes"`
	Errors    map[string]int  `json:"skipped"`
	Extents   []regularExtent `json:"extents"`
}

func fileVMAs(data []byte) []string {
	var rows []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[4] != "0" && strings.HasPrefix(fields[5], "/") {
			rows = append(rows, line)
		}
	}
	return rows
}

func collectRegularFiles() regularMapScan {
	var out regularMapScan
	out.Errors = make(map[string]int)
	out.Begin, _ = monotonic()
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, dir := range dirs {
		if filepath.Base(dir) == strconv.Itoa(os.Getpid()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "maps"))
		if err != nil {
			out.Errors["maps_read"]++
			continue
		}
		rows := fileVMAs(raw)
		if len(rows) == 0 {
			continue
		}
		pm, err := os.Open(filepath.Join(dir, "pagemap"))
		if err != nil {
			out.Errors["pagemap_open"]++
			continue
		}
		var accepted regularMapScan
		for _, row := range rows {
			key := collectRegularVMA(dir, row, pm, &accepted)
			if key != "" {
				out.Errors[key]++
			}
		}
		pm.Close()
		raw, err = os.ReadFile(filepath.Join(dir, "maps"))
		if err != nil || strings.Join(rows, "\n") != strings.Join(fileVMAs(raw), "\n") {
			out.Errors["file_maps_changed"]++
			continue
		}
		out.Extents = append(out.Extents, accepted.Extents...)
		out.Processes++
	}
	out.End, _ = monotonic()
	return out
}

func collectRegularVMA(dir, line string, pm *os.File, out *regularMapScan) string {
	f := strings.Fields(line)
	var lo, hi, offset, major, minor, inode uint64
	if _, e := fmt.Sscanf(f[0], "%x-%x", &lo, &hi); e != nil {
		return "address"
	}
	if _, e := fmt.Sscanf(f[3], "%x:%x", &major, &minor); e != nil {
		return "device"
	}
	var err error
	if offset, err = strconv.ParseUint(f[2], 16, 64); err != nil {
		return "offset"
	}
	if inode, err = strconv.ParseUint(f[4], 10, 64); err != nil {
		return "inode"
	}
	path := filepath.Join(dir, "map_files", f[0])
	var st unix.Stat_t
	if err = unix.Stat(path, &st); err != nil {
		return "map_file_stat"
	}
	if st.Ino != inode || st.Dev != unix.Mkdev(uint32(major), uint32(minor)) || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return "identity"
	}
	var fs unix.Statfs_t
	if unix.Statfs(path, &fs) != nil {
		return "statfs"
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC && fs.Type != unix.OVERLAYFS_SUPER_MAGIC {
		return "other_fs"
	}
	p := uint64(os.Getpagesize())
	if hi <= lo || hi-lo > 1<<30 || lo%p != 0 || offset%p != 0 {
		return "vma_range"
	}
	const presentFile = (uint64(1) << 63) | (uint64(1) << 61)
	buf := make([]byte, 8192)
	for va := lo; va < hi; {
		n := min(uint64(len(buf)/8), (hi-va)/p)
		block := buf[:8*n]
		if _, err = pm.ReadAt(block, int64(va/p*8)); err != nil {
			return "pagemap_read"
		}
		for i := uint64(0); i < n; i++ {
			entry := binary.NativeEndian.Uint64(block[i*8:])
			pfn := entry & ((1 << 55) - 1)
			filePage := (offset+va-lo)/p + i
			if entry&presentFile != presentFile || pfn == 0 || int64(filePage*p) >= st.Size {
				continue
			}
			rows := out.Extents
			if len(rows) > 0 {
				last := &rows[len(rows)-1]
				if last.Device == st.Dev && last.Inode == inode && last.PFN+last.Count == pfn && last.FilePage+last.Count == filePage {
					last.Count++
					continue
				}
			}
			out.Extents = append(rows, regularExtent{PFN: pfn, Count: 1, FilePage: filePage, Inode: inode, Device: st.Dev, FSType: fs.Type})
		}
		va += n * p
	}
	return ""
}
