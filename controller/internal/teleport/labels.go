// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"errors"
	"slices"
	"sort"
	"strings"
)

const (
	// LabelManagedBy is the label key. Its value is
	// config.TeleportConfig.ManagedByLabelValue, not a constant -- it is
	// per-install configurable.
	LabelManagedBy              = "managed-by"
	LabelResourceGroup          = "resource-group"
	LabelSite                   = "site"
	LabelNVKESiteControllerName = "nke-site-controller-name"
	LabelNodeID                 = "node-id"
	LabelCluster                = "cluster"
	LabelClusterName            = "cluster-name"
	LabelNodePool               = "node-pool"
	LabelNodeName               = "node-name"
	LabelNodeRole               = "node-role"
	LabelNodeRoleControlPlane   = "control-plane"
	LabelNodeRoleWorker         = "worker"
	LabelEnvironment            = "environment"
)

// NodeRoleLabel returns the Teleport label value for the node's role.
func NodeRoleLabel(controlPlane bool) string {
	if controlPlane {
		return LabelNodeRoleControlPlane
	}
	return LabelNodeRoleWorker
}

var ErrNotOwnedByController = errors.New("token is not managed by this controller")

// OwnershipLabels returns the standard ownership labels for tokens managed by
// this controller, stamped with managedByValue (config.TeleportConfig.ManagedByLabelValue).
func OwnershipLabels(managedByValue string) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
	}
}

// MergeLabels merges ownership labels with user-provided labels. Ownership labels take precedence.
func MergeLabels(userLabels map[string]string, managedByValue string) map[string]string {
	result := make(map[string]string)

	for k, v := range userLabels {
		result[k] = v
	}
	for k, v := range OwnershipLabels(managedByValue) {
		result[k] = v
	}

	return result
}

// IsOwnedByController reports whether a token carries a managed-by value this
// controller owns. It takes every accepted spelling, not just the one the
// controller writes: a deployment that moves ManagedByLabelValue keeps tokens
// and nodes stamped with the previous one until each is replaced, and a check
// against the written value alone disowns all of them at once -- they stop
// being listed, cannot be deleted, and nothing ever cleans them up.
//
// acceptedValues comes from config.TeleportConfig.OwnershipManagedByValues.
// An empty list owns nothing, which is the safe direction: it refuses tokens
// rather than claiming ones this controller did not write.
func IsOwnedByController(labels map[string]string, acceptedValues []string) bool {
	value, ok := labels[LabelManagedBy]
	if !ok {
		return false
	}
	return slices.Contains(acceptedValues, value)
}

// BuildLabelsString constructs a sorted, comma-separated "key=value" label string.
func BuildLabelsString(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(labels))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}

	return strings.Join(parts, ",")
}

// BuildLabelsYAML returns sorted YAML map lines ("key: value\n") for embedding in Teleport config.
// Used by userData templates for the ssh_service.labels block.
func BuildLabelsYAML(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(labels[k])
		b.WriteString("\n")
	}
	return b.String()
}
