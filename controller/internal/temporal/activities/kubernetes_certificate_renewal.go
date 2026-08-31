// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

const (
	legacyKubernetesCertificateRenewalRunbook = "k8s-cert-renewal"
	certificateRenewalStatusPollInterval      = 5 * time.Second
)

// KubernetesCertificateRenewalInput identifies one node-local renewal
// operation. OperationID must remain stable across Temporal activity retries.
type KubernetesCertificateRenewalInput struct {
	NodeID        uuid.UUID
	OperationID   string
	ThresholdDays int
}

// KubernetesCertificateRenewalResult is the bounded controller-facing result
// for either the typed operation or the legacy mixed-fleet fallback.
type KubernetesCertificateRenewalResult struct {
	Output       string
	ErrorMessage string
	ExitCode     int
}

// RenewKubernetesCertificates renews certificates through the typed node-agent
// operation, polling it to a terminal state. Only an initial Unimplemented
// response may select the legacy runbook; status errors never change execution
// mechanisms after the typed operation has been accepted.
func (a *Activities) RenewKubernetesCertificates(
	ctx context.Context,
	input KubernetesCertificateRenewalInput,
) (KubernetesCertificateRenewalResult, error) {
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return KubernetesCertificateRenewalResult{}, fmt.Errorf("failed to get node: %w", err)
	}
	if input.ThresholdDays <= 0 || input.ThresholdDays > math.MaxInt32 {
		return KubernetesCertificateRenewalResult{}, fmt.Errorf("threshold days must be between 1 and %d", math.MaxInt32)
	}
	response, err := a.nodeAgentClient.StartKubernetesCertificateRenewal(ctx, node, &nkeproto.StartKubernetesCertificateRenewalRequest{
		OperationId:   input.OperationID,
		ThresholdDays: int32(input.ThresholdDays),
	})
	if err != nil {
		if status.Code(err) != codes.Unimplemented {
			return KubernetesCertificateRenewalResult{}, fmt.Errorf("failed to start certificate renewal on node %s: %w", node.Name, err)
		}
		legacy, legacyErr := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
			HostID:      node.ID,
			RunbookName: legacyKubernetesCertificateRenewalRunbook,
			RunbookEnv: map[string]string{
				"THRESHOLD_DAYS": fmt.Sprintf("%d", input.ThresholdDays),
			},
		})
		if legacyErr != nil {
			return KubernetesCertificateRenewalResult{}, fmt.Errorf("failed to execute legacy certificate renewal on node %s: %w", node.Name, legacyErr)
		}
		return KubernetesCertificateRenewalResult(legacy), nil
	}
	if response == nil || response.GetOperation() == nil {
		return KubernetesCertificateRenewalResult{}, errors.New("node agent returned an empty certificate renewal operation")
	}

	operation := response.GetOperation()
	waitBeforePoll := false
	for {
		result, done, err := certificateRenewalTerminalResult(operation)
		if err != nil || done {
			return result, err
		}
		if waitBeforePoll {
			timer := time.NewTimer(certificateRenewalStatusPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return KubernetesCertificateRenewalResult{}, ctx.Err()
			case <-timer.C:
			}
		}
		statusResponse, statusErr := a.nodeAgentClient.GetKubernetesCertificateRenewalStatus(
			ctx,
			node,
			&nkeproto.GetKubernetesCertificateRenewalStatusRequest{OperationId: input.OperationID},
		)
		if statusErr != nil {
			return KubernetesCertificateRenewalResult{}, fmt.Errorf(
				"failed to get accepted certificate renewal operation %s from node %s: %w",
				input.OperationID,
				node.Name,
				statusErr,
			)
		}
		if statusResponse == nil || statusResponse.GetOperation() == nil {
			return KubernetesCertificateRenewalResult{}, errors.New("node agent returned an empty certificate renewal status")
		}
		operation = statusResponse.GetOperation()
		waitBeforePoll = true
	}
}

func certificateRenewalTerminalResult(
	operation *nkeproto.KubernetesCertificateRenewalOperation,
) (KubernetesCertificateRenewalResult, bool, error) {
	statusOperation := operation.GetOperation()
	if statusOperation == nil {
		return KubernetesCertificateRenewalResult{}, true, errors.New("certificate renewal operation status is empty")
	}
	switch statusOperation.GetPhase() {
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING:
		return KubernetesCertificateRenewalResult{}, false, nil
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED:
		switch operation.GetOutcome() {
		case nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED,
			nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED:
			return KubernetesCertificateRenewalResult{
				Output: fmt.Sprintf("certificate renewal %s", operation.GetOutcome()),
			}, true, nil
		case nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED,
			nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART,
			nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED,
			nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED:
			return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
				"certificate renewal operation %s succeeded with invalid outcome %s",
				statusOperation.GetOperationId(),
				operation.GetOutcome(),
			)
		default:
			return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
				"certificate renewal operation %s succeeded with unknown outcome %d",
				statusOperation.GetOperationId(),
				operation.GetOutcome(),
			)
		}
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED:
		return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
			"certificate renewal operation %s failed at %s with outcome %s: %s",
			statusOperation.GetOperationId(),
			statusOperation.GetStep(),
			operation.GetOutcome(),
			statusOperation.GetErrorMessage(),
		)
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED:
		return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
			"certificate renewal operation %s returned invalid phase %s",
			statusOperation.GetOperationId(),
			statusOperation.GetPhase(),
		)
	default:
		return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
			"certificate renewal operation %s returned unknown phase %d",
			statusOperation.GetOperationId(),
			statusOperation.GetPhase(),
		)
	}
}
