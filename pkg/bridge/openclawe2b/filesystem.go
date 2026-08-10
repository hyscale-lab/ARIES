package openclawe2b

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/runner"
	arsandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
)

const (
	maxFilesystemJSONBody = 1 << 20
	maxRawFileBody        = 64 << 20
)

type filesystemSandbox interface {
	runner.Sandbox
	ReadFile(context.Context, string) ([]byte, error)
	WriteFile(context.Context, string, []byte) error
	StatPath(context.Context, string) (arsandbox.FileInfo, error)
	ListDir(context.Context, string) ([]arsandbox.FileInfo, error)
	MakeDir(context.Context, string) error
	RemovePath(context.Context, string) error
	MovePath(context.Context, string, string) error
}

type filesystemPathRequest struct {
	Path string `json:"path"`
}

type filesystemMoveRequest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

type filesystemEntry struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	Mode       string `json:"mode,omitempty"`
	ModifiedAt string `json:"modifiedAt,omitempty"`
	LinkTarget string `json:"linkTarget,omitempty"`
}

func (server *Server) serveRawFile(response http.ResponseWriter, request *http.Request) {
	sandbox, ok := server.filesystemFor(request)
	if !ok {
		writeJSONError(response, http.StatusBadRequest, "sandbox does not support filesystem operations")
		return
	}
	target, err := validateFilesystemPath(request.URL.Query().Get("path"), false)
	if err != nil {
		writeJSONError(response, http.StatusBadRequest, err.Error())
		return
	}
	switch request.Method {
	case http.MethodGet:
		content, err := sandbox.ReadFile(request.Context(), target)
		if err != nil {
			writeJSONError(response, http.StatusNotFound, err.Error())
			return
		}
		response.Header().Set("Content-Type", "application/octet-stream")
		_, _ = response.Write(content)
	case http.MethodPost:
		mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/octet-stream" {
			writeJSONError(response, http.StatusUnsupportedMediaType, "raw file writes require application/octet-stream")
			return
		}
		request.Body = http.MaxBytesReader(response, request.Body, maxRawFileBody)
		content, err := io.ReadAll(request.Body)
		if err != nil {
			writeJSONError(response, http.StatusBadRequest, fmt.Sprintf("read raw file body: %v", err))
			return
		}
		if err := sandbox.WriteFile(request.Context(), target, content); err != nil {
			writeJSONError(response, http.StatusConflict, err.Error())
			return
		}
		writeJSONOK(response)
	default:
		writeJSONError(response, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (server *Server) serveFilesystem(response http.ResponseWriter, request *http.Request) {
	sandbox, ok := server.filesystemFor(request)
	if !ok {
		writeJSONError(response, http.StatusBadRequest, "sandbox does not support filesystem operations")
		return
	}
	switch request.URL.Path {
	case "/v1/filesystem/stat":
		payload, err := decodeFilesystemPath(response, request, false)
		if err != nil {
			writeJSONError(response, http.StatusBadRequest, err.Error())
			return
		}
		info, err := sandbox.StatPath(request.Context(), payload.Path)
		if err != nil {
			writeJSONError(response, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(response, http.StatusOK, map[string]filesystemEntry{"entry": filesystemEntryFrom(info)})
	case "/v1/filesystem/list-dir":
		payload, err := decodeFilesystemPath(response, request, false)
		if err != nil {
			writeJSONError(response, http.StatusBadRequest, err.Error())
			return
		}
		infos, err := sandbox.ListDir(request.Context(), payload.Path)
		if err != nil {
			writeJSONError(response, http.StatusNotFound, err.Error())
			return
		}
		entries := make([]filesystemEntry, 0, len(infos))
		for _, info := range infos {
			entries = append(entries, filesystemEntryFrom(info))
		}
		writeJSON(response, http.StatusOK, map[string][]filesystemEntry{"entries": entries})
	case "/v1/filesystem/make-dir":
		payload, err := decodeFilesystemPath(response, request, true)
		if err != nil {
			writeJSONError(response, http.StatusBadRequest, err.Error())
			return
		}
		if err := sandbox.MakeDir(request.Context(), payload.Path); err != nil {
			writeJSONError(response, http.StatusConflict, err.Error())
			return
		}
		writeJSONOK(response)
	case "/v1/filesystem/remove":
		payload, err := decodeFilesystemPath(response, request, true)
		if err != nil {
			writeJSONError(response, http.StatusBadRequest, err.Error())
			return
		}
		if err := sandbox.RemovePath(request.Context(), payload.Path); err != nil {
			writeJSONError(response, http.StatusConflict, err.Error())
			return
		}
		writeJSONOK(response)
	case "/v1/filesystem/move":
		payload, err := decodeFilesystemMove(response, request)
		if err != nil {
			writeJSONError(response, http.StatusBadRequest, err.Error())
			return
		}
		if err := sandbox.MovePath(request.Context(), payload.Source, payload.Destination); err != nil {
			writeJSONError(response, http.StatusConflict, err.Error())
			return
		}
		writeJSONOK(response)
	default:
		writeJSONError(response, http.StatusNotFound, "filesystem route not found")
	}
}

func (server *Server) filesystemFor(request *http.Request) (filesystemSandbox, bool) {
	registration := server.registrationForAdmittedRequest(request.Header.Get(sandboxIDHeader))
	sandbox, ok := registration.sandbox.(filesystemSandbox)
	return sandbox, ok
}

func decodeFilesystemPath(response http.ResponseWriter, request *http.Request, rejectRoot bool) (filesystemPathRequest, error) {
	var payload filesystemPathRequest
	if err := decodeFilesystemJSON(response, request, &payload); err != nil {
		return payload, err
	}
	clean, err := validateFilesystemPath(payload.Path, rejectRoot)
	payload.Path = clean
	return payload, err
}

func decodeFilesystemMove(response http.ResponseWriter, request *http.Request) (filesystemMoveRequest, error) {
	var payload filesystemMoveRequest
	if err := decodeFilesystemJSON(response, request, &payload); err != nil {
		return payload, err
	}
	var err error
	if payload.Source, err = validateFilesystemPath(payload.Source, true); err != nil {
		return payload, fmt.Errorf("invalid source: %w", err)
	}
	if payload.Destination, err = validateFilesystemPath(payload.Destination, true); err != nil {
		return payload, fmt.Errorf("invalid destination: %w", err)
	}
	return payload, nil
}

func decodeFilesystemJSON(response http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxFilesystemJSONBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode filesystem request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fmt.Errorf("decode filesystem request: %w", err)
	}
	return nil
}

func validateFilesystemPath(value string, rejectRoot bool) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) || !strings.HasPrefix(value, "/") {
		return "", errors.New("path must be absolute, nonempty, and NUL-free")
	}
	clean := path.Clean(value)
	if clean != value {
		return "", errors.New("path must be normalized")
	}
	if rejectRoot && clean == "/" {
		return "", errors.New("container root cannot be modified or removed")
	}
	return clean, nil
}

func filesystemEntryFrom(info arsandbox.FileInfo) filesystemEntry {
	entry := filesystemEntry{
		Path: info.Path, Name: info.Name, Type: info.Type, Size: info.Size,
		Mode: fmt.Sprintf("%04o", info.Mode.Perm()), LinkTarget: info.LinkTarget,
	}
	if !info.ModTime.IsZero() {
		entry.ModifiedAt = info.ModTime.UTC().Format(time.RFC3339Nano)
	}
	return entry
}

func writeJSONOK(response http.ResponseWriter) {
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
