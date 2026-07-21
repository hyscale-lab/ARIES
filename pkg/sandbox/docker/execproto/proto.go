package execproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const Magic = "ARIES-EXEC-1\n"

type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

func WriteHello(writer io.Writer) error {
	_, err := io.WriteString(writer, Magic)
	return err
}

func ReadHello(reader io.Reader) error {
	buffer := make([]byte, len(Magic))
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return err
	}
	if string(buffer) != Magic {
		return errors.New("invalid ARIES exec helper greeting")
	}
	return nil
}

func WriteInput(writer io.Writer, input []byte) error {
	if uint64(len(input)) > uint64(^uint32(0)) {
		return errors.New("exec helper input is too large")
	}
	if err := binary.Write(writer, binary.BigEndian, uint32(len(input))); err != nil {
		return err
	}
	_, err := writer.Write(input)
	return err
}

func ReadInput(reader io.Reader, limit int64) ([]byte, error) {
	var size uint32
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
		return nil, err
	}
	if int64(size) > limit {
		return nil, fmt.Errorf("exec helper input exceeds %d bytes", limit)
	}
	content := make([]byte, int(size))
	_, err := io.ReadFull(reader, content)
	return content, err
}

func WriteResult(writer io.Writer, result Result) error {
	if uint64(len(result.Stdout)) > uint64(^uint32(0)) || uint64(len(result.Stderr)) > uint64(^uint32(0)) {
		return errors.New("exec helper output is too large")
	}
	exitCode := int32(result.ExitCode)
	if int(exitCode) != result.ExitCode {
		return errors.New("exec helper exit code is out of range")
	}
	if err := binary.Write(writer, binary.BigEndian, exitCode); err != nil {
		return err
	}
	if err := binary.Write(writer, binary.BigEndian, uint32(len(result.Stdout))); err != nil {
		return err
	}
	if _, err := writer.Write(result.Stdout); err != nil {
		return err
	}
	if err := binary.Write(writer, binary.BigEndian, uint32(len(result.Stderr))); err != nil {
		return err
	}
	_, err := writer.Write(result.Stderr)
	return err
}

func ReadResult(reader io.Reader, limit int64) (Result, error) {
	var exitCode int32
	var stdoutSize uint32
	if err := binary.Read(reader, binary.BigEndian, &exitCode); err != nil {
		return Result{}, err
	}
	if err := binary.Read(reader, binary.BigEndian, &stdoutSize); err != nil {
		return Result{}, err
	}
	if int64(stdoutSize) > limit {
		return Result{}, fmt.Errorf("exec helper output exceeds %d bytes", limit)
	}
	stdout := make([]byte, int(stdoutSize))
	if _, err := io.ReadFull(reader, stdout); err != nil {
		return Result{}, err
	}
	var stderrSize uint32
	if err := binary.Read(reader, binary.BigEndian, &stderrSize); err != nil {
		return Result{}, err
	}
	if int64(stdoutSize)+int64(stderrSize) > limit {
		return Result{}, fmt.Errorf("exec helper output exceeds %d bytes", limit)
	}
	stderr := make([]byte, int(stderrSize))
	if _, err := io.ReadFull(reader, stderr); err != nil {
		return Result{}, err
	}
	return Result{ExitCode: int(exitCode), Stdout: stdout, Stderr: stderr}, nil
}
