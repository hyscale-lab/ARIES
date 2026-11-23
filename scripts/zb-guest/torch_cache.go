package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Selected files from the original 4b588047 replay and its sealed dependency
// receipt. Never fetch, hash file data, walk the task tree or parse guest requests.
func torchCachePaths() []string {
	paths := []string{"tmp/parallel-linear-pip.pyz", "app/parallel_linear.py"}
	for _, name := range []string{
		"lib/libtorch_cpu.so", "lib/libtorch_python.so", "lib/libtorch.so", "lib/libc10.so",
		"lib/libshm.so", "lib/libgomp-a34b3233.so.1", "_C.cpython-312-x86_64-linux-gnu.so",
		"__init__.py", "nn/modules/linear.py",
	} {
		paths = append(paths, "tmp/parallel-linear-deps/torch/"+name)
	}
	for _, url := range []string{
		"https://download-r2.pytorch.org/whl/cpu/torch-2.5.1%2Bcpu-cp312-cp312-linux_x86_64.whl",
		"https://download.pytorch.org/whl/jinja2-3.1.6-py3-none-any.whl",
		"https://download.pytorch.org/whl/markupsafe-3.0.3-cp312-cp312-manylinux2014_x86_64.manylinux_2_17_x86_64.manylinux_2_28_x86_64.whl",
		"https://download.pytorch.org/whl/setuptools-78.1.0-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/b2/fe/81695a1aa331a842b582453b605175f419fe8540355886031328089d840a/sympy-1.13.1-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/43/e3/7d92a15f894aa0c9c4b49b8ee9ac9850d6e63b03c9c32c0367a13ae62209/mpmath-1.3.0-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/49/d3/b8441a820a491ddfc024b0b0cf0393375b75ea13866d9c66727e54c2fc80/typing_extensions-4.16.0-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/a7/8e/50f46a9c0ce8d2861a394c1347caae037ea0431d2f67d7feb151cbc4649a/filelock-3.32.3-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/fd/3c/6a2bf344106328fd04963664a60b9bb6496fc25df8e962fcdc1367285fb9/fsspec-2026.7.0-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/9e/c9/b2622292ea83fbb4ec318f5b9ab867d0a28ab43c5717bb85b0a5f6b3b0a4/networkx-3.6.1-py3-none-any.whl",
	} {
		key := fmt.Sprintf("%x", sha256.Sum224([]byte(url)))
		paths = append(paths, filepath.Join("tmp/aries-torch-wheel-cache/http-v2", key[0:1], key[1:2], key[2:3], key[3:4], key[4:5], key+".body"))
	}
	return paths
}

type torchCacheFile struct {
	Device    uint64   `json:"device"`
	Inode     uint64   `json:"inode"`
	UID       uint32   `json:"uid"`
	Size      int64    `json:"size_bytes"`
	Paths     []string `json:"paths"`
	Pages     uint64   `json:"cached_pages"`
	Dirty     uint64   `json:"dirty_pages"`
	Writeback uint64   `json:"writeback_pages"`
	Before    int      `json:"vmas_before"`
	After     int      `json:"vmas_after"`
}

type cacheControl struct {
	Device    uint64 `json:"device"`
	Cold      uint64 `json:"cold_pages"`
	Written   uint64 `json:"written_pages"`
	Untouched uint64 `json:"untouched_sibling_pages"`
}

type torchCacheScan struct {
	Begin        int64             `json:"begin_ns"`
	End          int64             `json:"end_ns"`
	QueryStart   int64             `json:"query_begin_ns"`
	QueryEnd     int64             `json:"query_end_ns"`
	Root         string            `json:"task_root"`
	RootDevice   uint64            `json:"task_device"`
	EUID         int               `json:"euid"`
	Files        []*torchCacheFile `json:"files"`
	Missing      []string          `json:"missing_paths"`
	Errors       map[string]string `json:"errors"`
	MapsComplete bool              `json:"stable_complete_maps_census"`
	MapsPIDs     int               `json:"maps_pid_count"`
	Control      cacheControl      `json:"control"`
}

func fileCache(fd int, off, length uint64) (unix.Cachestat_t, error) {
	var out unix.Cachestat_t
	err := unix.Cachestat(uint(fd), &unix.CachestatRange{Off: off, Len: length}, &out, 0)
	return out, err
}

func checkCacheControl(root int, device uint64) (cacheControl, error) {
	var out cacheControl
	fd, err := unix.Openat(root, "tmp", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return out, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return out, err
	}
	if st.Dev != device {
		return out, errors.New("calibration mount differs")
	}
	out.Device = st.Dev
	page := int64(os.Getpagesize())
	if err = unix.Ftruncate(fd, 4*page); err != nil {
		return out, err
	}
	cold, err := fileCache(fd, 0, 0)
	if err != nil {
		return out, err
	}
	out.Cold = cold.Cache
	if n, err := unix.Pwrite(fd, make([]byte, page), page); err != nil || int64(n) != page {
		return out, fmt.Errorf("control write %d/%d: %v", n, page, err)
	}
	hot, err := fileCache(fd, uint64(page), uint64(page))
	if err != nil {
		return out, err
	}
	out.Written = hot.Cache
	untouched, err := fileCache(fd, 2*uint64(page), uint64(page))
	if err != nil {
		return out, err
	}
	out.Untouched = untouched.Cache
	if out.Cold != 0 || out.Written != 1 || out.Untouched != 0 {
		return out, errors.New("cold/written/untouched control failed")
	}
	return out, nil
}

