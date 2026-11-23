// A fixed guest file-backing calibration over a host-only vsock.
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
	"net"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

const page = 4096
const offset = page
const root = "/var/lib/zb-filecal"

type fixture struct {
	file     *os.File
	direct   int
	mapping  []byte
	stage    string
	baseline string
	synced   string
	fs       unix.Statfs_t
	stat     unix.Statx_t
}

type observation struct {
	Action      string  `json:"action"`
	Stage       string  `json:"stage"`
	Inode       uint64  `json:"inode"`
	DeviceMajor uint32  `json:"device_major"`
	DeviceMinor uint32  `json:"device_minor"`
	FSMagic     int64   `json:"filesystem_magic"`
	MountID     uint64  `json:"mount_id"`
	Mode        uint16  `json:"mode"`
	DAX         *bool   `json:"statx_dax"`
	Offset      int64   `json:"file_offset_bytes"`
	Size        uint64  `json:"file_size_bytes"`
	PTEPresent  bool    `json:"pte_present"`
	Cache       bool    `json:"mincore_present"`
	PFN         *uint64 `json:"guest_pfn,omitempty"`
	Flags       *uint64 `json:"guest_page_flags,omitempty"`
	Content     string  `json:"mapped_sha256,omitempty"`
	Backing     string  `json:"known_synced_backing_sha256,omitempty"`
}

func hash(bytes []byte) string {
	h := sha256.Sum256(bytes)
	return hex.EncodeToString(h[:])
}

