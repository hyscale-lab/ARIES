package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"reflect"
	"strings"
	"sync"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	imageapi "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
)

const testPullImage = "example.invalid/image:fixture@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

type fakeImageClient struct {
	mu           sync.Mutex
	present      bool
	inspectCalls int
	pullCalls    []string
	response     *fakePullResponse
	inspectErrs  []error
	pullErr      error
}

func (fake *fakeImageClient) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.inspectCalls++
	if len(fake.inspectErrs) != 0 {
		err := fake.inspectErrs[0]
		fake.inspectErrs = fake.inspectErrs[1:]
		if err != nil {
			return client.ImageInspectResult{}, err
		}
	}
	if fake.present {
		return client.ImageInspectResult{InspectResponse: imageapi.InspectResponse{ID: "sha256:prepared"}}, nil
	}
	return client.ImageInspectResult{}, cerrdefs.ErrNotFound
}

func TestPullImagesRejectsPresentImageWithoutUsableIdentity(t *testing.T) {
	fake := &fakeImageClient{present: true}
	// Override the minimal fake through a dedicated client so success cannot be
	// inferred from an error-free inspect alone.
	identityless := identitylessImageClient{fakeImageClient: fake}
	err := pullImages(context.Background(), identityless, []string{testPullImage})
	if err == nil || !strings.Contains(err.Error(), "empty image identity") || len(fake.pullCalls) != 0 {
		t.Fatalf("error=%v pulls=%v", err, fake.pullCalls)
	}
}

type identitylessImageClient struct{ *fakeImageClient }

func (fake identitylessImageClient) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	fake.inspectCalls++
	return client.ImageInspectResult{}, nil
}

func (fake *fakeImageClient) ImagePull(_ context.Context, image string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.pullCalls = append(fake.pullCalls, image)
	if fake.pullErr != nil {
		return nil, fake.pullErr
	}
	fake.present = true
	return fake.response, nil
}

func TestPullImagesPropagatesEveryDockerStageFailure(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeImageClient
		want string
	}{
		{name: "initial-inspect", fake: &fakeImageClient{inspectErrs: []error{errors.New("inspect unavailable")}}, want: "inspect Docker image"},
		{name: "pull-start", fake: &fakeImageClient{pullErr: errors.New("pull unavailable")}, want: "pull Docker image"},
		{name: "final-inspect", fake: &fakeImageClient{response: &fakePullResponse{}, inspectErrs: []error{cerrdefs.ErrNotFound, errors.New("confirm unavailable")}}, want: "confirm Docker image"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := pullImages(context.Background(), test.fake, []string{testPullImage})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPullImagesRetryAfterCancellationAndConcurrentCallersConverge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	first := &fakePullResponse{waitFunc: func(ctx context.Context) error { return ctx.Err() }}
	fake := &fakeImageClient{response: first, inspectErrs: []error{cerrdefs.ErrNotFound}}
	if err := pullImages(ctx, fake, []string{testPullImage}); !errors.Is(err, context.Canceled) {
		t.Fatalf("first error = %v", err)
	}
	// A subsequent caller must re-inspect and converge even if cancellation
	// happened after Docker had already made the image visible.
	if err := pullImages(context.Background(), fake, []string{testPullImage}); err != nil {
		t.Fatalf("retry = %v", err)
	}

	shared := &fakeImageClient{response: &fakePullResponse{}}
	start := make(chan struct{})
	errs := make(chan error, 8)
	for range 8 {
		go func() { <-start; errs <- pullImages(context.Background(), shared, []string{testPullImage}) }()
	}
	close(start)
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent pull = %v", err)
		}
	}
	shared.mu.Lock()
	pulls := len(shared.pullCalls)
	shared.mu.Unlock()
	if pulls < 1 || !shared.present {
		t.Fatalf("concurrent callers did not converge: pulls=%d present=%v", pulls, shared.present)
	}
}

type fakePullResponse struct {
	bytes.Reader
	mu       sync.Mutex
	waited   bool
	closed   bool
	waitErr  error
	closeErr error
	waitFunc func(context.Context) error
}

func (response *fakePullResponse) Close() error {
	response.mu.Lock()
	defer response.mu.Unlock()
	response.closed = true
	return response.closeErr
}

