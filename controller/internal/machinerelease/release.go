// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package machinerelease reads MachineImage, ClusterRelease, and
// NodePoolRelease.
package machinerelease

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// ErrInvalidRelease marks a release, or the image it references, that is
// missing or malformed.
var ErrInvalidRelease = errors.New("invalid release")

const (
	kindClusterRelease  = "ClusterRelease"
	kindNodePoolRelease = "NodePoolRelease"
	kindMachineImage    = "MachineImage"

	// crdProbeName is fetched only to learn whether the kind is installed.
	crdProbeName = "nvke-release-crd-probe"
)

// GetClusterRelease reads the ClusterRelease with the given name.
func GetClusterRelease(ctx context.Context, reader client.Reader, name string) (*enginev1.ClusterRelease, error) {
	var release enginev1.ClusterRelease
	if err := get(ctx, reader, name, &release, kindClusterRelease); err != nil {
		return nil, err
	}
	return &release, nil
}

// GetNodePoolRelease reads the NodePoolRelease with the given name.
func GetNodePoolRelease(ctx context.Context, reader client.Reader, name string) (*enginev1.NodePoolRelease, error) {
	var release enginev1.NodePoolRelease
	if err := get(ctx, reader, name, &release, kindNodePoolRelease); err != nil {
		return nil, err
	}
	return &release, nil
}

// GetMachineImage reads the MachineImage a release references.
func GetMachineImage(ctx context.Context, reader client.Reader, ref enginev1.ResourceName) (*enginev1.MachineImage, error) {
	if ref == "" {
		return nil, fmt.Errorf("%w: MachineImage reference is empty", ErrInvalidRelease)
	}
	var image enginev1.MachineImage
	if err := get(ctx, reader, string(ref), &image, kindMachineImage); err != nil {
		return nil, err
	}
	return &image, nil
}

// ResolveClusterRelease reads a ClusterRelease and the MachineImage it
// references.
func ResolveClusterRelease(ctx context.Context, reader client.Reader, name string) (*enginev1.ClusterRelease, *enginev1.MachineImage, error) {
	release, err := GetClusterRelease(ctx, reader, name)
	if err != nil {
		return nil, nil, err
	}
	image, err := GetMachineImage(ctx, reader, release.Spec.MachineImageRef.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("release %q: %w", release.Name, err)
	}
	return release, image, nil
}

// ResolveNodePoolRelease is ResolveClusterRelease for a NodePoolRelease.
func ResolveNodePoolRelease(ctx context.Context, reader client.Reader, name string) (*enginev1.NodePoolRelease, *enginev1.MachineImage, error) {
	release, err := GetNodePoolRelease(ctx, reader, name)
	if err != nil {
		return nil, nil, err
	}
	image, err := GetMachineImage(ctx, reader, release.Spec.MachineImageRef.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("release %q: %w", release.Name, err)
	}
	return release, image, nil
}

// ParseKubernetesVersionAnnotations reads the Kubernetes-version annotations of
// a release.
func ParseKubernetesVersionAnnotations(release metav1.Object) (string, bool, error) {
	name, annotations := release.GetName(), release.GetAnnotations()
	raw, ok := annotations[enginev1.MachineInitKubernetesVersionInjectedAnnotation]
	if !ok {
		return "", false, fmt.Errorf("%w: release %q is missing annotation %s", ErrInvalidRelease, name, enginev1.MachineInitKubernetesVersionInjectedAnnotation)
	}
	var injected bool
	switch raw {
	case "true":
		injected = true
	case "false":
		injected = false
	default:
		return "", false, fmt.Errorf("%w: release %q annotation %s is %q", ErrInvalidRelease, name, enginev1.MachineInitKubernetesVersionInjectedAnnotation, raw)
	}
	version := annotations[enginev1.MachineInitKubernetesVersionAnnotation]
	if !injected && version == "" {
		return "", false, fmt.Errorf("%w: release %q is missing annotation %s", ErrInvalidRelease, name, enginev1.MachineInitKubernetesVersionAnnotation)
	}
	return version, injected, nil
}

// CheckCRDs reports whether MachineImage, ClusterRelease, and NodePoolRelease
// are installed. A missing object is success; a missing kind is not.
func CheckCRDs(ctx context.Context, reader client.Reader) error {
	probes := []struct {
		obj  client.Object
		kind string
	}{
		{&enginev1.MachineImage{}, kindMachineImage},
		{&enginev1.ClusterRelease{}, kindClusterRelease},
		{&enginev1.NodePoolRelease{}, kindNodePoolRelease},
	}
	for _, probe := range probes {
		err := reader.Get(ctx, client.ObjectKey{Name: crdProbeName}, probe.obj)
		if err == nil || apierrors.IsNotFound(err) {
			continue
		}
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("%s CRD is not installed: %w", probe.kind, err)
		}
		return fmt.Errorf("checking %s: %w", probe.kind, err)
	}
	return nil
}

func get(ctx context.Context, reader client.Reader, name string, obj client.Object, kind string) error {
	if name == "" {
		return errors.New("release name is empty")
	}
	err := reader.Get(ctx, client.ObjectKey{Name: name}, obj)
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s %q is not installed", ErrInvalidRelease, kind, name)
	}
	if meta.IsNoMatchError(err) {
		return fmt.Errorf("%s CRD is not installed: %w", kind, err)
	}
	return fmt.Errorf("getting %s %q: %w", kind, name, err)
}

func listErr(kind string, err error) error {
	if meta.IsNoMatchError(err) {
		return fmt.Errorf("%s CRD is not installed: %w", kind, err)
	}
	return fmt.Errorf("listing %ss: %w", kind, err)
}
