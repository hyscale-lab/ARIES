package bridgekit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func EnsurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if resolved != absolute {
		return errors.New("directory path contains a symbolic link")
	}
	return os.Chmod(path, 0o700)
}

func HasCancellationCause(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func IsPureCancellation(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsPureCancellation(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsPureCancellation(wrapped.Unwrap())
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

// WithCancellation keeps the cancellation cause on an error returned after
// revocation.
func WithCancellation(ctx context.Context, err error) error {
	contextErr := ctx.Err()
	if contextErr == nil || HasCancellationCause(err) {
		return err
	}
	// A sandbox error returned after revocation is ambiguous unless it carries
	// the cancellation cause. Preserve both so Stop fails closed rather than
	// silently treating an unconfirmed tool termination as an earlier error.
	if err == nil {
		return contextErr
	}
	return errors.Join(contextErr, err)
}
