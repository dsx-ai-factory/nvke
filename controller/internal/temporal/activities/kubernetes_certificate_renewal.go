// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/runbook"
)

const (
	// CertificateRenewalLegacyRequired selects the legacy executor before any typed work was accepted.
	CertificateRenewalLegacyRequired = "CertificateRenewalLegacyRequired"

	legacyKubernetesCertificateRenewalRunbook = "k8s-cert-renewal"
	certificateRenewalStatusPollInterval      = 5 * time.Second
	renewalOutcomeMarkerPrefix                = "RENEWAL_OUTCOME="
)

// KubernetesCertificateRenewalInput identifies one node-local renewal
// operation. OperationID and renewal parameters must remain stable across retries:
// the agent reuses records by ID, not by comparing request parameters. A positive
// ExpectedEtcdMembers opts into V2 and must come from the workflow's fixed snapshot.
type KubernetesCertificateRenewalInput struct {
	NodeID              uuid.UUID
	OperationID         string
	ThresholdDays       int
	Force               bool
	ExpectedEtcdMembers int // Zero is reserved for V1 workflow replay.
}

// KubernetesCertificateRenewalResult is the bounded controller-facing result
// for either the typed operation or the legacy mixed-fleet fallback. Successful
// V2 calls always populate Outcome; Output is retained for diagnostics only.
type KubernetesCertificateRenewalResult struct {
	Outcome      db.CertificateRenewalOutcome
	Output       string
	ErrorMessage string
	ExitCode     int
}

// RenewKubernetesCertificates renews certificates through the typed node-agent
// operation, polling it to a terminal state. Inputs without an expected etcd
// member count retain the V1 RPC for existing workflow histories; hardened
// inputs use the capability-distinct V2 RPC. V2 reports a definitive first-attempt
// Unimplemented to the workflow so it can durably select the legacy executor.
// Later V2 attempts and status errors never change execution mechanisms.
// Cancellation stops this activity's observation, not accepted node work; the
// workflow must resolve the selected operation before releasing ownership.
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
	if input.ExpectedEtcdMembers < 0 || input.ExpectedEtcdMembers > math.MaxInt32 {
		return KubernetesCertificateRenewalResult{}, fmt.Errorf("expected etcd members must be between 0 and %d", math.MaxInt32)
	}
	if input.Force && input.ExpectedEtcdMembers == 0 {
		return KubernetesCertificateRenewalResult{}, errors.New("forced renewal requires an expected etcd member count")
	}

	var operation *nkeproto.KubernetesCertificateRenewalOperation
	if input.ExpectedEtcdMembers == 0 {
		var response *nkeproto.StartKubernetesCertificateRenewalResponse
		response, err = a.nodeAgentClient.StartKubernetesCertificateRenewal(
			ctx,
			node,
			&nkeproto.StartKubernetesCertificateRenewalRequest{
				OperationId:   input.OperationID,
				ThresholdDays: int32(input.ThresholdDays),
			},
		)
		if response != nil {
			operation = response.GetOperation()
		}
	} else {
		var response *nkeproto.StartKubernetesCertificateRenewalV2Response
		response, err = a.nodeAgentClient.StartKubernetesCertificateRenewalV2(
			ctx,
			node,
			&nkeproto.StartKubernetesCertificateRenewalV2Request{
				OperationId:         input.OperationID,
				ThresholdDays:       int32(input.ThresholdDays),
				Force:               input.Force,
				ExpectedEtcdMembers: int32(input.ExpectedEtcdMembers),
			},
		)
		if response != nil {
			operation = response.GetOperation()
		}
	}
	if err != nil {
		if status.Code(err) != codes.Unimplemented {
			return KubernetesCertificateRenewalResult{}, fmt.Errorf("failed to start certificate renewal on node %s: %w", node.Name, err)
		}
		if input.ExpectedEtcdMembers > 0 {
			// A prior attempt may have accepted work but lost its response. Even
			// Unimplemented cannot safely select a different executor on retry.
			if activity.GetInfo(ctx).Attempt != 1 {
				return KubernetesCertificateRenewalResult{}, fmt.Errorf("cannot select legacy renewal after an ambiguous typed attempt: %w", err)
			}
			return KubernetesCertificateRenewalResult{}, temporal.NewNonRetryableApplicationError(
				"node agent does not implement certificate renewal V2", CertificateRenewalLegacyRequired, err,
			)
		}
		return a.RenewLegacyKubernetesCertificates(ctx, input)
	}
	if operation == nil {
		return KubernetesCertificateRenewalResult{}, errors.New("node agent returned an empty certificate renewal operation")
	}

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