func candidateVMAs(device uint64) (map[fileID]int, []string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil, err
	}
	counts := make(map[fileID]int)
	var pids []string
	const maxMapsBytes = 1 << 20
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if len(pids) == 192 {
			return nil, nil, errors.New("process census limit")
		}
		pids = append(pids, e.Name())
		f, err := os.Open(filepath.Join("/proc", e.Name(), "maps"))
		if err != nil {
			return nil, nil, err
		}
		buf, err := io.ReadAll(io.LimitReader(f, maxMapsBytes+1))
		f.Close()
		if err != nil || len(buf) > maxMapsBytes {
			return nil, nil, fmt.Errorf("incomplete maps for PID %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(buf)), "\n") {
			if line == "" {
				continue
			}
			f := strings.Fields(line)
			if len(f) < 5 {
				return nil, nil, errors.New("malformed maps row")
			}
			if f[4] == "0" {
				continue
			}
			var major, minor uint32
			if _, err := fmt.Sscanf(f[3], "%x:%x", &major, &minor); err != nil {
				return nil, nil, err
			}
			if unix.Mkdev(major, minor) != device {
				continue
			}
			inode, err := strconv.ParseUint(f[4], 10, 64)
			if err != nil {
				return nil, nil, err
			}
			counts[fileID{device, inode}]++
		}
	}
	return counts, pids, nil
}

func torchCacheFileStat(root int, path string, device uint64, seen map[fileID]*torchCacheFile) (*torchCacheFile, error) {
	fd, err := unix.Openat2(root, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var before, after unix.Stat_t
	if err = unix.Fstat(fd, &before); err != nil {
		return nil, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || before.Dev != device || before.Size <= 0 || before.Size > 1<<30 {
		return nil, errors.New("candidate is not a bounded regular file on the task filesystem")
	}
	id := fileID{before.Dev, before.Ino}
	if alias := seen[id]; alias != nil {
		alias.Paths = append(alias.Paths, path)
		return nil, nil
	}
	s, err := fileCache(fd, 0, uint64(before.Size))
	if err != nil {
		return nil, err
	}
	if err = unix.Fstat(fd, &after); err != nil {
		return nil, err
	}
	if before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim || s.Cache > (uint64(after.Size)+uint64(os.Getpagesize())-1)/uint64(os.Getpagesize()) {
		return nil, errors.New("concurrent mutation or impossible page count")
	}
	out := &torchCacheFile{Device: before.Dev, Inode: before.Ino, UID: before.Uid, Size: before.Size, Paths: []string{path}, Pages: s.Cache, Dirty: s.Dirty, Writeback: s.Writeback}
	seen[id] = out
	return out, nil
}

var cachedTorchControl cacheControl
var cachedTorchControlErr error
var runTorchControl sync.Once

func collectTorchCache() (out torchCacheScan) {
	out.Begin, _ = monotonic()
	defer func() { out.End, _ = monotonic() }()
	out.Errors = make(map[string]string)
	out.EUID = os.Geteuid()
	roots, err := filepath.Glob("/container/aries-torch-tensor-parallelism-*/rootfs")
	if err != nil || len(roots) != 1 {
		out.Errors["root"] = "need exactly one mounted PyTorch task root"
		return
	}
	out.Root = roots[0]
	root, err := unix.Open(out.Root, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		out.Errors["root"] = err.Error()
		return
	}
	defer unix.Close(root)
	var fs unix.Statfs_t
	var st unix.Stat_t
	if unix.Fstatfs(root, &fs) != nil || unix.Fstat(root, &st) != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		out.Errors["root"] = "task root is not a verified ext4 filesystem"
		return
	}
	out.RootDevice = st.Dev
	runTorchControl.Do(func() { cachedTorchControl, cachedTorchControlErr = checkCacheControl(root, st.Dev) })
	out.Control = cachedTorchControl
	if cachedTorchControlErr != nil || out.Control.Device != st.Dev {
		out.Errors["control"] = fmt.Sprintf("unverified cachestat control: %v", cachedTorchControlErr)
		return
	}
	before, pidsA, errA := candidateVMAs(st.Dev)
	out.QueryStart, _ = monotonic()
	seen := make(map[fileID]*torchCacheFile)
	for _, path := range torchCachePaths() {
		file, err := torchCacheFileStat(root, path, st.Dev, seen)
		if errors.Is(err, unix.ENOENT) {
			out.Missing = append(out.Missing, path)
		} else if err != nil {
			out.Errors[path] = err.Error()
		} else if file != nil {
			out.Files = append(out.Files, file)
		}
	}
	out.QueryEnd, _ = monotonic()
	after, pidsB, errB := candidateVMAs(st.Dev)
	out.MapsPIDs = len(pidsB)
	out.MapsComplete = errA == nil && errB == nil && slices.Equal(pidsA, pidsB)
	if !out.MapsComplete {
		out.Errors["maps"] = fmt.Sprintf("incomplete or changing census: before=%v after=%v", errA, errB)
	}
	for id, file := range seen {
		file.Before, file.After = before[id], after[id]
	}
	return
}
