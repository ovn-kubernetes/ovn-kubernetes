// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// DAG resolver for component dependency management
// This package provides a Directed Acyclic Graph (DAG) resolver that manages
// component dependencies and determines the correct upgrade order for components
// based on their interdependencies. It detects cycles and ensures topological
// ordering for safe component upgrades.

package dependency

import (
	"fmt"
	"sort"
)

// Component represents a node in the DAG
type Component struct {
	Name         string
	Dependencies []string
}

// DAG represents the dependency graph
type DAG struct {
	components map[string]*Component // Map of component name to component
	edges      map[string][]string   // Map of component name to its dependencies
}

// NewDAG creates a new DAG
func NewDAG() *DAG {
	return &DAG{
		components: make(map[string]*Component),
		edges:      make(map[string][]string),
	}
}

// AddComponent adds a component to the DAG with its dependencies
func (d *DAG) AddComponent(name string, deps []string) {
	d.components[name] = &Component{
		Name:         name,
		Dependencies: deps,
	}
}

// buildEdges builds the edges in the DAG based on dependencies
func (d *DAG) buildEdges() {
	// Clear existing edges to prevent duplicates on multiple calls
	d.edges = make(map[string][]string)

	for name, comp := range d.components {
		for _, dep := range comp.Dependencies {
			d.edges[dep] = append(d.edges[dep], name)
		}
	}
}

// checkForCycles uses DFS to check if the DAG has any cycles
func (d *DAG) checkForCycles() error {
	visited := make(map[string]bool)
	recStack := make(map[string]bool)

	var checkCycles func(string) error
	checkCycles = func(node string) error {
		visited[node] = true
		recStack[node] = true

		for _, target := range d.edges[node] {
			if !visited[target] {
				if err := checkCycles(target); err != nil {
					return err
				}
			} else if recStack[target] {
				return fmt.Errorf("cycle detected: %s -> %s", node, target)
			}
		}

		recStack[node] = false
		return nil
	}

	for node := range d.components {
		if !visited[node] {
			if err := checkCycles(node); err != nil {
				return err
			}
		}
	}

	return nil
}

// GetUpgradeOrder returns the order in which components should be upgraded
func (d *DAG) GetUpgradeOrder() ([]string, error) {
	// Build the edges in the DAG
	d.buildEdges()

	// Check for cycles
	if err := d.checkForCycles(); err != nil {
		return nil, err
	}

	// Calculate in-degree for each node
	inDegree := make(map[string]int)
	for name := range d.components {
		inDegree[name] = 0
	}
	for _, edges := range d.edges {
		for _, target := range edges {
			inDegree[target]++
		}
	}

	// Initialize queue with nodes that have no dependencies
	var queue []string
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}

	// Sort queue to ensure consistent order
	sort.Strings(queue)

	var result []string
	for len(queue) > 0 {
		// Get the first node from queue
		node := queue[0]
		queue = queue[1:]

		result = append(result, node)

		// Process all edges from this node
		for _, target := range d.edges[node] {
			inDegree[target]--
			if inDegree[target] == 0 {
				queue = append(queue, target)
			}
		}
		// Sort queue again to maintain consistent order
		sort.Strings(queue)
	}

	// Check if we processed all nodes
	if len(result) != len(d.components) {
		return nil, fmt.Errorf("graph has cycles or disconnected components")
	}

	return result, nil
}
