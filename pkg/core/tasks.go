package core

import (
	"maps"
	"slices"
)

// CloneTasks gives each occurrence its own mutable environment and service declarations.
func CloneTasks(tasks []Task) []Task {
	result := slices.Clone(tasks)
	for i := range result {
		result[i].Environment.Env = maps.Clone(result[i].Environment.Env)
		result[i].Environment.Services.MCP = slices.Clone(result[i].Environment.Services.MCP)
	}
	return result
}
