package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/sirupsen/logrus"
)

func main() {
	logger := newLogger()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := dispatch(ctx, os.Args[1:], os.Stdout, app.Dependencies{Logger: logger, Wiring: commandWiring()}, app.Run, app.Setup); err != nil {
		logger.WithError(err).Error("aries failed")
		os.Exit(1)
	}
}

type useCase func(context.Context, string, io.Writer, app.Dependencies) error

func dispatch(ctx context.Context, args []string, stdout io.Writer, dependencies app.Dependencies, run, setup useCase) error {
	switch {
	case len(args) == 1:
		return run(ctx, args[0], stdout, dependencies)
	case len(args) == 2 && args[0] == "setup":
		return setup(ctx, args[1], stdout, dependencies)
	default:
		return errors.New("usage: aries PROFILE.json | aries setup PROFILE.json")
	}
}

func newLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	logger.SetLevel(logrus.InfoLevel)
	logger.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano})
	return logger
}
