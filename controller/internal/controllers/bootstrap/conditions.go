// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	// BootstrapIdentityResolvedCondition reports whether the Machine's expected bootstrap username has been resolved.
	BootstrapIdentityResolvedCondition = keys.Domain + "/BootstrapIdentityResolved"

	// BootstrapIdentityResolvedReason means the Machine bootstrap identity annotation was published.
	BootstrapIdentityResolvedReason = "BootstrapIdentityResolved"
	// BootstrapIdentityFailedReason means an operational failure prevented the bootstrap identity from being resolved or published.
	BootstrapIdentityFailedReason = "BootstrapIdentityFailed"
	// BootstrapIdentityInfrastructureInvalidReason means the Machine infrastructure is incompatible with the selected resolver.
	BootstrapIdentityInfrastructureInvalidReason = "InfrastructureInvalid"
	// BootstrapIdentityWaitingForInfrastructureReason means the controller is waiting for the Machine infrastructure resource.
	BootstrapIdentityWaitingForInfrastructureReason = "WaitingForInfrastructure"
	// BootstrapIdentityWaitingForIdentityDataReason means the controller is waiting for identity data from the infrastructure provider.
	BootstrapIdentityWaitingForIdentityDataReason = "WaitingForIdentityData"
	// BootstrapIdentityConfigurationInvalidReason means the selected resolver configuration is invalid.
	BootstrapIdentityConfigurationInvalidReason = "ConfigurationInvalid"
)

const (
	// BootstrapAuthorizationSyncedCondition reports whether the Machine's tenant bootstrap grants match its desired state.
	BootstrapAuthorizationSyncedCondition = keys.Domain + "/BootstrapAuthorizationSynced"

	// BootstrapAuthorizationSyncedReason means the tenant bootstrap bindings contain the desired Machine identities.
	BootstrapAuthorizationSyncedReason = "BootstrapAuthorizationSynced"
	// BootstrapAuthorizationWaitingForIdentityReason means the selected Machine has no resolved bootstrap identity yet.
	BootstrapAuthorizationWaitingForIdentityReason = "WaitingForBootstrapIdentity"
	// BootstrapAuthorizationWaitingForTenantAccessReason means the tenant Kubernetes API is not reachable yet.
	BootstrapAuthorizationWaitingForTenantAccessReason = "WaitingForTenantAccess"
	// BootstrapAuthorizationDuplicateIdentityReason means multiple selected Machines have the same resolved bootstrap identity.
	BootstrapAuthorizationDuplicateIdentityReason = "DuplicateBootstrapIdentity"
	// BootstrapAuthorizationSubjectLimitExceededReason means the cluster has more joining identities than the configured authorization subject limit.
	BootstrapAuthorizationSubjectLimitExceededReason = "BootstrapAuthorizationSubjectLimitExceeded"
	// BootstrapAuthorizationSyncFailedReason means the tenant bootstrap bindings could not be reconciled.
	BootstrapAuthorizationSyncFailedReason = "BootstrapAuthorizationSyncFailed"
)

const (
	// KubeletClientCertificateIssuedCondition reports whether the Machine's initial kubelet client certificate was issued.
	KubeletClientCertificateIssuedCondition = keys.Domain + "/KubeletClientCertificateIssued"

	// KubeletClientCertificateIssuedReason means a validated kubelet client certificate is present on the tenant CSR.
	KubeletClientCertificateIssuedReason = "ClientCertificateIssued"
	// KubeletClientCSRWaitingForBootstrapIdentityReason means the Machine has not joined and its bootstrap identity is not set.
	KubeletClientCSRWaitingForBootstrapIdentityReason = "WaitingForBootstrapIdentity"
	// KubeletClientCSRWaitingReason means the bootstrap identity is set and the client CSR has not arrived.
	KubeletClientCSRWaitingReason = "WaitingForClientCSR"
	// KubeletClientCSRInvalidReason means the controller will not approve a client certificate for this Machine.
	KubeletClientCSRInvalidReason = "ClientCSRInvalid"
	// KubeletClientCSRWaitingForSigningReason means the client CSR is approved and the signer has not issued the certificate yet.
	KubeletClientCSRWaitingForSigningReason = "WaitingForClientCertificateSigning"
	// KubeletClientCSRFailedReason means an operation failed while approving the client CSR. The message is the error.
	KubeletClientCSRFailedReason = "ClientCSRFailed"
	// KubeletClientCSRApprovalReason is recorded on the tenant CSR Approved condition when this controller approves it.
	KubeletClientCSRApprovalReason = "NVKEValidatedBootstrapIdentity"
)

func SetBootstrapIdentityResolvedCondition(
	ctx context.Context,
	k8sClient client.Client,
	machine *clusterv1.Machine,
	status metav1.ConditionStatus,
	reason string,
	message string,
	fieldOwner string,
) error {
	return setMachineCondition(
		ctx,
		k8sClient,
		machine,
		BootstrapIdentityResolvedCondition,
		status,
		reason,
		message,
		fieldOwner,
	)
}

func setBootstrapAuthorizationSyncedCondition(
	ctx context.Context,
	k8sClient client.Client,
	machine *clusterv1.Machine,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return setMachineCondition(
		ctx,
		k8sClient,
		machine,
		BootstrapAuthorizationSyncedCondition,
		status,
		reason,
		message,
		constants.FieldManagerBootstrapAuthorization,
	)
}

func setKubeletClientCertificateCondition(
	ctx context.Context,
	k8sClient client.Client,
	machine *clusterv1.Machine,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return setMachineCondition(
		ctx,
		k8sClient,
		machine,
		KubeletClientCertificateIssuedCondition,
		status,
		reason,
		message,
		constants.FieldManagerKubeletClientCSR,
	)
}

func setMachineCondition(
	ctx context.Context,
	k8sClient client.Client,
	machine *clusterv1.Machine,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
	fieldOwner string,
) error {
	conditions := []metav1.Condition{}
	if current := meta.FindStatusCondition(machine.Status.Conditions, conditionType); current != nil {
		conditions = append(conditions, *current)
	}

	if !meta.SetStatusCondition(&conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: machine.Generation,
		Reason:             reason,
		Message:            message,
	}) {
		return nil
	}

	applyMachine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      machine.Name,
			Namespace: machine.Namespace,
		},
		Status: clusterv1.MachineStatus{
			Conditions: conditions,
		},
	}

	if err := k8sClient.Status().Patch(
		ctx,
		applyMachine,
		client.Apply,
		client.FieldOwner(fieldOwner),
		client.ForceOwnership,
	); err != nil {
		return fmt.Errorf("failed to patch Machine %s condition: %w", conditionType, err)
	}

	return nil
}
