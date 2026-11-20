package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/internal/app"
	deploymentwiring "github.com/hyscale-lab/aries/internal/app/wiring/deployment"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/sirupsen/logrus"
)

func main() {
	logger := newLogger()
	// A run that RunInNamespace started waits here until its namespace has
	// internet access.
	if _, err := deploymentwiring.WaitForApptainerRunNamespace(); err != nil {
		logger.WithError(err).Error("aries could not enter its run namespace")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dependencies := app.Dependencies{Logger: logger, Wiring: commandWiring()}
	flag.BoolVar(&dependencies.RecordForReplay, "record-for-replay", false, "record sandbox calls for replay")
	flag.Parse()
	positional := flag.Args()
	if code, entered, err := runInOwnNamespace(ctx, os.Args[1:], positional, deploymentwiring.RunApptainerInNamespace); entered {
		if err != nil {
			logger.WithError(err).Error("aries failed")
		}
		os.Exit(code)
	}
	if err := dispatch(ctx, positional, os.Stdout, dependencies, app.Run, app.Setup); err != nil {
		logger.WithError(err).Error("aries failed")
		os.Exit(1)
	}
}

type useCase func(context.Context, string, io.Writer, app.Dependencies) error

func dispatch(ctx context.Context, args []string, stdout io.Writer, dependencies app.Dependencies, run, setup useCase) error {
	usage := errors.New("usage: aries [--record-for-replay] PROFILE.json | aries setup PROFILE.json")
	switch {
	case len(args) == 1:
		return run(ctx, args[0], stdout, dependencies)
	case len(args) == 2 && args[0] == "setup" && !dependencies.RecordForReplay:
		return setup(ctx, args[1], stdout, dependencies)
	default:
		return usage
	}
}

// runInOwnNamespace runs an Apptainer profile in a private network namespace:
// the run's services listen on its loopback, which no other user of a shared
// host can reach. It reports false for anything else, which runs as before.
func runInOwnNamespace(ctx context.Context, args, positional []string, enter func(context.Context, []string) (int, error)) (int, bool, error) {
	if len(positional) != 1 || os.Getenv("ARIES_RUN_NAMESPACE") != "" || !apptainerProfile(positional[0]) {
		return 0, false, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return 1, true, err
	}
	code, err := enter(ctx, append([]string{executable}, args...))
	if err != nil && code == 0 {
		code = 1
	}
	return code, true, err
}

// apptainerProfile reports whether a profile runs on Apptainer. A profile that
// does not load is left to dispatch, which reports why.
func apptainerProfile(path string) bool {
	cfg, err := config.Load(path)
	return err == nil && cfg.Sandbox.Deployment.Backend == "apptainer"
}

func newLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	logger.SetLevel(logrus.InfoLevel)
	logger.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano})
	return logger
}
