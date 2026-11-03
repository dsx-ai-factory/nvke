// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap contains Kubernetes-native bootstrap identity controllers and the helpers they share.
package bootstrap

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubeadmv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
)

// KubeadmConfigCRDInstalled reports whether the management cluster serves the CABPK KubeadmConfig CRD.
func KubeadmConfigCRDInstalled(mapper meta.RESTMapper) (bool, error) {
	return crdInstalled(mapper, kubeadmv1.GroupVersion, "KubeadmConfig")
}

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
