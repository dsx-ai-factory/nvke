// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"
)

// NicoMachineCRDInstalled reports whether the management cluster serves the
// CAPNICo NicoMachine CRD.
func NicoMachineCRDInstalled(mapper meta.RESTMapper) (bool, error) {
	return crdInstalled(mapper, nicov1.GroupVersion, "NicoMachine")
}

// MachineCRDInstalled reports whether the management cluster serves the Cluster
// API Machine CRD.
func MachineCRDInstalled(mapper meta.RESTMapper) (bool, error) {
	return crdInstalled(mapper, clusterv1.GroupVersion, "Machine")
}

// crdInstalled reports whether the management cluster serves kind in gv. A watch
// on a kind with no CRD never syncs, and controller-runtime then stops the whole
// manager.
//
// A missing CRD is false, not an error. Any other discovery failure is an error:
// it says nothing about whether the CRD is installed.
func crdInstalled(mapper meta.RESTMapper, gv schema.GroupVersion, kind string) (bool, error) {
	gk := schema.GroupKind{Group: gv.Group, Kind: kind}

	switch _, err := mapper.RESTMapping(gk, gv.Version); {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, fmt.Errorf("looking up %s: %w", gk, err)
	}
}
