// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mgmtns resolves the management-plane namespace the suite targets.
//
// The namespace is configurable per site (the controller chart renders
// nvkeSystemNamespace.name into the controller's managementNamespace), so the
// suite must not assume the default. Everything the suite reads or writes on
// the site controller -- published kubeconfig and trust-bundle Secrets, the
// RBAC ConfigMap, CAPI Clusters and Machines, verifier Pods -- lives in
// this one namespace, so one resolved value covers the whole suite.
package mgmtns

import (
	"os"
	"strings"
)

const (
	// EnvVar names the management namespace of the site under test. Set it when
	// running against a site that pinned a namespace other than Default --
	// a site installed before the namespace became configurable uses
	// "nke-system".
	EnvVar = "NVKE_E2E_MANAGEMENT_NAMESPACE"

	// Default is the namespace a current chart installs into, and the one the
	// suite's own CAPD deploy creates.
	Default = "nvke-system"
)

// Namespace returns the management-plane namespace of the site under test.
func Namespace() string {
	return resolve(os.Getenv(EnvVar))
}

func resolve(fromEnv string) string {
	if trimmed := strings.TrimSpace(fromEnv); trimmed != "" {
		return trimmed
	}
	return Default
}
