package roadmapbench

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const absencePredicate = `for path do [ ! -e "$path" ] && [ ! -L "$path" ] || exit 1; done`

// PrepareSandbox removes Harbor verifier/oracle staging before bridge access.
// Upstream images also stage a Diesel patch and a Polars oracle wheel cache.
func (b *Benchmark) PrepareSandbox(ctx context.Context, task core.Task, sandbox runner.Sandbox) error {
	if sandbox == nil {
		return errors.New("roadmapbench preparation requires a live sandbox")
	}
	b.mu.RLock()
	details, loaded := b.details[task.ID]
	b.mu.RUnlock()
	if !loaded {
		return fmt.Errorf("roadmapbench task %q was not loaded by Tasks", task.ID)
	}
	paths := []string{testsPath, verifierLogPath, "/solution", "/tmp/changes.patch", "/opt/polars-upgrade"}
	// This image downloads the oracle wheel with pip caching enabled. Remove
	// that second copy as well, including for scheduled task occurrences.
	if details.taskID == "plr-1.35.0-roadmap" {
		paths = append(paths, "/root/.cache/pip")
	}
	if err := execChecked(ctx, sandbox, core.Command{
		Path: "/bin/rm", Args: append([]string{"-rf", "--"}, paths...), User: "0:0",
	}); err != nil {
		return fmt.Errorf("remove private paths before harness: %w", err)
	}
	if err := execChecked(ctx, sandbox, core.Command{
		Path: "/bin/sh", Args: append([]string{"-c", absencePredicate, "aries-roadmapbench-absence"}, paths...), User: "0:0",
	}); err != nil {
		return fmt.Errorf("confirm private paths absent before harness: %w", err)
	}
	return nil
}

// These pinned scripts can reuse earlier results after a failed compiler/test
// pipeline. Remove only their output files, retaining dependency/object caches.
func verifierScratchPaths(id string) []string {
	switch id {
	case "opt-3.0.0-roadmap":
		return []string{"/tmp/opt_test_results"}
	case "glz-6.3.0-roadmap":
		return []string{"/tmp/test_01", "/tmp/test_02", "/tmp/test_03", "/tmp/test_04", "/tmp/test_05", "/tmp/test_06"}
	case "glz-4.0.0-roadmap":
		return []string{
			"/app/build/tests/phase_tests/test_01_strict_int_parsing",
			"/app/build/tests/phase_tests/test_02_array_char_string",
			"/app/build/tests/phase_tests/test_03_pair_range_roundtrip",
			"/app/build/tests/phase_tests/test_04_json_t_enhance",
			"/app/build/tests/phase_tests/test_05_atomic_and_fixes",
		}
	case "glz-6.2.0-roadmap":
		return []string{
			"/app/build/tests/benchmark_tests/test_01_jsonrpc_registry",
			"/app/build/tests/benchmark_tests/test_02_repe_buffer",
			"/app/build/tests/benchmark_tests/test_03_zero_copy_repe",
			"/app/build/tests/benchmark_tests/test_04_repe_plugin",
			"/app/build/tests/benchmark_tests/test_05_indexed_rename_key",
			"/app/build/tests/benchmark_tests/test_06_skip_null_on_read",
			"/app/build/tests/benchmark_tests/test_07_buffer_pool",
		}
	case "glz-6.5.1-roadmap":
		return []string{
			"/app/build/tests/benchmark_test/test_01_bounded_buffer",
			"/app/build/tests/benchmark_test/test_02_dos_prevention",
			"/app/build/tests/benchmark_test/test_03_allocation_limits",
			"/app/build/tests/benchmark_test/test_04_raw_pointers",
		}
	case "glz-7.0.1-roadmap":
		return []string{"/app/build_bench/bench_test_01", "/app/build_bench/bench_test_02", "/app/build_bench/bench_test_03", "/app/build_bench/bench_test_04"}
	case "ruf-0.3.0-roadmap", "ruf-0.12.0-roadmap":
		return []string{"/app/target/debug/ruff"}
	default:
		return nil
	}
}

func execChecked(ctx context.Context, sandbox runner.Sandbox, command core.Command) error {
	result, err := sandbox.Exec(ctx, command)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%s exited with code %d", command.Path, result.ExitCode)
	}
	return nil
}