func aligned() ([]byte, error) {
	return unix.Mmap(-1, 0, page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
}

func prepare() (_ *fixture, retErr error) {
	if os.Getpagesize() != page {
		return nil, errors.New("fixture requires 4096-byte pages")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(root, "backing-*")
	if err != nil {
		return nil, err
	}
	x := &fixture{file: f, direct: -1}
	defer func() {
		if retErr != nil {
			x.close()
		}
	}()
	if err := f.Truncate(2 * page); err != nil {
		return nil, err
	}
	x.direct, err = unix.Open(filepath.Clean(f.Name()), unix.O_RDWR|unix.O_DIRECT|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	buffer, err := aligned()
	if err != nil {
		return nil, err
	}
	defer unix.Munmap(buffer)
	for i := 0; i < len(buffer); i++ {
		buffer[i] = byte(i*139+7) ^ byte(i>>8)
	}
	x.baseline = hash(buffer)
	if n, err := unix.Pwrite(x.direct, buffer, offset); err != nil || n != page {
		return nil, fmt.Errorf("direct write %d: %w", n, shortIO(err))
	}
	if err := unix.Fsync(x.direct); err != nil {
		return nil, err
	}
	if n, err := unix.Pread(x.direct, buffer, offset); err != nil || n != page || hash(buffer) != x.baseline {
		return nil, fmt.Errorf("direct readback %d: %w", n, shortIO(err))
	}
	if err := unix.Fstatfs(int(f.Fd()), &x.fs); err != nil {
		return nil, err
	}
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH,
		unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &x.stat); err != nil {
		return nil, err
	}
	if x.stat.Mode&unix.S_IFMT != unix.S_IFREG || x.fs.Type == unix.TMPFS_MAGIC || x.stat.Attributes&unix.STATX_ATTR_DAX != 0 {
		return nil, errors.New("fixture must be an ordinary non-tmpfs buffered file")
	}
	x.mapping, err = unix.Mmap(int(f.Fd()), offset, page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	x.stage = "prepared"
	sample, err := x.observe("prepare", "", x.baseline)
	if err != nil {
		return nil, err
	}
	if sample.PTEPresent || sample.Cache {
		return nil, errors.New("preparation unexpectedly populated the guest file cache or PTE")
	}
	return x, nil
}

func shortIO(err error) error {
	if err == nil {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (f *fixture) close() {
	if f.mapping != nil {
		_ = unix.Munmap(f.mapping)
	}
	if f.direct >= 0 {
		_ = unix.Close(f.direct)
	}
	_ = f.file.Close()
	_ = os.Remove(f.file.Name())
}

func readWord(path string, offset uint64) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var data [8]byte
	n, err := f.ReadAt(data[:], int64(offset))
	if err != nil || n != len(data) {
		return 0, shortIO(err)
	}
	return binary.NativeEndian.Uint64(data[:]), nil
}

func (f *fixture) observe(action, content, backing string) (observation, error) {
	o := observation{
		Action: action, Stage: f.stage, Inode: f.stat.Ino, DeviceMajor: f.stat.Dev_major,
		DeviceMinor: f.stat.Dev_minor, FSMagic: f.fs.Type, MountID: f.stat.Mnt_id,
		Mode: f.stat.Mode, Offset: offset, Size: f.stat.Size, Content: content, Backing: backing,
	}
	if f.stat.Attributes_mask&unix.STATX_ATTR_DAX != 0 {
		dax := f.stat.Attributes&unix.STATX_ATTR_DAX != 0
		o.DAX = &dax
	}
	address := uintptr(unsafe.Pointer(&f.mapping[0]))
	var cache byte
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, address, page, uintptr(unsafe.Pointer(&cache)))
	if errno != 0 {
		return o, errno
	}
	o.Cache = cache&1 != 0
	pmOffset := uint64(address) / page * 8
	entry, err := readWord("/proc/self/pagemap", pmOffset)
	if err != nil {
		return o, err
	}
	o.PTEPresent = entry>>63 != 0
	if !o.PTEPresent {
		return o, nil
	}
	pfn := entry & ((1 << 55) - 1)
	if pfn == 0 {
		return o, errors.New("guest PFN masked or zero")
	}
	flags, err := readWord("/proc/kpageflags", pfn*8)
	if err != nil {
		return o, err
	}
	again, err := readWord("/proc/self/pagemap", pmOffset)
	if err != nil || entry != again {
		return o, errors.New("guest PFN changed while sampling file flags")
	}
	o.PFN, o.Flags = &pfn, &flags
	return o, nil
}

func (f *fixture) step(action string) (observation, error) {
	switch action {
	case "state":
		return f.observe("state", "", f.baseline)
	case "read":
		if f.stage != "prepared" {
			break
		}
		content := hash(f.mapping)
		if content != f.baseline {
			return observation{}, errors.New("initial mapped read differs from synced backing")
		}
		f.stage = "read"
		return f.observe(action, content, f.baseline)
	case "write":
		if f.stage != "read" {
			break
		}
		f.mapping[0] ^= 0xff
		f.stage = "written"
		return f.observe(action, hash(f.mapping), "")
	case "sync":
		if f.stage != "written" {
			break
		}
		if err := unix.Msync(f.mapping, unix.MS_SYNC); err != nil {
			return observation{}, err
		}
		if err := f.file.Sync(); err != nil {
			return observation{}, err
		}
		buffer, err := aligned()
		if err != nil {
			return observation{}, err
		}
		defer unix.Munmap(buffer)
		n, err := unix.Pread(f.direct, buffer, offset)
		if err != nil || n != page {
			return observation{}, fmt.Errorf("synced direct readback %d: %w", n, shortIO(err))
		}
		f.stage = "synced"
		f.synced = hash(buffer)
		return f.observe(action, hash(f.mapping), f.synced)
	case "evict":
		if f.stage != "synced" {
			break
		}
		if err := unix.Munmap(f.mapping); err != nil {
			return observation{}, err
		}
		f.mapping = nil
		f.stage = "unmapped"
		if err := unix.Fadvise(int(f.file.Fd()), offset, page, unix.FADV_DONTNEED); err != nil {
			return observation{}, fmt.Errorf("advise eviction: %w", err)
		}
		// Remap without touching the page so mincore can check the file cache.
		var err error
		f.mapping, err = unix.Mmap(int(f.file.Fd()), offset, page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return observation{}, err
		}
		f.stage = "eviction-requested"
		return f.observe(action, "", f.synced)
	case "reread":
		if f.stage != "eviction-requested" {
			break
		}
		f.stage = "reread"
		return f.observe(action, hash(f.mapping), f.synced)
	}
	return observation{}, fmt.Errorf("invalid action %q in stage %s", action, f.stage)
}

func serve() error {
	listener, err := vsock.Listen(4101, nil)
	if err != nil {
		return err
	}
	defer listener.Close()
	var f *fixture
	defer func() {
		if f != nil {
			f.close()
		}
	}()
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
			if conn.SetDeadline(time.Now().Add(10*time.Second)) != nil {
				return
			}
			action, err := bufio.NewReaderSize(conn, 32).ReadSlice('\n')
			if err != nil {
				return
			}
			encode := json.NewEncoder(conn)
			var value observation
			switch string(action) {
			case "prepare\n":
				if f != nil {
					err = errors.New("fixture already exists")
				} else {
					f, err = prepare()
					if err == nil {
						value, err = f.observe("prepare", "", f.baseline)
					}
				}
			case "close\n":
				if f != nil {
					f.close()
					f = nil
				}
				_ = encode.Encode(map[string]bool{"closed": true})
				return
			default:
				if f == nil {
					err = errors.New("fixture must be prepared")
				} else {
					value, err = f.step(string(action[:len(action)-1]))
				}
			}
			if err != nil {
				_ = encode.Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = encode.Encode(value)
		}()
	}
}

func main() {
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var _ net.Listener = (*vsock.Listener)(nil)
