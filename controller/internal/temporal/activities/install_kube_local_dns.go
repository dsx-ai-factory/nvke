// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

//go:embed kube_local_dns.yaml
var kubeLocalDNSManifest []byte

type InstallKubeLocalDNSInput struct {
	ClusterID uuid.UUID

	ClusterProvisioningConditionMessage string
}

// InstallKubeLocalDNS applies the node-local DNS resources directly to a tenant cluster.
func (a *Activities) InstallKubeLocalDNS(ctx context.Context, input InstallKubeLocalDNSInput) error {
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

			if err := applyKubeLocalDNSManifest(ctx, tenantClient); err != nil {
				return struct{}{}, err
			}

			return struct{}{}, nil
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

func applyKubeLocalDNSManifest(ctx context.Context, tenantClient ctrlclient.Client) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(kubeLocalDNSManifest), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("failed to decode kube-local-dns manifest: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}

		if err := tenantClient.Apply(
			ctx,
			ctrlclient.ApplyConfigurationFromUnstructured(obj),
			ctrlclient.FieldOwner(constants.FieldManagerCWE),
			ctrlclient.ForceOwnership,
		); err != nil {
			return fmt.Errorf("failed to apply kube-local-dns %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
}
