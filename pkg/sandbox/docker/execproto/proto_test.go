package execproto

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestProtocolRoundTrip(t *testing.T) {
	var stream bytes.Buffer
	if err := WriteHello(&stream); err != nil {
		t.Fatal(err)
	}
	if err := WriteInput(&stream, []byte("input")); err != nil {
		t.Fatal(err)
	}
	if err := WriteResult(&stream, Result{ExitCode: 7, Stdout: []byte("out"), Stderr: []byte("err")}); err != nil {
		t.Fatal(err)
	}
	if err := ReadHello(&stream); err != nil {
		t.Fatal(err)
	}
	input, err := ReadInput(&stream, 5)
	if err != nil || string(input) != "input" {
		t.Fatalf("ReadInput() = %q, %v", input, err)
	}
	result, err := ReadResult(&stream, 6)
	if err != nil || result.ExitCode != 7 || string(result.Stdout) != "out" || string(result.Stderr) != "err" {
		t.Fatalf("ReadResult() = %#v, %v", result, err)
	}
}

func TestProtocolRejectsMalformedOrOversizedFrames(t *testing.T) {
	if err := ReadHello(strings.NewReader("wrong-greeting")); err == nil {
		t.Fatal("ReadHello() accepted a wrong greeting")
	}
	var input bytes.Buffer
	_ = binary.Write(&input, binary.BigEndian, uint32(5))
	input.WriteString("12345")
	if _, err := ReadInput(&input, 4); err == nil {
		t.Fatal("ReadInput() accepted oversized input")
	}
	var output bytes.Buffer
	_ = binary.Write(&output, binary.BigEndian, int32(0))
	_ = binary.Write(&output, binary.BigEndian, uint32(3))
	output.WriteString("123")
	_ = binary.Write(&output, binary.BigEndian, uint32(2))
	output.WriteString("45")
	if _, err := ReadResult(&output, 4); err == nil {
		t.Fatal("ReadResult() accepted oversized combined output")
	}
}
