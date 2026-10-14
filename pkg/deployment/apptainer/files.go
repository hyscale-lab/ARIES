package apptainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
)

// errNoFileOperations answers the bridge's typed file operations, which only
// the gRPC transport uses and which an Apptainer run never reaches: wiring
// refuses hermes-grpc on this backend.
var errNoFileOperations = fmt.Errorf("typed file operations on Apptainer: %w", errors.ErrUnsupported)

// StatFile is not supported; see errNoFileOperations.
func (manager *Manager) StatFile(context.Context, string, string) (fs.FileInfo, error) {
	return nil, errNoFileOperations
}

// OpenFile is not supported; see errNoFileOperations.
func (manager *Manager) OpenFile(context.Context, string, string) (io.ReadCloser, fs.FileInfo, error) {
	return nil, nil, errNoFileOperations
}

// WriteFile is not supported; see errNoFileOperations.
func (manager *Manager) WriteFile(context.Context, string, string, io.Reader, int64, string) (bool, error) {
	return false, errNoFileOperations
}