// RenewLegacyKubernetesCertificates stays on the legacy executor selected by a
// V2 workflow (or reached by V1's inline fallback). V2 success requires both an
// outcome marker and a durable receipt verified under the runbook lock, because
// output alone cannot prevent delayed attempts from mutating a later cycle.
// Receipt-unaware overrides cannot satisfy that contract. V1 retains its older
// output-only result without receipt validation.
func (a *Activities) RenewLegacyKubernetesCertificates(ctx context.Context, input KubernetesCertificateRenewalInput) (KubernetesCertificateRenewalResult, error) {
	runbookEnv := map[string]string{
		"FORCE":          fmt.Sprintf("%t", input.Force),
		"OPERATION_ID":   input.OperationID,
		"THRESHOLD_DAYS": fmt.Sprintf("%d", input.ThresholdDays),
	}
	if input.ExpectedEtcdMembers > 0 {
		runbookEnv["EXPECTED_ETCD_MEMBERS"] = fmt.Sprintf("%d", input.ExpectedEtcdMembers)
	}
	legacy, legacyErr := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      input.NodeID,
		RunbookName: legacyKubernetesCertificateRenewalRunbook,
		RunbookEnv:  runbookEnv,
	})
	if legacyErr != nil {
		return KubernetesCertificateRenewalResult{}, fmt.Errorf("failed to execute legacy certificate renewal on node %s: %w", input.NodeID, legacyErr)
	}
	result := KubernetesCertificateRenewalResult{
		Output:       legacy.Output,
		ErrorMessage: legacy.ErrorMessage,
		ExitCode:     legacy.ExitCode,
	}
	if input.ExpectedEtcdMembers > 0 {
		var err error
		result.Outcome, err = certificateRenewalOutcome(result.Output)
		if err != nil {
			return result, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("invalid legacy certificate renewal result on node %s", input.NodeID),
				"InvalidCertificateRenewalOutcome", err, result,
			)
		}
		// Successful output alone cannot fence delayed legacy attempts. Require
		// the same receipt-backed evidence as failure cleanup before unlocking.
		resolutionCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := a.ResolveKubernetesCertificateRenewal(resolutionCtx, ResolveKubernetesCertificateRenewalInput{
			NodeID: input.NodeID, OperationID: input.OperationID, Legacy: true,
		}); err != nil {
			return result, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("legacy certificate renewal on node %s has no confirmed terminal receipt", input.NodeID),
				"UnconfirmedLegacyCertificateRenewal", err, result,
			)
		}
	}
	return result, nil
}

// ResolveKubernetesCertificateRenewalInput identifies the exact operation and
// executor dispatched by the workflow. Resolution never starts or resumes work.
type ResolveKubernetesCertificateRenewalInput struct {
	NodeID      uuid.UUID
	OperationID string
	Legacy      bool
}

