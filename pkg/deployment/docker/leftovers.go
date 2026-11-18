package docker

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/moby/moby/client"
)

// Leftover is a container or network that ARIES labelled with a run.
type Leftover struct {
	ID   string
	Name string
}

// RunLeftovers lists what carries runID on the daemon: the run's containers,
// running or stopped, and its networks. A run removes them as it ends; these
// are what a Runner that stopped without cleaning up left, for Stop and
// StopNetwork. Labels are checked again on what the daemon returns.
func (manager *Manager) RunLeftovers(ctx context.Context, runID string) ([]Leftover, []Leftover, error) {
	if err := validateIdentity("run", runID); err != nil {
		return nil, nil, err
	}
	filters := make(client.Filters).Add("label", "aries.managed=true", "aries.run="+runID)
	containers, err := manager.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, nil, fmt.Errorf("list the run's containers: %w", err)
	}
	var runtimes []Leftover
	for _, summary := range containers.Items {
		if summary.Labels["aries.managed"] != "true" || summary.Labels["aries.run"] != runID || summary.ID == "" {
			continue
		}
		name := summary.ID
		if len(summary.Names) > 0 {
			name = strings.TrimPrefix(summary.Names[0], "/")
		}
		runtimes = append(runtimes, Leftover{ID: summary.ID, Name: name})
	}
	listed, err := manager.client.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil {
		return nil, nil, fmt.Errorf("list the run's networks: %w", err)
	}
	var networks []Leftover
	for _, summary := range listed.Items {
		if summary.Labels["aries.managed"] != "true" || summary.Labels["aries.run"] != runID || summary.ID == "" {
			continue
		}
		networks = append(networks, Leftover{ID: summary.ID, Name: summary.Name})
	}
	byName := func(items []Leftover) func(int, int) bool {
		return func(i, j int) bool { return items[i].Name < items[j].Name }
	}
	sort.Slice(runtimes, byName(runtimes))
	sort.Slice(networks, byName(networks))
	return runtimes, networks, nil
}
