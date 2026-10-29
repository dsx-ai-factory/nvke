// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/validate/content"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	// The check runs before the label allowlist, so an allowlist entry cannot permit it.
	controlPlaneRoleKey = controllerconfig.NodeMetadataControlPlaneRoleKey
)

// reservedKubernetesDomains and their subdomains are reserved for Kubernetes. Only allowlisted label keys may be set under them.
func reservedKubernetesDomains() []string {
	return []string{"kubernetes.io", "k8s.io"}
}

// validateNodeLabels checks labels supplied on create. Keys under the Kubernetes domains are
// reserved unless they appear in allowlistedLabelKeys, which operators set through
// nodeMetadataAllowlistedLabelKeys in nvke-controller-config. Keys under the internal domains are always reserved.
func validateNodeLabels(labels map[string]string, allowlistedLabelKeys []string) error {
	//  sort the keys to get a deterministic error for failures
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		if err := validateLabelKey(key, labels[key], allowlistedLabelKeys); err != nil {
			return err
		}
	}
	return nil
}

// validateNodeTaints checks taints supplied on create. Custom taints are only relevant for worker pools, and are not supported on control-plane for now.
// Core bootstrap components add node-role.kubernetes.io/control-plane:NoSchedule to control-plane nodes, which keeps
// workloads that don't tolerate it from running on them by default.
func validateNodeTaints(taints []api.NodeTaint, controlPlane bool) error {
	if controlPlane && len(taints) > 0 {
		return fmt.Errorf("nodeTaints: not allowed on a control-plane pool")
	}
	type tempTaint struct {
		key    string
		effect api.NodeTaintEffect
	}
	seen := make(map[tempTaint]struct{}, len(taints))
	for _, taint := range taints {
		if err := validateTaintKey(taint.Key, taint.Value); err != nil {
			return err
		}
		id := tempTaint{key: taint.Key, effect: taint.Effect}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("nodeTaints: duplicate taint key %q with effect %q", taint.Key, taint.Effect)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// validateLabelKey rejects invalid, forbidden and reserved label keys.
func validateLabelKey(key, value string, allowlistedLabelKeys []string) error {
	const field = "nodeLabels"
	if err := validateKeyValue(field, key, value); err != nil {
		return err
	}
	if key == controlPlaneRoleKey {
		return forbiddenKeyError(field, key)
	}
	if domain, isReserved := reservedDomain(key, controllerconfig.NodeMetadataReservedInternalDomains()); isReserved {
		return reservedKeyError(field, key, domain, "labels in this domain cannot be set")
	}
	if domain, isReserved := reservedDomain(key, reservedKubernetesDomains()); isReserved && !slices.Contains(allowlistedLabelKeys, key) {
		return reservedKeyError(field, key, domain, "labels in this domain cannot be set")
	}
	return nil
}

// validateTaintKey checks that a taint key is valid.
// Rejects invalid keys, forbidden keys, and reserved keys.
func validateTaintKey(key, value string) error {
	const field = "nodeTaints"
	if err := validateKeyValue(field, key, value); err != nil {
		return err
	}
	if key == controlPlaneRoleKey {
		return forbiddenKeyError(field, key)
	}
	for _, roots := range [][]string{controllerconfig.NodeMetadataReservedInternalDomains(), reservedKubernetesDomains()} {
		if domain, isReserved := reservedDomain(key, roots); isReserved {
			return reservedKeyError(field, key, domain, "taints in this domain cannot be set")
		}
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

// reservedDomain returns the entry of roots that key's prefix equals or is a subdomain of. A bare key has no prefix.
func reservedDomain(key string, roots []string) (string, bool) {
	domain, _, prefixed := strings.Cut(key, "/")
	if !prefixed {
		return "", false
	}
	for _, root := range roots {
		if domain == root || strings.HasSuffix(domain, "."+root) {
			return root, true
		}
	}
	return "", false
}

func reservedKeyError(field, key, domain, reason string) error {
	return fmt.Errorf("%s: key %q is under reserved domain %q; %s", field, key, domain, reason)
}

func forbiddenKeyError(field, key string) error {
	return fmt.Errorf("%s: key %q is managed by core bootstrap components and cannot be set", field, key)
}

// validateMachineMetadata checks syntax and reserved domains, allowing only
// explicitly configured keys under those domains.
func validateMachineMetadata(annotations, labels map[string]string, allowlistedAnnotations, allowlistedLabels []string) error {
	completeAnnotations := db.CloneStringMap(annotations)
	// Use the longest transport mode; the zero UUID has the same length as an allocated host ID.
	maps.Copy(completeAnnotations, shared.MachineAnnotations(db.Node{
		NodeAgentExternalMTLSEnabled: true,
		NodeAgentTLSJWTEnabled:       true,
	}))
	if errs := apivalidation.ValidateAnnotations(completeAnnotations, field.NewPath("machineAnnotations")); len(errs) > 0 {
		return errs.ToAggregate()
	}
	for _, key := range slices.Sorted(maps.Keys(annotations)) {
		if keys.MachineAgentConnection.Matches(key) {
			return fmt.Errorf("machineAnnotations: key %q is controller-owned", key)
		}
		if err := validateMachineMetadataKey("machineAnnotations", key, allowlistedAnnotations); err != nil {
			return err
		}
	}
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		if err := validateKeyValue("machineLabels", key, labels[key]); err != nil {
			return err
		}
		if err := validateMachineMetadataKey("machineLabels", key, allowlistedLabels); err != nil {
			return err
		}
	}
	return nil
}

func validateMachineMetadataKey(field, key string, allowlistedKeys []string) error {
	domains := append(controllerconfig.NodeMetadataReservedInternalDomains(), reservedKubernetesDomains()...)
	domains = append(domains, "cluster.x-k8s.io")
	if domain, reserved := reservedDomain(key, domains); reserved && !slices.Contains(allowlistedKeys, key) {
		return reservedKeyError(field, key, domain, "key must be explicitly allowlisted")
	}
	return nil
}
