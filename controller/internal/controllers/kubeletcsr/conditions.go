// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeletcsr

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
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

const (
	// KubeletServingCSRApprovalReadyCondition reports whether the controller can evaluate kubelet serving CSRs for this Machine.
	KubeletServingCSRApprovalReadyCondition = keys.Domain + "/KubeletServingCSRApprovalReady"

	// KubeletServingCSRApprovalReadyReason means the controller can evaluate kubelet serving CSRs for this Machine.
	KubeletServingCSRApprovalReadyReason = "Ready"
	// KubeletServingCSRWaitingForNodeReason means CAPI has not associated the Machine with a Node yet.
	KubeletServingCSRWaitingForNodeReason = "WaitingForNodeAssociation"
	// KubeletServingCSRWaitingForAddressesReason means the Machine has no provider addresses yet.
	KubeletServingCSRWaitingForAddressesReason = "WaitingForAddresses"
	// KubeletServingCSRNodeNameConflictReason means the node name is associated with more than one Machine.
	KubeletServingCSRNodeNameConflictReason = "NodeNameConflict"
	// KubeletServingCSRFailedReason means an operation failed while approving a serving CSR. The message is the error.
	KubeletServingCSRFailedReason = "ServingCSRFailed"
	// KubeletServingCSROptedOutReason means the Machine is no longer opted in to kubelet serving CSR approval.
	KubeletServingCSROptedOutReason = "ServingCSRApprovalOptedOut"
)

const (
	// KubeletCSRApprovedReason is the Event reason when a kubelet CSR is approved.
	KubeletCSRApprovedReason = "KubeletCSRApproved"
	// KubeletServingCSRRejectedReason is the Event reason when a serving CSR fails validation.
	KubeletServingCSRRejectedReason = "ServingCSRRejected"
)

const (
	approveCSRAction      = "ApproveCSR"
	updateConditionAction = "UpdateCondition"
)

func setMachineCondition(
	ctx context.Context,
	k8sClient client.Client,
	machine *clusterv1.Machine,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
	fieldOwner string,
	recorder events.EventRecorder,
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
	if recorder != nil {
		recorder.Eventf(machine, nil, corev1.EventTypeNormal, reason, updateConditionAction, "%s", message)
	}
	return nil
}
