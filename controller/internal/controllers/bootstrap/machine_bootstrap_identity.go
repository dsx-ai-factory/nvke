// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func applyBootstrapIdentityAnnotation(
	ctx context.Context,
	k8sClient client.Client,
	machine *clusterv1.Machine,
	bootstrapIdentity string,
	fieldOwner string,
) error {
	applyMachine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      machine.Name,
			Namespace: machine.Namespace,
			Annotations: map[string]string{
				keys.MachineBootstrapIdentity.String(): bootstrapIdentity,
			},
		},
	}

	if err := k8sClient.Patch(
		ctx,
		applyMachine,
		client.Apply,
		client.FieldOwner(fieldOwner),
		client.ForceOwnership,
	); err != nil {
		return fmt.Errorf("failed to apply machine bootstrap identity annotation: %w", err)
	}
	return nil
}

func removeBootstrapIdentityAndFinalizer(
	ctx context.Context,
	k8sClient client.Client,
	machine *clusterv1.Machine,
	expectedIdentity string,
	finalizer string,
) error {
	patchMachine := machine.DeepCopy()
	controllerutil.RemoveFinalizer(patchMachine, finalizer)

	ownsIdentity := keys.MachineBootstrapIdentity.Value(machine.GetAnnotations()) == expectedIdentity
	if ownsIdentity {
		annotations := patchMachine.GetAnnotations()
		keys.MachineBootstrapIdentity.Delete(annotations)
		patchMachine.SetAnnotations(annotations)
	}

	patch := client.MergeFromWithOptions(machine, client.MergeFromWithOptimisticLock{})
	if err := k8sClient.Patch(ctx, patchMachine, patch); err != nil {
		return fmt.Errorf("failed to remove machine bootstrap identity state: %w", err)
	}
	return nil
}
