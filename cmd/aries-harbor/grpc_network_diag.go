package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
)

const grpcLoopbackTrial = "69ba9b75-cf5b-4722-94f4-7ce3ea31bbb4"

// The private guest OS changes only its own loopback address. This reads that
// state in the task's network namespace, outside the recorded tool intervals.
func readGRPCLoopback(ctx context.Context, guest groupCommand, phase string, log *json.Encoder) error {
	const probe = `import json, os, pathlib, socket
p=pathlib.Path
rows=p('/proc/net/if_inet6').read_text().splitlines()
lo=[row for row in rows if row.split()[-1]=='lo']
flags={n:int(p('/proc/sys/net/ipv6/'+path).read_text()) for n,path in {'all':'conf/all/disable_ipv6','lo':'conf/lo/disable_ipv6','bindv6only':'bindv6only'}.items()}
with socket.socket(socket.AF_INET6) as s: s.bind(('::',0))
with socket.socket(socket.AF_INET) as s: s.bind(('127.0.0.1',0))
with socket.socket(socket.AF_INET6) as s:
    s.settimeout(2); target_errno=s.connect_ex(('::1',5328))
r={'netns':os.readlink('/proc/self/ns/net'),'ipv6_interface_addresses':rows,'ipv6_lo_addresses':lo,'sysctls':flags,'ipv6_wildcard_bind':True,'ipv4_loopback_bind':True,'early_ipv6_connect_errno':target_errno,'source_port':5328}
print(json.dumps(r))
assert not lo and not any(flags.values())`
	out, err := guest(ctx, core.Command{Path: "/usr/bin/env", Args: []string{"python3", "-I", "-S", "-c", probe}})
	if err != nil {
		return err
	}
	var readback map[string]any
	if err := json.Unmarshal([]byte(out.Stdout), &readback); err != nil {
		return fmt.Errorf("decode loopback readback: %w", err)
	}
	if err := log.Encode(map[string]any{"event": "diagnostic_grpc_network", "phase": phase, "readback": readback, "exit_code": out.ExitCode}); err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return errors.New("guest loopback diagnostic preconditions differ")
	}
	return nil
}