func (response *fakePullResponse) Wait(ctx context.Context) error {
	response.mu.Lock()
	response.waited = true
	response.mu.Unlock()
	if response.waitFunc != nil {
		return response.waitFunc(ctx)
	}
	return response.waitErr
}

func TestPullImagesJoinsWaitAndCloseErrors(t *testing.T) {
	waitErr := errors.New("wait failed")
	closeErr := errors.New("close failed")
	response := &fakePullResponse{waitErr: waitErr, closeErr: closeErr}
	fake := &fakeImageClient{response: response}
	err := pullImages(context.Background(), fake, []string{testPullImage})
	if !errors.Is(err, waitErr) || !errors.Is(err, closeErr) || !response.waited || !response.closed {
		t.Fatalf("error=%v waited=%v closed=%v", err, response.waited, response.closed)
	}
}

func TestPullImagesClosesCanceledPullAndDoesNotRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := &fakePullResponse{waitFunc: func(ctx context.Context) error { return ctx.Err() }}
	fake := &fakeImageClient{response: response}
	err := pullImages(ctx, fake, []string{testPullImage})
	if !errors.Is(err, context.Canceled) || !response.closed || len(fake.pullCalls) != 1 {
		t.Fatalf("error=%v closed=%v pulls=%v", err, response.closed, fake.pullCalls)
	}
}

func (response *fakePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(func(jsonstream.Message, error) bool) {}
}

func TestPullImagesSkipsPresentAndDeduplicatesPulls(t *testing.T) {
	present := &fakeImageClient{present: true, response: &fakePullResponse{}}
	if err := pullImages(context.Background(), present, []string{testPullImage, testPullImage}); err != nil {
		t.Fatal(err)
	}
	if present.inspectCalls != 1 || len(present.pullCalls) != 0 {
		t.Fatalf("present image calls = inspect %d pull %v", present.inspectCalls, present.pullCalls)
	}

	response := &fakePullResponse{}
	missing := &fakeImageClient{response: response}
	if err := pullImages(context.Background(), missing, []string{testPullImage, testPullImage}); err != nil {
		t.Fatal(err)
	}
	if missing.inspectCalls != 2 || !reflect.DeepEqual(missing.pullCalls, []string{testPullImage}) || !response.waited || !response.closed {
		t.Fatalf("missing image calls = inspect %d pull %v waited=%v closed=%v", missing.inspectCalls, missing.pullCalls, response.waited, response.closed)
	}
}

func TestPullImagesRejectsImplicitAndMalformedReferencesBeforeDocker(t *testing.T) {
	fake := &fakeImageClient{}
	err := pullImages(context.Background(), fake, []string{"example.invalid/image"})
	if err == nil || fake.inspectCalls != 0 || len(fake.pullCalls) != 0 {
		t.Fatalf("pullImages() error=%v inspect=%d pull=%v", err, fake.inspectCalls, fake.pullCalls)
	}
}

func TestPullImagesAcceptsExplicitTaskTag(t *testing.T) {
	fake := &fakeImageClient{present: true}
	if err := pullImages(context.Background(), fake, []string{"example.invalid/task:20251031"}); err != nil {
		t.Fatal(err)
	}
	if fake.inspectCalls != 1 || len(fake.pullCalls) != 0 {
		t.Fatalf("inspect=%d pull=%v", fake.inspectCalls, fake.pullCalls)
	}
}

func TestPullImagesAcceptsTaskAndHarnessTagsWithDigestReference(t *testing.T) {
	const (
		taskImage    = "example.invalid/task:20251031"
		harnessImage = "ghcr.io/openclaw/openclaw:2026.7.1"
	)
	fake := &fakeImageClient{present: true}
	if err := pullImages(context.Background(), fake, []string{taskImage, harnessImage, testPullImage}); err != nil {
		t.Fatal(err)
	}
	if fake.inspectCalls != 3 || len(fake.pullCalls) != 0 {
		t.Fatalf("inspect=%d pull=%v", fake.inspectCalls, fake.pullCalls)
	}
}

var (
	_ io.ReadCloser            = (*fakePullResponse)(nil)
	_ client.ImagePullResponse = (*fakePullResponse)(nil)
	_ imageClient              = (*fakeImageClient)(nil)
)
