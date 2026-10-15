// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package operator is the nvkectl operator interface to a site's machine images
// and releases: the MachineImage, ClusterRelease and NodePoolRelease resources
// that the controller resolves every release from.
//
// Operators describe a resource with a portable document that carries only a
// metadata and a spec. The package supplies the group, version and kind, and
// projects responses back into the same shape, so nothing an operator types
// depends on how the resources are stored.
package operator

import (
	"context"
	"errors"
	"time"
)

// Kind is the kind of a machine image or release resource.
type Kind string

const (
	// KindMachineImage is the immutable identity and boot contract of a node image.
	KindMachineImage Kind = "MachineImage"
	// KindClusterRelease is a published control-plane release.
	KindClusterRelease Kind = "ClusterRelease"
	// KindNodePoolRelease is a published worker release.
	KindNodePoolRelease Kind = "NodePoolRelease"
)

// Kinds lists every machine image and release kind.
var Kinds = []Kind{KindMachineImage, KindClusterRelease, KindNodePoolRelease}

// SupportsDeprecation reports whether resources of the kind carry a
// spec.lifecycle that SetDeprecated can change. A MachineImage is immutable
// as a whole and has none.
func (k Kind) SupportsDeprecation() bool {
	return k == KindClusterRelease || k == KindNodePoolRelease
}

// Metadata is the portable metadata of a machine image or release. It carries only what
// an operator chooses: Kubernetes transport fields such as namespace, uid and
// resourceVersion are not part of it.
type Metadata struct {
	// Name is the resource name. A release is selected by this name, so it is
	// the release value that clusters and node pools store.
	Name string `json:"name"`
	// Annotations are free-form key/value pairs the resource carries.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Resource is the portable form of a machine image or release.
type Resource struct {
	Metadata Metadata `json:"metadata"`
	// Spec is the kind's spec as a generic document.
	Spec map[string]any `json:"spec"`
	// CreatedAt is when the backend stored the resource. It is zero on a
	// resource that has not been created and is not part of the portable
	// document.
	CreatedAt time.Time `json:"-"`
}

var (
	// ErrNotFound reports that the named resource does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists reports that a resource of that name already exists.
	// Published content is immutable, so a change is published under a new name.
	ErrAlreadyExists = errors.New("already exists")
	// ErrCRDsNotInstalled reports that the site does not serve the
	// MachineImage, ClusterRelease and NodePoolRelease resource types.
	ErrCRDsNotInstalled = errors.New("the release CRDs are not installed on the management cluster")
	// ErrForbidden reports that the caller may not perform the operation.
	ErrForbidden = errors.New("forbidden")
	// ErrNoLifecycle reports a deprecation request for a kind that has no
	// lifecycle to change.
	ErrNoLifecycle = errors.New("kind has no lifecycle")
)

// Client reads and writes a site's machine images and releases. It is backend neutral: a
// future implementation can speak to a REST API without changing a caller.
type Client interface {
	// Create stores r as a new resource of the kind and returns it as stored.
	// It fails with ErrAlreadyExists when the name is taken.
	Create(ctx context.Context, kind Kind, r Resource) (Resource, error)
	// Get returns the named resource, or ErrNotFound.
	Get(ctx context.Context, kind Kind, name string) (Resource, error)
	// List returns every resource of the kind, ordered by name.
	List(ctx context.Context, kind Kind) ([]Resource, error)
	// Delete removes the named resource, or fails with ErrNotFound.
	Delete(ctx context.Context, kind Kind, name string) error
	// SetDeprecated sets spec.lifecycle.deprecated of the named resource and
	// changes nothing else. It fails with ErrNoLifecycle for a kind without a
	// lifecycle.
	SetDeprecated(ctx context.Context, kind Kind, name string, deprecated bool) (Resource, error)
}
