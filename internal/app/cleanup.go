package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/sirupsen/logrus"
)

// Leftover is one runtime or network that a run left behind, with the removal
// its backend uses at the end of a run.
type Leftover struct {
	Kind   string
	Name   string
	Remove func(context.Context) error
}

// CleanupOptions select how Cleanup treats a run.
type CleanupOptions struct {
	// DryRun lists what would be removed and removes nothing.
	DryRun bool
	// Force skips the check that the run's Runner has stopped.
	Force bool
}

// Cleanup removes what a run left behind when its Runner stopped without
// cleaning up, such as after a kill or a lost node. It refuses while that
// Runner may still be running, unless options.Force is set. A run that left
// nothing is reported as such, so the command is safe to repeat.
func Cleanup(ctx context.Context, profilePath, runID string, options CleanupOptions, stdout io.Writer, dependencies Dependencies) (returnErr error) {
	logger := dependencies.Logger
	if logger == nil {
		logger = newLogger()
	}
	if runID == "" || runID != filepath.Base(runID) || runID == "." || runID == ".." || strings.ContainsAny(runID, `/\`) {
		return fmt.Errorf("run ID %q must be the name of a run directory", runID)
	}
	cfg, err := config.Load(profilePath)
	if err != nil {
		return err
	}
	if dependencies.Wiring.FindLeftovers == nil {
		return errors.New("cleanup wiring is incomplete")
	}
	outputRoot, err := resolveOutputRoot(cfg.OutputDir, runID)
	if err != nil {
		return err
	}
	if !options.Force {
		if err := refuseLiveRunner(outputRoot, dependencies.hostname(), dependencies.processStart()); err != nil {
			return err
		}
	}
	leftovers, closeLeftovers, err := dependencies.Wiring.FindLeftovers(ctx, cfg, runID, outputRoot, logger)
	if err != nil {
		return fmt.Errorf("find what run %s left: %w", runID, err)
	}
	if closeLeftovers != nil {
		defer func() { returnErr = errors.Join(returnErr, closeLeftovers()) }()
	}
	entry := logger.WithFields(logrus.Fields{"run_id": runID, "profile": cfg.Name, "component": "cleanup"})
	if len(leftovers) == 0 {
		_, err := fmt.Fprintf(stdout, "run %s left nothing to remove\n", runID)
		return err
	}
	var failures []error
	for _, leftover := range leftovers {
		if options.DryRun {
			if _, err := fmt.Fprintf(stdout, "would remove %s %s\n", leftover.Kind, leftover.Name); err != nil {
				return err
			}
			continue
		}
		if removeErr := leftover.Remove(ctx); removeErr != nil {
			entry.WithError(removeErr).WithFields(logrus.Fields{"kind": leftover.Kind, "name": leftover.Name}).Error("leftover not removed")
			failures = append(failures, fmt.Errorf("%s %s: %w", leftover.Kind, leftover.Name, removeErr))
			if _, err := fmt.Fprintf(stdout, "failed to remove %s %s\n", leftover.Kind, leftover.Name); err != nil {
				return errors.Join(append(failures, err)...)
			}
			continue
		}
		entry.WithFields(logrus.Fields{"kind": leftover.Kind, "name": leftover.Name}).Info("leftover removed")
		if _, err := fmt.Fprintf(stdout, "removed %s %s\n", leftover.Kind, leftover.Name); err != nil {
			return errors.Join(append(failures, err)...)
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("%d of %d left by run %s not removed: %w", len(failures), len(leftovers), runID, errors.Join(failures...))
	}
	return nil
}

// refuseLiveRunner returns an error unless the run's runner record shows that
// its Runner, on this host, has stopped. Without a record, or on another host,
// ARIES cannot tell, and the operator decides with --force.
func refuseLiveRunner(outputRoot string, hostname func() (string, error), processStart func(int) (uint64, bool, error)) error {
	record, err := readRunnerRecord(outputRoot)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s has no %s, so ARIES cannot tell whether that run has stopped; check that it has, then use --force", outputRoot, runnerRecordName)
	}
	if err != nil {
		return fmt.Errorf("%w, so ARIES cannot tell whether that run has stopped; check that it has, then use --force", err)
	}
	host, err := hostname()
	if err != nil {
		return fmt.Errorf("read this host's name: %w", err)
	}
	if record.Host != host {
		return fmt.Errorf("the run's Runner ran on host %q, not %q; check that it has stopped, then use --force", record.Host, host)
	}
	started, alive, err := processStart(record.PID)
	if err != nil {
		return fmt.Errorf("check the run's Runner (PID %d): %w", record.PID, err)
	}
	if alive && started == record.Start {
		return fmt.Errorf("the run's Runner (PID %d) is still running; stop it first", record.PID)
	}
	return nil
}