// ResolveKubernetesCertificateRenewal returns nil only after matching terminal
// evidence from the selected executor, without starting or resuming renewal.
// Terminal failure, including failed restoration, can resolve ownership; nil does
// not mean renewal succeeded or the node is healthy. Its caller must impose a
// bounded timeout independent of workflow cancellation and retain ownership on
// error, including missing or unsupported status.
func (a *Activities) ResolveKubernetesCertificateRenewal(ctx context.Context, input ResolveKubernetesCertificateRenewalInput) error {
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to get node for certificate-renewal resolution: %w", err)
	}
	if input.Legacy {
		return a.resolveLegacyKubernetesCertificateRenewal(ctx, node, input.OperationID)
	}
	for {
		response, err := a.nodeAgentClient.GetKubernetesCertificateRenewalStatus(ctx, node,
			&nkeproto.GetKubernetesCertificateRenewalStatusRequest{OperationId: input.OperationID})
		if err != nil {
			return fmt.Errorf("failed to resolve certificate renewal operation %s: %w", input.OperationID, err)
		}
		operation := response.GetOperation()
		statusOperation := operation.GetOperation()
		if statusOperation == nil || statusOperation.GetOperationId() != input.OperationID {
			return errors.New("certificate renewal status is missing or does not match the operation ID")
		}
		switch statusOperation.GetPhase() {
		case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED:
			_, _, err := certificateRenewalTerminalResult(operation)
			return err
		case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED:
			switch operation.GetOutcome() {
			case nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART,
				nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED,
				nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED:
				return nil
			case nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED,
				nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED,
				nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED:
				return errors.New("failed certificate renewal status has an invalid outcome")
			default:
				return errors.New("failed certificate renewal status has an unknown outcome")
			}
		case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING:
			// Only status is polled. NotFound/Unimplemented never select fallback.
		case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED:
			return errors.New("certificate renewal status has an unspecified phase")
		default:
			return errors.New("certificate renewal status has an unknown phase")
		}
		timer := time.NewTimer(certificateRenewalStatusPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// resolveLegacyKubernetesCertificateRenewal requires the embedded script's exact
// terminal marker. That script synchronizes and validates an operation-keyed
// receipt while holding the renewal flock, excluding active EXIT restoration.
// Old phase files are resumable but cannot supply receipt-aware provenance, so
// neither an old complete phase nor ordinary success output releases ownership.
func (a *Activities) resolveLegacyKubernetesCertificateRenewal(ctx context.Context, node db.Node, operationID string) error {
	// Always use the embedded status-only implementation: an older configured
	// runbook override might ignore STATUS_ONLY and start renewal instead.
	script, err := runbook.Script("", legacyKubernetesCertificateRenewalRunbook)
	if err != nil {
		return err
	}
	script, err = runbook.InjectEnvironment(map[string]string{
		"STATUS_ONLY": "true", "OPERATION_ID": operationID, "LOCK_TIMEOUT": "240",
	}, script)
	if err != nil {
		return fmt.Errorf("failed to inject certificate-renewal status environment: %w", err)
	}
	response, err := a.nodeAgentClient.ExecuteScript(ctx, node, legacyKubernetesCertificateRenewalRunbook, script, nil)
	if err != nil {
		return fmt.Errorf("failed to resolve legacy certificate renewal: %w", err)
	}
	if response == nil || response.GetExitCode() != 0 ||
		strings.TrimSpace(response.GetOutput()) != "RENEWAL_TERMINAL_OPERATION_ID="+operationID {
		return errors.New("legacy certificate renewal has no matching terminal evidence")
	}
	return nil
}

// certificateRenewalOutcome accepts plain or timestamped runbook markers. The
// last recognized marker is the requested operation's result after any pending
// operation was resumed; unknown markers fail rather than become a no-op.
func certificateRenewalOutcome(output string) (db.CertificateRenewalOutcome, error) {
	var outcome db.CertificateRenewalOutcome
	for line := range strings.SplitSeq(output, "\n") {
		marker := strings.TrimSpace(line)
		if timestampEnd := strings.Index(marker, "] "); strings.HasPrefix(marker, "[") && timestampEnd >= 0 {
			marker = marker[timestampEnd+2:]
		}
		value, ok := strings.CutPrefix(marker, renewalOutcomeMarkerPrefix)
		if !ok {
			continue
		}
		switch candidate := db.CertificateRenewalOutcome(value); candidate {
		case db.CertificateRenewalOutcomeUnchanged, db.CertificateRenewalOutcomeRenewed:
			outcome = candidate
		default:
			return "", fmt.Errorf("unknown outcome %q", value)
		}
	}
	if outcome == "" {
		return "", errors.New("renewal outcome marker is missing")
	}
	return outcome, nil
}

// certificateRenewalTerminalResult distinguishes ongoing work from a usable
// result. A succeeded phase still needs a recognized successful outcome; failed
// phases preserve restoration diagnostics rather than masquerading as a no-op.
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
			outcome := db.CertificateRenewalOutcomeUnchanged
			if operation.GetOutcome() == nkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED {
				outcome = db.CertificateRenewalOutcomeRenewed
			}
			return KubernetesCertificateRenewalResult{
				Outcome: outcome,
				Output:  fmt.Sprintf("certificate renewal %s", operation.GetOutcome()),
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
