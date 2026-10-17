package dsh

import "testing"

func TestPinnedHelperAndSandboxStreams(t *testing.T) {
	dialect := Dialect{}
	prepared, refusal := dialect.Prepare(HelperCommand, "/app")
	if refusal != nil || prepared.Command.Path != RuntimeRoot+"/node" || prepared.Command.Dir != "/app" {
		t.Fatalf("helper command: %+v %+v", prepared, refusal)
	}
	for _, wire := range []string{"bash -c 'echo test'", HelperCommand + " ; whoami", "node arbitrary.js"} {
		if _, refusal := dialect.Prepare(wire, "/app"); refusal == nil {
			t.Fatalf("accepted non-native invocation %q", wire)
		}
	}
	if prepared, refusal := dialect.PrepareStreamLocal("/tmp/dsh-ssh-a1b2/stream-1", "/app"); refusal != nil || prepared.Command.Args[1] != "/tmp/dsh-ssh-a1b2/stream-1" {
		t.Fatalf("native stream: %+v %+v", prepared, refusal)
	}
	for _, socket := range []string{"/var/run/docker.sock", "/tmp/dsh-ssh-a/../secret", "/tmp/dsh-ssh-a/", "/tmp/dsh-ssh-a/b\x00"} {
		if _, refusal := dialect.PrepareStreamLocal(socket, "/app"); refusal == nil {
			t.Fatalf("accepted unrelated socket %q", socket)
		}
	}
}
