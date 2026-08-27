package kubernetes

import "io"

// limitedWriter caps how many bytes are forwarded to the underlying writer.
// Bytes beyond the limit are discarded so a runaway command cannot exhaust
// host memory when output is being buffered.
type limitedWriter struct {
	writer  io.Writer
	limit   int
	written int
}

func (w *limitedWriter) Write(content []byte) (int, error) {
	if w.written >= w.limit {
		return len(content), nil
	}
	remaining := w.limit - w.written
	if remaining >= len(content) {
		n, err := w.writer.Write(content)
		w.written += n
		return n, err
	}
	if _, err := w.writer.Write(content[:remaining]); err != nil {
		w.written += remaining
		return len(content), err
	}
	w.written += remaining
	return len(content), nil
}
