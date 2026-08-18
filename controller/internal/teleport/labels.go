// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"errors"
	"sort"
	"strings"
)

const (
	LabelManagedBy             = "managed-by"
	LabelManagedByValue        = "nke-cloud-workflow-engine"
	LabelResourceGroup         = "resource-group"
	LabelResourceGroupNKEValue = "nke"
	LabelSite                  = "site"
	LabelNKESiteControllerName = "nke-site-controller-name"
	LabelNodeID                = "node-id"
	LabelCluster               = "cluster"
	LabelClusterName           = "cluster-name"
	LabelNodePool              = "node-pool"
	LabelNodeName              = "node-name"
	LabelNodeRole              = "node-role"
	LabelNodeRoleControlPlane  = "control-plane"
	LabelNodeRoleWorker        = "worker"
	LabelEnvironment           = "environment"
)

// NodeRoleLabel returns the Teleport label value for the node's role.
func NodeRoleLabel(controlPlane bool) string {
	if controlPlane {
		return LabelNodeRoleControlPlane
	}
	return LabelNodeRoleWorker
}

var ErrNotOwnedByCWE = errors.New("token is not managed by nke-cloud-workflow-engine")

// OwnershipLabels returns the standard ownership labels for CWE-managed tokens.
func OwnershipLabels() map[string]string {
	return map[string]string{
		LabelManagedBy: LabelManagedByValue,
	}
}

// MergeLabels merges ownership labels with user-provided labels. Ownership labels take precedence.
func MergeLabels(userLabels map[string]string) map[string]string {
	result := make(map[string]string)

	for k, v := range userLabels {
		result[k] = v
	}
	for k, v := range OwnershipLabels() {
		result[k] = v
	}

	return result
}

// IsOwnedByCWE checks if a token has the correct managed-by label.
func IsOwnedByCWE(labels map[string]string) bool {
	return labels[LabelManagedBy] == LabelManagedByValue
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
