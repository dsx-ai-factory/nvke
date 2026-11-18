// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// RotateFailureDomain passes deps.RotateFailureDomain whether CAPNICo reports the
// failure domain machine requests out of capacity. It is a no-op when the hook is nil.
func RotateFailureDomain(ctx context.Context, deps shared.MachineAdvanceDeps, node db.Node, machine *clusterv1.Machine) (bool, error) {
	if deps.RotateFailureDomain == nil {
		return false, nil
	}
	unavailable, err := failureDomainUnavailable(ctx, deps.K8sClient, machine)
	if err != nil {
		return false, err
	}
	return deps.RotateFailureDomain(ctx, node, machine, unavailable)
}

// failureDomainUnavailable reports whether CAPNICo's latest instance create for
// machine failed because NICo had no machine free in the domain machine requests
// now. A NicoMachine that does not exist yet reports false.
func failureDomainUnavailable(ctx context.Context, k8sClient client.Client, machine *clusterv1.Machine) (bool, error) {
	if machine.Spec.FailureDomain == "" {
		return false, nil
	}
	nicoMachine := &nicov1.NicoMachine{}
	if err := k8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}, nicoMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, machine.Spec.InfrastructureRef.Name, err)
	}
	return isFailureDomainUnavailable(nicoMachine, machine.Spec.FailureDomain), nil
}

func isFailureDomainUnavailable(nicoMachine *nicov1.NicoMachine, domain string) bool {
	provisioned := nicoMachineProvisionedCondition(nicoMachine.Status.Conditions)
	return nicoMachine.Status.InstanceID == "" &&
		provisioned != nil &&
		provisioned.Status == metav1.ConditionFalse &&
		provisioned.Reason == nicov1.FailureDomainUnavailableReason &&
		(provisioned.ObservedGeneration == 0 || provisioned.ObservedGeneration >= nicoMachine.Generation) &&
		// A Machine patch does not bump the NicoMachine generation; CAPNICo names the
		// domain in the message, which keeps a report about the previous domain from counting.
		strings.HasPrefix(provisioned.Message, fmt.Sprintf("failure domain %q:", domain))
}
