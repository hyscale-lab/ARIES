// exec-reference measures local child execution inside an already running
// sandbox. The per-child timer excludes this helper's startup and JSON output.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type reference struct {
	SamplesMS []float64 `json:"samples_ms"`
	Output    string    `json:"output"`
}

func measure(ctx context.Context, body string, count, warmups int) (reference, error) {
	result := reference{SamplesMS: make([]float64, 0, count)}
	if body == "" || count < 1 || warmups < 0 {
		return result, errors.New("command and positive sample count are required; warmups must be nonnegative")
	}
	for i := 0; i < count+warmups; i++ {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", body)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		start := time.Now()
		err := cmd.Run()
		duration := time.Since(start)
		if err != nil {
			return result, fmt.Errorf("native child %d: %w: %s", i, err, stderr.String())
		}
		if stderr.Len() != 0 {
			return result, fmt.Errorf("native child %d wrote stderr: %s", i, stderr.String())
		}
		if i == 0 {
			result.Output = stdout.String()
		} else if stdout.String() != result.Output {
			return result, fmt.Errorf("native child %d changed output", i)
		}
		if i >= warmups {
			result.SamplesMS = append(result.SamplesMS, float64(duration)/float64(time.Millisecond))
		}
	}
	return result, nil
}

func main() {
	body := flag.String("command", "", "exact /bin/sh -c body")
	count := flag.Int("n", 50, "measured local child launches")
	warmups := flag.Int("warmups", 2, "unmeasured local child launches")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := measure(ctx, *body, *count, *warmups)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
