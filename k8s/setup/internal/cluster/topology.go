package cluster

import (
	"github.com/hyscale-lab/aries/k8s/setup/internal/configs"
)

// Workers is every node to join, deduplicated and without the master. Nodes
// carry no ARIES role labels or taints: harness, sandbox, bridge and
// monitoring pods all schedule wherever there is room. The runner host is not
// a worker; configs.Cluster.Validate rejects listing it as one.
func Workers(cluster configs.Cluster) []string {
	seen := map[string]bool{cluster.Master: true}
	var out []string
	for _, target := range cluster.Workers {
		if target != "" && !seen[target] {
			seen[target] = true
			out = append(out, target)
		}
	}
	return out
}
