package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

type calibrationRecord struct {
	Schema       string  `json:"schema"`
	Phase        string  `json:"phase"`
	Clock        string  `json:"clock"`
	BeginNS      int64   `json:"begin_monotonic_ns"`
	EndNS        int64   `json:"end_monotonic_ns"`
	FSMagic      int64   `json:"filesystem_magic"`
	MountID      *uint64 `json:"mount_id,omitempty"`
	DAX          *bool   `json:"statx_dax,omitempty"`
	PageBytes    int     `json:"page_bytes"`
	ContentHash  string  `json:"content_sha256,omitempty"`
	Mapped       *bool   `json:"pagemap_present,omitempty"`
	GuestPFN     *uint64 `json:"guest_pfn,omitempty"`
	Flags        *uint64 `json:"flags_uint64,omitempty"`
	FlagClass    string  `json:"class,omitempty"`
	ObserveError string  `json:"observation_error,omitempty"`
}

func calibrate(dir string, encoder *json.Encoder) error {
	f, err := os.CreateTemp(dir, ".zb-page-calibration-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &stat); err != nil {
		return err
	}
	var attr unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &attr); err != nil {
		return err
	}
	template := calibrationRecord{
		Schema: "aries.zb.guest.calibration.v1", Clock: "guest-CLOCK_MONOTONIC",
		PageBytes: os.Getpagesize(), FSMagic: stat.Type,
	}
	if attr.Mask&unix.STATX_MNT_ID != 0 {
		template.MountID = &attr.Mnt_id
	}
	if attr.Attributes_mask&unix.STATX_ATTR_DAX != 0 {
		value := attr.Attributes&unix.STATX_ATTR_DAX != 0
		template.DAX = &value
	}

	starter := bytes.Repeat([]byte{0x56}, template.PageBytes)
	if _, err := f.Write(starter); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	// This advice applies to a single private calibration file. It can be
	// ignored by the kernel, so a later first touch alone does not prove I/O.
	if err := unix.Fadvise(int(f.Fd()), 0, int64(len(starter)), unix.FADV_DONTNEED); err != nil {
		template.ObserveError = fmt.Sprintf("initial cache discard unconfirmed: %v", err)
	}
	mapping, err := unix.Mmap(int(f.Fd()), 0, template.PageBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	defer unix.Munmap(mapping)

	for _, phase := range []struct {
		name   string
		action func() error
	}{
		{"before_touch", nil},
		{"known_read", func() error {
			if !bytes.Equal(mapping, starter) {
				return fmt.Errorf("initial calibration bytes mismatch")
			}
			return nil
		}},
		{"buffered_write", func() error { mapping[0] = 0x1d; return nil }},
		{"synced_write", func() error {
			if err := unix.Msync(mapping, unix.MS_SYNC); err != nil {
				return err
			}
			return f.Sync()
		}},
	} {
		event := template
		event.Phase = phase.name
		event.BeginNS, err = monotonic()
		if err != nil {
			return err
		}
		if phase.action != nil {
			if err := phase.action(); err != nil {
				return fmt.Errorf("%s: %w", phase.name, err)
			}
			h := sha256.Sum256(mapping)
			event.ContentHash = hex.EncodeToString(h[:])
		}
		event.observe(mapping)
		event.EndNS, err = monotonic()
		if err != nil {
			return err
		}
		if err := encoder.Encode(event); err != nil {
			return err
		}
	}
	return nil
}

func (event *calibrationRecord) observe(mapping []byte) {
	address := uintptr(unsafe.Pointer(&mapping[0]))
	raw, err := readWord("/proc/self/pagemap", int64(address)/int64(event.PageBytes))
	if err != nil {
		event.addError(err)
		return
	}
	present := raw&(1<<63) != 0
	event.Mapped = &present
	if !present {
		return
	}
	pfn := raw & ((1 << 55) - 1)
	if pfn == 0 {
		event.addError(fmt.Errorf("guest pagemap PFN is masked or zero"))
		return
	}
	event.GuestPFN = &pfn
	flags, err := readWord("/proc/kpageflags", int64(pfn))
	if err != nil {
		event.addError(err)
		return
	}
	// Recheck the mapping after reading guest PFN metadata. Equal endpoints
	// cannot detect a page removed and reinserted between the two reads.
	again, err := readWord("/proc/self/pagemap", int64(address)/int64(event.PageBytes))
	if err != nil {
		event.addError(err)
		return
	}
	if again&(1<<63) == 0 || again&((1<<55)-1) != pfn {
		event.addError(fmt.Errorf("guest PFN changed during observation"))
		return
	}
	event.Flags, event.FlagClass = &flags, flagClass(flags)
}

func (event *calibrationRecord) addError(err error) {
	if event.ObserveError != "" {
		event.ObserveError += "; "
	}
	event.ObserveError += err.Error()
}

func readWord(path string, index int64) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var data [8]byte
	n, err := f.ReadAt(data[:], index*8)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if n != len(data) {
		return 0, fmt.Errorf("%s: %w", path, io.ErrUnexpectedEOF)
	}
	return binary.NativeEndian.Uint64(data[:]), nil
}
