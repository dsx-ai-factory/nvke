// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

const (
	// Image is the kube-bench release the scan runs, published for amd64 and
	// arm64.
	Image = "docker.io/aquasec/kube-bench:v0.16.0@sha256:75506f222d1eb6ce2a751a5533bdc0a3b54c898e2e49e7751d0ee22cfb862679"

	// Benchmark is the kube-bench benchmark the scan selects. Nothing in
	// kube-bench maps Kubernetes 1.36 to a benchmark, so it is always named.
	Benchmark = "cis-2.0"

	// BenchmarkCommit is the kube-bench commit the benchmark files come from:
	// "feat: add CIS-2.0 support" (#2126), which no release includes yet.
	BenchmarkCommit = "9f133cb7509ce1dbedfc860e94474588000e25ac"

	// BenchmarkURL is where the benchmark files are fetched from. A file's
	// path under kube-bench's cfg/ directory follows it.
	BenchmarkURL = "https://raw.githubusercontent.com/aquasecurity/kube-bench/" + BenchmarkCommit + "/cfg/"

	// ConfigDir is where each scan pod mounts the benchmark files, and the
	// --config-dir it passes to kube-bench.
	ConfigDir = "/etc/kube-bench-cfg"

	// PodContainer is the scan pod's one container.
	PodContainer = "kube-bench"

	// PodLabel and PodLabelValue mark everything a scan creates, so a test or
	// a person can find what a scan left behind.
	PodLabel      = "app.kubernetes.io/name"
	PodLabelValue = "kube-bench"
)

// Cluster is the kind of cluster a scan judges: an exception can apply to one
// kind or both.
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
