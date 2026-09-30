package sshserve

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestByteCounterTracksConcurrentPipeTraffic(t *testing.T) {
	const chunks = 128
	payload := bytes.Repeat([]byte("late-stream-content"), 32)
	want := int64(chunks * len(payload))

	readPipe, writePipe := io.Pipe()
	readCounter := &ByteCounter{Reader: readPipe}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, readCounter)
		readDone <- err
	}()
	stopReadPolling := pollCounter(readCounter)
	for range chunks {
		if _, err := writePipe.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := writePipe.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	stopReadPolling()
	if got := readCounter.Count(); got != want {
		t.Fatalf("read count = %d, want %d", got, want)
	}

	readPipe, writePipe = io.Pipe()
	writeCounter := &ByteCounter{Writer: writePipe}
	drainDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, readPipe)
		drainDone <- err
	}()
	stopWritePolling := pollCounter(writeCounter)
	for range chunks {
		if _, err := writeCounter.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := writePipe.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-drainDone; err != nil {
		t.Fatal(err)
	}
	stopWritePolling()
	if got := writeCounter.Count(); got != want {
		t.Fatalf("write count = %d, want %d", got, want)
	}
}

func TestRecordedInputKeepsRawAndUsesSafeStructuredEncoding(t *testing.T) {
	for _, test := range []struct {
		name, want, encoding string
		content              []byte
	}{
		{name: "utf8", content: []byte("actual stdin\n"), want: "actual stdin\n", encoding: "utf-8"},
		{name: "binary", content: []byte{0, 0xff}, want: "[binary input omitted; 2 bytes retained in ssh_raw.log]", encoding: "binary-omitted"},
		{name: "utf8 control", content: []byte("prefix\x00suffix"), want: "[binary input omitted; 13 bytes retained in ssh_raw.log]", encoding: "binary-omitted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := &RecordedInput{Reader: bytes.NewReader(test.content)}
			if _, err := io.Copy(io.Discard, input); err != nil {
				t.Fatal(err)
			}
			count, content, encoding, raw, overflow := input.Record(true)
			if count != int64(len(test.content)) || content != test.want || encoding != test.encoding || !bytes.Equal(raw, test.content) || overflow {
				t.Fatalf("record = %d %q %q", count, content, encoding)
			}
		})
	}
	t.Run("bounded", func(t *testing.T) {
		input := &RecordedInput{Reader: io.LimitReader(zeroReader{}, MaxRecordedInputBytes+1)}
		if _, err := io.Copy(io.Discard, input); err == nil || !strings.Contains(err.Error(), "stdin exceeds") {
			t.Fatalf("oversized stdin error = %v", err)
		}
		count, content, encoding, raw, overflow := input.Record(true)
		if count <= MaxRecordedInputBytes || content != "" || encoding != "utf-8" || len(raw) != 0 || !overflow {
			t.Fatalf("bounded record = count %d content %d encoding %q", count, len(content), encoding)
		}
	})
}

func TestRecordedInputSnapshotsCountAndContentTogether(t *testing.T) {
	input := &RecordedInput{Reader: &singleByteReader{remaining: 1 << 16}}
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, input)
		done <- err
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			count, content, encoding, raw, overflow := input.Record(true)
			if encoding != "utf-8" || count != int64(len(content)) || !bytes.Equal(raw, []byte(content)) || overflow {
				t.Fatalf("final snapshot = count %d content %d encoding %q", count, len(content), encoding)
			}
			return
		default:
			count, content, encoding, raw, overflow := input.Record(true)
			if encoding != "utf-8" || count != int64(len(content)) || !bytes.Equal(raw, []byte(content)) || overflow {
				t.Fatalf("inconsistent snapshot = count %d content %d encoding %q", count, len(content), encoding)
			}
		}
	}
}

type singleByteReader struct{ remaining int }

func (reader *singleByteReader) Read(content []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	content[0] = 'x'
	reader.remaining--
	return 1, nil
}

type zeroReader struct{}

func (zeroReader) Read(content []byte) (int, error) {
	clear(content)
	return len(content), nil
}

func pollCounter(counter *ByteCounter) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = counter.Count()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func TestBinaryStdinNoteMatchesRawRetention(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		retained bool
		want     string
	}{
		{"retained", true, "retained in ssh_raw.log"},
		{"omitted", false, "not retained"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input := &RecordedInput{Reader: bytes.NewReader(nil)}
			input.data.Write([]byte{0x00, 0x01, 0x02})
			input.n = 3
			_, note, encoding, _, _ := input.Record(testCase.retained)
			if encoding != "binary-omitted" {
				t.Fatalf("encoding = %q", encoding)
			}
			if !strings.Contains(note, testCase.want) {
				t.Fatalf("note = %q, want it to mention %q", note, testCase.want)
			}
		})
	}
}
