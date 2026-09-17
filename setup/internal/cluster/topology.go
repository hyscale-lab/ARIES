package cluster

import (
	"fmt"
	"slices"

	"github.com/hyscale-lab/aries/setup/internal/configs"
)

// RoleLabel is the label and taint key that dedicates a node to an ARIES role.
const RoleLabel = "aries.dev/role"

// The role pools. RoleAries also runs the monitoring stack by default and is
// where the ARIES chart is pinned.
const (
	RoleAries   = "aries"
	RoleHarness = "harness"
	RoleSandbox = "sandbox"
)

// Roles in the order they are applied. A node listed in several pools takes
// the last one, since it can hold only one aries.dev/role.
var Roles = []string{RoleAries, RoleHarness, RoleSandbox}

func pool(cluster configs.Cluster, role string) []string {
	switch role {
	case RoleAries:
		return cluster.AriesNodes
	case RoleHarness:
		return cluster.HarnessNodes
	case RoleSandbox:
		return cluster.SandboxNodes
	}
	return nil
}

// Workers is every node to join, deduplicated and without the master. Listing
// the master in a pool is harmless, and a node in several pools is installed
// once.
func Workers(cluster configs.Cluster) []string {
	seen := map[string]bool{cluster.Master: true}
	var out []string
	add := func(targets []string) {
		for _, target := range targets {
			if target != "" && !seen[target] {
				seen[target] = true
				out = append(out, target)
			}
		}
	}
	for _, role := range Roles {
		add(pool(cluster, role))
	}
	add(cluster.Workers)
	return out
}

// RoleOf returns the role a node ends up with, or "worker" for a plain one.
func RoleOf(cluster configs.Cluster, target string) string {
	role := "worker"
	for _, candidate := range Roles {
		for _, node := range pool(cluster, candidate) {
			if node == target {
				role = candidate
			}
		}
	}
	return role
}

// CheckPrometheusPlacement rejects a deploy_prometheus configuration whose
// monitoring pods could never schedule, before create_cluster touches a node.
func CheckPrometheusPlacement(cluster configs.Cluster, prom configs.Prometheus) error {
	if cluster.SkipRoleTaints {
		return fmt.Errorf("deploy_prometheus needs role labels to place monitoring, but skip_role_taints is set")
	}
	// The master is labelled but keeps kubeadm's control-plane taint, which
	// the role toleration does not cover.
	if !slices.Contains(Roles, prom.NodeRole) {
		return fmt.Errorf("prom_config.json node_role %q must be one of %v", prom.NodeRole, Roles)
	}
	for _, assignment := range Assignments(cluster) {
		if assignment.Role == prom.NodeRole {
			return nil
		}
	}
	return fmt.Errorf("deploy_prometheus places monitoring on role %q, but no node in cluster.json ends up with that role", prom.NodeRole)
}

// Assignment is one node's final role.
type Assignment struct {
	Target string
	Role   string
}

// Assignments lists each role-pool node once with its final role, in pool
// order, excluding the master (which keeps kubeadm's own control-plane taint).
func Assignments(cluster configs.Cluster) []Assignment {
	var out []Assignment
	for _, target := range Workers(cluster) {
		if role := RoleOf(cluster, target); role != "worker" {
			out = append(out, Assignment{Target: target, Role: role})
		}
	}
	return out
}
