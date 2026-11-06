// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

const (
	// Image is the kube-bench release the scan runs, published for amd64 and
	// arm64.
	Image = "docker.io/aquasec/kube-bench:v0.16.0@sha256:75506f222d1eb6ce2a751a5533bdc0a3b54c898e2e49e7751d0ee22cfb862679"

	// Benchmark is the benchmark the scan selects: the newest CIS Kubernetes
	// benchmark Image ships. Image maps Kubernetes versions only up to 1.34 and
	// reaches cis-1.12 on a newer cluster by falling back, silently, through
	// the versions it does map; naming it keeps the choice here. Image and
	// Benchmark move together.
	Benchmark = "cis-1.12"

	// PodContainer is the scan pod's one container.
	PodContainer = "kube-bench"

	// PodLabel and PodLabelValue mark everything a scan creates, so a test or
	// a person can find what a scan left behind.
	PodLabel      = "app.kubernetes.io/name"
	PodLabelValue = "kube-bench"
)

// Cluster is the kind of cluster a scan judges, named in its results.
type Cluster string

const (
	// ClusterSite is the site cluster NVKE's management plane runs on.
	ClusterSite Cluster = "site"
	// ClusterTenant is a workload cluster NVKE creates for a customer.
	ClusterTenant Cluster = "tenant"
)

// Role is a node's role in its cluster.
type Role string

const (
	// RoleControlPlane is a node carrying node-role.kubernetes.io/control-plane.
	RoleControlPlane Role = "control-plane"
	// RoleWorker is every other node.
	RoleWorker Role = "worker"
)

// Status is kube-bench's verdict on one check.
type Status string

const (
	// StatusPass is a check kube-bench found met.
	StatusPass Status = "PASS"
	// StatusFail is a scored check kube-bench found not met.
	StatusFail Status = "FAIL"
	// StatusWarn is a manual check, or an unscored check found not met.
	StatusWarn Status = "WARN"
	// StatusInfo is a check kube-bench only reports on.
	StatusInfo Status = "INFO"
)
