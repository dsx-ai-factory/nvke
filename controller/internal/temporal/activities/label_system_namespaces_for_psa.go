// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	psaEnforceLabelKey     = "pod-security.kubernetes.io/enforce"
	psaLevelPrivileged     = "privileged"
	tigeraOperatorNS       = "tigera-operator"
	ovnKubernetesNamespace = "ovn-kubernetes"
)

type LabelSystemNamespacesForPSAInput struct {
	ClusterID uuid.UUID

	ClusterProvisioningConditionMessage string
}

// LabelSystemNamespacesForPSA labels the tenant cluster's system namespaces,
// kube-system and the CNI's namespace, privileged for Pod Security Admission.
// Those namespaces run components (kube-proxy, the CNI, node-local-dns) that
// need host access, so privileged is the level they need. The label makes that
// an explicit, auditable decision. Tenant namespaces are never touched.
func (a *Activities) LabelSystemNamespacesForPSA(ctx context.Context, input LabelSystemNamespacesForPSAInput) error {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[struct{}]{
		Message:         input.ClusterProvisioningConditionMessage,
		UpdateCondition: a.clusterProvisioningConditionMessageUpdater(input.ClusterID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (struct{}, error) {
			cluster, err := a.db.GetCluster(ctx, input.ClusterID)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					return struct{}{}, temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
				}
				return struct{}{}, fmt.Errorf("failed to get cluster %s: %w", input.ClusterID, err)
			}

			tenantClient, err := a.tenantClientForCluster(ctx, cluster)
			if err != nil {
				return struct{}{}, err
			}

			return struct{}{}, labelSystemNamespacesPrivileged(ctx, tenantClient, cluster.CniPlugin)
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

// labelSystemNamespacesPrivileged labels kube-system and the CNI's namespace
// privileged. An unrecognized plugin has no CNI namespace to label, which is a
// no-op so provisioning isn't blocked on it.
func labelSystemNamespacesPrivileged(ctx context.Context, tenantClient ctrlclient.Client, plugin db.CniPlugin) error {
	if err := labelNamespacePrivileged(ctx, tenantClient, metav1.NamespaceSystem); err != nil {
		return err
	}
	for _, ns := range cniNamespacesForPlugin(plugin) {
		if err := labelNamespacePrivileged(ctx, tenantClient, ns); err != nil {
			return err
		}
	}
	return nil
}

// cniNamespacesForPlugin returns the CNI namespaces to label. The CNI install
// creates each one synchronously and finishes before this activity starts, so
// a missing one is an error. calico-system is left out: the tigera operator
// creates it asynchronously, already labeled enforce=privileged, and keeps it
// that way.
//
// agent/internal/agent/cni.go uses the same "tigera-operator" literal for its
// Calico --namespace flag. It lives in a separate Go module, so there is no
// shared constant to import.
func cniNamespacesForPlugin(plugin db.CniPlugin) []string {
	switch plugin {
	case db.CNICalico:
		return []string{tigeraOperatorNS}
	case db.CNIOvnKubernetes, db.CNIOvnKubernetesDPUHost:
		return []string{ovnKubernetesNamespace}
	default:
		return nil
	}
}

func labelNamespacePrivileged(ctx context.Context, tenantClient ctrlclient.Client, name string) error {
	var ns corev1.Namespace
	if err := tenantClient.Get(ctx, ctrlclient.ObjectKey{Name: name}, &ns); err != nil {
		return fmt.Errorf("getting namespace %s: %w", name, err)
	}
	if ns.Labels[psaEnforceLabelKey] == psaLevelPrivileged {
		return nil
	}
	before := ns.DeepCopy()
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels[psaEnforceLabelKey] = psaLevelPrivileged
	if err := tenantClient.Patch(ctx, &ns, ctrlclient.MergeFrom(before)); err != nil {
		return fmt.Errorf("labeling namespace %s: %w", name, err)
	}
	return nil
}
