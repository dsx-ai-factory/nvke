// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

func (a *Activities) isDockerMachineBackedNode(ctx context.Context, cluster db.Cluster, node db.Node) (bool, error) {
	// LOCAL-DEV ONLY: the local NICo container flow provisions nodes as bare Docker
	// containers with no real cloud-init, so force them through the
	// DockerMachine bootstrap/preflight (bootstrap-success sentinel) +
	// container-mode kubeadm contract. This flag must NEVER be set in a real
	// environment; when it is off the normal IsDockerMachineTemplate path below
	// is unchanged.
	if a.cfg.LocalDevForceDockerMachineBootstrap {
		return true, nil
	}

	// Intentionally not routed through a provider-capability client: Docker
	// Machine bootstrap is a CAPI/CAPD-only concept with no equivalent on any
	// other provider, so adding a provider never requires touching this
	// guard — it already answers false for anything that isn't CAPI. Revisit
	// only if a future provider gains its own Docker-Machine-equivalent local
	// dev path.
	if cluster.ProviderType != db.ProviderTypeCAPI {
		return false, nil
	}

	// GetNodeContext resolves the effective template for both node shapes we can
	// see here: nodepool-managed nodes (from their node set) and control-plane
	// nodes (from their pinned snapshot / cluster config). It errors on a
	// non-nodepool worker, which is an invalid state.
	nodeSet, _, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return false, fmt.Errorf("failed to resolve node context: %w", err)
	}

	capiNodeSetProvider, err := nodeSet.CAPIProvider()
	if err != nil {
		return false, nil
	}

	if capi.IsEmptyManifest(capiNodeSetProvider.InfraMachineTemplate) {
		return false, nil
	}

	isDockerMachineTemplate, err := capi.IsDockerMachineTemplate(capiNodeSetProvider.InfraMachineTemplate)
	if err != nil {
		return false, fmt.Errorf("failed to inspect CAPI infra machine template: %w", err)
	}
	return isDockerMachineTemplate, nil
}

func waitForObjectDeleted(ctx context.Context, k8sClient client.Client, obj client.Object) error {
	key := client.ObjectKeyFromObject(obj)
	description := fmt.Sprintf("%s %s/%s", obj.GetObjectKind().GroupVersionKind().Kind, key.Namespace, key.Name)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		probe, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return fmt.Errorf("failed to create probe object for %s", description)
		}

		err := k8sClient.Get(ctx, key, probe)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to get %s while waiting for deletion: %w", description, err)
		}

		activity.RecordHeartbeat(ctx, description)

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for deletion of %s: %w", description, ctx.Err())
		case <-ticker.C:
		}
	}
}
