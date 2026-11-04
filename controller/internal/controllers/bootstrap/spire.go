// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeadmv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	spireWorkloadEntryDefaultSelector = "unix:path:/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth"
	spireWorkloadEntryDefaultHint     = "kubeadm-join"
)

// spireWorkloadEntryConfig is the ClusterStaticEntry hint and selectors for a kubeadm join workload.
// The published values must correlate with spec.joinConfiguration.discovery.file.kubeConfig.user.exec
// on the same KubeadmConfig so that discovery process can obtain the SVID. They are not a copy of the
// exec fields.
//
// For the reference plugin, one selector is the unix:path of exec.command and the hint is the hint
// that command requests through SPIFFE_JWT_HINT. Another plugin can request the hint differently, or
// select the process with selectors other than that path:
//
//	joinConfiguration:
//	  discovery:
//	    file:
//	      kubeConfig:
//	        user:
//	          exec:
//	            command: /opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth
//	            env:
//	              - name: SPIFFE_JWT_HINT
//	                value: kubeadm-join
//
// Hint is optional. It only disambiguates which SVID a workload takes when several entries match the same
// selectors. A command that sends no hint needs the field empty.
//
// Selectors are required. SPIRE validates each selector's syntax. Every selector on the entry is ANDed:
// a second value does not mean an alternate path.
type spireWorkloadEntryConfig struct {
	Hint      string   `json:"hint,omitempty"`
	Selectors []string `json:"selectors,omitempty"`
}

// spireWorkloadEntryConfigForKubeadmConfig returns the entry config for kubeadmConfig.
func spireWorkloadEntryConfigForKubeadmConfig(kubeadmConfig *kubeadmv1.KubeadmConfig) (spireWorkloadEntryConfig, error) {
	raw := keys.BootstrapIdentitySpireWorkloadEntryConfig.Value(kubeadmConfig.GetAnnotations())
	if raw == "" {
		return spireWorkloadEntryConfig{
			Hint:      spireWorkloadEntryDefaultHint,
			Selectors: []string{spireWorkloadEntryDefaultSelector},
		}, nil
	}

	var entryConfig spireWorkloadEntryConfig
	annotation := keys.BootstrapIdentitySpireWorkloadEntryConfig.String()
	if err := json.Unmarshal([]byte(raw), &entryConfig); err != nil {
		return spireWorkloadEntryConfig{}, fmt.Errorf("invalid %s annotation: %w", annotation, err)
	}
	if len(entryConfig.Selectors) == 0 {
		return spireWorkloadEntryConfig{}, fmt.Errorf("invalid %s annotation: selectors must be set", annotation)
	}

	return entryConfig, nil
}

// spireBootstrapIdentity returns the SPIFFE ID for the bootstrap identity of a machine.
//
// Example: spiffe://<trust-domain>/bootstrap/<machine-uid>
func spireBootstrapIdentity(trustDomain string, machine *clusterv1.Machine) string {
	return fmt.Sprintf("spiffe://%s%s%s", trustDomain, constants.BootstrapSpiffeIDPathPrefix, machine.UID)
}

func clusterStaticEntryName(machine *clusterv1.Machine) string {
	return "bootstrap-" + string(machine.UID)
}

func applyClusterStaticEntry(
	ctx context.Context,
	k8sClient client.Client,
	machine *clusterv1.Machine,
	spireClassName string,
	bootstrapSpiffeID string,
	parentID string,
	entryConfig spireWorkloadEntryConfig,
	fieldOwner string,
) error {
	cse := &spirev1alpha1.ClusterStaticEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
			Kind:       "ClusterStaticEntry",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterStaticEntryName(machine),
			Labels: map[string]string{
				keys.ManagedBy.String(): fieldOwner,
			},
		},
		Spec: spirev1alpha1.ClusterStaticEntrySpec{
			ClassName: spireClassName,
			ParentID:  parentID,
			Selectors: entryConfig.Selectors,
			SPIFFEID:  bootstrapSpiffeID,
			Hint:      entryConfig.Hint,
		},
	}

	if err := k8sClient.Patch(
		ctx,
		cse,
		client.Apply,
		client.FieldOwner(fieldOwner),
		client.ForceOwnership,
	); err != nil {
		return fmt.Errorf("failed to apply cluster static entry %s: %w", cse.Name, err)
	}
	return nil
}

func deleteClusterStaticEntry(ctx context.Context, k8sClient client.Client, machine *clusterv1.Machine) error {
	cse := &spirev1alpha1.ClusterStaticEntry{}
	cse.SetName(clusterStaticEntryName(machine))

	if err := k8sClient.Delete(ctx, cse); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete cluster static entry %s: %w", cse.GetName(), err)
	}
	return nil
}
