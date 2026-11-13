// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
)

// FailureDomainUnavailable reports whether CAPNICo's latest instance create for
// machine failed because NICo had no machine free in the requested failure
// domain. A NicoMachine that does not exist yet reports false.
func (*Client) FailureDomainUnavailable(ctx context.Context, deps shared.MachineAdvanceDeps, machine *clusterv1.Machine) (bool, error) {
	nicoMachine := &nicov1.NicoMachine{}
	if err := deps.K8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}, nicoMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, machine.Spec.InfrastructureRef.Name, err)
	}
	return isFailureDomainUnavailable(nicoMachine), nil
}

func isFailureDomainUnavailable(nicoMachine *nicov1.NicoMachine) bool {
	provisioned := nicoMachineProvisionedCondition(nicoMachine.Status.Conditions)
	return provisioned != nil &&
		provisioned.Status == metav1.ConditionFalse &&
		provisioned.Reason == nicov1.FailureDomainUnavailableReason &&
		// A condition from an older generation predates a spec change CAPNICo has not retried yet.
		(provisioned.ObservedGeneration == 0 || provisioned.ObservedGeneration >= nicoMachine.Generation)
}
