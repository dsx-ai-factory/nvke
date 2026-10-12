// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/validate/content"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

const (
	// controlPlaneRoleKey is written by kubeadm when a node joins as a control
	// plane. Callers cannot set it as a label or a taint. The check runs before
	// the label allowlist, so an allowlist entry cannot permit it.
	controlPlaneRoleKey = "node-role.kubernetes.io/control-plane"
)

// nkeReservedDomains are rejected for labels and taints, including subdomains.
var nkeReservedDomains = []string{ //nolint:gochecknoglobals // fixed domain list, not mutable state
	"nke.nvidia.com",
	"nke.io",
}

// k8sDomains are rejected for labels and taints, including subdomains, unless
// the key is on the label allowlist. The allowlist does not apply to taints.
var k8sDomains = []string{ //nolint:gochecknoglobals // fixed domain list, not mutable state
	"kubernetes.io",
	"k8s.io",
}

// allowedKubernetesLabelKeys is the allowlist of keys inside the reserved
// system domains (kubernetes.io and k8s.io) that a caller may set as labels.
// Every other key in those domains is reserved and cannot be set by the user.
// Keys under the NKE domains are reserved with no allowlist.
// The list is curated from labels in the current fleet config profiles, except
// node-role.kubernetes.io/control-plane, which will stay forbidden.
// FIXME: load this allowlist from runtime config.
var allowedKubernetesLabelKeys = map[string]struct{}{ //nolint:gochecknoglobals // fixed label allowlist, not mutable state
	"node.kubernetes.io/instance-type":   {},
	"node.kubernetes.io/cluster-type":    {},
	"node-role.kubernetes.io/gpu-worker": {},
	"node-role.kubernetes.io/infra":      {},
	"node-role.kubernetes.io/monitoring": {},
	"node-role.kubernetes.io/user-cpu":   {},
	"node-role.kubernetes.io/worker":     {},
}

// validateNodeLabels checks labels supplied on create.
func validateNodeLabels(labels map[string]string) error {
	for key, value := range labels {
		if err := validateLabelKey(key, value); err != nil {
			return err
		}
	}
	return nil
}

// validateNodeTaints checks taints supplied on create. taints cannot be on control-plane nodes. control-plane taint based on its role is already set by kubeadm when registering a node.
func validateNodeTaints(taints []api.NodeTaint, controlPlane bool) error {
	if controlPlane && len(taints) > 0 {
		return fmt.Errorf("nodeTaints: not allowed on a control-plane pool")
	}
	seen := make(map[string]struct{}, len(taints))
	for _, taint := range taints {
		if err := validateTaintKey(taint.Key, taint.Value); err != nil {
			return err
		}
		id := taint.Key + "\x00" + string(taint.Effect)
		if _, ok := seen[id]; ok {
			return fmt.Errorf("nodeTaints: duplicate taint key %q with effect %q", taint.Key, taint.Effect)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validateLabelKey(key, value string) error {
	const field = "nodeLabels"
	if err := validateKeyValue(field, key, value); err != nil {
		return err
	}
	if key == controlPlaneRoleKey {
		return fmt.Errorf("%s: key %q is forbidden", field, key)
	}
	domain, prefixed := metadataDomain(key)
	if prefixed && isReservedDomain(domain, nkeReservedDomains) {
		return fmt.Errorf("%s: key %q is reserved", field, key)
	}
	_, allowed := allowedKubernetesLabelKeys[key]
	if prefixed && isReservedDomain(domain, k8sDomains) && !allowed {
		return fmt.Errorf("%s: key %q is reserved", field, key)
	}
	return nil
}

func validateTaintKey(key, value string) error {
	const field = "nodeTaints"
	if err := validateKeyValue(field, key, value); err != nil {
		return err
	}
	if key == controlPlaneRoleKey {
		return fmt.Errorf("%s: key %q is forbidden", field, key)
	}
	domain, prefixed := metadataDomain(key)
	if prefixed && (isReservedDomain(domain, nkeReservedDomains) || isReservedDomain(domain, k8sDomains)) {
		return fmt.Errorf("%s: key %q is reserved", field, key)
	}
	return nil
}

func validateKeyValue(field, key, value string) error {
	if errs := content.IsLabelKey(key); len(errs) > 0 {
		return fmt.Errorf("%s: key %q is invalid: %s", field, key, strings.Join(errs, "; "))
	}
	if errs := content.IsLabelValue(value); len(errs) > 0 {
		return fmt.Errorf("%s: value %q for key %q is invalid: %s", field, value, key, strings.Join(errs, "; "))
	}
	return nil
}

// metadataDomain returns the prefix of a prefixed key. A bare key has no prefix.
func metadataDomain(key string) (string, bool) {
	domain, name, hasDomain := strings.Cut(key, "/")
	if !hasDomain || name == "" {
		return "", false
	}
	return domain, true
}

// isReservedDomain reports whether domain is one of roots or a subdomain of one.
func isReservedDomain(domain string, roots []string) bool {
	for _, root := range roots {
		if domain == root || strings.HasSuffix(domain, "."+root) {
			return true
		}
	}
	return false
}

func reservedKeyError(field, key string) error {
	return fmt.Errorf("%s: key %q is reserved", field, key)
}
