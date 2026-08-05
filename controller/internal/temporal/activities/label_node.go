// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/kube"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

type LabelNodeInput struct {
	NodeID uuid.UUID
	// If true, the config profile labels will be added to the node
	AddConfigProfileLabels bool

	// If true, Forge topology labels (site/vpc) are resolved and added to the
	// node. Resolution happens inside the activity because the name labels
	// require Forge-provider reads, which are not permitted in workflow code.
	AddTopologyLabels bool

	// Optional labels to apply to the node.
	// If AddConfigProfileLabels is true, these labels along with the config profile labels will be added to the node
	// Labels should be in `key=value` format or `key-` (trailing dash to negate or delete label)
	Labels []string

	NodeProvisioningConditionMessage string
	NodeRebootingConditionMessage    string
}

type LabelNodeResult struct {
	Output string
}

func (a *Activities) LabelNode(ctx context.Context, input LabelNodeInput) (LabelNodeResult, error) {
	var conditionMessage string
	var updateCondition func(ctx context.Context, message string) error

	if input.NodeProvisioningConditionMessage != "" {
		conditionMessage = input.NodeProvisioningConditionMessage
		updateCondition = a.nodeProvisioningConditionMessageUpdater(input.NodeID)
	} else if input.NodeRebootingConditionMessage != "" {
		conditionMessage = input.NodeRebootingConditionMessage
		updateCondition = a.nodeRebootingConditionMessageUpdater(input.NodeID)
	}

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[LabelNodeResult]{
		Message:         conditionMessage,
		UpdateCondition: updateCondition,
		RenderError:     messageForTemporalError,
		ActivityBody: func() (LabelNodeResult, error) {
			return a.labelNode(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) labelNode(ctx context.Context, input LabelNodeInput) (LabelNodeResult, error) {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	nodeSet, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to get node context: %w", err)
	}

	// Initialize labels with labels from input
	labels := append([]string(nil), input.Labels...)

	// If AddConfigProfileLabels is true, add labels from config profile
	if input.AddConfigProfileLabels {
		configProfile, missing, err := getConfigProfile(a.cfg.ProfilesDir, nodeSet.ConfigProfile)
		if err != nil {
			return LabelNodeResult{}, fmt.Errorf("failed to get config profile: %w", err)
		}
		if missing {
			logger.Warn("config profile file not found, treating as empty (profile retired)",
				"configProfile", nodeSet.ConfigProfile)
		}
		labels = append(labels, configProfile.Labels...)
	}

	// Append Forge topology labels (site/vpc) resolved from CWE + the cached
	// Forge provider. A name-resolution miss skips only that *-name label; it
	// never fails provisioning (see topologyLabels).
	if input.AddTopologyLabels {
		topo, err := a.topologyLabels(ctx, node, nodePool)
		if err != nil {
			return LabelNodeResult{}, fmt.Errorf("failed to resolve topology labels: %w", err)
		}
		labels = append(labels, topo...)
	}

	if len(labels) == 0 {
		return LabelNodeResult{}, fmt.Errorf("no labels to apply to node (AddConfigProfileLabels=%v, explicit labels provided=%v)",
			input.AddConfigProfileLabels, len(input.Labels) > 0)
	}

	logger.Info("Labeling node", "nodeID", input.NodeID, "labels", labels)
	labelChanges, err := parseNodeLabelChanges(labels)
	if err != nil {
		return LabelNodeResult{}, err
	}

	cluster, err := a.db.GetCluster(ctx, nodePool.ClusterID)
	if err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return LabelNodeResult{}, err
	}

	var kubeNode corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: node.Name}, &kubeNode); err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to get Kubernetes node %s: %w", node.Name, err)
	}

	before := kubeNode.DeepCopy()
	applyNodeLabelChanges(&kubeNode, labelChanges)
	if err := tenantClient.Patch(ctx, &kubeNode, client.MergeFrom(before)); err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to label Kubernetes node %s: %w", node.Name, err)
	}

	return LabelNodeResult{Output: fmt.Sprintf("node/%s labeled", node.Name)}, nil
}

type nodeLabelChange struct {
	key    string
	value  string
	remove bool
}

