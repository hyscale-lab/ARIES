package docker

import (
	"bytes"
	"context"
	"io"
	"iter"
	"reflect"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
)

const testPullImage = "example.invalid/image:fixture@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

type fakeImageClient struct {
	present      bool
	inspectCalls int
	pullCalls    []string
	response     *fakePullResponse
}

func (fake *fakeImageClient) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	fake.inspectCalls++
	if fake.present {
		return client.ImageInspectResult{}, nil
	}
	return client.ImageInspectResult{}, cerrdefs.ErrNotFound
}

func (fake *fakeImageClient) ImagePull(_ context.Context, image string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	fake.pullCalls = append(fake.pullCalls, image)
	fake.present = true
	return fake.response, nil
}

type fakePullResponse struct {
	bytes.Reader
	waited bool
	closed bool
}

func (response *fakePullResponse) Close() error {
	response.closed = true
	return nil
}

func (response *fakePullResponse) Wait(context.Context) error {
	response.waited = true
	return nil
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
