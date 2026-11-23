package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGuestPageFlagsKeepsOverlapAndNoInferredFreeTail(t *testing.T) {
	values := []uint64{buddy, 0, lru | upToDate, lru | upToDate,
		lru | upToDate | dirty | writeback, compound | swapback, slab | compound}
	flags := make([]byte, len(values)*8)
	for i, value := range values {
		binary.NativeEndian.PutUint64(flags[i*8:], value)
	}
	out := pfnScan{Count: uint64(len(values))}
	if err := scanFlags(bytes.NewReader(flags), &out, maxRuns); err != nil {
		t.Fatal(err)
	}
	want := []pfnRun{
		{0, 1, buddy, "buddy-flagged"},
		{1, 2, 0, "unknown"},
		{2, 4, upToDate | lru, "file-cache-candidate-clean"},
		{4, 5, values[4], "file-cache-candidate-writeback"},
		{5, 6, values[5], "compound-unresolved"},
		{6, 7, values[6], "slab-flagged"},
	}
	if out.State != "complete" || out.End != uint64(len(values)) || len(out.Runs) != len(want) {
		t.Fatalf("unexpected PFN scan %#v", out)
	}
	for i, r := range want {
		if out.Runs[i] != r {
			t.Errorf("range %d = %+v, want %+v", i, out.Runs[i], r)
		}
	}
}

func TestIncompleteCoverageStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		runs  int
		state string
		end   uint64
	}{
		{"truncated_runs", 1, "range_limit", 1},
		{"metadata_file_shorter", 10, "end_of_kpageflags", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := make([]byte, 16)
			binary.NativeEndian.PutUint64(flags[8:], buddy)
			out := pfnScan{Count: 3}
			if err := scanFlags(bytes.NewReader(flags), &out, tc.runs); err != nil {
				t.Fatal(err)
			}
			if out.State != tc.state || out.End != tc.end {
				t.Fatalf("unexpected coverage %#v", out)
			}
		})
	}
}

func TestDaemonOnlyAllowsFixedSnapshot(t *testing.T) {
	check := func(message string) []byte {
		t.Helper()
		server, client := net.Pipe()
		defer client.Close()
		client.SetDeadline(time.Now().Add(5 * time.Second))
		go handleSnapshot(server, 0, 0)
		if _, err := client.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		var response bytes.Buffer
		if _, err := response.ReadFrom(client); err != nil {
			t.Fatal(err)
		}
		return response.Bytes()
	}
	if reply := check("exec who\n"); len(reply) != 0 {
		t.Fatalf("unexpected non-snapshot response %s", reply)
	}
	var result sample
	if err := json.Unmarshal(check(snapshotRequest), &result); err != nil {
		t.Fatal(err)
	}
	if result.Schema != "aries.zb.guest.snapshot.v1" || result.BeginNS <= 0 || result.EndNS < result.BeginNS ||
		!strings.Contains(result.Sources["/proc/meminfo"], "MemTotal:") || result.PFNs != nil {
		t.Fatalf("invalid telemetry reply")
	}
}

func TestCalibrationSeparatesReadBufferedWriteAndSync(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := calibrate(dir, json.NewEncoder(&out)); err != nil {
		t.Fatal(err)
	}
	var got []calibrationRecord
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var event calibrationRecord
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		got = append(got, event)
	}
	if len(got) != 4 || got[0].Phase != "before_touch" || got[1].Phase != "known_read" ||
		got[2].Phase != "buffered_write" || got[3].Phase != "synced_write" {
		t.Fatalf("unexpected calibration events %+v", got)
	}
	if len(got[1].ContentHash) != 64 || got[1].ContentHash == got[2].ContentHash ||
		got[2].ContentHash != got[3].ContentHash {
		t.Fatalf("unexpected calibration hashes %+v", got)
	}
	children, err := os.ReadDir(dir)
	if err != nil || len(children) != 0 {
		t.Fatalf("calibration left files %v %v", children, err)
	}
}

func TestMappedFileDictionaryRetainsAliasesAndUnknowns(t *testing.T) {
	index := make(pathIndex)
	index.add(fileID{8, 100}, "/a (deleted)")
	index.add(fileID{8, 100}, "/a")
	index.add(fileID{8, 100}, "/a")
	index.add(fileID{7, 99}, "")
	b, err := json.Marshal(index.files())
	if err != nil || string(b) != `[{"device":7,"inode":99,"paths":[]},{"device":8,"inode":100,"paths":["/a","/a (deleted)"]}]` {
		t.Fatalf("identity map %s %v", b, err)
	}
}
