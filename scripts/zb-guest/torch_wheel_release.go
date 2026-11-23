package main

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

type wheelAdvice struct {
	Path        string `json:"path"`
	Device      uint64 `json:"device"`
	Inode       uint64 `json:"inode"`
	Size        int64  `json:"size_bytes"`
	BeforePages uint64 `json:"rechecked_cached_pages"`
	ReadOnly    bool   `json:"fd_read_only"`
	Begin       int64  `json:"begin_ns"`
	End         int64  `json:"end_ns"`
	Error       string `json:"error,omitempty"`
}

type wheelRelease struct {
	Schema string        `json:"schema"`
	Before sample        `json:"before"`
	After  *sample       `json:"after,omitempty"`
	Advice []wheelAdvice `json:"advice"`
	Error  string        `json:"error,omitempty"`
}

// The host may request this once in the dedicated study fixture. Scope comes
// from the fixed 4b58 candidate list, excluding all installed libraries.
func releaseTorchWheels() (out wheelRelease) {
	out.Schema = "aries.zb.guest.torch-wheel-dontneed.v1"
	out.Before = snapshot(0, 0)
	before := out.Before.TorchFileCache
	if before == nil || len(before.Errors) != 0 || !before.MapsComplete {
		out.Error = "wheel release requires a complete controlled initial cache and process-map census"
		return
	}
	root, err := unix.Open(before.Root, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		out.Error = err.Error()
		return
	}
	defer unix.Close(root)
	byName := make(map[string]*torchCacheFile)
	for _, f := range before.Files {
		if len(f.Paths) == 1 {
			byName[f.Paths[0]] = f
		}
	}
	type prepared struct {
		fd     int
		stat   unix.Stat_t
		result wheelAdvice
	}
	var ready []prepared
	defer func() {
		for _, item := range ready {
			unix.Close(item.fd)
		}
	}()
	for _, path := range torchCachePaths() {
		if !strings.HasPrefix(path, "tmp/aries-torch-wheel-cache/http-v2/") {
			continue
		}
		f := byName[path]
		if f == nil || f.Before != 0 || f.After != 0 {
			out.Error = fmt.Sprintf("missing or mapped wheel candidate: %s", path)
			return
		}
		fd, err := unix.Openat2(root, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK,
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
		if err != nil {
			out.Error = fmt.Sprintf("open %s: %v", path, err)
			return
		}
		var st unix.Stat_t
		check := unix.Fstat(fd, &st)
		flags, flagErr := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if check != nil || flagErr != nil || flags&unix.O_ACCMODE != unix.O_RDONLY || st.Mode&unix.S_IFMT != unix.S_IFREG ||
			st.Dev != f.Device || st.Ino != f.Inode || st.Size != f.Size || st.Uid != 0 {
			unix.Close(fd)
			out.Error = fmt.Sprintf("wheel identity changed: %s (%v)", path, check)
			return
		}
		cache, filePages, err := cacheResidency(fd, 0, st.Size, true)
		if err != nil || cache != f.Pages || !slices.Equal(filePages, f.PageRuns) {
			unix.Close(fd)
			out.Error = fmt.Sprintf("wheel residency changed before advice: %s, %d versus %d (%v)", path, cache, f.Pages, err)
			return
		}
		ready = append(ready, prepared{fd, st, wheelAdvice{Path: path, Device: st.Dev, Inode: st.Ino, Size: st.Size, BeforePages: cache, ReadOnly: true}})
	}
	if len(ready) != 10 {
		out.Error = "unexpected wheel candidate count"
		return
	}
	maps, pids, err := candidateVMAs(before.RootDevice)
	if err != nil || len(pids) != before.MapsPIDs {
		out.Error = fmt.Sprintf("final wheel mapping census changed (%v)", err)
		return
	}
	for _, item := range ready {
		var current unix.Stat_t
		if maps[fileID{item.stat.Dev, item.stat.Ino}] != 0 || unix.Fstat(item.fd, &current) != nil ||
			current.Size != item.stat.Size || current.Mtim != item.stat.Mtim || current.Ctim != item.stat.Ctim ||
			current.Dev != item.stat.Dev || current.Ino != item.stat.Ino {
			out.Error = fmt.Sprintf("wheel mapping or metadata changed: %s", item.result.Path)
			return
		}
	}
	for _, item := range ready {
		r := item.result
		r.Begin, _ = monotonic()
		if err := unix.Fadvise(item.fd, 0, 0, unix.FADV_DONTNEED); err != nil {
			r.Error = err.Error()
		}
		r.End, _ = monotonic()
		out.Advice = append(out.Advice, r)
	}
	after := snapshot(0, 0)
	out.After = &after
	return
}
