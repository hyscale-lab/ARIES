package docker

import (
	"github.com/moby/moby/api/types/container"
	"testing"
)

func TestHostPIDAccessRequiresExactRuntimeRequest(t *testing.T) {
	for _, hostPID := range []bool{false, true} {
		name := "private"
		if hostPID {
			name = "host"
		}
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker()
			m := &Manager{client: fake}
			r := deploymentRequest()
			r.HostPID = hostPID
			id, err := m.Create(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			want := container.PidMode("")
			if hostPID {
				want = "host"
			}
			if fake.created.HostConfig.PidMode != want {
				t.Fatalf("PID mode=%q want=%q", fake.created.HostConfig.PidMode, want)
			}
			if err := m.Validate(t.Context(), id, r, nil); err != nil {
				t.Fatal(err)
			}
			for _, unexpected := range []container.PidMode{"container:foreign", "host", ""} {
				if unexpected == want {
					continue
				}
				fake.container.HostConfig.PidMode = unexpected
				if err := m.Validate(t.Context(), id, r, nil); err == nil {
					t.Fatalf("accepted unexpected PID mode %q", unexpected)
				}
			}
		})
	}
}
