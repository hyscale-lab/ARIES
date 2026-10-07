package core

import "testing"

func TestEndpointHostRejectsSSHOptionsAndInvalidDNS(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "bridge.task.svc.cluster.local", "task-bridge"} {
		if !ValidEndpointHost(host) {
			t.Errorf("valid host %q rejected", host)
		}
	}
	for _, host := range []string{"", "0.0.0.0", "224.0.0.1", "::1", "-oProxyCommand=x", "bad..svc", "bridge/other", "bridge\nother", "bridge."} {
		if ValidEndpointHost(host) {
			t.Errorf("invalid host %q accepted", host)
		}
	}
}
