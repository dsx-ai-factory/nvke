// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rollout

import (
	"errors"
	"fmt"
)

// ErrNotRegistered reports that a name has no entry in the Registry.
// ErrGuardConfig reports that a guard factory failed to instantiate.
// Both are SDK-free sentinels: callers (e.g. the Temporal executor) map
// them to their own retry/error-code semantics; the Registry itself
// stays free of any executor dependency.
var (
	ErrNotRegistered = errors.New("not registered")
	ErrGuardConfig   = errors.New("guard configuration failed")
)

// Registry maps operation, preflight, and guard names to the concrete
// code that implements them. It is the single extension point of the
// rollout framework: consumers register entries at worker startup and
// the executor resolves them by name when a workflow runs.
//
// Lifecycle: populate once at startup (single-threaded), then resolve
// (concurrent, read-only) once dispatch begins. Writes and reads never
// overlap, so concurrent map reads are safe without a lock. Registering
// after dispatch has begun is a programming error, not a supported
// concurrent path.
type Registry struct {
	operations       map[string]CompiledOperation
	clusterPreflight map[string]ClusterPreflightFunc
	nodePreflight    map[string]NodePreflightFunc
	nodeGuard        map[string]NodeGuardFactory
	clusterGuard     map[string]ClusterGuardFactory
}

// NewRegistry returns an empty Registry ready to be populated.
func NewRegistry() *Registry {
	return &Registry{
		operations:       map[string]CompiledOperation{},
		clusterPreflight: map[string]ClusterPreflightFunc{},
		nodePreflight:    map[string]NodePreflightFunc{},
		nodeGuard:        map[string]NodeGuardFactory{},
		clusterGuard:     map[string]ClusterGuardFactory{},
	}
}

// RegisterOperation adds op keyed by op.Name(). Panics on empty name or
// duplicate registration.
func (r *Registry) RegisterOperation(op CompiledOperation) {
	name := op.Name()
	if name == "" {
		panic("rollout: empty Operation registration name")
	}
	if _, exists := r.operations[name]; exists {
		panic(fmt.Sprintf("rollout: duplicate Operation registration %q", name))
	}
	r.operations[name] = op
}

// RegisterClusterPreflight adds fn under name. Panics on empty name, nil
// function, or duplicate.
func (r *Registry) RegisterClusterPreflight(name string, fn ClusterPreflightFunc) {
	if name == "" {
		panic("rollout: empty ClusterPreflight registration name")
	}
	if fn == nil {
		panic(fmt.Sprintf("rollout: nil ClusterPreflight function for %q", name))
	}
	if _, exists := r.clusterPreflight[name]; exists {
		panic(fmt.Sprintf("rollout: duplicate ClusterPreflight registration %q", name))
	}
	r.clusterPreflight[name] = fn
}

// RegisterNodePreflight adds fn under name. Panics on empty name, nil
// function, or duplicate.
func (r *Registry) RegisterNodePreflight(name string, fn NodePreflightFunc) {
	if name == "" {
		panic("rollout: empty NodePreflight registration name")
	}
	if fn == nil {
		panic(fmt.Sprintf("rollout: nil NodePreflight function for %q", name))
	}
	if _, exists := r.nodePreflight[name]; exists {
		panic(fmt.Sprintf("rollout: duplicate NodePreflight registration %q", name))
	}
	r.nodePreflight[name] = fn
}

// RegisterNodeGuard adds factory under name. Panics on empty name, nil
// factory, or duplicate.
func (r *Registry) RegisterNodeGuard(name string, factory NodeGuardFactory) {
	if name == "" {
		panic("rollout: empty NodeGuard registration name")
	}
	if factory == nil {
		panic(fmt.Sprintf("rollout: nil NodeGuard factory for %q", name))
	}
	if _, exists := r.nodeGuard[name]; exists {
		panic(fmt.Sprintf("rollout: duplicate NodeGuard registration %q", name))
	}
	r.nodeGuard[name] = factory
}

// RegisterClusterGuard adds factory under name. Panics on empty name,
// nil factory, or duplicate.
func (r *Registry) RegisterClusterGuard(name string, factory ClusterGuardFactory) {
	if name == "" {
		panic("rollout: empty ClusterGuard registration name")
	}
	if factory == nil {
		panic(fmt.Sprintf("rollout: nil ClusterGuard factory for %q", name))
	}
	if _, exists := r.clusterGuard[name]; exists {
		panic(fmt.Sprintf("rollout: duplicate ClusterGuard registration %q", name))
	}
	r.clusterGuard[name] = factory
}

// LookupOperation returns the operation registered under name.
func (r *Registry) LookupOperation(name string) (CompiledOperation, bool) {
	op, ok := r.operations[name]
	return op, ok
}

// LookupClusterPreflight returns the cluster preflight registered under name.
func (r *Registry) LookupClusterPreflight(name string) (ClusterPreflightFunc, bool) {
	fn, ok := r.clusterPreflight[name]
	return fn, ok
}

// LookupNodePreflight returns the node preflight registered under name.
func (r *Registry) LookupNodePreflight(name string) (NodePreflightFunc, bool) {
	fn, ok := r.nodePreflight[name]
	return fn, ok
}

// ResolveNodeGuard looks up the named NodeGuard factory and runs it with
// params. Returns ErrNotRegistered if no factory is registered, or an
// error wrapping ErrGuardConfig if the factory fails.
func (r *Registry) ResolveNodeGuard(name string, params map[string]string) (NodeGuard, error) {
	factory, ok := r.nodeGuard[name]
	if !ok {
		return nil, fmt.Errorf("node guard %q: %w", name, ErrNotRegistered)
	}
	g, err := factory(params)
	if err != nil {
		return nil, fmt.Errorf("instantiate node guard %q: %w", name, errors.Join(ErrGuardConfig, err))
	}
	return g, nil
}

// ResolveClusterGuard is the cluster-scoped sibling of ResolveNodeGuard.
func (r *Registry) ResolveClusterGuard(name string, params map[string]string) (ClusterGuard, error) {
	factory, ok := r.clusterGuard[name]
	if !ok {
		return nil, fmt.Errorf("cluster guard %q: %w", name, ErrNotRegistered)
	}
	g, err := factory(params)
	if err != nil {
		return nil, fmt.Errorf("instantiate cluster guard %q: %w", name, errors.Join(ErrGuardConfig, err))
	}
	return g, nil
}