func parseNodeLabelChanges(labels []string) ([]nodeLabelChange, error) {
	changes := make([]nodeLabelChange, 0, len(labels))
	for _, label := range labels {
		if !strings.Contains(label, "=") && strings.HasSuffix(label, "-") {
			key := strings.TrimSuffix(label, "-")
			if errs := validation.IsQualifiedName(key); len(errs) > 0 {
				return nil, fmt.Errorf("invalid label %q: %s", label, strings.Join(errs, "; "))
			}
			changes = append(changes, nodeLabelChange{key: key, remove: true})
			continue
		}

		key, value, ok := strings.Cut(label, "=")
		if !ok {
			return nil, fmt.Errorf("invalid label %q: expected key=value or key-", label)
		}
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return nil, fmt.Errorf("invalid label %q: %s", label, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return nil, fmt.Errorf("invalid label %q: %s", label, strings.Join(errs, "; "))
		}
		changes = append(changes, nodeLabelChange{key: key, value: value})
	}
	return changes, nil
}

func applyNodeLabelChanges(node *corev1.Node, changes []nodeLabelChange) {
	for _, change := range changes {
		if change.remove {
			delete(node.Labels, change.key)
			continue
		}
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		node.Labels[change.key] = change.value
	}
}

// topologyLabels resolves the Forge topology labels for a node:
//
//	nke.nvidia.com/site-id|site-name|vpc-id|vpc-name
//
// ID labels (site-id, vpc-id) come straight from the cluster row and are passed
// through byte-stable. Name labels require cached Forge-provider reads; a miss
// (transient cache expiry, value not found) skips only that *-name label and is
// logged, never failing provisioning — the *-id labels still apply and the
// activity retry self-heals a transient miss. Only an unexpected DB failure
// fetching the cluster is returned as an error.
//
// node.kubernetes.io/instance-type is intentionally NOT set here — see the
// LabelKeyNode* block in internal/constants for the Prometheus SLO rationale.
func (a *Activities) topologyLabels(ctx context.Context, node db.Node, nodePool db.NodePool) ([]string, error) {
	logger := activity.GetLogger(ctx)

	cluster, err := a.db.GetCluster(ctx, nodePool.ClusterID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}

	orgID := cluster.OrgID
	siteID := cluster.SiteID.String()

	var labels []string

	if cluster.SiteID != uuid.Nil {
		labels = append(labels, constants.LabelKeyNodeSiteID+"="+cluster.SiteID.String())

		if site, err := a.forgeProvider.GetSiteByID(ctx, orgID, siteID); err != nil {
			logger.Warn("skipping site-name label: failed to resolve site",
				"nodeID", node.ID, "siteID", siteID, "error", err)
		} else if name := kube.NormalizeLabelValue(site.GetName()); name != "" {
			labels = append(labels, constants.LabelKeyNodeSiteName+"="+name)
		}
	}

	if cluster.VpcID != uuid.Nil {
		labels = append(labels, constants.LabelKeyNodeVPCID+"="+cluster.VpcID.String())

		if name := a.resolveVPCName(ctx, orgID, siteID, cluster.VpcID.String()); name != "" {
			labels = append(labels, constants.LabelKeyNodeVPCName+"="+name)
		} else {
			logger.Warn("skipping vpc-name label: vpc not resolved",
				"nodeID", node.ID, "vpcID", cluster.VpcID.String())
		}
	}

	// NOTE: node.kubernetes.io/instance-type is intentionally NOT set. Prod tenant
	// Prometheus SLO rules exact-match instance_type="cpu" to exclude CPU nodes
	// from GPU capacity/health math; the Forge instance-type name ("cpu.t1") would
	// break that match. The upstream/kubelet-provided value is left untouched.

	return labels, nil
}

// resolveVPCName returns the normalized VPC name for vpcID, or "" if the VPC
// list cannot be read or the id is not present in the cached set.
func (a *Activities) resolveVPCName(ctx context.Context, orgID, siteID, vpcID string) string {
	vpcs, err := a.forgeProvider.GetVPCs(ctx, orgID, siteID)
	if err != nil {
		activity.GetLogger(ctx).Warn("failed to list VPCs for vpc-name label", "siteID", siteID, "error", err)
		return ""
	}
	for _, vpc := range vpcs {
		if vpc.GetId() == vpcID {
			return kube.NormalizeLabelValue(vpc.GetName())
		}
	}
	return ""
}
