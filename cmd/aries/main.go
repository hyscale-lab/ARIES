package main

import (
	"context"
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
	if err := app.RunCommand(ctx, os.Args[1:], os.Stdout, app.Dependencies{Logger: logger, Wiring: commandWiring()}); err != nil {
		logger.WithError(err).Error("aries failed")
		os.Exit(1)
	}
}

func newLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	logger.SetLevel(logrus.InfoLevel)
	logger.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano})
	return logger
}
