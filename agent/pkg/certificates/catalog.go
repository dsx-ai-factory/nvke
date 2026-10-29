// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package certificates defines the bounded credential inventory inspected on kubeadm nodes.
package certificates

// Names returns the supported credential names for a node role. Paths and certificate identities are not metric labels.
func Names(controlPlane bool) []string {
	names := []string{"kubelet-client", "kubelet-serving"}
	if controlPlane {
		names = append(names, "apiserver", "apiserver-kubelet-client", "front-proxy-client", "apiserver-etcd-client", "etcd-server", "etcd-peer", "etcd-healthcheck-client", "kubernetes-ca", "front-proxy-ca", "etcd-ca", "admin", "super-admin", "controller-manager", "scheduler")
	}
	return names
}

// Supports reports whether a name belongs to the fixed credential inventory.
func Supports(name string, controlPlane bool) bool {
	for _, supported := range Names(controlPlane) {
		if supported == name {
			return true
		}
	}
	return false
}

// Required reports whether a missing credential signals incomplete node inventory.
// kubeadm creates super-admin.conf only during init, not on joined control planes.
func Required(name string) bool { return name != "super-admin" }
